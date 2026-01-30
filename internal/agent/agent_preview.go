package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/getfinn/finn/internal/devserver"
	"github.com/getfinn/finn/internal/tunnel"
	ws "github.com/getfinn/finn/internal/websocket"
)

// PreviewType represents the type of preview
type PreviewType string

const (
	PreviewTypeWeb         PreviewType = "web"         // Standard web dev server preview
	PreviewTypeSpreadsheet PreviewType = "spreadsheet" // Excel/CSV spreadsheet preview
	PreviewTypeFile        PreviewType = "file"        // Generic file server
)

// handlePreviewStart starts a live preview tunnel for a folder.
// This enables real-time web preview of the development server running
// on the user's machine through a secure tunnel.
//
// Supports multiple preview types with auto-detection:
// - "web": Starts dev server and proxies through tunnel (supports Node, Go, Python, Ruby, etc.)
// - "spreadsheet": Starts file server for Excel/CSV files with live updates
// - "file": Generic file server for any folder
//
// For monorepos and multi-project folders:
// - Discovers all projects using marker files (package.json, go.mod, Gemfile, etc.)
// - Auto-selects based on: cached config > recent activity > context path > single project
// - If multiple projects exist and no auto-selection is possible, prompts user to choose
func (a *Agent) handlePreviewStart(msg *ws.Message) {
	var payload struct {
		FolderID    string `json:"folder_id"`
		LocalPort   int    `json:"local_port"`
		PreviewType string `json:"preview_type"`  // "web", "spreadsheet", or "file"
		File        string `json:"file"`          // For spreadsheet: specific file to preview
		ProjectPath string `json:"project_path"`  // For project picker: relative path to selected project
		DevCommand  string `json:"dev_command"`   // For custom command: user-specified dev command
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to unmarshal preview_start payload: %v", err)
		a.sendPreviewStatus(payload.FolderID, "error", "Invalid request payload")
		return
	}

	log.Printf("🔗 Preview start requested: folder=%s port=%d type=%s file=%s project_path=%s",
		payload.FolderID, payload.LocalPort, payload.PreviewType, payload.File, payload.ProjectPath)

	// Validate folder exists in config
	folder := a.cfg.GetFolderByID(payload.FolderID)
	if folder == nil {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendPreviewStatus(payload.FolderID, "error", "Folder not found")
		return
	}

	// Auto-detect preview type based on folder contents
	previewType := a.detectPreviewType(payload.PreviewType, payload.File, folder.Path)
	log.Printf("📋 Detected preview type: %s (requested: %s)", previewType, payload.PreviewType)

	// Smart port detection for all project types
	localPort := a.smartPortDetection(previewType, payload.LocalPort, folder.Path, payload.FolderID)
	log.Printf("🔌 Using port: %d (requested: %d, type: %s)", localPort, payload.LocalPort, previewType)

	// Check if tunnel already exists for this folder
	a.tunnelsMu.Lock()
	if existing, ok := a.tunnels[payload.FolderID]; ok {
		if existing.IsConnected() {
			a.tunnelsMu.Unlock()
			log.Printf("⚠️  Tunnel already active for folder %s", payload.FolderID)
			a.sendPreviewReadyByType(payload.FolderID, existing.LocalPort(), previewType, payload.File, folder.Path)
			return
		}
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

	// Handle different preview types
	switch previewType {
	case PreviewTypeSpreadsheet:
		file := payload.File
		if file == "" {
			files, _ := devserver.GetSpreadsheetFiles(folder.Path)
			if len(files) > 0 {
				file = filepath.Base(files[0])
				log.Printf("📊 Auto-detected spreadsheet file: %s", file)
			}
		}
		a.startSpreadsheetPreview(payload.FolderID, folder.Path, localPort, file, token)
		return

	case PreviewTypeFile:
		a.startFilePreview(payload.FolderID, folder.Path, localPort, token)
		return
	}

	// PreviewTypeWeb: Multi-ecosystem project detection
	var selectedProject *devserver.DiscoveredProject

	// If user selected a specific project, use it
	if payload.ProjectPath != "" {
		// Normalize the path - "." means root
		normalizedPath := payload.ProjectPath
		if normalizedPath == "." {
			normalizedPath = ""
		}

		projectPath := folder.Path
		if normalizedPath != "" {
			projectPath = filepath.Join(folder.Path, normalizedPath)
		}

		// Find the project matching this path
		projects, _ := devserver.DiscoverProjects(folder.Path, 3)
		for i := range projects {
			// Match by absolute path or relative path (handle "" vs "." for root)
			projectRelPath := projects[i].RelPath
			if projects[i].Path == projectPath ||
				projectRelPath == normalizedPath ||
				(projectRelPath == "" && normalizedPath == "") {
				// Make a copy to avoid modifying the slice
				project := projects[i]
				selectedProject = &project
				// Apply custom dev command if provided
				if payload.DevCommand != "" {
					selectedProject.DevCommand = payload.DevCommand
					selectedProject.HasDevCmd = true
				}
				// Save this selection for future (use original RelPath)
				a.devServers.SavePreviewConfig(payload.FolderID, projects[i].RelPath, payload.DevCommand)
				log.Printf("✅ User selected project: %s (%s)", selectedProject.Name, selectedProject.RelPath)
				break
			}
		}
		if selectedProject == nil {
			log.Printf("⚠️  Selected project not found at path: %s", payload.ProjectPath)
		}
	}

	// If no explicit selection, use smart detection
	if selectedProject == nil {
		selection := a.devServers.SelectProject(payload.FolderID, folder.Path, "")

		log.Printf("📦 Project discovery: found %d previewable projects (reason: %s)",
			selection.PreviewableCount, selection.SelectionReason)

		if selection.NeedsUserInput && selection.PreviewableCount > 1 {
			// Multiple projects - send picker to mobile
			a.sendProjectPicker(payload.FolderID, selection.AllProjects, localPort)
			return
		}

		if selection.Selected != nil {
			selectedProject = selection.Selected
			log.Printf("✅ Auto-selected project: %s (reason: %s)", selectedProject.Name, selection.SelectionReason)
		}
	}

	// Start the dev server
	var err error
	var projectPath string
	if selectedProject != nil {
		projectPath = selectedProject.Path
	} else {
		projectPath = folder.Path
	}

	if selectedProject != nil && selectedProject.HasDevCmd {
		// Use new multi-ecosystem start
		_, err = a.devServers.StartDiscoveredProject(payload.FolderID, *selectedProject, localPort)
	} else {
		// Fallback to legacy Node.js start
		_, err = a.devServers.Start(payload.FolderID, folder.Path, localPort)
	}

	if err != nil {
		log.Printf("❌ Dev server start failed: %v", err)
		errMsg := err.Error()

		// Check if this is a missing dependencies error we can help with
		if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "dependencies not installed") {
			// Get the correct install command for the ecosystem
			var installInfo devserver.InstallInfo
			if selectedProject != nil {
				installInfo = devserver.GetInstallCommand(projectPath, selectedProject.Ecosystem)
			} else {
				// Fallback to Node.js if we don't know the ecosystem
				installInfo = devserver.GetInstallCommand(projectPath, devserver.EcosystemNode)
			}
			a.sendInstallRequired(payload.FolderID, projectPath, installInfo.Command, errMsg)
			return
		}

		// Send error to client immediately
		a.sendPreviewStatus(payload.FolderID, "error", errMsg)
		return
	}

	// Wait for the port to be ready (up to 30 seconds)
	log.Printf("⏳ Waiting for dev server on port %d...", localPort)
	if err := devserver.WaitForPort(localPort, 30*time.Second); err != nil {
		log.Printf("❌ Dev server not ready: %v", err)
		a.sendPreviewStatus(payload.FolderID, "error", "Dev server failed to start. Check that dependencies are installed and the project builds correctly.")
		return
	}
	log.Printf("✅ Dev server is ready on port %d", localPort)

	// Mark the dev server as running (transitions from StateStarting to StateRunning)
	a.devServers.MarkRunning(payload.FolderID)

	// Create tunnel client
	client := tunnel.NewClient(
		a.cfg.RelayURL,
		token,
		a.cfg.UserID,
		a.cfg.DeviceID,
		payload.FolderID,
		localPort,
	)

	// Set up tunnel state change callback to notify mobile of reconnection events
	client.SetStateChangeCallback(a.makeTunnelStateCallback(payload.FolderID))

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
	a.sendPreviewReady(payload.FolderID, localPort)
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

	// Stop the file server if we started it (for spreadsheet/file previews)
	a.devServers.StopFileServer(payload.FolderID)

	// Stop watching spreadsheet files for this folder
	if a.spreadsheetWatcher != nil {
		a.spreadsheetWatcher.UnwatchFolder(payload.FolderID)
	}

	// Stop watching static files for this folder
	if a.staticWatcher != nil {
		a.staticWatcher.UnwatchFolder(payload.FolderID)
	}

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

// makeTunnelStateCallback creates a standard tunnel state change callback.
// This is used by all preview types to handle reconnection events consistently.
func (a *Agent) makeTunnelStateCallback(folderID string) func(string, tunnel.ConnectionState, int, int) {
	return func(fid string, state tunnel.ConnectionState, attempt, maxAttempts int) {
		switch state {
		case tunnel.StateReconnecting:
			log.Printf("🔄 Tunnel reconnecting for folder %s (attempt %d/%d)", fid, attempt, maxAttempts)
			a.sendPreviewStatus(fid, "reconnecting", fmt.Sprintf("Reconnecting... (attempt %d/%d)", attempt, maxAttempts))
		case tunnel.StateConnected:
			if attempt > 0 {
				log.Printf("✅ Tunnel reconnected for folder %s", fid)
				a.sendPreviewStatus(fid, "reconnected", "")
			}
		case tunnel.StateDisconnected:
			if attempt > 0 {
				log.Printf("❌ Tunnel disconnected for folder %s after %d attempts", fid, maxAttempts)
				a.sendPreviewStatus(fid, "disconnected", "Connection lost - tap to retry")
			}
		}
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

	// Stop all file servers
	a.devServers.StopAllFileServers()

	// Close spreadsheet watcher
	if a.spreadsheetWatcher != nil {
		a.spreadsheetWatcher.Close()
	}

	// Close static file watcher
	if a.staticWatcher != nil {
		a.staticWatcher.Close()
	}

	a.tunnelsMu.Lock()
	defer a.tunnelsMu.Unlock()

	for folderID, client := range a.tunnels {
		log.Printf("🔌 Closing tunnel for folder %s", folderID)
		client.Close()
	}
	a.tunnels = make(map[string]*tunnel.Client)
}

// startSpreadsheetPreview starts a preview for spreadsheet files.
// This uses a file server instead of a dev server and sets up file watching.
func (a *Agent) startSpreadsheetPreview(folderID, folderPath string, localPort int, file, token string) {
	log.Printf("📊 Starting spreadsheet preview: folder=%s file=%s", folderID, file)

	// Start file server
	_, err := a.devServers.StartFileServer(folderID, folderPath, localPort)
	if err != nil {
		log.Printf("❌ Failed to start file server: %v", err)
		a.sendPreviewStatus(folderID, "error", "Failed to start file server: "+err.Error())
		return
	}

	// Wait for the port to be ready
	log.Printf("⏳ Waiting for file server on port %d...", localPort)
	if err := devserver.WaitForPort(localPort, 10*time.Second); err != nil {
		log.Printf("❌ File server not ready: %v", err)
		a.sendPreviewStatus(folderID, "error", "File server failed to start")
		return
	}
	log.Printf("✅ File server is ready on port %d", localPort)

	// Set up file watching for the spreadsheet file
	if file != "" {
		filePath := filepath.Join(folderPath, file)
		a.setupSpreadsheetWatcher(folderID, filePath, file)
	}

	// Create tunnel client
	client := tunnel.NewClient(
		a.cfg.RelayURL,
		token,
		a.cfg.UserID,
		a.cfg.DeviceID,
		folderID,
		localPort,
	)

	// Set up tunnel state change callback
	client.SetStateChangeCallback(a.makeTunnelStateCallback(folderID))

	// Connect tunnel
	if err := client.Connect(); err != nil {
		log.Printf("❌ Failed to connect tunnel: %v", err)
		a.sendPreviewStatus(folderID, "error", err.Error())
		a.devServers.StopFileServer(folderID)
		return
	}

	// Store tunnel
	a.tunnelsMu.Lock()
	a.tunnels[folderID] = client
	a.tunnelsMu.Unlock()

	// Send preview ready with spreadsheet info
	a.sendSpreadsheetPreviewReady(folderID, localPort, file)
}

// setupSpreadsheetWatcher sets up file watching for a spreadsheet file.
func (a *Agent) setupSpreadsheetWatcher(folderID, filePath, filename string) {
	a.spreadsheetWatcherMu.Lock()
	defer a.spreadsheetWatcherMu.Unlock()

	// Initialize watcher if needed (thread-safe with mutex)
	if a.spreadsheetWatcher == nil {
		watcher, err := devserver.NewSpreadsheetWatcher()
		if err != nil {
			log.Printf("⚠️  Failed to create spreadsheet watcher: %v", err)
			return
		}
		a.spreadsheetWatcher = watcher
	}

	// Watch the file
	err := a.spreadsheetWatcher.WatchFile(filePath, folderID, func() {
		// File changed - notify clients
		a.sendSpreadsheetUpdate(folderID, filename)
	})
	if err != nil {
		log.Printf("⚠️  Failed to watch spreadsheet file: %v", err)
	}
}

// sendSpreadsheetPreviewReady sends a preview_ready message for spreadsheet previews.
func (a *Agent) sendSpreadsheetPreviewReady(folderID string, localPort int, file string) {
	// The preview URL will be the spreadsheet viewer URL
	// Format: /spreadsheet/{folder_id}/{filename}
	previewURL := fmt.Sprintf("spreadsheet://%s/%s", folderID, file)

	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":    folderID,
		"preview_url":  previewURL,
		"preview_type": "spreadsheet",
		"file":         file,
		"local_port":   localPort,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypePreviewReady,
		Payload:    payload,
	})

	log.Printf("✅ Spreadsheet preview ready: folder=%s file=%s port=%d", folderID, file, localPort)
}

// sendSpreadsheetUpdate notifies clients that a spreadsheet file has been updated.
func (a *Agent) sendSpreadsheetUpdate(folderID, filename string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id": folderID,
		"file":      filename,
		"timestamp": time.Now().Unix(),
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeSpreadsheetUpdate,
		Payload:    payload,
	})

	log.Printf("📊 Spreadsheet update sent: folder=%s file=%s", folderID, filename)
}

// smartPortDetection determines the best port to use based on preview type and folder.
// Priority:
// 1. If we already have a dev server running for this folder, use its port
// 2. Find a free port for starting a new server
//
// This prevents showing the wrong project when port 3000 is occupied by a different project.
func (a *Agent) smartPortDetection(previewType PreviewType, requestedPort int, folderPath string, folderID string) int {
	// First, check if we already have a managed dev server for this folder
	if existingPort := a.devServers.GetPort(folderID); existingPort > 0 {
		log.Printf("✅ Found existing managed dev server for folder on port %d", existingPort)
		return existingPort
	}

	// For non-web projects, always find a free port (we control the server)
	if previewType != PreviewTypeWeb {
		port, err := devserver.FindAvailablePort(3000)
		if err != nil {
			log.Printf("⚠️  Could not find available port, defaulting to 3000: %v", err)
			return 3000
		}
		return port
	}

	// For web projects:
	// 1. If requested port is not in use, use it (we'll start a server there)
	// 2. If requested port IS in use, we can't be sure it's for our project
	//    So find a free port to guarantee correct project
	if requestedPort > 0 && !devserver.IsPortInUse(requestedPort) {
		log.Printf("✅ Requested port %d is free, will start dev server there", requestedPort)
		return requestedPort
	}

	// Find a free port starting from common dev ports
	startPort := 3000
	if requestedPort > 0 {
		startPort = requestedPort
	}

	// If requested port is in use, log why we're picking a different one
	if requestedPort > 0 && devserver.IsPortInUse(requestedPort) {
		log.Printf("⚠️  Port %d is already in use (could be another project), finding free port", requestedPort)
	}

	port, err := devserver.FindAvailablePort(startPort)
	if err != nil {
		// Try from 3000 if the range after requestedPort is full
		port, err = devserver.FindAvailablePort(3000)
		if err != nil {
			log.Printf("⚠️  Could not find available port, defaulting to 3000")
			return 3000
		}
	}

	if requestedPort > 0 && port != requestedPort {
		log.Printf("🔌 Using free port %d (port %d was occupied)", port, requestedPort)
	}
	return port
}

// detectPreviewType determines the appropriate preview type based on folder contents.
// Priority: explicit type > file detection > folder analysis
// Uses multi-ecosystem detection to support Node.js, Go, Python, Ruby, Rust, Java, PHP, .NET, Elixir
func (a *Agent) detectPreviewType(requestedType, file, folderPath string) PreviewType {
	// 1. If explicitly requested, use that
	if requestedType != "" {
		return PreviewType(requestedType)
	}

	// 2. If a specific file is requested and it's a spreadsheet
	if file != "" && devserver.IsSpreadsheetFile(file) {
		return PreviewTypeSpreadsheet
	}

	// 3. Use multi-ecosystem project discovery
	projects, _ := devserver.DiscoverProjects(folderPath, 3)
	hasPreviewableProject := false
	for _, p := range projects {
		if p.HasDevCmd {
			hasPreviewableProject = true
			break
		}
	}

	// 4. Check if folder has spreadsheet files
	spreadsheetFiles, _ := devserver.GetSpreadsheetFiles(folderPath)
	hasSpreadsheets := len(spreadsheetFiles) > 0

	// 5. If spreadsheets exist but no previewable project, use spreadsheet preview
	if hasSpreadsheets && !hasPreviewableProject {
		log.Printf("📊 Auto-detected spreadsheet project: %d Excel/CSV file(s) found", len(spreadsheetFiles))
		return PreviewTypeSpreadsheet
	}

	// 6. If any previewable project exists, use web preview
	if hasPreviewableProject {
		return PreviewTypeWeb
	}

	// 7. Default to file server for non-project folders
	log.Printf("📁 No previewable project detected, using file server mode")
	return PreviewTypeFile
}

// sendPreviewReadyByType sends the appropriate preview ready message based on type.
func (a *Agent) sendPreviewReadyByType(folderID string, localPort int, previewType PreviewType, file, folderPath string) {
	switch previewType {
	case PreviewTypeSpreadsheet:
		// Auto-detect file if not specified
		if file == "" {
			files, _ := devserver.GetSpreadsheetFiles(folderPath)
			if len(files) > 0 {
				file = filepath.Base(files[0])
			}
		}
		a.sendSpreadsheetPreviewReady(folderID, localPort, file)
	case PreviewTypeFile:
		a.sendFilePreviewReady(folderID, localPort)
	default:
		a.sendPreviewReady(folderID, localPort)
	}
}

// startFilePreview starts a generic file server preview for non-web projects.
func (a *Agent) startFilePreview(folderID, folderPath string, localPort int, token string) {
	log.Printf("📁 Starting file preview: folder=%s", folderID)

	// Start file server
	_, err := a.devServers.StartFileServer(folderID, folderPath, localPort)
	if err != nil {
		log.Printf("❌ Failed to start file server: %v", err)
		a.sendPreviewStatus(folderID, "error", "Failed to start file server: "+err.Error())
		return
	}

	// Wait for the port to be ready
	log.Printf("⏳ Waiting for file server on port %d...", localPort)
	if err := devserver.WaitForPort(localPort, 10*time.Second); err != nil {
		log.Printf("❌ File server not ready: %v", err)
		a.sendPreviewStatus(folderID, "error", "File server failed to start")
		return
	}
	log.Printf("✅ File server is ready on port %d", localPort)

	// Create tunnel client
	client := tunnel.NewClient(
		a.cfg.RelayURL,
		token,
		a.cfg.UserID,
		a.cfg.DeviceID,
		folderID,
		localPort,
	)

	// Set up tunnel state change callback
	client.SetStateChangeCallback(a.makeTunnelStateCallback(folderID))

	// Connect tunnel
	if err := client.Connect(); err != nil {
		log.Printf("❌ Failed to connect tunnel: %v", err)
		a.sendPreviewStatus(folderID, "error", err.Error())
		a.devServers.StopFileServer(folderID)
		return
	}

	// Store tunnel
	a.tunnelsMu.Lock()
	a.tunnels[folderID] = client
	a.tunnelsMu.Unlock()

	// Set up file watching for live reload
	a.setupStaticWatcher(folderID, folderPath)

	// Send preview ready
	a.sendFilePreviewReady(folderID, localPort)
}

// setupStaticWatcher sets up file watching for static HTML/CSS/JS files.
func (a *Agent) setupStaticWatcher(folderID, folderPath string) {
	a.staticWatcherMu.Lock()
	defer a.staticWatcherMu.Unlock()

	// Initialize watcher if needed (thread-safe with mutex)
	if a.staticWatcher == nil {
		watcher, err := devserver.NewStaticWatcher()
		if err != nil {
			log.Printf("⚠️  Failed to create static file watcher: %v", err)
			return
		}
		a.staticWatcher = watcher
	}

	// Watch the folder
	err := a.staticWatcher.WatchFolder(folderPath, folderID, func() {
		// File changed - notify clients to reload
		a.sendStaticFileUpdate(folderID)
	})
	if err != nil {
		log.Printf("⚠️  Failed to watch static files: %v", err)
	}
}

// sendStaticFileUpdate notifies clients that a static file has been updated.
// The mobile client should reload the WebView when receiving this message.
func (a *Agent) sendStaticFileUpdate(folderID string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id": folderID,
		"timestamp": time.Now().Unix(),
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeStaticFileUpdate,
		Payload:    payload,
	})

	log.Printf("📄 Static file update sent: folder=%s", folderID)
}

// sendFilePreviewReady sends a preview_ready message for file server previews.
func (a *Agent) sendFilePreviewReady(folderID string, localPort int) {
	previewURL := fmt.Sprintf("preview://%s", folderID)

	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":    folderID,
		"preview_url":  previewURL,
		"preview_type": "file",
		"local_port":   localPort,
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypePreviewReady,
		Payload:    payload,
	})

	log.Printf("✅ File preview ready: folder=%s port=%d", folderID, localPort)
}

// sendProjectPicker sends a project picker message to mobile when multiple projects are detected.
// The mobile client will display a selection UI and respond with a preview_start containing the selected project.
func (a *Agent) sendProjectPicker(folderID string, projects []devserver.DiscoveredProject, suggestedPort int) {
	// Convert to serializable format with relevant fields
	type ProjectOption struct {
		Name       string `json:"name"`
		Path       string `json:"path"`        // Relative path from folder root
		Ecosystem  string `json:"ecosystem"`   // node, go, python, ruby, etc.
		Framework  string `json:"framework"`   // nextjs, rails, django, etc.
		HasDevCmd  bool   `json:"has_dev_cmd"`
		DevCommand string `json:"dev_command,omitempty"`
	}

	options := make([]ProjectOption, 0, len(projects))
	for _, p := range projects {
		if !p.HasDevCmd {
			continue // Only show previewable projects
		}
		relPath := p.RelPath
		if relPath == "" {
			relPath = "." // Root folder
		}
		options = append(options, ProjectOption{
			Name:       p.Name,
			Path:       relPath,
			Ecosystem:  string(p.Ecosystem),
			Framework:  p.Framework,
			HasDevCmd:  p.HasDevCmd,
			DevCommand: p.DevCommand,
		})
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":      folderID,
		"projects":       options,
		"suggested_port": suggestedPort,
		"message":        "Multiple projects detected. Select which one to preview.",
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeProjectPicker,
		Payload:    payload,
	})

	// Also send a status update
	a.sendPreviewStatus(folderID, "project_selection", "")

	log.Printf("📋 Project picker sent: folder=%s projects=%d", folderID, len(options))
}

// sendInstallRequired sends a message to mobile when dependencies need to be installed.
// Mobile can then prompt the user and send back an install request.
func (a *Agent) sendInstallRequired(folderID, projectPath, installCommand, errorMsg string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":       folderID,
		"project_path":    projectPath,
		"install_command": installCommand,
		"error":           errorMsg,
		"message":         fmt.Sprintf("Dependencies not installed. Run '%s' to install them.", installCommand),
	})

	a.wsClient.SendMessage(&ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeInstallRequired,
		Payload:    payload,
	})

	// Also send status update so mobile knows we're waiting
	a.sendPreviewStatus(folderID, "install_required", errorMsg)

	log.Printf("📦 Install required: folder=%s command=%s", folderID, installCommand)
}

// handleInstallDeps handles a request from mobile to install dependencies.
func (a *Agent) handleInstallDeps(msg *ws.Message) {
	var payload struct {
		FolderID       string `json:"folder_id"`
		ProjectPath    string `json:"project_path"`
		InstallCommand string `json:"install_command"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("❌ Failed to unmarshal install_deps payload: %v", err)
		return
	}

	log.Printf("📦 Install deps requested: folder=%s path=%s cmd=%s",
		payload.FolderID, payload.ProjectPath, payload.InstallCommand)

	// Validate folder
	folder := a.cfg.GetFolderByID(payload.FolderID)
	if folder == nil {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendPreviewStatus(payload.FolderID, "error", "Folder not found")
		return
	}

	// Determine project path
	projectPath := payload.ProjectPath
	if projectPath == "" {
		projectPath = folder.Path
	}

	// Calculate relative path for auto-restart after install
	relProjectPath, _ := filepath.Rel(folder.Path, projectPath)
	if relProjectPath == "." {
		relProjectPath = ""
	}

	// Send installing status
	a.sendPreviewStatus(payload.FolderID, "installing", fmt.Sprintf("Running %s...", payload.InstallCommand))

	// Run the install command (will auto-restart preview on success)
	go a.runInstallCommand(payload.FolderID, projectPath, relProjectPath, payload.InstallCommand)
}

// runInstallCommand executes an install command and auto-restarts preview on success.
func (a *Agent) runInstallCommand(folderID, projectPath, relProjectPath, installCommand string) {
	log.Printf("📦 Running install: %s in %s", installCommand, projectPath)

	// Parse the command
	parts := strings.Fields(installCommand)
	if len(parts) == 0 {
		a.sendPreviewStatus(folderID, "error", "Invalid install command")
		return
	}

	// Create command
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = projectPath

	// Run and capture output
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("❌ Install failed: %v\nOutput: %s", err, string(output))
		// Send error with last few lines of output
		lines := strings.Split(string(output), "\n")
		lastLines := lines
		if len(lines) > 5 {
			lastLines = lines[len(lines)-5:]
		}
		a.sendPreviewStatus(folderID, "error",
			fmt.Sprintf("Install failed: %s\n%s", err.Error(), strings.Join(lastLines, "\n")))
		return
	}

	log.Printf("✅ Install completed successfully, auto-starting preview...")

	// Auto-restart preview with the same project
	// Create a synthetic message to trigger preview start
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":    folderID,
		"project_path": relProjectPath,
	})

	syntheticMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypePreviewStart,
		Payload:    payload,
	}

	// Handle preview start (this will skip the picker since project_path is specified)
	a.handlePreviewStart(syntheticMsg)
}
