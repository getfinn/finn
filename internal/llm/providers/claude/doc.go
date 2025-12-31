// Package claude provides integration with the Claude Code CLI for AI-powered
// code generation and modification.
//
// # Security Model
//
// This package uses the --dangerously-skip-permissions flag when invoking Claude Code.
// This is intentional and safe for the following reasons:
//
// 1. Claude Code's built-in permission prompts require interactive terminal input,
// which doesn't work when running Claude Code programmatically (via stdin/stdout pipes).
//
// 2. PocketVibe implements its own security model that provides equivalent
// or stronger protections:
//
//   - Folder Approval: Users must explicitly approve folders before any code
//     can be generated there. This is managed via the system tray or dashboard.
//
//   - Git-based Review: All code changes are captured as git diffs. Users review
//     the actual changes before committing them.
//
//   - Multi-turn Conversation: Users can request plan revisions, reject changes,
//     or discard all modifications before they're persisted.
//
//   - No Direct File System Access: Generated code exists only as diffs until
//     explicitly approved and committed by the user.
//
// # Usage Modes
//
// The package provides two execution modes:
//
// TaskExecutor: One-shot execution for simple tasks. Runs a prompt and returns
// the result without maintaining session state.
//
// InteractiveTaskExecutor: Multi-turn conversation support with decision points,
// plan approvals, and diff review. Maintains session state for follow-up prompts.
//
// # Event Streaming
//
// Both executors emit events via callbacks as Claude processes the request:
//   - Thinking: Claude's reasoning process
//   - ToolUse: File operations, searches, etc.
//   - Decision: AskUserQuestion tool calls requiring user input
//   - Diff: File modification diffs for review
//   - Complete: Task completion
//   - Error: Error conditions
package claude
