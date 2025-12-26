// Package llm provides a unified interface for different LLM code assistants.
// Currently supports Claude Code CLI with planned support for Gemini and Codex.
package llm

import (
	"encoding/json"
)

// Provider represents an LLM provider.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderGemini Provider = "gemini"
	ProviderCodex  Provider = "codex"
)

// EventType represents different event types during execution.
type EventType string

const (
	EventTypeThinking EventType = "thinking"
	EventTypeToolUse  EventType = "tool_use"
	EventTypeDecision EventType = "decision"
	EventTypeProgress EventType = "progress"
	EventTypeDiff     EventType = "diff"
	EventTypeComplete EventType = "complete"
	EventTypeError    EventType = "error"
)

// Event represents an event during task execution.
// All LLM providers emit events in this common format.
type Event struct {
	Type    EventType       `json:"type"`
	Content json.RawMessage `json:"content"`
}

// EventHandler is called for each event during execution.
type EventHandler func(Event)

// Decision represents a question requiring user input.
type Decision struct {
	Question string   `json:"question"`
	Options  []Option `json:"options"`
}

// Option represents a choice in a decision.
type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Executor is the core interface that all LLM providers must implement.
// This enables swapping between Claude, Gemini, Codex, etc.
type Executor interface {
	// ExecuteTask runs a task with the given prompt.
	// Events are emitted via the EventHandler provided at construction.
	ExecuteTask(prompt string) error

	// Provider returns which LLM provider this executor uses.
	Provider() Provider
}

// InteractiveExecutor extends Executor with multi-turn conversation support.
type InteractiveExecutor interface {
	Executor

	// Start begins an interactive session.
	Start(initialPrompt string) error

	// SendChoice sends the user's choice for a decision point.
	SendChoice(choiceID string) error

	// SendFollowUp sends a follow-up prompt in the conversation.
	SendFollowUp(prompt string) error

	// Resume resumes a previous session by ID.
	ResumeSession(sessionID string, prompt string) error

	// Stop terminates the interactive session.
	Stop()

	// IsRunning returns whether the session is active.
	IsRunning() bool

	// SetSessionLinkedHandler sets callback for session ID detection.
	SetSessionLinkedHandler(handler func(sessionID string))
}

// Config holds configuration for creating an executor.
type Config struct {
	Provider    Provider
	ProjectPath string
	OnEvent     EventHandler

	// Provider-specific settings
	APIKey      string            // For API-based providers (Gemini, Codex)
	Model       string            // Model variant to use
	ExtraConfig map[string]string // Provider-specific configuration
}

// Factory creates executors based on configuration.
// This is the main entry point for creating LLM executors.
type Factory interface {
	// CreateExecutor creates a one-shot executor.
	CreateExecutor(cfg Config) (Executor, error)

	// CreateInteractiveExecutor creates an interactive executor.
	CreateInteractiveExecutor(cfg Config) (InteractiveExecutor, error)

	// SupportedProviders returns list of available providers.
	SupportedProviders() []Provider
}
