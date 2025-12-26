package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/getfinn/finn/internal/claude"
	"github.com/getfinn/finn/internal/git"
	"github.com/getfinn/finn/internal/watcher"
	ws "github.com/getfinn/finn/internal/websocket"
)

// initSessionWatcher initializes the watcher for external Claude Code sessions.
func (a *Agent) initSessionWatcher() {
	callbacks := watcher.SessionCallbacks{
		OnNewSession:     a.handleExternalSessionDetected,
		OnSessionUpdated: a.handleExternalSessionUpdated,
		OnSessionEnd:     a.handleExternalSessionEnded,
		// Only watch sessions in approved folders
		ShouldWatchProject: func(projectPath string) bool {
			for _, folder := range a.cfg.ApprovedFolders {
				if folder.Path == projectPath {
					return true
				}
			}
			return false
		},
	}

	w, err := watcher.NewWatcher(callbacks)
	if err != nil {
		log.Printf("⚠️ Failed to create session watcher: %v", err)
		return
	}

	a.sessionWatcher = w
	w.Start()
}

// handleExternalSessionDetected is called when a new Claude Code session is found.
func (a *Agent) handleExternalSessionDetected(session *watcher.SessionInfo) {
	if !a.hasActiveClients() {
		log.Printf("📡 New session %s (skipping broadcast - no clients)", session.SessionID)
		return
	}

	log.Printf("📡 Broadcasting new external session: %s", session.SessionID)

	folderID := ""
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.Path == session.ProjectPath {
			folderID = folder.ID
			break
		}
	}

	// Ensure git is initialized
	if folderID != "" {
		if err := git.EnsureGitRepo(session.ProjectPath); err != nil {
			log.Printf("⚠️ Failed to ensure git repo: %v", err)
		}
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"session_id":     session.SessionID,
		"project_path":   session.ProjectPath,
		"folder_id":      folderID,
		"title":          session.Title,
		"model":          session.Model,
		"message_count":  session.MessageCount,
		"total_cost_usd": session.TotalCostUSD,
		"last_activity":  session.LastActivity,
		"is_active":      session.IsRecentlyActive(),
		"status":         session.GetStatus(),
		"source":         "claude_code_cli",
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "external_session_detected",
		Payload:    payload,
	}

	a.wsClient.SendMessage(msg)
}

// handleExternalSessionUpdated is called when session metadata changes.
func (a *Agent) handleExternalSessionUpdated(session *watcher.SessionInfo) {
	if !a.hasActiveClients() {
		return
	}

	log.Printf("📊 Session updated: %s (messages: %d, cost: $%.4f)",
		session.SessionID, session.MessageCount, session.TotalCostUSD)

	folderID := ""
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.Path == session.ProjectPath {
			folderID = folder.ID
			break
		}
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"session_id":     session.SessionID,
		"project_path":   session.ProjectPath,
		"folder_id":      folderID,
		"title":          session.Title,
		"model":          session.Model,
		"message_count":  session.MessageCount,
		"total_cost_usd": session.TotalCostUSD,
		"last_activity":  session.LastActivity,
		"is_active":      session.IsRecentlyActive(),
		"status":         session.GetStatus(),
	})

	wsMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "external_session_updated",
		Payload:    payload,
	}

	a.wsClient.SendMessage(wsMsg)
}

// handleExternalSessionEnded is called when a session file stops being modified.
func (a *Agent) handleExternalSessionEnded(sessionID string) {
	if !a.hasActiveClients() {
		log.Printf("🏁 Session %s ended (skipping broadcast - no clients)", sessionID)
		return
	}

	log.Printf("🏁 External session ended: %s", sessionID)

	payload, _ := json.Marshal(map[string]interface{}{
		"session_id": sessionID,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "external_session_ended",
		Payload:    payload,
	}

	a.wsClient.SendMessage(msg)
}

// handleGetSessionMessages handles on-demand request for session messages.
func (a *Agent) handleGetSessionMessages(msg *ws.Message) {
	var payload struct {
		SessionID string `json:"session_id"`
		Offset    int    `json:"offset"`
		Limit     int    `json:"limit"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to parse get_session_messages payload: %v", err)
		return
	}

	if a.sessionWatcher == nil {
		log.Println("⚠️ Session watcher not initialized")
		return
	}

	log.Printf("📥 Fetching messages for session %s (offset: %d, limit: %d)",
		payload.SessionID, payload.Offset, payload.Limit)

	messages, err := a.sessionWatcher.GetSessionMessages(payload.SessionID)
	if err != nil {
		log.Printf("❌ Failed to get session messages: %v", err)
		a.sendSessionMessagesError(payload.SessionID, err.Error())
		return
	}

	// Apply offset and limit
	if payload.Offset > 0 && payload.Offset < len(messages) {
		messages = messages[payload.Offset:]
	}
	if payload.Limit > 0 && payload.Limit < len(messages) {
		messages = messages[:payload.Limit]
	}

	// Convert to serializable format
	messageData := make([]map[string]interface{}, 0, len(messages))
	var pendingTools []string
	var lastToolTimestamp time.Time

	flushTools := func() {
		if len(pendingTools) == 0 {
			return
		}
		var content string
		if len(pendingTools) == 1 {
			content = "🔧 Used: " + pendingTools[0]
		} else if len(pendingTools) <= 3 {
			content = "🔧 Used: " + strings.Join(pendingTools, ", ")
		} else {
			content = fmt.Sprintf("🔧 Used: %s ... and %d more",
				strings.Join(pendingTools[:3], ", "), len(pendingTools)-3)
		}
		messageData = append(messageData, map[string]interface{}{
			"uuid":      fmt.Sprintf("tools-%d", lastToolTimestamp.UnixNano()),
			"type":      "system",
			"role":      "system",
			"content":   content,
			"timestamp": lastToolTimestamp,
		})
		pendingTools = nil
	}

	for _, m := range messages {
		tools := m.GetToolUses()
		if len(tools) > 0 {
			for _, tool := range tools {
				if tool.Name != "" {
					found := false
					for _, pt := range pendingTools {
						if pt == tool.Name {
							found = true
							break
						}
					}
					if !found {
						pendingTools = append(pendingTools, tool.Name)
					}
				}
			}
			lastToolTimestamp = m.Timestamp
		}

		content := m.GetTextContent()
		if content != "" {
			flushTools()
			messageData = append(messageData, map[string]interface{}{
				"uuid":        m.UUID,
				"parent_uuid": m.ParentUUID,
				"type":        m.Type,
				"role":        m.GetRole(),
				"content":     content,
				"model":       m.GetModel(),
				"timestamp":   m.Timestamp,
				"cost_usd":    m.CostUSD,
				"duration_ms": m.DurationMs,
			})
		}
	}

	flushTools()

	responsePayload, _ := json.Marshal(map[string]interface{}{
		"session_id":  payload.SessionID,
		"messages":    messageData,
		"total_count": len(messages),
		"offset":      payload.Offset,
		"has_more":    false,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "session_messages",
		Payload:    responsePayload,
	})

	log.Printf("📤 Sent %d display messages for session %s (from %d raw)", len(messageData), payload.SessionID, len(messages))
}

// sendSessionMessagesError sends an error response for session message requests.
func (a *Agent) sendSessionMessagesError(sessionID, errMsg string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"session_id": sessionID,
		"error":      errMsg,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "session_messages",
		Payload:    payload,
	})
}

// handleResumeSession handles a request to resume an external session from mobile.
func (a *Agent) handleResumeSession(msg *ws.Message) {
	var payload struct {
		SessionID      string `json:"session_id"`
		FolderID       string `json:"folder_id"`
		ProjectPath    string `json:"project_path"`
		ConversationID string `json:"conversation_id"`
		Prompt         string `json:"prompt"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to parse resume_session payload: %v", err)
		a.sendError(payload.ConversationID, "Invalid payload")
		return
	}

	log.Printf("🔄 Resuming session %s from mobile", payload.SessionID)

	var folderPath string
	var actualFolderID string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			actualFolderID = folder.ID
			break
		}
	}

	// Fallback: try matching by path
	if folderPath == "" && payload.ProjectPath != "" {
		log.Printf("⚠️ Folder ID %s not found, trying path lookup: %s", payload.FolderID, payload.ProjectPath)
		for _, folder := range a.cfg.ApprovedFolders {
			if folder.Path == payload.ProjectPath {
				folderPath = folder.Path
				actualFolderID = folder.ID
				log.Printf("✅ Found folder by path: %s (ID: %s)", folder.Path, folder.ID)
				break
			}
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found - ID: %s, Path: %s", payload.FolderID, payload.ProjectPath)
		a.sendError(payload.ConversationID, "Folder not found or not approved")
		return
	}

	payload.FolderID = actualFolderID

	onEvent := func(event claude.Event) {
		a.sendClaudeEvent(payload.ConversationID, event)
	}

	executor := claude.NewInteractiveTaskExecutor(folderPath, onEvent)

	a.executors[payload.ConversationID] = executor
	a.conversationStates[payload.ConversationID] = &ConversationState{
		executor:     executor,
		folderPath:   folderPath,
		folderID:     payload.FolderID,
		pendingDiffs: make(map[string]bool),
	}

	go func() {
		if err := executor.ResumeSession(payload.SessionID, payload.Prompt); err != nil {
			log.Printf("❌ Failed to resume session: %v", err)
			a.sendError(payload.ConversationID, err.Error())
			delete(a.executors, payload.ConversationID)
			delete(a.conversationStates, payload.ConversationID)
			return
		}
	}()

	responsePayload, _ := json.Marshal(map[string]interface{}{
		"session_id":      payload.SessionID,
		"conversation_id": payload.ConversationID,
		"status":          "resuming",
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "session_resumed",
		Payload:    responsePayload,
	})
}

// handleGetExternalSessions returns list of detected external sessions.
func (a *Agent) handleGetExternalSessions(msg *ws.Message) {
	if a.sessionWatcher == nil {
		log.Println("⚠️ Session watcher not initialized")
		return
	}

	sessions := a.sessionWatcher.GetSessions()

	pathToFolderID := make(map[string]string)
	for _, folder := range a.cfg.ApprovedFolders {
		pathToFolderID[folder.Path] = folder.ID
	}

	enrichedSessions := make([]map[string]interface{}, 0)
	for _, session := range sessions {
		if folderID, ok := pathToFolderID[session.ProjectPath]; ok {
			enrichedSessions = append(enrichedSessions, map[string]interface{}{
				"session_id":     session.SessionID,
				"project_path":   session.ProjectPath,
				"folder_id":      folderID,
				"title":          session.Title,
				"model":          session.Model,
				"message_count":  session.MessageCount,
				"total_cost_usd": session.TotalCostUSD,
				"last_activity":  session.LastActivity,
				"is_active":      session.IsRecentlyActive(),
				"status":         session.GetStatus(),
				"source":         "claude_code_cli",
			})
		}
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"sessions": enrichedSessions,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "external_sessions_list",
		Payload:    payload,
	})

	log.Printf("📤 Sent %d external sessions (filtered from %d total)", len(enrichedSessions), len(sessions))
}

// sendExternalSessionsList sends a batch of sessions for a specific project folder.
func (a *Agent) sendExternalSessionsList(sessions []*watcher.SessionInfo, projectPath string) {
	if !a.hasActiveClients() {
		log.Printf("📁 Discovered %d sessions for %s (skipping broadcast - no clients)", len(sessions), projectPath)
		return
	}

	var folderID string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.Path == projectPath {
			folderID = folder.ID
			break
		}
	}

	enrichedSessions := make([]map[string]interface{}, 0, len(sessions))
	for _, session := range sessions {
		enrichedSessions = append(enrichedSessions, map[string]interface{}{
			"session_id":     session.SessionID,
			"project_path":   session.ProjectPath,
			"folder_id":      folderID,
			"title":          session.Title,
			"model":          session.Model,
			"message_count":  session.MessageCount,
			"total_cost_usd": session.TotalCostUSD,
			"last_activity":  session.LastActivity,
			"is_active":      session.IsRecentlyActive(),
			"status":         session.GetStatus(),
			"source":         "claude_code_cli",
		})
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"sessions":     enrichedSessions,
		"folder_id":    folderID,
		"project_path": projectPath,
		"batch_type":   "folder_add",
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "external_sessions_list",
		Payload:    payload,
	})

	log.Printf("📤 Batch-sent %d sessions for newly added folder %s", len(sessions), projectPath)
}
