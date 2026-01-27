// Package gemini provides the Gemini CLI implementation of the LLM executor interface.
//
// This implementation uses the Gemini CLI (https://github.com/google-gemini/gemini-cli)
// with streaming JSON output, similar to how Claude Code CLI is used.
//
// Authentication:
//   - Google OAuth: Browser-based login, credentials stored in ~/.gemini/
//   - API Key: Set GEMINI_API_KEY environment variable
//
// Session storage: ~/.gemini/tmp/<project_hash>/chats/session-*.json
package gemini

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/getfinn/finn/internal/llm"
)

func init() {
	// Register Gemini provider with the global factory
	factory := llm.GetFactory()
	factory.RegisterExecutor(llm.ProviderGemini, NewExecutor)
	factory.RegisterInteractiveExecutor(llm.ProviderGemini, NewInteractiveExecutor)
}

// Stream message types from Gemini CLI --output-format stream-json
type GeminiStreamMessage struct {
	Type      string          `json:"type"`                // init, message, tool_use, tool_result, result, error
	Timestamp string          `json:"timestamp,omitempty"` // ISO timestamp
	SessionID string          `json:"session_id,omitempty"`
	Model     string          `json:"model,omitempty"`
	Role      string          `json:"role,omitempty"`    // user, assistant
	Content   string          `json:"content,omitempty"` // message content
	Delta     bool            `json:"delta,omitempty"`   // true if streaming chunk
	ToolName  string          `json:"tool_name,omitempty"`
	ToolID    string          `json:"tool_id,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"` // tool parameters
	Status    string          `json:"status,omitempty"`      // success, error
	Output    string          `json:"output,omitempty"`      // tool output
	Error     *GeminiError    `json:"error,omitempty"`
	Stats     *GeminiStats    `json:"stats,omitempty"`
}

type GeminiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type GeminiStats struct {
	TotalTokens  int   `json:"total_tokens"`
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	Cached       int   `json:"cached"`
	Input        int   `json:"input"`
	DurationMs   int64 `json:"duration_ms"`
	ToolCalls    int   `json:"tool_calls"`
}

// Executor implements llm.Executor for Gemini CLI (one-shot mode).
type Executor struct {
	projectPath     string
	git             *git.Repository
	onEvent         llm.EventHandler
	model           string
	filesBeforeExec []string // Track files changed before execution
}

// NewExecutor creates a new Gemini CLI executor.
func NewExecutor(cfg llm.Config) (llm.Executor, error) {
	// Check if Gemini CLI is installed
	if !IsInstalled() {
		return nil, fmt.Errorf("gemini CLI not installed (run: npm install -g @google/gemini-cli)")
	}

	model := cfg.Model
	if model == "" {
		model = "auto-gemini-2.5" // Let Gemini choose best model
	}

	return &Executor{
		projectPath: cfg.ProjectPath,
		git:         git.NewRepository(cfg.ProjectPath),
		onEvent:     cfg.OnEvent,
		model:       model,
	}, nil
}

// ExecuteTask runs a task with the given prompt using Gemini CLI.
func (e *Executor) ExecuteTask(prompt string) error {
	log.Printf("🚀 [Gemini] Executing task: %s", prompt)

	// Capture files changed BEFORE execution (so we can exclude them from diffs)
	filesBeforeExec, err := e.git.DetectChangedFiles()
	if err != nil {
		log.Printf("⚠️  [Gemini] Failed to detect files before execution: %v", err)
		filesBeforeExec = []string{} // Continue anyway
	}
	e.filesBeforeExec = filesBeforeExec
	if len(filesBeforeExec) > 0 {
		log.Printf("📋 [Gemini] Detected %d uncommitted files before execution (will be excluded from conversation diffs)", len(filesBeforeExec))
	}

	// Security instructions (same as Claude)
	securityInstructions := fmt.Sprintf(`CRITICAL SECURITY RULES:
1. You are RESTRICTED to working ONLY within the approved project folder: %s
2. DO NOT access, read, or modify ANY files outside this directory
3. If the user requests access to files outside this folder, politely decline
4. DO NOT commit any changes to git - just make the file changes and stop
5. DO NOT use commands like 'cd ..' or absolute paths that go outside the approved folder

User request: `, e.projectPath)
	fullPrompt := securityInstructions + prompt

	// Build command: gemini -p "prompt" --output-format stream-json --yolo
	cmd := exec.Command("gemini",
		"-p", fullPrompt,
		"--output-format", "stream-json",
		"--yolo") // Auto-approve all tool calls (like --dangerously-skip-permissions)

	cmd.Dir = e.projectPath
	cmd.Env = os.Environ()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gemini: %w", err)
	}

	// Process streaming output
	go e.processStreamOutput(stdout)
	go e.processStderr(stderr)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("gemini CLI failed: %w", err)
	}

	return nil
}

func (e *Executor) processStreamOutput(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	// Increase buffer for large outputs
	buf := make([]byte, 1024*1024) // 1MB
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg GeminiStreamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("⚠️  [Gemini] Failed to parse stream message: %v", err)
			continue
		}

		e.handleStreamMessage(msg)
	}
}

func (e *Executor) handleStreamMessage(msg GeminiStreamMessage) {
	switch msg.Type {
	case "init":
		log.Printf("🔗 [Gemini] Session: %s, Model: %s", msg.SessionID, msg.Model)

	case "message":
		if msg.Role == "assistant" && msg.Content != "" {
			// Send thinking event
			thinkingJSON, _ := json.Marshal(map[string]string{"text": msg.Content})
			e.sendEvent(llm.Event{
				Type:    llm.EventTypeThinking,
				Content: thinkingJSON,
			})
		}

	case "tool_use":
		log.Printf("🔧 [Gemini] Tool: %s", msg.ToolName)
		toolInfo := map[string]interface{}{
			"tool":    normalizeToolName(msg.ToolName),
			"tool_id": msg.ToolID,
			"input":   msg.Parameters,
		}
		toolJSON, _ := json.Marshal(toolInfo)
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeToolUse,
			Content: toolJSON,
		})

	case "tool_result":
		// Log tool results but don't emit events (internal)
		if msg.Status == "error" {
			log.Printf("❌ [Gemini] Tool %s failed: %s", msg.ToolID, msg.Output)
		}

	case "result":
		log.Printf("✅ [Gemini] Task complete")
		if msg.Stats != nil {
			usageJSON, _ := json.Marshal(map[string]interface{}{
				"input_tokens":  msg.Stats.InputTokens,
				"output_tokens": msg.Stats.OutputTokens,
				"cached_tokens": msg.Stats.Cached,
				"total_tokens":  msg.Stats.TotalTokens,
				"duration_ms":   msg.Stats.DurationMs,
				"tool_calls":    msg.Stats.ToolCalls,
			})
			e.sendEvent(llm.Event{
				Type:    llm.EventTypeUsage,
				Content: usageJSON,
			})
		}
		// Generate diffs and send complete event (like Claude does)
		e.handleCompletion()

	case "error":
		errMsg := "unknown error"
		if msg.Error != nil {
			errMsg = msg.Error.Message
		}
		log.Printf("❌ [Gemini] Error: %s", errMsg)
		errorJSON, _ := json.Marshal(map[string]string{"message": errMsg})
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeError,
			Content: errorJSON,
		})
	}
}

func (e *Executor) processStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		// Filter out non-error messages
		if !strings.Contains(line, "YOLO mode") && !strings.Contains(line, "Loaded cached") {
			log.Printf("[Gemini stderr] %s", line)
		}
	}
}

func (e *Executor) sendEvent(event llm.Event) {
	if e.onEvent != nil {
		e.onEvent(event)
	}
}

// handleCompletion generates diffs for changed files and sends completion event
func (e *Executor) handleCompletion() {
	// Get all changed files after execution
	filesAfterExec, err := e.git.DetectChangedFiles()
	if err != nil {
		log.Printf("⚠️  [Gemini] Failed to detect changes: %v", err)
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeComplete,
			Content: json.RawMessage(`{"status":"success","files_changed":0}`),
		})
		return
	}

	// Filter out files that existed before execution
	filesBeforeMap := make(map[string]bool)
	for _, file := range e.filesBeforeExec {
		filesBeforeMap[file] = true
	}

	var newFiles []string
	for _, file := range filesAfterExec {
		if !filesBeforeMap[file] {
			newFiles = append(newFiles, file)
		}
	}

	if len(newFiles) == 0 {
		log.Println("📊 [Gemini] No new changes made during this conversation")
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return
	}

	// Generate diffs only for NEW files
	log.Printf("📊 [Gemini] Generating diffs for %d files changed in this conversation...", len(newFiles))
	diffs := make(map[string]string)

	for _, file := range newFiles {
		diff, err := e.git.GenerateDiff(file)
		if err != nil {
			log.Printf("⚠️  [Gemini] Failed to generate diff for %s: %v", file, err)
			continue
		}
		diffs[file] = diff
	}

	if len(diffs) == 0 {
		log.Println("⚠️  [Gemini] No valid diffs generated (all empty)")
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return
	}

	log.Printf("📊 [Gemini] Generated diffs for %d files", len(diffs))

	// Send diffs to mobile (matching Claude's format)
	diffData := map[string]interface{}{
		"diffs":         diffs,
		"files_changed": len(diffs),
	}
	diffJSON, _ := json.Marshal(diffData)

	log.Println("📤 [Gemini] Sending diff to mobile")
	e.sendEvent(llm.Event{
		Type:    llm.EventTypeDiff,
		Content: diffJSON,
	})

	// Send complete event after diff
	log.Printf("✅ [Gemini] Sent %d diffs to mobile - task complete", len(diffs))
	e.sendEvent(llm.Event{
		Type:    llm.EventTypeComplete,
		Content: json.RawMessage(fmt.Sprintf(`{"files_changed":%d}`, len(diffs))),
	})
}

// Provider returns the provider type.
func (e *Executor) Provider() llm.Provider {
	return llm.ProviderGemini
}

// InteractiveExecutor implements llm.InteractiveExecutor for Gemini CLI.
type InteractiveExecutor struct {
	projectPath string
	git         *git.Repository
	onEvent     llm.EventHandler
	model       string

	// Process management
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	isRunning bool
	mutex     sync.Mutex

	// Session tracking
	currentSessionID            string
	sessionHandler              func(sessionID string)
	existingSessionsBeforeStart map[string]bool
	sessionDetected             bool

	// File tracking for diff generation
	filesBeforeExec       []string
	sentDiffs             map[string]bool
	diffMutex             sync.Mutex
	filesModifiedThisTurn map[string]bool
	turnCompleted         bool
}

// NewInteractiveExecutor creates a new Gemini CLI interactive executor.
func NewInteractiveExecutor(cfg llm.Config) (llm.InteractiveExecutor, error) {
	// Check if Gemini CLI is installed
	if !IsInstalled() {
		return nil, fmt.Errorf("gemini CLI not installed (run: npm install -g @google/gemini-cli)")
	}

	model := cfg.Model
	if model == "" {
		model = "auto-gemini-2.5"
	}

	return &InteractiveExecutor{
		projectPath:                 cfg.ProjectPath,
		git:                         git.NewRepository(cfg.ProjectPath),
		onEvent:                     cfg.OnEvent,
		model:                       model,
		isRunning:                   false,
		sentDiffs:                   make(map[string]bool),
		filesModifiedThisTurn:       make(map[string]bool),
		existingSessionsBeforeStart: make(map[string]bool),
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *InteractiveExecutor) ExecuteTask(prompt string) error {
	return e.Start(prompt)
}

// Provider returns the provider type.
func (e *InteractiveExecutor) Provider() llm.Provider {
	return llm.ProviderGemini
}

// Start begins an interactive session with Gemini CLI.
func (e *InteractiveExecutor) Start(initialPrompt string) error {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.isRunning {
		return fmt.Errorf("session already running")
	}

	log.Printf("🚀 [Gemini] Starting interactive task: %s", initialPrompt)

	// Capture files before execution
	filesBeforeExec, err := e.git.DetectChangedFiles()
	if err != nil {
		log.Printf("⚠️  [Gemini] Failed to detect files before execution: %v", err)
		filesBeforeExec = []string{}
	}
	e.filesBeforeExec = filesBeforeExec

	// Capture existing sessions before starting
	e.captureExistingSessions()

	// Security instructions
	securityInstructions := fmt.Sprintf(`CRITICAL SECURITY RULES:
1. You are RESTRICTED to working ONLY within the approved project folder: %s
2. DO NOT access, read, or modify ANY files outside this directory
3. If the user requests access to files outside this folder, politely decline
4. DO NOT commit any changes to git - just make the file changes and stop
5. DO NOT use commands like 'cd ..' or absolute paths outside the approved folder

User request: `, e.projectPath)
	fullPrompt := securityInstructions + initialPrompt

	// Build command for one-shot mode (Gemini CLI doesn't have true interactive mode like Claude)
	// We'll use -p for each prompt and --resume for continuing conversations
	e.cmd = exec.Command("gemini",
		"-p", fullPrompt,
		"--output-format", "stream-json",
		"--yolo")

	e.cmd.Dir = e.projectPath
	e.cmd.Env = os.Environ()

	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := e.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gemini: %w", err)
	}

	e.isRunning = true
	e.turnCompleted = false
	e.filesModifiedThisTurn = make(map[string]bool)

	// Start session detection in background
	go e.detectNewSession()

	// Process output in goroutine
	go e.streamOutput(stdout)
	go e.processStderr(stderr)

	// Wait for completion in goroutine
	go func() {
		err := e.cmd.Wait()
		e.mutex.Lock()
		e.isRunning = false
		e.mutex.Unlock()

		if err != nil {
			log.Printf("❌ [Gemini] Process exited with error: %v", err)
		}

		// Handle completion (generate diffs)
		e.handleCompletion()
	}()

	return nil
}

func (e *InteractiveExecutor) streamOutput(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, len(buf))

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var msg GeminiStreamMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("⚠️  [Gemini] Failed to parse: %v", err)
			continue
		}

		e.handleInteractiveStreamMessage(msg)
	}
}

func (e *InteractiveExecutor) handleInteractiveStreamMessage(msg GeminiStreamMessage) {
	switch msg.Type {
	case "init":
		log.Printf("🔗 [Gemini] Session: %s, Model: %s", msg.SessionID, msg.Model)
		e.currentSessionID = msg.SessionID
		// Notify session handler
		if e.sessionHandler != nil && !e.sessionDetected {
			e.sessionDetected = true
			e.sessionHandler(msg.SessionID)
		}

	case "message":
		if msg.Role == "assistant" && msg.Content != "" {
			thinkingJSON, _ := json.Marshal(map[string]string{"text": msg.Content})
			e.sendEvent(llm.Event{
				Type:    llm.EventTypeThinking,
				Content: thinkingJSON,
			})
		}

	case "tool_use":
		log.Printf("🔧 [Gemini] Tool: %s", msg.ToolName)

		// Track file modifications
		if msg.ToolName == "write_file" {
			var params struct {
				FilePath string `json:"file_path"`
			}
			if err := json.Unmarshal(msg.Parameters, &params); err == nil {
				e.diffMutex.Lock()
				e.filesModifiedThisTurn[params.FilePath] = true
				e.diffMutex.Unlock()
			}
		}

		toolInfo := map[string]interface{}{
			"tool":    normalizeToolName(msg.ToolName),
			"tool_id": msg.ToolID,
			"input":   msg.Parameters,
		}
		toolJSON, _ := json.Marshal(toolInfo)
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeToolUse,
			Content: toolJSON,
		})

	case "tool_result":
		if msg.Status == "error" {
			log.Printf("❌ [Gemini] Tool error: %s", msg.Output)
		}

	case "result":
		log.Printf("✅ [Gemini] Turn complete")
		if msg.Stats != nil {
			usageJSON, _ := json.Marshal(map[string]interface{}{
				"input_tokens":  msg.Stats.InputTokens,
				"output_tokens": msg.Stats.OutputTokens,
				"cached_tokens": msg.Stats.Cached,
				"duration_ms":   msg.Stats.DurationMs,
				"tool_calls":    msg.Stats.ToolCalls,
			})
			e.sendEvent(llm.Event{
				Type:    llm.EventTypeUsage,
				Content: usageJSON,
			})
		}
		// Generate and send diffs immediately when result is received (like Claude does)
		e.handleCompletion()

	case "error":
		errMsg := "unknown error"
		if msg.Error != nil {
			errMsg = msg.Error.Message
		}
		errorJSON, _ := json.Marshal(map[string]string{"message": errMsg})
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeError,
			Content: errorJSON,
		})
	}
}

func (e *InteractiveExecutor) handleCompletion() {
	if e.turnCompleted {
		return
	}
	e.turnCompleted = true

	// Generate diffs for changed files
	filesAfterExec, err := e.git.DetectChangedFiles()
	if err != nil {
		log.Printf("⚠️  [Gemini] Failed to detect changed files: %v", err)
		return
	}

	// Filter to only new files (not existing before this conversation)
	filesBeforeMap := make(map[string]bool)
	for _, f := range e.filesBeforeExec {
		filesBeforeMap[f] = true
	}

	var newFiles []string
	for _, f := range filesAfterExec {
		if !filesBeforeMap[f] {
			newFiles = append(newFiles, f)
		}
	}

	if len(newFiles) == 0 {
		log.Println("📊 [Gemini] No new changes")
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return
	}

	// Generate diffs
	diffs := make(map[string]string)
	for _, file := range newFiles {
		diff, err := e.git.GenerateDiff(file)
		if err != nil {
			log.Printf("⚠️  [Gemini] Failed to generate diff for %s: %v", file, err)
			continue
		}
		diffs[file] = diff
	}

	// Check if we got any valid diffs (matching Claude's behavior)
	if len(diffs) == 0 {
		log.Println("⚠️  [Gemini] No valid diffs generated (all empty)")
		e.sendEvent(llm.Event{
			Type:    llm.EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return
	}

	log.Printf("📊 [Gemini] Generated diffs for %d files", len(diffs))

	// Send ALL diffs in one batch (matching Claude's format)
	diffData := map[string]interface{}{
		"files_changed": len(diffs),
		"diffs":         diffs,
	}
	diffJSON, _ := json.Marshal(diffData)
	e.sendEvent(llm.Event{
		Type:    llm.EventTypeDiff,
		Content: diffJSON,
	})

	log.Printf("✅ [Gemini] Sent %d diffs to mobile - task complete", len(diffs))

	// Send complete event after diff (so mobile knows task is done)
	e.sendEvent(llm.Event{
		Type:    llm.EventTypeComplete,
		Content: json.RawMessage(fmt.Sprintf(`{"message":"%d files changed"}`, len(diffs))),
	})
}

func (e *InteractiveExecutor) processStderr(stderr io.Reader) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "YOLO mode") && !strings.Contains(line, "Loaded cached") {
			log.Printf("[Gemini stderr] %s", line)
		}
	}
}

// SendChoice sends the user's choice for a decision point.
func (e *InteractiveExecutor) SendChoice(choiceID string) error {
	// For Gemini, we need to resume the session with the choice as a follow-up
	return e.SendFollowUp(fmt.Sprintf("I choose option %s", choiceID))
}

// SendFollowUp sends a follow-up prompt in the conversation.
func (e *InteractiveExecutor) SendFollowUp(prompt string) error {
	if e.currentSessionID == "" {
		return fmt.Errorf("no active session to continue")
	}
	return e.ResumeSession(e.currentSessionID, prompt)
}

// ResumeSession resumes a previous session by ID.
func (e *InteractiveExecutor) ResumeSession(sessionID string, prompt string) error {
	e.mutex.Lock()
	if e.isRunning {
		e.mutex.Unlock()
		return fmt.Errorf("session already running")
	}
	e.mutex.Unlock()

	log.Printf("🔄 [Gemini] Resuming session %s with: %s", sessionID, prompt)

	// Reset turn state
	e.turnCompleted = false
	e.filesModifiedThisTurn = make(map[string]bool)

	// Build resume command
	e.cmd = exec.Command("gemini",
		"--resume", sessionID,
		"-p", prompt,
		"--output-format", "stream-json",
		"--yolo")

	e.cmd.Dir = e.projectPath
	e.cmd.Env = os.Environ()

	stdout, err := e.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := e.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := e.cmd.Start(); err != nil {
		return fmt.Errorf("failed to start gemini: %w", err)
	}

	e.mutex.Lock()
	e.isRunning = true
	e.currentSessionID = sessionID
	e.mutex.Unlock()

	go e.streamOutput(stdout)
	go e.processStderr(stderr)

	go func() {
		err := e.cmd.Wait()
		e.mutex.Lock()
		e.isRunning = false
		e.mutex.Unlock()

		if err != nil {
			log.Printf("❌ [Gemini] Resume process exited with error: %v", err)
		}
		e.handleCompletion()
	}()

	return nil
}

// Stop terminates the interactive session.
func (e *InteractiveExecutor) Stop() {
	e.mutex.Lock()
	defer e.mutex.Unlock()

	if e.cmd != nil && e.cmd.Process != nil {
		log.Println("🛑 [Gemini] Stopping process...")
		e.cmd.Process.Kill()
	}
	e.isRunning = false
}

// IsRunning returns whether the session is active.
func (e *InteractiveExecutor) IsRunning() bool {
	e.mutex.Lock()
	defer e.mutex.Unlock()
	return e.isRunning
}

// SetSessionLinkedHandler sets callback for session ID detection.
func (e *InteractiveExecutor) SetSessionLinkedHandler(handler func(sessionID string)) {
	e.sessionHandler = handler
}

func (e *InteractiveExecutor) sendEvent(event llm.Event) {
	if e.onEvent != nil {
		e.onEvent(event)
	}
}

// Session management helpers

// getGeminiSessionDir returns the path to Gemini's session directory for this project.
// Gemini uses SHA-256 hash of project path: ~/.gemini/tmp/<hash>/chats/
func (e *InteractiveExecutor) getGeminiSessionDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	hash := sha256.Sum256([]byte(e.projectPath))
	hashStr := hex.EncodeToString(hash[:])
	return filepath.Join(home, ".gemini", "tmp", hashStr, "chats")
}

func (e *InteractiveExecutor) captureExistingSessions() {
	e.existingSessionsBeforeStart = make(map[string]bool)
	sessionDir := e.getGeminiSessionDir()
	if sessionDir == "" {
		return
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "session-") && strings.HasSuffix(entry.Name(), ".json") {
			sessionID := extractSessionID(entry.Name())
			if sessionID != "" {
				e.existingSessionsBeforeStart[sessionID] = true
			}
		}
	}
	log.Printf("📂 [Gemini] Found %d existing sessions", len(e.existingSessionsBeforeStart))
}

func (e *InteractiveExecutor) detectNewSession() {
	if e.sessionHandler == nil {
		return
	}

	sessionDir := e.getGeminiSessionDir()
	if sessionDir == "" {
		return
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.After(10 * time.Second)

	for {
		select {
		case <-timeout:
			return
		case <-ticker.C:
			if e.sessionDetected {
				return
			}

			entries, err := os.ReadDir(sessionDir)
			if err != nil {
				continue
			}

			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), "session-") || !strings.HasSuffix(entry.Name(), ".json") {
					continue
				}

				sessionID := extractSessionID(entry.Name())
				if sessionID != "" && !e.existingSessionsBeforeStart[sessionID] {
					e.sessionDetected = true
					e.currentSessionID = sessionID
					log.Printf("🔗 [Gemini] Detected new session: %s", sessionID)
					e.sessionHandler(sessionID)
					return
				}
			}
		}
	}
}

// Helper functions

// IsInstalled checks if Gemini CLI is installed.
func IsInstalled() bool {
	_, err := exec.LookPath("gemini")
	return err == nil
}

// extractSessionID extracts the session ID from a filename like "session-2025-12-27T23-52-f96da40e.json"
func extractSessionID(filename string) string {
	// Remove "session-" prefix and ".json" suffix
	name := strings.TrimPrefix(filename, "session-")
	name = strings.TrimSuffix(name, ".json")

	// The format is: YYYY-MM-DDTHH-MM-<shortid>
	// We want to extract the shortid (last part after the last -)
	parts := strings.Split(name, "-")
	if len(parts) >= 6 {
		// Return the full session ID (shortid is at the end)
		return parts[len(parts)-1]
	}
	return ""
}

// normalizeToolName converts Gemini tool names to a common format.
func normalizeToolName(geminiTool string) string {
	// Map Gemini tool names to common names
	mapping := map[string]string{
		"write_file":        "Write",
		"read_file":         "Read",
		"run_shell_command": "Bash",
		"glob":              "Glob",
		"list_directory":    "ListDirectory",
		"search_files":      "Grep",
	}
	if common, ok := mapping[geminiTool]; ok {
		return common
	}
	return geminiTool
}
