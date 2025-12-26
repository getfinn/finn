// Package claude provides the Claude Code CLI implementation of the LLM executor interface.
package claude

import (
	"github.com/getfinn/finn/internal/claude"
	"github.com/getfinn/finn/internal/llm"
)

func init() {
	// Register Claude provider with the global factory
	factory := llm.GetFactory()
	factory.RegisterExecutor(llm.ProviderClaude, NewExecutor)
	factory.RegisterInteractiveExecutor(llm.ProviderClaude, NewInteractiveExecutor)
}

// Executor wraps claude.TaskExecutor to implement llm.Executor.
type Executor struct {
	inner *claude.TaskExecutor
}

// NewExecutor creates a new Claude executor.
func NewExecutor(cfg llm.Config) (llm.Executor, error) {
	// Convert llm.EventHandler to claude.EventHandler
	claudeHandler := func(e claude.Event) {
		cfg.OnEvent(llm.Event{
			Type:    llm.EventType(e.Type),
			Content: e.Content,
		})
	}

	return &Executor{
		inner: claude.NewTaskExecutor(cfg.ProjectPath, false, claudeHandler),
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *Executor) ExecuteTask(prompt string) error {
	return e.inner.ExecuteTask(prompt)
}

// Provider returns the provider type.
func (e *Executor) Provider() llm.Provider {
	return llm.ProviderClaude
}

// InteractiveExecutor wraps claude.InteractiveTaskExecutor to implement llm.InteractiveExecutor.
type InteractiveExecutor struct {
	inner   *claude.InteractiveTaskExecutor
	running bool
}

// NewInteractiveExecutor creates a new Claude interactive executor.
func NewInteractiveExecutor(cfg llm.Config) (llm.InteractiveExecutor, error) {
	// Convert llm.EventHandler to claude.EventHandler
	claudeHandler := func(e claude.Event) {
		cfg.OnEvent(llm.Event{
			Type:    llm.EventType(e.Type),
			Content: e.Content,
		})
	}

	return &InteractiveExecutor{
		inner: claude.NewInteractiveTaskExecutor(cfg.ProjectPath, claudeHandler),
	}, nil
}

// ExecuteTask runs a task with the given prompt.
func (e *InteractiveExecutor) ExecuteTask(prompt string) error {
	return e.inner.ExecuteTask(prompt)
}

// Provider returns the provider type.
func (e *InteractiveExecutor) Provider() llm.Provider {
	return llm.ProviderClaude
}

// Start begins an interactive session.
// For Claude, this is equivalent to ExecuteTask.
func (e *InteractiveExecutor) Start(initialPrompt string) error {
	e.running = true
	return e.inner.ExecuteTask(initialPrompt)
}

// SendChoice sends the user's choice for a decision point.
// Claude uses SendMessage for this.
func (e *InteractiveExecutor) SendChoice(choiceID string) error {
	return e.inner.SendMessage(choiceID)
}

// SendFollowUp sends a follow-up prompt in the conversation.
func (e *InteractiveExecutor) SendFollowUp(prompt string) error {
	return e.inner.SendMessage(prompt)
}

// ResumeSession resumes a previous session by ID.
func (e *InteractiveExecutor) ResumeSession(sessionID string, prompt string) error {
	e.running = true
	return e.inner.ResumeSession(sessionID, prompt)
}

// Stop terminates the interactive session.
func (e *InteractiveExecutor) Stop() {
	e.running = false
	_ = e.inner.Stop() // Ignore error - best effort cleanup
}

// IsRunning returns whether the session is active.
func (e *InteractiveExecutor) IsRunning() bool {
	return e.running
}

// SetSessionLinkedHandler sets callback for session ID detection.
func (e *InteractiveExecutor) SetSessionLinkedHandler(handler func(sessionID string)) {
	e.inner.SetSessionLinkedHandler(handler)
}
