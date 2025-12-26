package agent

import (
	"encoding/json"
	"log"

	ws "github.com/getfinn/finn/internal/websocket"
)

// handleMessage is the main message router for incoming WebSocket messages.
// It dispatches messages to the appropriate handler based on message type.
func (a *Agent) handleMessage(msg *ws.Message) {
	// Skip logging for high-frequency messages to reduce noise
	if msg.Type != ws.MessageTypePresence {
		log.Printf("Handling message of type: %s", msg.Type)
	}

	switch msg.Type {
	// Claude execution messages
	case ws.MessageTypePrompt:
		a.handlePrompt(msg)
	case ws.MessageTypeChoice:
		a.handleChoice(msg)
	case ws.MessageTypeApproval:
		a.handleApproval(msg)
	case ws.MessageTypeDiffApproved:
		a.handleDiffApproved(msg)
	case ws.MessageTypeReprompt:
		a.handleReprompt(msg)
	case ws.MessageTypeSettingsUpdate:
		a.handleSettingsUpdate(msg)

	// Folder management messages
	case "folder_sync":
		log.Println("📋 Dashboard requested folder list")
		a.sendFolderListUpdate()
	case "folder_add_request":
		a.handleFolderAddRequest(msg)
	case "folder_remove_request":
		a.handleFolderRemoveRequest(msg)
	case "folder_select":
		a.handleFolderSelectRequest(msg)
	case "browse_folders":
		a.handleBrowseFolders(msg)

	// Git messages
	case "git_init":
		a.handleGitInit(msg)
	case ws.MessageTypeGetCommits:
		log.Println("📜 Mobile requested commit list")
		a.handleGetCommits(msg)
	case ws.MessageTypeGetCommitDetail:
		log.Println("📜 Mobile requested commit details")
		a.handleGetCommitDetail(msg)
	case "request_commit_sync":
		a.handleRequestCommitSync(msg)

	// Session messages
	case "resume_session":
		a.handleResumeSession(msg)
	case "get_external_sessions":
		a.handleGetExternalSessions(msg)
	case "get_session_messages":
		a.handleGetSessionMessages(msg)

	// Preview messages
	case ws.MessageTypePreviewStart:
		a.handlePreviewStart(msg)
	case ws.MessageTypePreviewStop:
		a.handlePreviewStop(msg)

	// System messages
	case "error":
		a.handleErrorMessage(msg)
	case "presence":
		a.handlePresenceUpdate(msg)

	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// handleErrorMessage handles error messages from the relay server.
func (a *Agent) handleErrorMessage(msg *ws.Message) {
	var payload struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("⚠️ Received error from relay (unparseable payload): %s", string(msg.Payload))
		return
	}

	// Log the error with appropriate severity
	errorMsg := payload.Error
	if errorMsg == "" {
		errorMsg = payload.Message
	}

	if payload.Code == "rate_limit" {
		log.Printf("⚠️ Rate limited by relay server: %s", errorMsg)
	} else {
		log.Printf("⚠️ Error from relay server: %s (code: %s)", errorMsg, payload.Code)
	}
}

// handlePresenceUpdate handles presence updates from the relay server.
// This tells us when mobile/web clients connect or disconnect.
func (a *Agent) handlePresenceUpdate(msg *ws.Message) {
	var payload struct {
		DeviceType string `json:"device_type"`
		Online     bool   `json:"online"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("⚠️ Failed to parse presence payload: %v", err)
		return
	}

	switch payload.DeviceType {
	case "mobile":
		// Only log if state actually changed (reduces noise during hot reload)
		if a.mobileOnline != payload.Online {
			a.mobileOnline = payload.Online
			if payload.Online {
				log.Println("📱 Mobile client connected")
			} else {
				log.Println("📱 Mobile client disconnected")
			}
		}
	case "web":
		// Only log if state actually changed
		if a.webOnline != payload.Online {
			a.webOnline = payload.Online
			if payload.Online {
				log.Println("🌐 Web client connected")
			} else {
				log.Println("🌐 Web client disconnected")
			}
		}
	}
}

// hasActiveClients returns true if any mobile or web clients are connected.
func (a *Agent) hasActiveClients() bool {
	return a.mobileOnline || a.webOnline
}
