// Package providers imports all LLM provider implementations to register them
// with the global factory. Import this package to enable all providers.
//
// Usage:
//
//	import (
//	    "github.com/getfinn/finn/internal/llm"
//	    _ "github.com/getfinn/finn/internal/llm/providers" // Register all providers
//	)
//
//	func main() {
//	    executor, err := llm.NewInteractiveExecutor(llm.ProviderClaude, "/path/to/project", eventHandler)
//	    // or
//	    executor, err := llm.NewInteractiveExecutor(llm.ProviderGemini, "/path/to/project", eventHandler)
//	}
package providers

import (
	// Import providers to trigger their init() functions which register with factory
	_ "github.com/getfinn/finn/internal/llm/providers/claude"
	_ "github.com/getfinn/finn/internal/llm/providers/codex"
	_ "github.com/getfinn/finn/internal/llm/providers/gemini"
)
