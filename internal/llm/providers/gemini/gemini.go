// Package gemini provides the Google Gemini API implementation of the LLM executor interface.
//
// TODO(post-launch): Implement Gemini API integration to allow users to use Google's
// Gemini models as an alternative to Claude Code. This will require:
//   - Gemini API client with function calling support
//   - Streaming response handling
//   - Tool definitions for file operations and command execution
//   - Session/conversation state management
//
// This package is registered with the factory but returns "not yet implemented" errors.
package gemini

import (
	"fmt"

	"github.com/getfinn/finn/internal/llm"
)

func init() {
	// Register Gemini provider with the global factory
	factory := llm.GetFactory()
	factory.RegisterExecutor(llm.ProviderGemini, NewExecutor)
	factory.RegisterInteractiveExecutor(llm.ProviderGemini, NewInteractiveExecutor)
}

// Executor implements llm.Executor for Gemini API.
type Executor struct {
	projectPath string
	onEvent     llm.EventHandler
	apiKey      string
	model       string
}

// NewExecutor creates a new Gemini executor.
func NewExecutor(cfg llm.Config) (llm.Executor, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = cfg.ExtraConfig["api_key"]
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini requires API key (set GEMINI_API_KEY or provide in config)")
	}

	model := cfg.Model
	if model == "" {
		model = "gemini-2.0-flash-exp" // Default to latest model
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
	// TODO: Implement Gemini API integration
	// 1. Create system prompt with project context
	// 2. Call Gemini API with function calling for code operations
	// 3. Parse responses and emit events
	// 4. Handle tool calls (file read/write, command execution)
	return fmt.Errorf("gemini executor not yet implemented")
}

// Provider returns the provider type.
func (e *Executor) Provider() llm.Provider {
	return llm.ProviderGemini
}

// InteractiveExecutor implements llm.InteractiveExecutor for Gemini API.
type InteractiveExecutor struct {
	projectPath      string
	onEvent          llm.EventHandler
	apiKey           string
	model            string
	conversationID   string
	isRunning        bool
	sessionHandler   func(sessionID string)
}

// NewInteractiveExecutor creates a new Gemini interactive executor.
func NewInteractiveExecutor(cfg llm.Config) (llm.InteractiveExecutor, error) {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = cfg.ExtraConfig["api_key"]
	}
	if apiKey == "" {
		return nil, fmt.Errorf("gemini requires API key")
	}

	model := cfg.Model
	if model == "" {
		model = "gemini-2.0-flash-exp"
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
	return llm.ProviderGemini
}

// Start begins an interactive session.
func (e *InteractiveExecutor) Start(initialPrompt string) error {
	// TODO: Implement
	// 1. Create new conversation with Gemini API
	// 2. Set up streaming response handler
	// 3. Process initial prompt
	return fmt.Errorf("gemini interactive executor not yet implemented")
}

// SendChoice sends the user's choice for a decision point.
func (e *InteractiveExecutor) SendChoice(choiceID string) error {
	// TODO: Send user's choice as next turn in conversation
	return fmt.Errorf("gemini interactive executor not yet implemented")
}

// SendFollowUp sends a follow-up prompt in the conversation.
func (e *InteractiveExecutor) SendFollowUp(prompt string) error {
	// TODO: Continue conversation with new prompt
	return fmt.Errorf("gemini interactive executor not yet implemented")
}

// ResumeSession resumes a previous session by ID.
func (e *InteractiveExecutor) ResumeSession(sessionID string, prompt string) error {
	// TODO: Load conversation history and continue
	return fmt.Errorf("gemini session resume not yet implemented")
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
