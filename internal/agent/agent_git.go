package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/getfinn/finn/internal/git"
	ws "github.com/getfinn/finn/internal/websocket"
)

// handleGitInit handles a request to initialize git in a folder.
func (a *Agent) handleGitInit(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal git_init payload: %v", err)
		a.sendGitInitResponse(payload.FolderID, false, "Invalid request")
		return
	}

	log.Printf("📥 Received git_init request for folder: %s", payload.FolderID)

	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendGitInitResponse(payload.FolderID, false, "Folder not found")
		return
	}

	if git.IsGitRepo(folderPath) {
		log.Printf("⚠️ Folder is already a git repository: %s", folderPath)
		a.sendGitInitResponse(payload.FolderID, true, "Already a git repository")
		a.sendFolderListUpdate()
		return
	}

	if err := git.EnsureGitRepo(folderPath); err != nil {
		log.Printf("❌ Failed to init git: %v", err)
		a.sendGitInitResponse(payload.FolderID, false, fmt.Sprintf("Failed to initialize git: %v", err))
		return
	}

	log.Printf("✅ Git initialized in folder: %s", folderPath)
	a.sendGitInitResponse(payload.FolderID, true, "Git repository initialized successfully")
	a.sendFolderListUpdate()
}

// sendGitInitResponse sends the result of a git init operation.
func (a *Agent) sendGitInitResponse(folderID string, success bool, message string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id": folderID,
		"success":   success,
		"message":   message,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "git_init_response",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("Failed to send git_init_response: %v", err)
	}
}

// getCommitsForFolder retrieves git commits for a folder.
func (a *Agent) getCommitsForFolder(folderPath string) []map[string]interface{} {
	if !git.IsGitRepo(folderPath) {
		return nil
	}

	repo := git.NewRepository(folderPath)
	commits, err := repo.GetCommits(50)
	if err != nil {
		log.Printf("⚠️  Failed to get commits for %s: %v", folderPath, err)
		return nil
	}

	if len(commits) == 0 {
		return nil
	}

	result := make([]map[string]interface{}, 0, len(commits))
	for _, commit := range commits {
		result = append(result, map[string]interface{}{
			"commit_hash":   commit.FullHash,
			"short_hash":    commit.Hash,
			"message":       commit.Message,
			"author":        commit.Author,
			"author_email":  commit.Email,
			"committed_at":  time.Unix(commit.Timestamp, 0).Format(time.RFC3339),
			"additions":     commit.Stats.Additions,
			"deletions":     commit.Stats.Deletions,
			"files_changed": commit.Stats.FilesChanged,
		})
	}

	return result
}

// startGitSyncChecker periodically checks for git changes in approved folders.
func (a *Agent) startGitSyncChecker() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	log.Println("🔄 Git sync checker started (checking every 30s)")

	// Initial check after short delay
	time.Sleep(5 * time.Second)
	a.checkAndSyncGitChanges()

	for {
		select {
		case <-ticker.C:
			a.checkAndSyncGitChanges()
		case <-a.gitSyncStop:
			log.Println("🛑 Git sync checker stopped")
			return
		}
	}
}

// checkAndSyncGitChanges checks all approved folders for git changes.
func (a *Agent) checkAndSyncGitChanges() {
	if !a.wsClient.IsConnected() {
		return
	}

	for _, folder := range a.cfg.ApprovedFolders {
		if !git.IsGitRepo(folder.Path) {
			continue
		}
		a.checkFolderForGitChanges(folder.ID, folder.Path, folder.Name)
	}
}

// checkFolderForGitChanges checks a single folder for git changes.
func (a *Agent) checkFolderForGitChanges(folderID, folderPath, folderName string) {
	repo := git.NewRepository(folderPath)

	currentHead, err := repo.GetHeadHash()
	if err != nil {
		return
	}

	a.lastKnownHeadsMu.RLock()
	lastHead := a.lastKnownHeads[folderID]
	a.lastKnownHeadsMu.RUnlock()

	if lastHead == "" {
		a.lastKnownHeadsMu.Lock()
		a.lastKnownHeads[folderID] = currentHead
		a.lastKnownHeadsMu.Unlock()
		log.Printf("📝 Recorded initial HEAD for %s: %s", folderName, currentHead[:7])
		return
	}

	if currentHead == lastHead {
		return
	}

	log.Printf("🔄 Git change detected in %s: %s → %s", folderName, lastHead[:7], currentHead[:7])

	newCommits, err := repo.GetCommitsSince(lastHead, 50)
	if err != nil {
		log.Printf("⚠️  Failed to get new commits for %s: %v", folderName, err)
		a.lastKnownHeadsMu.Lock()
		a.lastKnownHeads[folderID] = currentHead
		a.lastKnownHeadsMu.Unlock()
		return
	}

	if len(newCommits) > 0 {
		log.Printf("📦 Found %d new commits in %s, syncing to relay...", len(newCommits), folderName)
		a.sendSyncCommits(folderID, newCommits)
	}

	a.lastKnownHeadsMu.Lock()
	a.lastKnownHeads[folderID] = currentHead
	a.lastKnownHeadsMu.Unlock()
}

// sendSyncCommits sends new commits to the relay server.
func (a *Agent) sendSyncCommits(folderID string, commits []git.CommitInfo) {
	commitData := make([]map[string]interface{}, 0, len(commits))
	for _, commit := range commits {
		commitData = append(commitData, map[string]interface{}{
			"commit_hash":   commit.FullHash,
			"short_hash":    commit.Hash,
			"message":       commit.Message,
			"author":        commit.Author,
			"author_email":  commit.Email,
			"committed_at":  time.Unix(commit.Timestamp, 0).Format(time.RFC3339),
			"additions":     commit.Stats.Additions,
			"deletions":     commit.Stats.Deletions,
			"files_changed": commit.Stats.FilesChanged,
		})
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id": folderID,
		"commits":   commitData,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "sync_commits",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send sync_commits: %v", err)
	} else {
		log.Printf("✅ Synced %d commits for folder %s", len(commits), folderID)
	}
}

// handleRequestCommitSync handles mobile/web request for immediate commit sync.
func (a *Agent) handleRequestCommitSync(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id,omitempty"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal request_commit_sync payload: %v", err)
	}

	log.Printf("📥 Received commit sync request from mobile (folder: %s)", payload.FolderID)

	foldersCount := 0
	totalSynced := 0

	for _, folder := range a.cfg.ApprovedFolders {
		if payload.FolderID != "" && folder.ID != payload.FolderID {
			continue
		}

		foldersCount++

		if !git.IsGitRepo(folder.Path) {
			continue
		}

		repo := git.NewRepository(folder.Path)
		commits, err := repo.GetCommits(50)
		if err != nil {
			log.Printf("⚠️ Failed to get commits for %s: %v", folder.Name, err)
			continue
		}

		if len(commits) > 0 {
			a.sendSyncCommits(folder.ID, commits)
			totalSynced += len(commits)
		}
	}

	if foldersCount == 0 {
		log.Printf("⚠️ No folders to sync for request")
	} else {
		log.Printf("✅ Commit sync complete: synced %d commits from %d folders", totalSynced, foldersCount)
	}

	ackPayload, _ := json.Marshal(map[string]interface{}{
		"folder_id":     payload.FolderID,
		"folders_count": foldersCount,
		"commits_count": totalSynced,
	})

	ackMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "commit_sync_complete",
		Payload:    ackPayload,
	}

	if err := a.wsClient.SendMessage(ackMsg); err != nil {
		log.Printf("❌ Failed to send commit_sync_complete: %v", err)
	}
}

// handleGetCommits handles a request for commit history.
func (a *Agent) handleGetCommits(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
		Limit    int    `json:"limit,omitempty"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal get_commits payload: %v", err)
		return
	}

	if payload.Limit == 0 {
		payload.Limit = 50
	}

	log.Printf("📥 Get commits request: folder=%s limit=%d", payload.FolderID, payload.Limit)

	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendCommitListError(payload.FolderID, "Folder not found")
		return
	}

	if !git.IsGitRepo(folderPath) {
		log.Printf("⚠️ Folder is not a git repository: %s", folderPath)
		a.sendCommitListError(payload.FolderID, "Not a git repository")
		return
	}

	repo := git.NewRepository(folderPath)
	commits, err := repo.GetCommits(payload.Limit)
	if err != nil {
		log.Printf("❌ Failed to get commits: %v", err)
		a.sendCommitListError(payload.FolderID, fmt.Sprintf("Failed to get commits: %v", err))
		return
	}

	commitData := make([]map[string]interface{}, 0, len(commits))
	for _, commit := range commits {
		commitData = append(commitData, map[string]interface{}{
			"commit_hash":   commit.FullHash,
			"short_hash":    commit.Hash,
			"message":       commit.Message,
			"author":        commit.Author,
			"author_email":  commit.Email,
			"committed_at":  time.Unix(commit.Timestamp, 0).Format(time.RFC3339),
			"additions":     commit.Stats.Additions,
			"deletions":     commit.Stats.Deletions,
			"files_changed": commit.Stats.FilesChanged,
		})
	}

	responsePayload, _ := json.Marshal(map[string]interface{}{
		"folder_id": payload.FolderID,
		"commits":   commitData,
	})

	responseMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeCommitsList,
		Payload:    responsePayload,
	}

	if err := a.wsClient.SendMessage(responseMsg); err != nil {
		log.Printf("❌ Failed to send commit list: %v", err)
	} else {
		log.Printf("📤 Sent %d commits for folder %s", len(commits), payload.FolderID)
	}
}

// sendCommitListError sends an error response for commit list request.
func (a *Agent) sendCommitListError(folderID, errMsg string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id": folderID,
		"error":     errMsg,
		"commits":   []interface{}{},
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeCommitsList,
		Payload:    payload,
	}

	a.wsClient.SendMessage(msg)
}

// handleGetCommitDetail handles a request for details of a specific commit.
func (a *Agent) handleGetCommitDetail(msg *ws.Message) {
	var payload struct {
		FolderID   string `json:"folder_id"`
		CommitHash string `json:"commit_hash"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal get_commit_detail payload: %v", err)
		return
	}

	log.Printf("📥 Get commit detail request: folder=%s hash=%s", payload.FolderID, payload.CommitHash)

	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendCommitDetailError(payload.FolderID, payload.CommitHash, "Folder not found")
		return
	}

	repo := git.NewRepository(folderPath)
	detail, err := repo.GetCommitDetails(payload.CommitHash)
	if err != nil {
		log.Printf("❌ Failed to get commit detail: %v", err)
		a.sendCommitDetailError(payload.FolderID, payload.CommitHash, fmt.Sprintf("Failed to get commit: %v", err))
		return
	}

	responsePayload, _ := json.Marshal(map[string]interface{}{
		"folder_id":     payload.FolderID,
		"commit_hash":   detail.FullHash,
		"short_hash":    detail.Hash,
		"message":       detail.Message,
		"author":        detail.Author,
		"author_email":  detail.Email,
		"committed_at":  time.Unix(detail.Timestamp, 0).Format(time.RFC3339),
		"additions":     detail.Stats.Additions,
		"deletions":     detail.Stats.Deletions,
		"files_changed": detail.Stats.FilesChanged,
		"files":         detail.Files,
	})

	responseMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeCommitDetail,
		Payload:    responsePayload,
	}

	if err := a.wsClient.SendMessage(responseMsg); err != nil {
		log.Printf("❌ Failed to send commit detail: %v", err)
	} else {
		log.Printf("📤 Sent commit detail for %s", payload.CommitHash[:7])
	}
}

// sendCommitDetailError sends an error response for commit detail request.
func (a *Agent) sendCommitDetailError(folderID, commitHash, errMsg string) {
	payload, _ := json.Marshal(map[string]interface{}{
		"folder_id":   folderID,
		"commit_hash": commitHash,
		"error":       errMsg,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeCommitDetail,
		Payload:    payload,
	}

	a.wsClient.SendMessage(msg)
}

// sendCommitSuccess sends a commit_success event to mobile.
func (a *Agent) sendCommitSuccess(conversationID string, folderPath string, folderID string) {
	repo := git.NewRepository(folderPath)

	latestCommit, err := repo.GetCommits(1)
	if err != nil || len(latestCommit) == 0 {
		log.Printf("⚠️  Could not get latest commit for success message")
		return
	}

	commit := latestCommit[0]

	payload, _ := json.Marshal(map[string]interface{}{
		"conversation_id": conversationID,
		"folder_id":       folderID,
		"commit_hash":     commit.FullHash,
		"short_hash":      commit.Hash,
		"message":         commit.Message,
		"author":          commit.Author,
		"author_email":    commit.Email,
		"committed_at":    time.Unix(commit.Timestamp, 0).Format(time.RFC3339),
		"additions":       commit.Stats.Additions,
		"deletions":       commit.Stats.Deletions,
		"files_changed":   commit.Stats.FilesChanged,
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "commit_success",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send commit_success: %v", err)
	} else {
		log.Printf("📤 Sent commit_success: %s - %s", commit.Hash, commit.Message)
	}

	// Also send updated folder list with new commits
	a.sendFolderListUpdate()
}
