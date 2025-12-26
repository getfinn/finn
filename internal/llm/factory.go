package llm

import (
	"fmt"
)

// DefaultFactory is the default executor factory implementation.
type DefaultFactory struct {
	// Registry of provider constructors
	executorConstructors            map[Provider]func(Config) (Executor, error)
	interactiveExecutorConstructors map[Provider]func(Config) (InteractiveExecutor, error)
}

// NewFactory creates a new executor factory.
// Provider implementations register themselves via Register methods.
func NewFactory() *DefaultFactory {
	return &DefaultFactory{
		executorConstructors:            make(map[Provider]func(Config) (Executor, error)),
		interactiveExecutorConstructors: make(map[Provider]func(Config) (InteractiveExecutor, error)),
	}
}

// RegisterExecutor registers a constructor for a one-shot executor.
func (f *DefaultFactory) RegisterExecutor(provider Provider, constructor func(Config) (Executor, error)) {
	f.executorConstructors[provider] = constructor
}

// RegisterInteractiveExecutor registers a constructor for an interactive executor.
func (f *DefaultFactory) RegisterInteractiveExecutor(provider Provider, constructor func(Config) (InteractiveExecutor, error)) {
	f.interactiveExecutorConstructors[provider] = constructor
}

// CreateExecutor creates a one-shot executor for the specified provider.
func (f *DefaultFactory) CreateExecutor(cfg Config) (Executor, error) {
	constructor, ok := f.executorConstructors[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("unsupported provider: %s", cfg.Provider)
	}
	return constructor(cfg)
}

// CreateInteractiveExecutor creates an interactive executor for the specified provider.
func (f *DefaultFactory) CreateInteractiveExecutor(cfg Config) (InteractiveExecutor, error) {
	constructor, ok := f.interactiveExecutorConstructors[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("unsupported interactive provider: %s", cfg.Provider)
	}
	return constructor(cfg)
}

// SupportedProviders returns list of registered providers.
func (f *DefaultFactory) SupportedProviders() []Provider {
	providers := make([]Provider, 0, len(f.executorConstructors))
	seen := make(map[Provider]bool)

	for p := range f.executorConstructors {
		if !seen[p] {
			providers = append(providers, p)
			seen[p] = true
		}
	}
	for p := range f.interactiveExecutorConstructors {
		if !seen[p] {
			providers = append(providers, p)
			seen[p] = true
		}
	}

	return providers
}

// Global factory instance with registered providers
var globalFactory *DefaultFactory

// GetFactory returns the global factory instance.
// Providers register themselves during init().
func GetFactory() *DefaultFactory {
	if globalFactory == nil {
		globalFactory = NewFactory()
	}
	return globalFactory
}

// Quick helper functions for common use cases

// NewExecutor creates a one-shot executor using the global factory.
func NewExecutor(provider Provider, projectPath string, onEvent EventHandler) (Executor, error) {
	return GetFactory().CreateExecutor(Config{
		Provider:    provider,
		ProjectPath: projectPath,
		OnEvent:     onEvent,
	})
}

// NewInteractiveExecutor creates an interactive executor using the global factory.
func NewInteractiveExecutor(provider Provider, projectPath string, onEvent EventHandler) (InteractiveExecutor, error) {
	return GetFactory().CreateInteractiveExecutor(Config{
		Provider:    provider,
		ProjectPath: projectPath,
		OnEvent:     onEvent,
	})
}
