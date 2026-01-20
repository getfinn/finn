package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/getfinn/finn/internal/llm/providers/claude"
	"github.com/getfinn/finn/internal/git"
	"github.com/getfinn/finn/internal/llm"
	_ "github.com/getfinn/finn/internal/llm/providers" // Register all LLM providers
	"github.com/getfinn/finn/internal/llm/providers/gemini"
	ws "github.com/getfinn/finn/internal/websocket"
)

// handlePrompt handles a prompt message from mobile.
// This starts an LLM task execution (Claude or Gemini based on config or message override).
func (a *Agent) handlePrompt(msg *ws.Message) {
	var payload struct {
		ConversationID string `json:"conversation_id"`
		FolderID       string `json:"folder_id"`
		Text           string `json:"text"`
		SessionID      string `json:"session_id,omitempty"`    // If provided, resume this session
		LLMProvider    string `json:"llm_provider,omitempty"` // LLM provider to use: "claude", "gemini", etc.
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal prompt payload: %v", err)
		return
	}

	// Use provider from message if specified, otherwise fall back to config default
	llmProvider := payload.LLMProvider
	if llmProvider == "" {
		llmProvider = a.cfg.ExecutionMode.GetLLMProvider()
	}
	log.Printf("📝 Received prompt: %s (folder: %s, session: %s, provider: %s)", payload.Text, payload.FolderID, payload.SessionID, llmProvider)

	// Find the approved folder
	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found or not approved: %s", payload.FolderID)
		a.sendError(payload.ConversationID, "Folder not found or not approved")
		return
	}

	// Check if the selected LLM CLI is installed
	if llmProvider == "gemini" {
		if !gemini.IsInstalled() {
			log.Println("❌ Gemini CLI not installed")
			a.sendError(payload.ConversationID, "Gemini CLI not installed. Please run: npm install -g @google/gemini-cli")
			return
		}
	} else {
		// Default to Claude
		if !claude.IsInstalled() {
			log.Println("❌ Claude Code CLI not installed")
			a.sendError(payload.ConversationID, "Claude Code CLI not installed. Please run: npm install -g @anthropic-ai/claude-code")
			return
		}
	}

	// Create event handlers - one for Claude (uses claude.Event) and one for LLM factory (uses llm.Event)
	onClaudeEvent := func(event claude.Event) {
		// Track diff events to manage approval flow
		if event.Type == claude.EventTypeDiff {
			a.conversationStatesMu.RLock()
			state := a.conversationStates[payload.ConversationID]
			a.conversationStatesMu.RUnlock()
			if state != nil {
				a.trackDiffEvent(state, event)
			}
		}
		// Convert Claude events to WebSocket messages and send to mobile
		a.sendClaudeEvent(payload.ConversationID, event)
	}

	onLLMEvent := func(event llm.Event) {
		// Convert llm.Event to claude.Event for compatibility
		claudeEvent := claude.Event{
			Type:    claude.EventType(event.Type),
			Content: event.Content,
		}
		// Track diff events to manage approval flow
		if event.Type == llm.EventTypeDiff {
			a.conversationStatesMu.RLock()
			state := a.conversationStates[payload.ConversationID]
			a.conversationStatesMu.RUnlock()
			if state != nil {
				a.trackDiffEvent(state, claudeEvent)
			}
		}
		// Send event to mobile
		a.sendClaudeEvent(payload.ConversationID, claudeEvent)
	}

	// Mark this folder as having a PocketVibe-initiated session
	// This prevents the session watcher from creating duplicate conversation entries
	a.MarkPocketVibeSession(folderPath)

	// Branch based on provider and execution mode
	if llmProvider == "gemini" {
		if !a.cfg.ExecutionMode.InteractiveMode {
			a.startGeminiOneShotExecution(payload.ConversationID, payload.FolderID, folderPath, payload.Text, onLLMEvent)
		} else {
			a.startGeminiInteractiveExecution(payload.ConversationID, payload.FolderID, folderPath, payload.Text, payload.SessionID, onLLMEvent)
		}
	} else {
		// Default to Claude
		if !a.cfg.ExecutionMode.InteractiveMode {
			a.startOneShotExecution(payload.ConversationID, payload.FolderID, folderPath, payload.Text, onClaudeEvent)
		} else {
			a.startInteractiveExecution(payload.ConversationID, payload.FolderID, folderPath, payload.Text, payload.SessionID, onClaudeEvent)
		}
	}
}

// startOneShotExecution starts a one-shot execution that auto-approves everything.
// Creates conversation state so approvals still work (user can commit changes after viewing).
func (a *Agent) startOneShotExecution(conversationID, folderID, folderPath, prompt string, onEvent func(claude.Event)) {
	log.Println("🚀 Using one-shot mode (auto-approve)")
	requiresApproval := false
	executor := claude.NewTaskExecutor(folderPath, requiresApproval, onEvent)

	// Store executor
	a.executorsMu.Lock()
	a.executors[conversationID] = executor
	a.executorsMu.Unlock()

	// Create conversation state for tracking diffs and handling approvals
	// Even in auto-approve mode, user may want to commit changes after viewing
	a.conversationStatesMu.Lock()
	a.conversationStates[conversationID] = &ConversationState{
		executor:     executor,
		provider:     llm.ProviderClaude,
		pendingDiffs: make(map[string]bool),
		diffContents: make(map[string]string),
		totalDiffs:   0,
		folderPath:   folderPath,
		folderID:     folderID,
	}
	a.conversationStatesMu.Unlock()
	log.Printf("📊 Created conversation state for one-shot: %s (folder: %s)", conversationID, folderID)

	// Execute and mark completed after finish (don't delete state - TTL handles cleanup)
	go func() {
		if err := executor.ExecuteTask(prompt); err != nil {
			log.Printf("❌ Task execution failed: %v", err)
			a.sendError(conversationID, err.Error())
		}
		// Clean up executor but keep conversation state for approvals
		a.executorsMu.Lock()
		delete(a.executors, conversationID)
		a.executorsMu.Unlock()

		// Mark conversation as completed (TTL cleanup will handle deletion)
		a.conversationStatesMu.Lock()
		if state, exists := a.conversationStates[conversationID]; exists {
			state.isCompleted = true
			now := time.Now()
			state.completedAt = &now
			log.Printf("✅ One-shot task completed, state preserved for approvals: %s", conversationID)
		}
		a.conversationStatesMu.Unlock()

		// Unmark PocketVibe session now that execution is complete
		a.UnmarkPocketVibeSession(folderPath)
	}()
}

// startInteractiveExecution starts an interactive execution that asks for decisions.
func (a *Agent) startInteractiveExecution(conversationID, folderID, folderPath, prompt, sessionID string, onEvent func(claude.Event)) {
	log.Println("🤝 Using interactive mode (user decisions required)")
	interactiveExec := claude.NewInteractiveTaskExecutor(folderPath, onEvent)

	// Set up session linking callback
	interactiveExec.SetSessionLinkedHandler(func(sid string) {
		a.sendSessionLinked(conversationID, sid, folderID)
		// Unmark the PocketVibe session once it's linked (dedup is now handled by relay)
		a.UnmarkPocketVibeSession(folderPath)
	})

	// Store executor
	a.executorsMu.Lock()
	a.executors[conversationID] = interactiveExec
	a.executorsMu.Unlock()

	// Create conversation state for tracking approvals
	a.conversationStatesMu.Lock()
	a.conversationStates[conversationID] = &ConversationState{
		executor:     interactiveExec,
		provider:     llm.ProviderClaude,
		pendingDiffs: make(map[string]bool),
		diffContents: make(map[string]string),
		totalDiffs:   0,
		folderPath:   folderPath,
		folderID:     folderID,
	}
	a.conversationStatesMu.Unlock()
	log.Printf("📊 Created conversation state for: %s (folder: %s, provider: claude)", conversationID, folderID)

	if sessionID != "" {
		// Resume existing session
		log.Printf("🔄 Resuming existing session: %s", sessionID)
		go func() {
			if err := interactiveExec.ResumeSession(sessionID, prompt); err != nil {
				log.Printf("❌ Session resume failed: %v", err)
				a.sendError(conversationID, err.Error())
				a.executorsMu.Lock()
				delete(a.executors, conversationID)
				a.executorsMu.Unlock()
				a.conversationStatesMu.Lock()
				delete(a.conversationStates, conversationID)
				a.conversationStatesMu.Unlock()
				// Unmark PocketVibe session on error to prevent leaks
				a.UnmarkPocketVibeSession(folderPath)
			}
		}()
	} else {
		// Start new session
		go func() {
			if err := interactiveExec.ExecuteTask(prompt); err != nil {
				log.Printf("❌ Task execution failed: %v", err)
				a.sendError(conversationID, err.Error())
				a.executorsMu.Lock()
				delete(a.executors, conversationID)
				a.executorsMu.Unlock()
				a.conversationStatesMu.Lock()
				delete(a.conversationStates, conversationID)
				a.conversationStatesMu.Unlock()
				// Unmark PocketVibe session on error to prevent leaks
				a.UnmarkPocketVibeSession(folderPath)
			}
		}()
	}
}

// startGeminiOneShotExecution starts a one-shot Gemini execution.
// Creates conversation state so approvals still work (user can commit changes after viewing).
func (a *Agent) startGeminiOneShotExecution(conversationID, folderID, folderPath, prompt string, onEvent func(llm.Event)) {
	log.Println("🚀 [Gemini] Using one-shot mode (auto-approve)")

	executor, err := llm.NewExecutor(llm.ProviderGemini, folderPath, onEvent)
	if err != nil {
		log.Printf("❌ [Gemini] Failed to create executor: %v", err)
		a.sendError(conversationID, fmt.Sprintf("Failed to create Gemini executor: %v", err))
		// Unmark PocketVibe session on error to prevent leaks
		a.UnmarkPocketVibeSession(folderPath)
		return
	}

	// Store executor (wrapped to satisfy interface)
	a.executorsMu.Lock()
	a.llmExecutors[conversationID] = executor
	a.executorsMu.Unlock()

	// Create conversation state for tracking diffs and handling approvals
	// Note: executor field left nil since we don't need it for approval handling
	a.conversationStatesMu.Lock()
	a.conversationStates[conversationID] = &ConversationState{
		provider:     llm.ProviderGemini,
		pendingDiffs: make(map[string]bool),
		diffContents: make(map[string]string),
		totalDiffs:   0,
		folderPath:   folderPath,
		folderID:     folderID,
	}
	a.conversationStatesMu.Unlock()
	log.Printf("📊 [Gemini] Created conversation state for one-shot: %s (folder: %s)", conversationID, folderID)

	go func() {
		if err := executor.ExecuteTask(prompt); err != nil {
			log.Printf("❌ [Gemini] Task execution failed: %v", err)
			a.sendError(conversationID, err.Error())
		}
		a.executorsMu.Lock()
		delete(a.llmExecutors, conversationID)
		a.executorsMu.Unlock()

		// Mark conversation as completed (TTL cleanup will handle deletion)
		a.conversationStatesMu.Lock()
		if state, exists := a.conversationStates[conversationID]; exists {
			state.isCompleted = true
			now := time.Now()
			state.completedAt = &now
			log.Printf("✅ [Gemini] One-shot task completed, state preserved for approvals: %s", conversationID)
		}
		a.conversationStatesMu.Unlock()

		// Unmark PocketVibe session now that execution is complete
		a.UnmarkPocketVibeSession(folderPath)
	}()
}

// startGeminiInteractiveExecution starts an interactive Gemini execution.
func (a *Agent) startGeminiInteractiveExecution(conversationID, folderID, folderPath, prompt, sessionID string, onEvent func(llm.Event)) {
	log.Println("🤝 [Gemini] Using interactive mode")

	executor, err := llm.NewInteractiveExecutor(llm.ProviderGemini, folderPath, onEvent)
	if err != nil {
		log.Printf("❌ [Gemini] Failed to create interactive executor: %v", err)
		a.sendError(conversationID, fmt.Sprintf("Failed to create Gemini executor: %v", err))
		// Unmark PocketVibe session on error to prevent leaks
		a.UnmarkPocketVibeSession(folderPath)
		return
	}

	// Set up session linking callback
	executor.SetSessionLinkedHandler(func(sid string) {
		a.sendSessionLinked(conversationID, sid, folderID)
		// Unmark the PocketVibe session once it's linked (dedup is now handled by relay)
		a.UnmarkPocketVibeSession(folderPath)
	})

	// Store executor
	a.executorsMu.Lock()
	a.llmInteractiveExecutors[conversationID] = executor
	a.executorsMu.Unlock()

	// Create conversation state for tracking approvals
	a.conversationStatesMu.Lock()
	a.conversationStates[conversationID] = &ConversationState{
		llmExecutor:  executor,
		provider:     llm.ProviderGemini,
		pendingDiffs: make(map[string]bool),
		diffContents: make(map[string]string),
		totalDiffs:   0,
		folderPath:   folderPath,
		folderID:     folderID,
	}
	a.conversationStatesMu.Unlock()
	log.Printf("📊 [Gemini] Created conversation state for: %s (folder: %s, provider: gemini)", conversationID, folderID)

	if sessionID != "" {
		log.Printf("🔄 [Gemini] Resuming session: %s", sessionID)
		go func() {
			if err := executor.ResumeSession(sessionID, prompt); err != nil {
				log.Printf("❌ [Gemini] Session resume failed: %v", err)
				a.sendError(conversationID, err.Error())
				a.executorsMu.Lock()
				delete(a.llmInteractiveExecutors, conversationID)
				a.executorsMu.Unlock()
				a.conversationStatesMu.Lock()
				delete(a.conversationStates, conversationID)
				a.conversationStatesMu.Unlock()
				// Unmark PocketVibe session on error to prevent leaks
				a.UnmarkPocketVibeSession(folderPath)
			}
		}()
	} else {
		go func() {
			if err := executor.Start(prompt); err != nil {
				log.Printf("❌ [Gemini] Task execution failed: %v", err)
				a.sendError(conversationID, err.Error())
				a.executorsMu.Lock()
				delete(a.llmInteractiveExecutors, conversationID)
				a.executorsMu.Unlock()
				a.conversationStatesMu.Lock()
				delete(a.conversationStates, conversationID)
				a.conversationStatesMu.Unlock()
				// Unmark PocketVibe session on error to prevent leaks
				a.UnmarkPocketVibeSession(folderPath)
			}
		}()
	}
}

// trackDiffEvent tracks a diff event for approval management.
// Also stores diff content for potential resend on mobile reconnection.
func (a *Agent) trackDiffEvent(state *ConversationState, event claude.Event) {
	var diffData map[string]interface{}
	if err := json.Unmarshal(event.Content, &diffData); err != nil {
		return
	}

	if state.pendingDiffs == nil {
		state.pendingDiffs = make(map[string]bool)
	}
	if state.diffContents == nil {
		state.diffContents = make(map[string]string)
	}

	// Incremental diff (single file)
	if filePath, ok := diffData["file_path"].(string); ok && filePath != "" {
		if !state.pendingDiffs[filePath] {
			state.pendingDiffs[filePath] = false
			state.totalDiffs++
			state.files = append(state.files, filePath)
			log.Printf("📊 Tracking diff for approval: %s (total: %d)", filePath, state.totalDiffs)
		}
		// Store the diff content for potential resend on reconnection
		if diffContent, ok := diffData["diff"].(string); ok {
			state.diffContents[filePath] = diffContent
		}
	}

	// Batch diff format (multiple files in "diffs" map)
	if diffsMap, ok := diffData["diffs"].(map[string]interface{}); ok {
		for filePath, diffContent := range diffsMap {
			if !state.pendingDiffs[filePath] {
				state.pendingDiffs[filePath] = false
				state.totalDiffs++
				state.files = append(state.files, filePath)
				log.Printf("📊 Tracking diff for approval: %s (total: %d)", filePath, state.totalDiffs)
			}
			// Store the diff content for potential resend on reconnection
			if content, ok := diffContent.(string); ok {
				state.diffContents[filePath] = content
			}
		}
	}
}

// handleChoice handles a user's choice response.
func (a *Agent) handleChoice(msg *ws.Message) {
	var payload struct {
		ConversationID string `json:"conversation_id"`
		SelectedID     string `json:"selected_id"`
		Remember       bool   `json:"remember,omitempty"`
		ToolName       string `json:"tool_name,omitempty"`
		DecisionType   string `json:"decision_type,omitempty"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal choice payload: %v", err)
		return
	}

	log.Printf("✅ User selected: %s for conversation: %s (remember=%v, tool=%s)",
		payload.SelectedID, payload.ConversationID, payload.Remember, payload.ToolName)

	// Build choice message
	var choiceMessage string
	if payload.DecisionType == "plan_approval" {
		if payload.SelectedID == "approve" {
			choiceMessage = "Yes, proceed with the plan"
		} else {
			choiceMessage = "No, let me suggest some changes"
		}
	} else {
		choiceMessage = fmt.Sprintf("I choose option %s", payload.SelectedID)
	}

	// Check for LLM interactive executor (Gemini, etc.)
	a.executorsMu.RLock()
	llmExecutor, llmExists := a.llmInteractiveExecutors[payload.ConversationID]
	a.executorsMu.RUnlock()
	if llmExists {
		log.Printf("🔄 Sending choice to LLM interactive executor")

		if err := llmExecutor.SendChoice(choiceMessage); err != nil {
			log.Printf("❌ Failed to send choice to LLM executor: %v", err)
			a.sendError(payload.ConversationID, fmt.Sprintf("Failed to send choice: %v", err))
			return
		}

		log.Println("✅ Choice sent - waiting for LLM to continue...")
		return
	}

	// Check for Claude executor
	a.executorsMu.RLock()
	executor, exists := a.executors[payload.ConversationID]
	a.executorsMu.RUnlock()
	if !exists {
		log.Printf("❌ No active executor for conversation: %s", payload.ConversationID)
		a.sendError(payload.ConversationID, "No active task for this conversation")
		return
	}

	if interactive, ok := executor.(*claude.InteractiveTaskExecutor); ok {
		log.Printf("🔄 Sending choice to Claude interactive executor")

		if err := interactive.SendMessage(choiceMessage); err != nil {
			log.Printf("❌ Failed to send choice: %v", err)
			a.sendError(payload.ConversationID, fmt.Sprintf("Failed to send choice: %v", err))
			return
		}

		log.Println("✅ Choice sent - waiting for Claude to continue...")
	} else {
		log.Printf("⚠️  Choice received for one-shot executor (unexpected) - sending mock completion")
		a.sendMockCompletion(payload.ConversationID)
	}
}

// handleApproval handles a user's approval of changes.
func (a *Agent) handleApproval(msg *ws.Message) {
	var payload struct {
		ConversationID string `json:"conversation_id"`
		Approved       bool   `json:"approved"`
		CommitMessage  string `json:"commit_message,omitempty"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal approval payload: %v", err)
		return
	}

	a.conversationStatesMu.RLock()
	state, hasState := a.conversationStates[payload.ConversationID]
	a.conversationStatesMu.RUnlock()

	if !hasState {
		log.Printf("❌ No conversation state for: %s (daemon may have restarted)", payload.ConversationID)
		a.sendError(payload.ConversationID, "Conversation has expired. Please restart the task.")
		return
	}

	// Check if already completed (late duplicate approval)
	if state.isCompleted {
		log.Printf("⚠️ Conversation already completed: %s (ignoring duplicate approval)", payload.ConversationID)
		return
	}

	folderPath := state.folderPath
	if folderPath == "" {
		log.Printf("❌ No folder path in state for conversation: %s", payload.ConversationID)
		a.sendError(payload.ConversationID, "Unable to determine project folder")
		return
	}

	repo := git.NewRepository(folderPath)

	if payload.Approved {
		log.Printf("✅ Changes approved - committing %d files in folder: %s", len(state.files), folderPath)
		commitMsg := payload.CommitMessage
		if commitMsg == "" {
			commitMsg = "Apply changes via Finn"
		}
		log.Printf("📝 Using commit message: %s", commitMsg)
		if err := repo.CommitAndPush(commitMsg); err != nil {
			log.Printf("❌ Failed to commit changes: %v", err)
			a.sendError(payload.ConversationID, fmt.Sprintf("Failed to commit: %v", err))
		} else {
			log.Println("✅ Changes committed successfully")
			a.sendCommitSuccess(payload.ConversationID, folderPath, state.folderID)
		}
	} else {
		log.Printf("❌ Changes rejected - discarding %d conversation files in folder: %s", len(state.files), folderPath)

		var failedFiles []string
		for _, filePath := range state.files {
			log.Printf("  🗑️  Discarding: %s", filePath)
			if err := repo.DiscardFile(filePath); err != nil {
				log.Printf("  ❌ Failed to discard %s: %v", filePath, err)
				failedFiles = append(failedFiles, filePath)
			}
		}

		if len(failedFiles) > 0 {
			log.Printf("❌ Failed to discard %d files", len(failedFiles))
			a.sendError(payload.ConversationID, fmt.Sprintf("Failed to discard some files: %v", failedFiles))
		} else {
			log.Printf("✅ Successfully discarded %d conversation files", len(state.files))
		}
	}

	// Mark conversation as completed (TTL cleanup will handle actual deletion)
	// This allows late approvals from reconnecting mobile clients to still work
	a.conversationStatesMu.Lock()
	if state != nil {
		now := time.Now()
		state.completedAt = &now
		state.isCompleted = true
		// Clear diff contents since changes are now committed or discarded
		state.diffContents = nil
		log.Printf("✅ Marked conversation as completed: %s (TTL cleanup in %v)", payload.ConversationID, ConversationStateTTL)
	}
	a.conversationStatesMu.Unlock()

	// Clean up executors immediately (no longer needed)
	a.executorsMu.Lock()
	delete(a.executors, payload.ConversationID)
	delete(a.llmExecutors, payload.ConversationID)
	delete(a.llmInteractiveExecutors, payload.ConversationID)
	a.executorsMu.Unlock()
}

// handleDiffApproved handles approval of a specific diff file.
func (a *Agent) handleDiffApproved(msg *ws.Message) {
	var payload struct {
		ConversationID string `json:"conversation_id"`
		FilePath       string `json:"file_path"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal diff_approved payload: %v", err)
		return
	}

	log.Printf("✅ Diff approved for file: %s (conversation: %s)", payload.FilePath, payload.ConversationID)

	a.conversationStatesMu.RLock()
	state, exists := a.conversationStates[payload.ConversationID]
	a.conversationStatesMu.RUnlock()

	if !exists {
		log.Printf("⚠️  No conversation state for: %s (may have already completed)", payload.ConversationID)
		return
	}

	// Skip if conversation already completed
	if state.isCompleted {
		log.Printf("⚠️ Conversation already completed: %s (ignoring diff approval)", payload.ConversationID)
		return
	}

	state.pendingDiffs[payload.FilePath] = true
	approvedCount := 0
	for _, approved := range state.pendingDiffs {
		if approved {
			approvedCount++
		}
	}

	log.Printf("📊 Diff approval progress: %d/%d files approved", approvedCount, state.totalDiffs)

	if approvedCount >= state.totalDiffs {
		log.Println("✅ All diffs approved - continuing execution...")

		if interactive, ok := state.executor.(*claude.InteractiveTaskExecutor); ok {
			if err := interactive.ContinueAfterApproval(); err != nil {
				log.Printf("❌ Failed to continue after approval: %v", err)
				a.sendError(payload.ConversationID, fmt.Sprintf("Failed to continue: %v", err))
			}
		} else {
			log.Println("⚠️  Executor is not interactive, cannot continue")
		}
	} else {
		log.Printf("⏳ Waiting for more approvals (%d/%d)", approvedCount, state.totalDiffs)
	}
}

// handleReprompt handles a reprompt request to revise changes.
func (a *Agent) handleReprompt(msg *ws.Message) {
	var payload struct {
		ConversationID string `json:"conversation_id"`
		RepromptText   string `json:"reprompt_text"`
		DiffContext    []struct {
			FilePath string `json:"file_path"`
			Diff     string `json:"diff"`
		} `json:"diff_context"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal reprompt payload: %v", err)
		return
	}

	log.Printf("🔄 Reprompt received: %s (conversation: %s)", payload.RepromptText, payload.ConversationID)

	a.conversationStatesMu.RLock()
	state, exists := a.conversationStates[payload.ConversationID]
	a.conversationStatesMu.RUnlock()

	if !exists {
		log.Printf("❌ No conversation state for: %s", payload.ConversationID)
		a.sendError(payload.ConversationID, "Conversation not found")
		return
	}

	// Skip if conversation already completed
	if state.isCompleted {
		log.Printf("⚠️ Conversation already completed: %s (ignoring reprompt)", payload.ConversationID)
		a.sendError(payload.ConversationID, "Conversation has already completed. Please start a new conversation.")
		return
	}

	contextPrompt := buildRepromptWithContext(payload.RepromptText, payload.DiffContext)

	// Clear the approval state
	state.pendingDiffs = make(map[string]bool)
	state.totalDiffs = 0

	// Branch based on which provider was used for this conversation
	if state.provider == llm.ProviderGemini {
		log.Println("🔄 [Gemini] Creating new executor for reprompt iteration")

		// LLM event handler (for Gemini)
		onLLMEvent := func(event llm.Event) {
			// Convert to claude.Event for compatibility
			claudeEvent := claude.Event{
				Type:    claude.EventType(event.Type),
				Content: event.Content,
			}
			if event.Type == llm.EventTypeDiff {
				a.trackDiffEvent(state, claudeEvent)
			}
			a.sendClaudeEvent(payload.ConversationID, claudeEvent)
		}

		executor, err := llm.NewInteractiveExecutor(llm.ProviderGemini, state.folderPath, onLLMEvent)
		if err != nil {
			log.Printf("❌ [Gemini] Failed to create executor for reprompt: %v", err)
			a.sendError(payload.ConversationID, fmt.Sprintf("Failed to create Gemini executor: %v", err))
			return
		}

		a.executorsMu.Lock()
		a.llmInteractiveExecutors[payload.ConversationID] = executor
		a.executorsMu.Unlock()
		state.llmExecutor = executor

		go func() {
			if err := executor.Start(contextPrompt); err != nil {
				log.Printf("❌ [Gemini] Reprompt execution failed: %v", err)
				a.sendError(payload.ConversationID, err.Error())
			}
		}()
	} else {
		// Default to Claude
		log.Println("🔄 [Claude] Creating new executor for reprompt iteration")

		onEvent := func(event claude.Event) {
			if event.Type == claude.EventTypeDiff {
				a.trackDiffEvent(state, event)
			}
			a.sendClaudeEvent(payload.ConversationID, event)
		}

		executor := claude.NewInteractiveTaskExecutor(state.folderPath, onEvent)

		a.executorsMu.Lock()
		a.executors[payload.ConversationID] = executor
		a.executorsMu.Unlock()
		state.executor = executor

		go func() {
			if err := executor.ExecuteTask(contextPrompt); err != nil {
				log.Printf("❌ [Claude] Reprompt execution failed: %v", err)
				a.sendError(payload.ConversationID, err.Error())
			}
		}()
	}
}

// buildRepromptWithContext builds a context-aware prompt with diff context.
func buildRepromptWithContext(repromptText string, diffs []struct {
	FilePath string `json:"file_path"`
	Diff     string `json:"diff"`
}) string {
	prompt := fmt.Sprintf(`You just made some changes to the codebase. The user reviewed them and wants you to make adjustments.

User's feedback: "%s"

Here are the changes you made:

`, repromptText)

	for _, diff := range diffs {
		prompt += fmt.Sprintf("File: %s\n```diff\n%s\n```\n\n", diff.FilePath, diff.Diff)
	}

	prompt += "Please revise the changes based on the user's feedback."

	return prompt
}

// handleSettingsUpdate handles execution mode settings updates from mobile.
func (a *Agent) handleSettingsUpdate(msg *ws.Message) {
	var payload struct {
		InteractiveMode  bool   `json:"interactiveMode"`
		DiffApprovalMode string `json:"diffApprovalMode"`
		LLMProvider      string `json:"llm_provider,omitempty"` // Default LLM provider
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal settings_update payload: %v", err)
		return
	}

	log.Printf("⚙️  Settings update received - InteractiveMode: %v, DiffApprovalMode: %s, LLMProvider: %s",
		payload.InteractiveMode, payload.DiffApprovalMode, payload.LLMProvider)

	a.cfg.ExecutionMode.InteractiveMode = payload.InteractiveMode
	a.cfg.ExecutionMode.DiffApprovalMode = payload.DiffApprovalMode
	if payload.LLMProvider != "" {
		a.cfg.ExecutionMode.LLMProvider = payload.LLMProvider
	}

	if err := a.cfg.Save(); err != nil {
		log.Printf("❌ Failed to save settings: %v", err)
		return
	}

	log.Println("✅ Settings saved successfully")
}

// sendMockDecision sends a mock decision response (for testing).
func (a *Agent) sendMockDecision(conversationID string) {
	decision := map[string]interface{}{
		"conversation_id": conversationID,
		"question":        "Which styling approach for dark mode?",
		"context":         "I'll help you implement dark mode",
		"options": []map[string]string{
			{"id": "tailwind", "label": "Tailwind dark: variants", "description": "Use Tailwind's built-in dark mode"},
			{"id": "css-vars", "label": "CSS Variables", "description": "Custom properties with theme toggle"},
			{"id": "styled", "label": "styled-components", "description": "ThemeProvider approach"},
			{"id": "custom", "label": "Custom CSS", "description": "Manual CSS with data-theme"},
		},
	}

	payload, _ := json.Marshal(decision)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeDecision,
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send decision: %v", err)
	} else {
		log.Println("📤 Sent decision to mobile")
	}
}

// sendMockCompletion sends a mock completion response (for testing).
func (a *Agent) sendMockCompletion(conversationID string) {
	completion := map[string]interface{}{
		"conversation_id": conversationID,
		"commit_sha":      "abc123",
		"message":         "Changes committed and pushed!",
	}

	payload, _ := json.Marshal(completion)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeComplete,
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send completion: %v", err)
	} else {
		log.Println("📤 Sent completion to mobile")
	}
}

// sendClaudeEvent converts a Claude event to a WebSocket message and sends it.
func (a *Agent) sendClaudeEvent(conversationID string, event claude.Event) {
	var msgType ws.MessageType

	switch event.Type {
	case claude.EventTypeThinking:
		msgType = ws.MessageTypeThinking
	case claude.EventTypeToolUse:
		msgType = ws.MessageTypeToolUse
		// Track file activity for preview auto-selection
		a.trackToolUseActivity(conversationID, event)
	case claude.EventTypeDecision:
		msgType = ws.MessageTypeDecision
	case claude.EventTypeProgress:
		msgType = ws.MessageTypeProgress
	case claude.EventTypeDiff:
		msgType = ws.MessageTypeDiff
	case claude.EventTypeComplete:
		msgType = ws.MessageTypeComplete
	case claude.EventTypeUsage:
		msgType = ws.MessageTypeUsage
	case claude.EventTypeError:
		msgType = ws.MessageTypeError
	default:
		log.Printf("Unknown event type: %s", event.Type)
		return
	}

	payload := map[string]interface{}{
		"conversation_id": conversationID,
		"data":            json.RawMessage(event.Content),
	}
	payloadBytes, _ := json.Marshal(payload)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       msgType,
		Payload:    payloadBytes,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send event: %v", err)
	} else {
		log.Printf("📤 Sent %s event to mobile", msgType)
	}
}

// trackToolUseActivity extracts file paths from tool_use events and records them
// for preview auto-selection in monorepo scenarios.
func (a *Agent) trackToolUseActivity(conversationID string, event claude.Event) {
	// Parse tool_use content: {"tool": "...", "input": {"file_path": "..."}}
	var toolInfo struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(event.Content, &toolInfo); err != nil {
		return
	}

	// Only track file modification tools
	switch toolInfo.Tool {
	case "Edit", "Write", "edit", "write", "write_file", "edit_file":
		// Extract file_path from input
		var input struct {
			FilePath string `json:"file_path"`
		}
		if err := json.Unmarshal(toolInfo.Input, &input); err != nil {
			return
		}
		if input.FilePath == "" {
			return
		}

		// Get the folder ID for this conversation
		a.conversationStatesMu.RLock()
		state := a.conversationStates[conversationID]
		a.conversationStatesMu.RUnlock()
		if state == nil {
			return
		}

		// Record activity for preview auto-selection
		a.devServers.RecordFileActivity(state.folderID, input.FilePath)
	}
}

// sendError sends an error message to mobile.
func (a *Agent) sendError(conversationID string, message string) {
	errorData := map[string]interface{}{
		"conversation_id": conversationID,
		"message":         message,
	}
	payload, _ := json.Marshal(errorData)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeError,
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send error: %v", err)
	}
}

// sendSessionLinked sends a session_linked event to relay server.
// This links the mobile-initiated conversation_id with Claude's session_id
// so they can be merged in the database.
func (a *Agent) sendSessionLinked(conversationID, sessionID, folderID string) {
	log.Printf("🔗 Linking session: conversation_id=%s, session_id=%s, folder_id=%s",
		conversationID, sessionID, folderID)

	payload, _ := json.Marshal(map[string]interface{}{
		"conversation_id": conversationID,
		"session_id":      sessionID,
		"folder_id":       folderID,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeSessionLinked,
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send session_linked: %v", err)
	} else {
		log.Printf("✅ Sent session_linked event to relay")
	}
}
