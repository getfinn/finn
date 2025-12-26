// Package codex provides the OpenAI Codex/GPT-4 API implementation of the LLM executor interface.
//
// TODO(post-launch): Implement OpenAI API integration to allow users to use GPT-4
// and future OpenAI models as an alternative to Claude Code. This will require:
//   - OpenAI API client with function calling support
//   - Streaming response handling via Server-Sent Events
//   - Tool definitions for file operations and command execution
//   - Assistants API integration for conversation state management
//
// This package is registered with the factory but returns "not yet implemented" errors.
package codex

import (
	"fmt"

	"github.com/getfinn/finn/internal/llm"
)

func init() {
	// Register Codex provider with the global factory
	factory := llm.GetFactory()
	factory.RegisterExecutor(llm.ProviderCodex, NewExecutor)
	factory.RegisterInteractiveExecutor(llm.ProviderCodex, NewInteractiveExecutor)
}

// Executor implements llm.Executor for OpenAI Codex/GPT-4 API.
type Executor struct {
	projectPath string
	onEvent     llm.EventHandler
	apiKey      string
	model       string
}

// NewExecutor creates a new Codex executor.
func NewExecutor(cfg llm.Config) (llm.Executor, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = cfg.ExtraConfig["api_key"]
	}
	if apiKey == "" {
		return nil, fmt.Errorf("codex requires API key (set OPENAI_API_KEY or provide in config)")
	}

	model := cfg.Model
	if model == "" {
		model = "gpt-4o" // Default to GPT-4o
	}

	return &Executor{
		projectPath: cfg.ProjectPath,
		onEvent:     cfg.OnEvent,
		apiKey:      apiKey,
		model:       model,
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *Executor) ExecuteTask(prompt string) error {
	// TODO: Implement OpenAI API integration
	// 1. Create system prompt with project context and code assistant instructions
	// 2. Call OpenAI API with function calling for code operations
	// 3. Parse responses and emit events
	// 4. Handle tool calls (file read/write, command execution)
	return fmt.Errorf("codex executor not yet implemented")
}

// Provider returns the provider type.
func (e *Executor) Provider() llm.Provider {
	return llm.ProviderCodex
}

// InteractiveExecutor implements llm.InteractiveExecutor for OpenAI API.
type InteractiveExecutor struct {
	projectPath      string
	onEvent          llm.EventHandler
	apiKey           string
	model            string
	conversationID   string
	isRunning        bool
	sessionHandler   func(sessionID string)
}

// NewInteractiveExecutor creates a new Codex interactive executor.
func NewInteractiveExecutor(cfg llm.Config) (llm.InteractiveExecutor, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = cfg.ExtraConfig["api_key"]
	}
	if apiKey == "" {
		return nil, fmt.Errorf("codex requires API key")
	}

	model := cfg.Model
	if model == "" {
		model = "gpt-4o"
	}

	return &InteractiveExecutor{
		projectPath: cfg.ProjectPath,
		onEvent:     cfg.OnEvent,
		apiKey:      apiKey,
		model:       model,
		isRunning:   false,
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *InteractiveExecutor) ExecuteTask(prompt string) error {
	return e.Start(prompt)
}

// Provider returns the provider type.
func (e *InteractiveExecutor) Provider() llm.Provider {
	return llm.ProviderCodex
}

// Start begins an interactive session.
func (e *InteractiveExecutor) Start(initialPrompt string) error {
	// TODO: Implement
	// 1. Create new conversation with OpenAI API (using Assistants API or Chat Completions)
	// 2. Set up streaming response handler
	// 3. Process initial prompt with function calling
	return fmt.Errorf("codex interactive executor not yet implemented")
}

// SendChoice sends the user's choice for a decision point.
func (e *InteractiveExecutor) SendChoice(choiceID string) error {
	// TODO: Send user's choice as next turn in conversation
	return fmt.Errorf("codex interactive executor not yet implemented")
}

// SendFollowUp sends a follow-up prompt in the conversation.
func (e *InteractiveExecutor) SendFollowUp(prompt string) error {
	// TODO: Continue conversation with new prompt
	return fmt.Errorf("codex interactive executor not yet implemented")
}

// ResumeSession resumes a previous session by ID.
func (e *InteractiveExecutor) ResumeSession(sessionID string, prompt string) error {
	// TODO: Load conversation history and continue (using Assistants API threads)
	return fmt.Errorf("codex session resume not yet implemented")
}

// Stop terminates the interactive session.
func (e *InteractiveExecutor) Stop() {
	e.isRunning = false
}

// IsRunning returns whether the session is active.
func (e *InteractiveExecutor) IsRunning() bool {
	return e.isRunning
}

// SetSessionLinkedHandler sets callback for session ID detection.
func (e *InteractiveExecutor) SetSessionLinkedHandler(handler func(sessionID string)) {
	e.sessionHandler = handler
}
