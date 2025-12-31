package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// Executor handles Claude Code CLI execution
type Executor struct {
	projectPath string
}

// NewExecutor creates a new Claude Code executor
func NewExecutor(projectPath string) *Executor {
	return &Executor{
		projectPath: projectPath,
	}
}

// StreamMessage represents a message from Claude's streaming output
type StreamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	Message struct {
		Content []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text,omitempty"`
			Name    string          `json:"name,omitempty"`
			Input   json.RawMessage `json:"input,omitempty"`
			ID      string          `json:"id,omitempty"`
		} `json:"content"`
		StopReason string     `json:"stop_reason,omitempty"`
		Model      string     `json:"model,omitempty"`
		Usage      *UsageInfo `json:"usage,omitempty"`
	} `json:"message,omitempty"`
	Result string `json:"result,omitempty"`

	// Top-level fields for "result" messages
	TopLevelUsage *UsageInfo `json:"usage,omitempty"`          // Final aggregated usage (result messages)
	TotalCostUSD  float64    `json:"total_cost_usd,omitempty"` // Total cost in USD
	DurationMs    int64      `json:"duration_ms,omitempty"`    // Duration in milliseconds
}

// MessageHandler is called for each streaming message
type MessageHandler func(msg StreamMessage) error

// Execute runs a Claude Code prompt and streams the output
func (e *Executor) Execute(prompt string, handler MessageHandler) error {
	// Prepend security instructions
	securityInstructions := fmt.Sprintf(`CRITICAL SECURITY RULES:
1. You are RESTRICTED to working ONLY within the approved project folder: %s
2. DO NOT access, read, or modify ANY files outside this directory under any circumstances
3. If the user requests access to files outside this folder, politely decline and explain the restriction
4. DO NOT commit any changes to git - just make the file changes and stop
5. DO NOT use commands like 'cd ..' or absolute paths that go outside the approved folder

User request: `, e.projectPath)
	fullPrompt := securityInstructions + prompt

	// Build command
	cmd := exec.Command("claude", "-p",
		"--output-format", "stream-json",
		"--verbose", // Required for stream-json output format
		"--dangerously-skip-permissions", // Auto-approve for trusted use
		fullPrompt)

	cmd.Dir = e.projectPath
	cmd.Env = os.Environ() // Use existing environment (Claude Code subscription)

	// Get stdout and stderr pipes
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

	// Stream stdout (Claude's output)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()

			var msg StreamMessage
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				// Call handler for each message
				if handler != nil {
					handler(msg)
				}
			}
		}
	}()

	// Stream stderr (errors)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			// Log errors
			fmt.Printf("Claude stderr: %s\n", scanner.Text())
		}
	}()

	// Wait for completion
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude code failed: %w", err)
	}

	return nil
}

// IsInstalled checks if Claude Code CLI is installed
func IsInstalled() bool {
	_, err := exec.LookPath("claude")
	return err == nil
}

// Note: Claude Code CLI authentication is handled by the user's subscription.
// No API key setup is required - the daemon uses the existing authenticated session.
