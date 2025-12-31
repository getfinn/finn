// Package claude provides the Claude Code CLI implementation of the LLM executor interface.
package claude

import (
	"github.com/getfinn/finn/internal/llm"
)

func init() {
	// Register Claude provider with the global factory
	factory := llm.GetFactory()
	factory.RegisterExecutor(llm.ProviderClaude, NewLLMExecutor)
	factory.RegisterInteractiveExecutor(llm.ProviderClaude, NewLLMInteractiveExecutor)
}

// LLMExecutor wraps TaskExecutor to implement llm.Executor.
type LLMExecutor struct {
	inner *TaskExecutor
}

// NewLLMExecutor creates a new Claude executor for the LLM factory.
func NewLLMExecutor(cfg llm.Config) (llm.Executor, error) {
	// Convert llm.EventHandler to claude.EventHandler
	claudeHandler := func(e Event) {
		cfg.OnEvent(llm.Event{
			Type:    llm.EventType(e.Type),
			Content: e.Content,
		})
	}

	return &LLMExecutor{
		inner: NewTaskExecutor(cfg.ProjectPath, false, claudeHandler),
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *LLMExecutor) ExecuteTask(prompt string) error {
	return e.inner.ExecuteTask(prompt)
}

// Provider returns the provider type.
func (e *LLMExecutor) Provider() llm.Provider {
	return llm.ProviderClaude
}

// LLMInteractiveExecutor wraps InteractiveTaskExecutor to implement llm.InteractiveExecutor.
type LLMInteractiveExecutor struct {
	inner   *InteractiveTaskExecutor
	running bool
}

// NewLLMInteractiveExecutor creates a new Claude interactive executor for the LLM factory.
func NewLLMInteractiveExecutor(cfg llm.Config) (llm.InteractiveExecutor, error) {
	// Convert llm.EventHandler to claude.EventHandler
	claudeHandler := func(e Event) {
		cfg.OnEvent(llm.Event{
			Type:    llm.EventType(e.Type),
			Content: e.Content,
		})
	}

	return &LLMInteractiveExecutor{
		inner: NewInteractiveTaskExecutor(cfg.ProjectPath, claudeHandler),
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *LLMInteractiveExecutor) ExecuteTask(prompt string) error {
	return e.inner.ExecuteTask(prompt)
}

// Provider returns the provider type.
func (e *LLMInteractiveExecutor) Provider() llm.Provider {
	return llm.ProviderClaude
}

// Start begins an interactive session.
// For Claude, this is equivalent to ExecuteTask.
func (e *LLMInteractiveExecutor) Start(initialPrompt string) error {
	e.running = true
	return e.inner.ExecuteTask(initialPrompt)
}

// SendChoice sends the user's choice for a decision point.
// Claude uses SendMessage for this.
func (e *LLMInteractiveExecutor) SendChoice(choiceID string) error {
	return e.inner.SendMessage(choiceID)
}

// SendFollowUp sends a follow-up prompt in the conversation.
func (e *LLMInteractiveExecutor) SendFollowUp(prompt string) error {
	return e.inner.SendMessage(prompt)
}

// ResumeSession resumes a previous session by ID.
func (e *LLMInteractiveExecutor) ResumeSession(sessionID string, prompt string) error {
	e.running = true
	return e.inner.ResumeSession(sessionID, prompt)
}

// Stop terminates the interactive session.
func (e *LLMInteractiveExecutor) Stop() {
	e.running = false
	_ = e.inner.Stop() // Ignore error - best effort cleanup
}

// IsRunning returns whether the session is active.
func (e *LLMInteractiveExecutor) IsRunning() bool {
	return e.running
}

// SetSessionLinkedHandler sets callback for session ID detection.
func (e *LLMInteractiveExecutor) SetSessionLinkedHandler(handler func(sessionID string)) {
	e.inner.SetSessionLinkedHandler(handler)
}
