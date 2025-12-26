package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/getfinn/finn/internal/git"
	ws "github.com/getfinn/finn/internal/websocket"
)

// handleFolderAdd handles adding a new approved folder from the system tray.
func (a *Agent) handleFolderAdd(path string) {
	name := filepath.Base(path)

	log.Printf("Adding folder: %s (%s)", name, path)

	// Check subscription limit before adding
	if err := a.cfg.AddFolder(name, path); err != nil {
		log.Printf("❌ Failed to add folder: %v", err)
		if a.tray != nil {
			a.tray.ShowNotification("Folder Limit Reached", err.Error())
		}
		return
	}

	if err := a.cfg.Save(); err != nil {
		log.Printf("Failed to save config: %v", err)
		return
	}

	log.Printf("✅ Folder approved: %s (%d/%d folders)",
		name, len(a.cfg.ApprovedFolders), a.cfg.Subscription.MaxFolders)

	a.sendFolderListUpdate()

	if a.tray != nil {
		a.tray.ShowNotification("Folder Approved", fmt.Sprintf("Added: %s (%d/%d)",
			name, len(a.cfg.ApprovedFolders), a.cfg.Subscription.MaxFolders))
	}
}

// handleFolderRemove handles removing an approved folder from the system tray.
func (a *Agent) handleFolderRemove(path string) {
	log.Printf("Removing folder: %s", path)

	a.cfg.RemoveFolder(path)

	if err := a.cfg.Save(); err != nil {
		log.Printf("Failed to save config: %v", err)
		return
	}

	log.Printf("✅ Folder removed")
	a.sendFolderListUpdate()
}

// handleFolderAddRequest handles a request from web dashboard to add a folder.
func (a *Agent) handleFolderAddRequest(msg *ws.Message) {
	log.Println("📥 Received folder add request from web dashboard")

	var payload struct {
		Path string `json:"path"`
	}

	var path string
	if err := json.Unmarshal(msg.Payload, &payload); err == nil && payload.Path != "" {
		path = payload.Path
		log.Printf("Using provided path: %s", path)
	} else {
		if a.tray == nil {
			log.Println("❌ Cannot open file picker in headless mode")
			a.sendFolderResponse(false, "File picker not available in headless mode. Please use 'Add by Path' button to enter the folder path manually.", "")
			return
		}

		path = a.tray.SelectFolder()
		if path == "" {
			log.Println("⚠️ Folder selection cancelled or not available")
			a.sendFolderResponse(false, "Folder selection cancelled. Please try 'Add by Path' to enter the folder path manually.", "")
			return
		}
		log.Printf("Selected path from picker: %s", path)
	}

	// Validate that the path exists
	if _, err := os.Stat(path); os.IsNotExist(err) {
		log.Printf("❌ Path does not exist: %s", path)
		a.sendFolderResponse(false, fmt.Sprintf("Folder does not exist: %s", path), "")
		return
	}

	name := filepath.Base(path)
	log.Printf("Adding folder from web request: %s (%s)", name, path)

	if err := a.cfg.AddFolder(name, path); err != nil {
		log.Printf("❌ Failed to add folder: %v", err)
		a.sendFolderResponse(false, err.Error(), "")
		if a.tray != nil {
			a.tray.ShowNotification("Folder Limit Reached", err.Error())
		}
		return
	}

	if err := a.cfg.Save(); err != nil {
		log.Printf("Failed to save config: %v", err)
		a.sendFolderResponse(false, fmt.Sprintf("Failed to save: %v", err), "")
		return
	}

	log.Printf("✅ Folder approved: %s (%d/%d folders)",
		name, len(a.cfg.ApprovedFolders), a.cfg.Subscription.MaxFolders)

	a.sendFolderResponse(true, "Folder added successfully", "")
	a.sendFolderListUpdate()

	// Batch-discover existing sessions for this folder
	if a.sessionWatcher != nil {
		sessions := a.sessionWatcher.ScanProjectSessions(path)
		if len(sessions) > 0 {
			a.sendExternalSessionsList(sessions, path)
		}
	}

	if a.tray != nil {
		a.tray.ShowNotification("Folder Approved", fmt.Sprintf("Added: %s (%d/%d)",
			name, len(a.cfg.ApprovedFolders), a.cfg.Subscription.MaxFolders))
	}
}

// handleFolderRemoveRequest handles a request from web dashboard to remove a folder.
func (a *Agent) handleFolderRemoveRequest(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal folder remove payload: %v", err)
		a.sendFolderResponse(false, "Invalid request", "")
		return
	}

	log.Printf("📥 Received folder remove request for ID: %s", payload.FolderID)

	// Get the folder path BEFORE removing (needed to clear sessions)
	var folderPath string
	for _, f := range a.cfg.ApprovedFolders {
		if f.ID == payload.FolderID {
			folderPath = f.Path
			break
		}
	}

	if err := a.cfg.RemoveFolderByID(payload.FolderID); err != nil {
		log.Printf("❌ Failed to remove folder: %v", err)
		a.sendFolderResponse(false, err.Error(), "")
		return
	}

	if err := a.cfg.Save(); err != nil {
		log.Printf("Failed to save config: %v", err)
		a.sendFolderResponse(false, fmt.Sprintf("Failed to save: %v", err), "")
		return
	}

	log.Printf("✅ Folder removed: %s", payload.FolderID)

	// Clear sessions from watcher
	if a.sessionWatcher != nil && folderPath != "" {
		a.sessionWatcher.ClearProjectSessions(folderPath)
	}

	a.sendFolderResponse(true, "Folder removed successfully", "")
	a.sendFolderListUpdate()
}

// handleFolderSelectRequest handles a request from web dashboard to select a folder.
func (a *Agent) handleFolderSelectRequest(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal folder select payload: %v", err)
		a.sendFolderResponse(false, "Invalid request", "")
		return
	}

	log.Printf("📥 Received folder select request for ID: %s", payload.FolderID)

	if err := a.cfg.SelectFolder(payload.FolderID); err != nil {
		log.Printf("❌ Failed to select folder: %v", err)
		a.sendFolderResponse(false, err.Error(), "")
		return
	}

	if err := a.cfg.Save(); err != nil {
		log.Printf("Failed to save config: %v", err)
		a.sendFolderResponse(false, fmt.Sprintf("Failed to save: %v", err), "")
		return
	}

	log.Printf("✅ Folder selected: %s", payload.FolderID)
	a.sendFolderResponse(true, "Folder selected successfully", payload.FolderID)
	a.sendFolderListUpdate()
}

// handleBrowseFolders handles a request to browse the filesystem.
func (a *Agent) handleBrowseFolders(msg *ws.Message) {
	var payload struct {
		Path string `json:"path"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal browse folders payload: %v", err)
		a.sendBrowseResponse("", nil, "Invalid request")
		return
	}

	// Default to user's home directory if no path provided
	browsePath := payload.Path
	if browsePath == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			log.Printf("Failed to get home directory: %v", err)
			a.sendBrowseResponse("", nil, "Failed to get home directory")
			return
		}
		browsePath = homeDir
	}

	log.Printf("📂 Browsing directory: %s", browsePath)

	// Security: Ensure requested path is within user's home directory
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Printf("Failed to get home directory: %v", err)
		a.sendBrowseResponse("", nil, "Failed to get home directory")
		return
	}

	cleanPath := filepath.Clean(browsePath)
	absPath, err := filepath.Abs(cleanPath)
	if err != nil {
		log.Printf("Failed to resolve path: %v", err)
		a.sendBrowseResponse("", nil, "Invalid path")
		return
	}

	absHomeDir, _ := filepath.Abs(homeDir)
	if !filepath.HasPrefix(absPath, absHomeDir) {
		log.Printf("⚠️ Attempted to browse outside home directory: %s", absPath)
		a.sendBrowseResponse("", nil, "Access denied: can only browse within your home directory")
		return
	}

	fileInfo, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Path does not exist: %s", absPath)
			a.sendBrowseResponse("", nil, fmt.Sprintf("Path does not exist: %s", absPath))
		} else {
			log.Printf("Failed to stat path: %v", err)
			a.sendBrowseResponse("", nil, fmt.Sprintf("Failed to access path: %v", err))
		}
		return
	}

	if !fileInfo.IsDir() {
		log.Printf("Path is not a directory: %s", absPath)
		a.sendBrowseResponse("", nil, "Path is not a directory")
		return
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		log.Printf("Failed to read directory: %v", err)
		a.sendBrowseResponse("", nil, fmt.Sprintf("Failed to read directory: %v", err))
		return
	}

	type DirectoryEntry struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	}

	var directories []DirectoryEntry
	skipDirs := map[string]bool{
		"node_modules": true,
		".git":         true,
		".vscode":      true,
		".idea":        true,
		"__pycache__":  true,
		".cache":       true,
	}

	for _, entry := range entries {
		// Skip hidden files/directories
		if entry.Name()[0] == '.' {
			continue
		}
		if skipDirs[entry.Name()] {
			continue
		}

		if entry.IsDir() {
			fullPath := filepath.Join(absPath, entry.Name())
			directories = append(directories, DirectoryEntry{
				Name:  entry.Name(),
				Path:  fullPath,
				IsDir: true,
			})
		}
	}

	log.Printf("✅ Found %d directories in %s", len(directories), absPath)
	a.sendBrowseResponse(absPath, directories, "")
}

// IsFolderApproved checks if a folder path is approved.
func (a *Agent) IsFolderApproved(path string) bool {
	return a.cfg.IsFolderApproved(path)
}

// sendFolderResponse sends a response back to the web dashboard about a folder operation.
func (a *Agent) sendFolderResponse(success bool, message string, folderID string) {
	status := "success"
	if !success {
		status = "error"
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"status":    status,
		"message":   message,
		"folder_id": folderID,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "folder_response",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send folder response: %v", err)
	} else {
		log.Printf("📤 Sent folder response: %s - %s", status, message)
	}
}

// sendBrowseResponse sends directory listing back to web dashboard.
func (a *Agent) sendBrowseResponse(currentPath string, directories interface{}, errorMessage string) {
	status := "success"
	if errorMessage != "" {
		status = "error"
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"status":       status,
		"current_path": currentPath,
		"directories":  directories,
		"error":        errorMessage,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "folder_browse_response",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send browse response: %v", err)
	} else {
		if status == "success" {
			log.Printf("📤 Sent browse response for: %s", currentPath)
		} else {
			log.Printf("📤 Sent browse error: %s", errorMessage)
		}
	}
}

// sendFolderListUpdate sends updated folder list to relay server (for dashboard).
// Includes git commits for each folder to avoid race conditions.
func (a *Agent) sendFolderListUpdate() {
	foldersWithCommits := make([]map[string]interface{}, 0, len(a.cfg.ApprovedFolders))

	totalCommits := 0
	for _, folder := range a.cfg.ApprovedFolders {
		isGitRepo := git.IsGitRepo(folder.Path)
		folderData := map[string]interface{}{
			"id":          folder.ID,
			"name":        folder.Name,
			"path":        folder.Path,
			"is_git_repo": isGitRepo,
		}

		if isGitRepo {
			repo := git.NewRepository(folder.Path)

			if branch, err := repo.GetCurrentBranch(); err == nil && branch != "" {
				folderData["current_branch"] = branch
			}

			commits := a.getCommitsForFolder(folder.Path)
			if len(commits) > 0 {
				folderData["commits"] = commits
				totalCommits += len(commits)
				log.Printf("📦 Folder %s: %d commits (branch: %v)", folder.Name, len(commits), folderData["current_branch"])
			}
		}

		foldersWithCommits = append(foldersWithCommits, folderData)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"folders":            foldersWithCommits,
		"selected_folder_id": a.cfg.SelectedFolderID,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "folder_list",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send folder list: %v", err)
	} else {
		log.Printf("📤 Sent folder list to dashboard (%d folders, %d total commits, selected: %s)",
			len(a.cfg.ApprovedFolders), totalCommits, a.cfg.SelectedFolderID)
	}
}
