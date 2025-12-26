package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/getfinn/finn/internal/devserver"
	"github.com/getfinn/finn/internal/tunnel"
	ws "github.com/getfinn/finn/internal/websocket"
)

// handlePreviewStart starts a live preview tunnel for a folder.
// This enables real-time web preview of the development server running
// on the user's machine through a secure tunnel.
func (a *Agent) handlePreviewStart(msg *ws.Message) {
	var payload struct {
		FolderID  string `json:"folder_id"`
		LocalPort int    `json:"local_port"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to unmarshal preview_start payload: %v", err)
		a.sendPreviewStatus(payload.FolderID, "error", "Invalid request payload")
		return
	}

	log.Printf("🔗 Preview start requested: folder=%s port=%d", payload.FolderID, payload.LocalPort)

	// Validate folder exists in config
	folder := a.cfg.GetFolderByID(payload.FolderID)
	if folder == nil {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendPreviewStatus(payload.FolderID, "error", "Folder not found")
		return
	}

	// Check if tunnel already exists for this folder
	a.tunnelsMu.Lock()
	if existing, ok := a.tunnels[payload.FolderID]; ok {
		if existing.IsConnected() {
			a.tunnelsMu.Unlock()
			log.Printf("⚠️  Tunnel already active for folder %s", payload.FolderID)
			// Re-send the preview ready message
			a.sendPreviewReady(payload.FolderID, existing.LocalPort())
			return
		}
		// Close stale tunnel
		existing.Close()
		delete(a.tunnels, payload.FolderID)
	}
	a.tunnelsMu.Unlock()

	// Get auth token
	token := a.cfg.GetToken(a.cfg.RelayURL)
	if token == "" {
		log.Printf("❌ No auth token available for preview")
		a.sendPreviewStatus(payload.FolderID, "error", "Not authenticated")
		return
	}

	// Send starting status
	a.sendPreviewStatus(payload.FolderID, "starting", "")

	// Auto-start dev server if not running
	_, err := a.devServers.Start(payload.FolderID, folder.Path, payload.LocalPort)
	if err != nil {
		log.Printf("⚠️  Could not auto-start dev server: %v", err)
		// Don't fail - maybe user started it manually or it's a different setup
	}

	// Wait for the port to be ready (up to 30 seconds)
	log.Printf("⏳ Waiting for dev server on port %d...", payload.LocalPort)
	if err := devserver.WaitForPort(payload.LocalPort, 30*time.Second); err != nil {
		log.Printf("❌ Dev server not ready: %v", err)
		a.sendPreviewStatus(payload.FolderID, "error", "Dev server failed to start - check that the project is set up correctly")
		return
	}
	log.Printf("✅ Dev server is ready on port %d", payload.LocalPort)

	// Mark the dev server as running (transitions from StateStarting to StateRunning)
	a.devServers.MarkRunning(payload.FolderID)

	// Create tunnel client
	client := tunnel.NewClient(
		a.cfg.RelayURL,
		token,
		a.cfg.UserID,
		a.cfg.DeviceID,
		payload.FolderID,
		payload.LocalPort,
	)

	// Set up tunnel state change callback to notify mobile of reconnection events
	client.SetStateChangeCallback(func(folderID string, state tunnel.ConnectionState, attempt, maxAttempts int) {
		switch state {
		case tunnel.StateReconnecting:
			log.Printf("🔄 Tunnel reconnecting for folder %s (attempt %d/%d)", folderID, attempt, maxAttempts)
			a.sendPreviewStatus(folderID, "reconnecting", fmt.Sprintf("Reconnecting... (attempt %d/%d)", attempt, maxAttempts))
		case tunnel.StateConnected:
			if attempt > 0 {
				// This is a reconnection, not initial connection
				log.Printf("✅ Tunnel reconnected for folder %s", folderID)
				a.sendPreviewStatus(folderID, "reconnected", "")
			}
		case tunnel.StateDisconnected:
			if attempt > 0 {
				// Failed to reconnect after all attempts
				log.Printf("❌ Tunnel disconnected for folder %s after %d attempts", folderID, maxAttempts)
				a.sendPreviewStatus(folderID, "disconnected", "Connection lost - tap to retry")
			}
		}
	})

	// Connect tunnel
	if err := client.Connect(); err != nil {
		log.Printf("❌ Failed to connect tunnel: %v", err)
		a.sendPreviewStatus(payload.FolderID, "error", err.Error())
		return
	}

	// Store tunnel (check again in case another goroutine created one while we were connecting)
	a.tunnelsMu.Lock()
	if existing, ok := a.tunnels[payload.FolderID]; ok && existing.IsConnected() {
		// Another tunnel was created while we were connecting - close ours and use existing
		a.tunnelsMu.Unlock()
		client.Close()
		log.Printf("⚠️  Race: tunnel already created for folder %s, closing duplicate", payload.FolderID)
		a.sendPreviewReady(payload.FolderID, existing.LocalPort())
		return
	}
	a.tunnels[payload.FolderID] = client
	a.tunnelsMu.Unlock()

	// Send preview ready (the actual URL will be constructed by the relay based on the token)
	a.sendPreviewReady(payload.FolderID, payload.LocalPort)
}

// handlePreviewStop stops a live preview tunnel for a folder.
func (a *Agent) handlePreviewStop(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to unmarshal preview_stop payload: %v", err)
		return
	}

	log.Printf("🔌 Preview stop requested: folder=%s", payload.FolderID)

	// Stop the dev server if we started it
	a.devServers.Stop(payload.FolderID)

	a.tunnelsMu.Lock()
	if client, ok := a.tunnels[payload.FolderID]; ok {
		client.Close()
		delete(a.tunnels, payload.FolderID)
		a.tunnelsMu.Unlock()
		a.sendPreviewStatus(payload.FolderID, "stopped", "")
	} else {
		a.tunnelsMu.Unlock()
		log.Printf("⚠️  No active tunnel for folder %s", payload.FolderID)
	}
}

// sendPreviewReady sends a preview_ready message to mobile/web clients.
func (a *Agent) sendPreviewReady(folderID string, localPort int) {
	// The preview URL will be constructed by the relay/mobile based on the folder ID
	// Format: https://{token}.preview.finn.dev
	previewURL := fmt.Sprintf("preview://%s", folderID)

	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":   folderID,
		"preview_url": previewURL,
		"local_port":  localPort,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypePreviewReady,
		Payload:    payload,
	})

	log.Printf("✅ Preview ready: folder=%s port=%d", folderID, localPort)
}

// sendPreviewStatus sends a preview_status message to mobile/web clients.
func (a *Agent) sendPreviewStatus(folderID, status, errorMsg string) {
	payload := map[string]interface{}{
		"folder_id": folderID,
		"status":    status,
	}
	if errorMsg != "" {
		payload["error"] = errorMsg
	}

	payloadBytes, _ := json.Marshal(payload)

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypePreviewStatus,
		Payload:    payloadBytes,
	})

	if errorMsg != "" {
		log.Printf("❌ Preview status: folder=%s status=%s error=%s", folderID, status, errorMsg)
	} else {
		log.Printf("📊 Preview status: folder=%s status=%s", folderID, status)
	}
}

// closeAllTunnels closes all active preview tunnels and stops all dev servers.
// Called during agent shutdown.
func (a *Agent) closeAllTunnels() {
	// Stop all dev servers first
	a.devServers.StopAll()

	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	for folderID, client := range a.tunnels {
		log.Printf("🔌 Closing tunnel for folder %s", folderID)
		client.Close()
	}
	a.tunnels = make(map[string]*tunnel.Client)
}
