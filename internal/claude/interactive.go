package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/getfinn/finn/internal/git"
)

// SessionLinkedHandler is called when Claude's session_id is detected
type SessionLinkedHandler func(sessionID string)

// InteractiveTaskExecutor manages a long-lived interactive conversation with Claude
type InteractiveTaskExecutor struct {
	projectPath string
	git         *git.Repository
	parser      *DecisionParser
	onEvent     EventHandler

	// Session linking callback - called when Claude's session_id is detected
	onSessionLinked SessionLinkedHandler

	// Process management
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	isRunning bool
	mutex     sync.Mutex

	// Session detection
	existingSessionsBeforeStart map[string]bool // Session files that existed before Claude started
	sessionDetected             bool            // Whether we've already detected and reported the session

	// Tracking
	filesBeforeExec       []string
	lastThinkingText      string
	sentDiffs             map[string]bool // Track which files we've sent diffs for (prevent duplicates)
	diffMutex             sync.Mutex
	filesModifiedThisTurn map[string]bool // Track files written in current turn (prevent re-execution)
	turnCompleted         bool            // Track if complete event sent this turn
}

// NewInteractiveTaskExecutor creates a new interactive task executor
func NewInteractiveTaskExecutor(projectPath string, onEvent EventHandler) *InteractiveTaskExecutor {
	return &InteractiveTaskExecutor{
		projectPath:                 projectPath,
		git:                         git.NewRepository(projectPath),
		parser:                      NewDecisionParser(),
		onEvent:                     onEvent,
		isRunning:                   false,
		sentDiffs:                   make(map[string]bool),
		filesModifiedThisTurn:       make(map[string]bool),
		turnCompleted:               false,
		existingSessionsBeforeStart: make(map[string]bool),
		sessionDetected:             false,
	}
}

// SetSessionLinkedHandler sets the callback for when Claude's session_id is detected
func (e *InteractiveTaskExecutor) SetSessionLinkedHandler(handler SessionLinkedHandler) {
	e.onSessionLinked = handler
}

// getClaudeSessionDir returns the path to Claude's session directory for this project
func (e *InteractiveTaskExecutor) getClaudeSessionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	// Claude encodes paths by replacing / with -
	encoded := strings.ReplaceAll(e.projectPath, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded)
}

// captureExistingSessions records all session files that exist before Claude starts
func (e *InteractiveTaskExecutor) captureExistingSessions() {
	e.existingSessionsBeforeStart = make(map[string]bool)
	sessionDir := e.getClaudeSessionDir()
	if sessionDir == "" {
		return
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		log.Printf("⚠️  Could not read session dir %s: %v", sessionDir, err)
		return
	}

	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".jsonl") {
			sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
			e.existingSessionsBeforeStart[sessionID] = true
		}
	}

	log.Printf("📂 Captured %d existing sessions before task execution", len(e.existingSessionsBeforeStart))
}

// detectNewSession polls for a new session file and calls the callback when found
func (e *InteractiveTaskExecutor) detectNewSession() {
	if e.onSessionLinked == nil {
		return
	}

	sessionDir := e.getClaudeSessionDir()
	if sessionDir == "" {
		return
	}

	// Poll for new session file (check every 100ms for up to 10 seconds)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	timeout := time.After(10 * time.Second)

	for {
		select {
		case <-timeout:
			log.Printf("⚠️  Timed out waiting for new session file")
			return
		case <-ticker.C:
			if e.sessionDetected {
				return // Already detected
			}

			entries, err := os.ReadDir(sessionDir)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if !strings.HasSuffix(entry.Name(), ".jsonl") {
					continue
				}

				sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")

				// Check if this is a new session
				if !e.existingSessionsBeforeStart[sessionID] {
					e.sessionDetected = true
					log.Printf("🔗 Detected new session: %s", sessionID)
					e.onSessionLinked(sessionID)
					return
				}
			}
		}
	}
}

// ExecuteTask starts an interactive conversation with an initial prompt
func (e *InteractiveTaskExecutor) ExecuteTask(prompt string) error {
	log.Printf("🚀 Starting interactive task: %s", prompt)

	// Capture existing sessions before starting Claude
	e.captureExistingSessions()

	// Start new turn
	e.startNewTurn()

	// Capture files changed BEFORE execution (so we can exclude them from diffs)
	filesBeforeExec, err := e.git.DetectChangedFiles()
	if err != nil {
		log.Printf("⚠️  Failed to detect files before execution: %v", err)
		filesBeforeExec = []string{} // Continue anyway
	}
	e.filesBeforeExec = filesBeforeExec
	if len(filesBeforeExec) > 0 {
		log.Printf("📋 Detected %d uncommitted files before execution (will be excluded from conversation diffs)", len(filesBeforeExec))
	}

	// Prepend security instructions
	securityInstructions := fmt.Sprintf(`CRITICAL SECURITY RULES:
1. You are RESTRICTED to working ONLY within the approved project folder: %s
2. DO NOT access, read, or modify ANY files outside this directory under any circumstances
3. If the user requests access to files outside this folder, politely decline and explain the restriction
4. DO NOT commit any changes to git - just make the file changes and stop
5. DO NOT use commands like 'cd ..' or absolute paths that go outside the approved folder

User request: `, e.projectPath)
	fullPrompt := securityInstructions + prompt

	// Build interactive command
	// Interactive mode with auto-execute
	// Use --dangerously-skip-permissions because:
	// 1. Claude Code's permission system doesn't work programmatically via stdin
	// 2. We provide safety through folder approval + git-based diff review
	// 3. User reviews actual code changes (diffs) before committing
	// 4. Multi-turn conversation for plan approval and revisions
	//
	// We send prompts via stdin in JSON format (requires --input-format flag)
	// Output format is stream-json so we can parse events
	cmd := exec.Command("claude",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions")

	cmd.Dir = e.projectPath
	cmd.Env = os.Environ()

	// Get stdin, stdout, stderr pipes
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	e.stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Start command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude: %w", err)
	}

	e.cmd = cmd
	e.isRunning = true

	// Detect new session file in background (for linking with conversation_id)
	go e.detectNewSession()

	// Stream stdout in goroutine
	go e.streamOutput(stdout)

	// Stream stderr in goroutine
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("Claude stderr: %s", scanner.Text())
		}
	}()

	// Send initial message via stdin
	return e.SendMessage(fullPrompt)
}

// SendMessage sends a message to the ongoing conversation
func (e *InteractiveTaskExecutor) SendMessage(message string) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if !e.isRunning {
		return fmt.Errorf("executor not running")
	}

	log.Printf("📤 Sending message to Claude: %s", message)

	// Build message in Claude CLI's expected format for --input-format stream-json
	// Format: {"type": "user", "message": {"role": "user", "content": "..."}}
	msg := map[string]interface{}{
		"type": "user",
		"message": map[string]interface{}{
			"role":    "user",
			"content": message,
		},
	}

	// Write to stdin as JSON line
	msgJSON, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Append newline for line-delimited JSON
	msgJSON = append(msgJSON, '\n')

	_, err = e.stdin.Write(msgJSON)
	if err != nil {
		return fmt.Errorf("failed to write to stdin: %w", err)
	}

	log.Printf("✅ Message sent to Claude: %s", string(msgJSON))
	return nil
}

// isCompletionText checks if text indicates task completion
func isCompletionText(text string) bool {
	textLower := strings.ToLower(text)
	// Check for completion indicators
	completionIndicators := []string{
		"all changes have been made",
		"changes have been made and are ready",
		"all done",
		"task complete",
		"implementation complete",
		"i've completed",
		"successfully created",
		"successfully added",
		"successfully updated",
		"ready to use",
		"all set",
		"finished implementing",
		"perfect! i've created",
		"i've created both files",
		"created both files",
		"you can now access",
		"you can now use",
	}

	for _, indicator := range completionIndicators {
		if strings.Contains(textLower, indicator) {
			return true
		}
	}
	return false
}

// streamOutput handles streaming output from Claude
func (e *InteractiveTaskExecutor) streamOutput(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	// Increase buffer size for large outputs
	const maxCapacity = 1024 * 1024 // 1MB
	buf := make([]byte, maxCapacity)
	scanner.Buffer(buf, maxCapacity)

	for scanner.Scan() {
		line := scanner.Text()

		var msg StreamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("⚠️  Failed to parse stream message: %v", err)
			continue
		}

		// Handle the stream message
		if err := e.handleStreamMessage(msg); err != nil {
			log.Printf("❌ Error handling stream message: %v", err)
			e.sendEvent(Event{
				Type:    EventTypeError,
				Content: json.RawMessage(fmt.Sprintf(`{"message":"%s"}`, err.Error())),
			})
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("❌ Error reading stdout: %v", err)
	}

	// Process exited
	log.Println("🏁 Claude process exited")
	e.mutex.Lock()
	e.isRunning = false
	e.mutex.Unlock()

	// Handle completion
	e.handleCompletion()
}

// handleStreamMessage processes a streaming message from Claude
func (e *InteractiveTaskExecutor) handleStreamMessage(msg StreamMessage) error {
	switch msg.Type {
	case "user":
		// Claude Code CLI sends tool_result messages as "user" type
		for _, content := range msg.Message.Content {
			if content.Type == "tool_result" {
				// Log tool execution results
				if content.Text != "" {
					log.Printf("🔧 Tool result: %s", content.Text)
				} else {
					log.Printf("🔧 Tool execution completed")
				}
			}
		}

	case "system":
		// System messages (init, etc.)
		log.Printf("⚙️  System: %s", msg.Subtype)

	case "assistant":
		// Process assistant message content
		for _, content := range msg.Message.Content {
			switch content.Type {
			case "text":
				// Claude's thinking/response
				log.Printf("💭 Thinking: %s", content.Text)

				// Store the last thinking text for plan detection
				e.lastThinkingText = content.Text

				// Add to parser buffer
				e.parser.AddContent(content.Text)

				// Send thinking to mobile
				thinkingJSON, _ := json.Marshal(map[string]string{"text": content.Text})
				e.sendEvent(Event{
					Type:    EventTypeThinking,
					Content: thinkingJSON,
				})

				// Check if this is a decision point
				decision, err := e.parser.ExtractDecision()
				if err == nil && decision != nil {
					log.Printf("❓ Decision point found: %d options", len(decision.Options))
					// Send decision to mobile
					decisionJSON, _ := json.Marshal(decision)
					e.sendEvent(Event{
						Type:    EventTypeDecision,
						Content: decisionJSON,
					})

					// Reset parser for next decision
					e.parser.Reset()
				}

			case "tool_use":
				// Claude is using a tool
				log.Printf("🔧 Tool: %s", content.Name)

				// Send tool use event to mobile (for "Tools Used" display)
				toolInfo := map[string]interface{}{
					"tool":  content.Name,
					"input": content.Input,
				}
				toolJSON, _ := json.Marshal(toolInfo)
				e.sendEvent(Event{
					Type:    EventTypeToolUse,
					Content: toolJSON,
				})

				// Note: Tools execute automatically with --dangerously-skip-permissions
				// Diffs are generated AFTER execution in handleCompletion()

				// Check if this is AskUserQuestion - convert to decision event for user
				if content.Name == "AskUserQuestion" {
					log.Println("❓ Detected AskUserQuestion - sending as decision")

					// Parse the AskUserQuestion input
					var askInput struct {
						Questions []struct {
							Question string `json:"question"`
							Header   string `json:"header"`
							Options  []struct {
								Label       string `json:"label"`
								Description string `json:"description"`
							} `json:"options"`
						} `json:"questions"`
					}

					if err := json.Unmarshal(content.Input, &askInput); err == nil && len(askInput.Questions) > 0 {
						// Take the first question for now
						q := askInput.Questions[0]

						// Convert to our decision format (all questions are strategic now)
						options := []map[string]string{}
						for i, opt := range q.Options {
							options = append(options, map[string]string{
								"id":          fmt.Sprintf("%d", i+1),
								"label":       opt.Label,
								"description": opt.Description,
							})
						}

						decision := map[string]interface{}{
							"question":      q.Question,
							"options":       options,
							"context":       q.Header,
							"decision_type": "question",
						}

						decisionJSON, _ := json.Marshal(decision)
						e.sendEvent(Event{
							Type:    EventTypeDecision,
							Content: decisionJSON,
						})

						// Don't send as tool_use - we converted it to decision
						continue
					}
				}

				// Check if this is ExitPlanMode - show plan for approval
				if content.Name == "ExitPlanMode" {
					log.Println("📋 Detected ExitPlanMode - sending plan for approval")

					// Parse the ExitPlanMode input to get the plan
					var planInput struct {
						Plan string `json:"plan"`
					}

					if err := json.Unmarshal(content.Input, &planInput); err == nil && planInput.Plan != "" {
						// Send as a special "plan" event with approve/reject options
						decision := map[string]interface{}{
							"question": "Ready to execute this plan?",
							"context":  planInput.Plan,
							"options": []map[string]string{
								{
									"id":          "approve",
									"label":       "Approve & Execute",
									"description": "Start executing the plan",
								},
								{
									"id":          "revise",
									"label":       "Ask for Changes",
									"description": "Tell Claude what to change",
								},
							},
						}

						decisionJSON, _ := json.Marshal(decision)
						e.sendEvent(Event{
							Type:    EventTypeDecision,
							Content: decisionJSON,
						})

						// Don't send as tool_use - we converted it to plan approval
						continue
					}
				}

				// Note: tool_use event already sent at top of case block
				// Diffs are generated AFTER execution completes in handleCompletion()
			}
		}

		// Check stop_reason to see if Claude is waiting for input or continuing
		if msg.Message.StopReason == "end_turn" {
			// Claude has finished its turn and is waiting for user input
			log.Println("⏸️  Claude finished turn - waiting for user response")
			e.lastThinkingText = ""
		} else if msg.Message.StopReason == "tool_use" {
			// Claude is using tools - conversation continues automatically
			log.Println("🔄 Claude is using tools - conversation continues")
		}

		// Extract and send usage data if available
		if msg.Message.Usage != nil {
			log.Printf("📊 Token usage - Input: %d, Output: %d, Cache Read: %d, Cache Create: %d",
				msg.Message.Usage.InputTokens,
				msg.Message.Usage.OutputTokens,
				msg.Message.Usage.CacheReadInputTokens,
				msg.Message.Usage.CacheCreationInputTokens)

			usageData := map[string]interface{}{
				"input_tokens":                msg.Message.Usage.InputTokens,
				"output_tokens":               msg.Message.Usage.OutputTokens,
				"cache_read_input_tokens":     msg.Message.Usage.CacheReadInputTokens,
				"cache_creation_input_tokens": msg.Message.Usage.CacheCreationInputTokens,
				"model":                       msg.Message.Model,
			}
			// Add cost if available at top level
			if msg.TotalCostUSD > 0 {
				usageData["cost_usd"] = msg.TotalCostUSD
			}
			if msg.DurationMs > 0 {
				usageData["duration_ms"] = msg.DurationMs
			}

			usageJSON, _ := json.Marshal(usageData)
			e.sendEvent(Event{
				Type:    EventTypeUsage,
				Content: usageJSON,
			})
		}

	case "result":
		// Task complete - all tools have executed, files are written
		log.Printf("✅ Claude Code execution complete: %s", msg.Result)

		// Extract final usage data from result message (aggregated totals)
		if msg.TopLevelUsage != nil {
			log.Printf("📊 Final usage - Input: %d, Output: %d, Cache Read: %d, Cache Create: %d, Cost: $%.6f",
				msg.TopLevelUsage.InputTokens,
				msg.TopLevelUsage.OutputTokens,
				msg.TopLevelUsage.CacheReadInputTokens,
				msg.TopLevelUsage.CacheCreationInputTokens,
				msg.TotalCostUSD)

			usageData := map[string]interface{}{
				"input_tokens":                msg.TopLevelUsage.InputTokens,
				"output_tokens":               msg.TopLevelUsage.OutputTokens,
				"cache_read_input_tokens":     msg.TopLevelUsage.CacheReadInputTokens,
				"cache_creation_input_tokens": msg.TopLevelUsage.CacheCreationInputTokens,
				"cost_usd":                    msg.TotalCostUSD,
				"duration_ms":                 msg.DurationMs,
				"is_final":                    true, // Mark as final aggregated usage
			}

			usageJSON, _ := json.Marshal(usageData)
			e.sendEvent(Event{
				Type:    EventTypeUsage,
				Content: usageJSON,
			})
		}

		// NOW generate diffs (files exist on disk)
		if err := e.handleCompletion(); err != nil {
			return err
		}

	case "error":
		log.Printf("❌ Claude error: %s", msg.Result)
		return fmt.Errorf("claude error: %s", msg.Result)
	}

	return nil
}

// handleCompletion handles task completion (generate diffs, etc.)
// Called when Claude Code sends "result" message - all tools have executed
func (e *InteractiveTaskExecutor) handleCompletion() error {
	// Get all changed files after execution (tools have finished, files exist)
	filesAfterExec, err := e.git.DetectChangedFiles()
	if err != nil {
		e.sendEvent(Event{
			Type:    EventTypeError,
			Content: json.RawMessage(fmt.Sprintf(`{"message":"Failed to detect changes: %s"}`, err.Error())),
		})
		return fmt.Errorf("failed to detect changes: %w", err)
	}

	// Filter out files that existed before execution
	filesBeforeMap := make(map[string]bool)
	for _, f := range e.filesBeforeExec {
		filesBeforeMap[f] = true
	}

	var conversationFiles []string
	for _, f := range filesAfterExec {
		if !filesBeforeMap[f] {
			conversationFiles = append(conversationFiles, f)
		}
	}

	if len(conversationFiles) == 0 {
		log.Println("✅ No new files changed by this conversation")
		e.sendEvent(Event{
			Type:    EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return nil
	}

	log.Printf("🔍 Generating diffs for %d conversation files (after execution complete)", len(conversationFiles))

	// Generate diffs for conversation files
	diffs := make(map[string]string)
	for _, filePath := range conversationFiles {
		log.Printf("  📄 Generating diff for: %s", filePath)
		diff, err := e.git.GenerateDiff(filePath)
		if err != nil {
			log.Printf("  ❌ Failed to generate diff for %s: %v", filePath, err)
			continue
		}
		if diff == "" || len(diff) == 0 {
			log.Printf("  ⚠️  Diff is empty for %s - skipping", filePath)
			continue
		}
		log.Printf("  ✅ Generated diff (%d bytes)", len(diff))
		diffs[filePath] = diff
	}

	if len(diffs) == 0 {
		log.Println("⚠️  No valid diffs generated (all empty)")
		e.sendEvent(Event{
			Type:    EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return nil
	}

	// Send ALL diffs in one batch (not incremental)
	diffData := map[string]interface{}{
		"files_changed": len(diffs),
		"diffs":         diffs,
	}
	diffJSON, _ := json.Marshal(diffData)
	e.sendEvent(Event{
		Type:    EventTypeDiff,
		Content: diffJSON,
	})

	log.Printf("✅ Sent %d diffs to mobile - conversation complete", len(diffs))

	// Send complete event immediately - don't block conversation
	// User can review and commit at their leisure
	e.sendCompleteEvent(fmt.Sprintf("%d files changed", len(diffs)))

	return nil
}

// sendEvent sends an event to the mobile app via the event handler
func (e *InteractiveTaskExecutor) sendEvent(event Event) {
	if e.onEvent != nil {
		e.onEvent(event)
	}
}

// CommitChanges commits the changes after approval
func (e *InteractiveTaskExecutor) CommitChanges(message string) error {
	log.Printf("📝 Committing changes: %s", message)
	return e.git.CommitAndPush(message)
}

// DiscardChanges discards all changes
func (e *InteractiveTaskExecutor) DiscardChanges() error {
	log.Println("🗑️  Discarding changes")
	return e.git.DiscardChanges()
}

// startNewTurn resets turn-specific state
func (e *InteractiveTaskExecutor) startNewTurn() {
	e.filesModifiedThisTurn = make(map[string]bool)
	e.turnCompleted = false
	log.Println("🔄 Started new turn")
}

// sendCompleteEvent sends complete event (only once per turn)
func (e *InteractiveTaskExecutor) sendCompleteEvent(message string) {
	if e.turnCompleted {
		log.Println("⏭️  Complete event already sent this turn")
		return
	}
	e.turnCompleted = true
	e.sendEvent(Event{
		Type:    EventTypeComplete,
		Content: json.RawMessage(fmt.Sprintf(`{"message":"%s"}`, message)),
	})
	log.Println("✅ Sent complete event")
}

// ContinueAfterApproval commits changes after user approval
// Note: Complete event was already sent in handleCompletion()
func (e *InteractiveTaskExecutor) ContinueAfterApproval() error {
	log.Println("✅ ContinueAfterApproval called - committing changes...")

	// Commit the changes
	if err := e.CommitChanges("Apply changes via PocketVibe"); err != nil {
		log.Printf("❌ Failed to commit changes: %v", err)
		e.sendEvent(Event{
			Type:    EventTypeError,
			Content: json.RawMessage(fmt.Sprintf(`{"message":"Failed to commit: %s"}`, err.Error())),
		})
		return fmt.Errorf("failed to commit changes: %w", err)
	}

	log.Println("✅ Changes committed successfully")

	// Note: Conversation already complete (complete event sent in handleCompletion)
	// Mobile app will show success message locally

	return nil
}

// Stop stops the interactive executor and cleans up
func (e *InteractiveTaskExecutor) Stop() error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if !e.isRunning {
		return nil
	}

	log.Println("🛑 Stopping interactive executor")

	// Close stdin to signal end of input
	if e.stdin != nil {
		e.stdin.Close()
	}

	// Wait for process to exit (with timeout would be better in production)
	if e.cmd != nil {
		if err := e.cmd.Wait(); err != nil {
			log.Printf("⚠️  Process exited with error: %v", err)
		}
	}

	e.isRunning = false
	log.Println("✅ Interactive executor stopped")
	return nil
}

// ResumeSession resumes an existing Claude Code session by ID
func (e *InteractiveTaskExecutor) ResumeSession(sessionID string, continuationPrompt string) error {
	log.Printf("🔄 Resuming session: %s", sessionID)

	e.startNewTurn()

	// Capture files before resuming
	filesBeforeExec, _ := e.git.DetectChangedFiles()
	e.filesBeforeExec = filesBeforeExec

	// Build resume command
	// If we have a continuation prompt, use -p mode with --resume
	// Otherwise use interactive mode with --continue
	var cmd *exec.Cmd
	if continuationPrompt != "" {
		// Print mode with resume - run the continuation prompt in the existing session
		cmd = exec.Command("claude",
			"-p", continuationPrompt,
			"--resume", sessionID,
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions")
	} else {
		// Interactive mode to continue the session
		cmd = exec.Command("claude",
			"--resume", sessionID,
			"--input-format", "stream-json",
			"--output-format", "stream-json",
			"--verbose",
			"--dangerously-skip-permissions")
	}

	cmd.Dir = e.projectPath
	cmd.Env = os.Environ()

	// Get pipes
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}
	e.stdin = stdin

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	// Start command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to resume session: %w", err)
	}

	e.cmd = cmd
	e.isRunning = true

	// Stream output (reuse existing methods)
	go e.streamOutput(stdout)

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("Claude stderr: %s", scanner.Text())
		}
	}()

	log.Printf("✅ Session %s resumed, waiting for output", sessionID)
	return nil
}
