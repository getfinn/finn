package claude

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/getfinn/finn/internal/git"
)

// TaskRunner is an interface for both one-shot and interactive task executors
type TaskRunner interface {
	ExecuteTask(prompt string) error
}

// TaskExecutor manages the execution of a Claude Code task with decision points
type TaskExecutor struct {
	claude            *Executor
	git               *git.Repository
	parser            *DecisionParser
	onEvent           EventHandler
	filesBeforeExec   []string // Track files changed before execution
	requiresApproval  bool     // Whether diffs require manual approval
}

// EventType represents different event types during execution
type EventType string

const (
	EventTypeThinking EventType = "thinking"
	EventTypeToolUse  EventType = "tool_use"
	EventTypeDecision EventType = "decision"
	EventTypeProgress EventType = "progress"
	EventTypeDiff     EventType = "diff"
	EventTypeComplete EventType = "complete"
	EventTypeError    EventType = "error"
	EventTypeUsage    EventType = "usage" // Token usage data from Claude API
)

// Event represents an event during task execution
type Event struct {
	Type    EventType       `json:"type"`
	Content json.RawMessage `json:"content"`
}

// EventHandler is called for each event during execution
type EventHandler func(Event)

// NewTaskExecutor creates a new task executor
func NewTaskExecutor(projectPath string, requiresApproval bool, onEvent EventHandler) *TaskExecutor {
	return &TaskExecutor{
		claude:           NewExecutor(projectPath),
		git:              git.NewRepository(projectPath),
		parser:           NewDecisionParser(),
		onEvent:          onEvent,
		requiresApproval: requiresApproval,
	}
}

// ExecuteTask runs a Claude Code task with decision extraction
func (e *TaskExecutor) ExecuteTask(prompt string) error {
	log.Printf("🚀 Executing task: %s", prompt)

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

	// Execute Claude Code with streaming
	err = e.claude.Execute(prompt, func(msg StreamMessage) error {
		switch msg.Type {
		case "assistant":
			// Process assistant message content
			for _, content := range msg.Message.Content {
				switch content.Type {
				case "text":
					// Claude's thinking/response
					log.Printf("💭 Thinking: %s", content.Text)

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

					toolInfo := map[string]interface{}{
						"tool":  content.Name,
						"input": content.Input,
					}
					toolJSON, _ := json.Marshal(toolInfo)

					e.sendEvent(Event{
						Type:    EventTypeToolUse,
						Content: toolJSON,
					})
				}
			}

		case "result":
			// Task complete - generate diffs
			log.Printf("✅ Claude Code execution complete: %s", msg.Result)
			return e.handleCompletion()
		}

		return nil
	})

	if err != nil {
		e.sendEvent(Event{
			Type:    EventTypeError,
			Content: json.RawMessage(fmt.Sprintf(`{"message":"%s"}`, err.Error())),
		})
		return err
	}

	return nil
}

// handleCompletion handles task completion (generate diffs, etc.)
func (e *TaskExecutor) handleCompletion() error {
	// Get all changed files after execution
	filesAfterExec, err := e.git.DetectChangedFiles()
	if err != nil {
		return fmt.Errorf("failed to detect changes: %w", err)
	}

	// Filter out files that existed before execution
	filesBeforeMap := make(map[string]bool)
	for _, file := range e.filesBeforeExec {
		filesBeforeMap[file] = true
	}

	newFiles := []string{}
	for _, file := range filesAfterExec {
		if !filesBeforeMap[file] {
			newFiles = append(newFiles, file)
		}
	}

	if len(newFiles) == 0 {
		// No NEW changes from this conversation - task complete
		log.Println("📊 No new changes made during this conversation")
		e.sendEvent(Event{
			Type:    EventTypeComplete,
			Content: json.RawMessage(`{"files_changed":0}`),
		})
		return nil
	}

	// Generate diffs only for NEW files
	log.Printf("📊 Generating diffs for %d files changed in this conversation...", len(newFiles))
	diffs := make(map[string]string)

	for _, file := range newFiles {
		diff, err := e.git.GenerateDiff(file)
		if err != nil {
			log.Printf("⚠️  Failed to generate diff for %s: %v", file, err)
			continue
		}
		diffs[file] = diff
	}

	log.Printf("📊 Generated diffs for %d files", len(diffs))

	// Log the diffs for visibility
	for file, diff := range diffs {
		log.Printf("📄 File: %s", file)
		if len(diff) > 200 {
			log.Printf("   Diff preview: %s... (%d chars total)", diff[:200], len(diff))
		} else {
			log.Printf("   Diff: %s", diff)
		}
	}

	// Send diffs to mobile
	diffData := map[string]interface{}{
		"diffs":             diffs,
		"files_changed":     len(diffs),
		"requires_approval": e.requiresApproval, // Include approval flag based on execution mode
	}
	diffJSON, _ := json.Marshal(diffData)

	if e.requiresApproval {
		log.Println("📤 Sending diff to mobile - waiting for manual approval...")
	} else {
		log.Println("📤 Sending diff to mobile - auto-approved mode")
	}
	e.sendEvent(Event{
		Type:    EventTypeDiff,
		Content: diffJSON,
	})

	// If in auto-approve mode, task is complete immediately after showing diffs
	if !e.requiresApproval {
		log.Println("✅ Task complete - auto-approved mode")
		e.sendEvent(Event{
			Type:    EventTypeComplete,
			Content: json.RawMessage(fmt.Sprintf(`{"files_changed":%d,"auto_approved":true}`, len(diffs))),
		})
	}
	// If manual approval mode, we wait for user to approve before completing
	// (complete will be sent when all diffs are approved)

	return nil
}

// CommitChanges commits and pushes changes
func (e *TaskExecutor) CommitChanges(message string) error {
	log.Printf("📝 Committing changes: %s", message)

	if err := e.git.CommitAndPush(message); err != nil {
		return fmt.Errorf("failed to commit and push: %w", err)
	}

	// Send completion
	e.sendEvent(Event{
		Type:    EventTypeComplete,
		Content: json.RawMessage(`{"committed":true,"pushed":true}`),
	})

	return nil
}

// DiscardChanges discards all uncommitted changes
func (e *TaskExecutor) DiscardChanges() error {
	log.Println("🗑️  Discarding changes...")

	if err := e.git.DiscardChanges(); err != nil {
		return fmt.Errorf("failed to discard changes: %w", err)
	}

	// Send completion
	e.sendEvent(Event{
		Type:    EventTypeComplete,
		Content: json.RawMessage(`{"committed":false,"discarded":true}`),
	})

	return nil
}

// sendEvent sends an event to the handler
func (e *TaskExecutor) sendEvent(event Event) {
	if e.onEvent != nil {
		e.onEvent(event)
	}
}
