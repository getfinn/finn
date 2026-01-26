package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
	payload, _ := json.Marshal(map[string]any{
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
func (a *Agent) getCommitsForFolder(folderPath string) []map[string]any {
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

	result := make([]map[string]any, 0, len(commits))
	for _, commit := range commits {
		result = append(result, map[string]any{
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
	commitData := make([]map[string]any, 0, len(commits))
	for _, commit := range commits {
		commitData = append(commitData, map[string]any{
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

	payload, _ := json.Marshal(map[string]any{
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

	ackPayload, _ := json.Marshal(map[string]any{
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

	commitData := make([]map[string]any, 0, len(commits))
	for _, commit := range commits {
		commitData = append(commitData, map[string]any{
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

	responsePayload, _ := json.Marshal(map[string]any{
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
	payload, _ := json.Marshal(map[string]any{
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

	responsePayload, _ := json.Marshal(map[string]any{
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
	payload, _ := json.Marshal(map[string]any{
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

	payload, _ := json.Marshal(map[string]any{
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

// Constants for diff size limiting
const (
	// maxDiffSize is the maximum size of a single diff in bytes (50KB)
	maxDiffSize = 50 * 1024
	// maxTotalDiffsSize is the maximum total size of all diffs in bytes (400KB, leaving room for JSON overhead)
	maxTotalDiffsSize = 400 * 1024
)

// skipPatterns contains path patterns that should be skipped when generating diffs
var skipPatterns = []string{
	".next/",
	".nuxt/",
	".svelte-kit/",
	".astro/",
	".vercel/",
	".netlify/",
	".turbo/",
	".cache/",
	".parcel-cache/",
	"node_modules/",
	"__pycache__/",
	".venv/",
	"venv/",
	"vendor/",
	"target/",
	"dist/",
	"build/",
	"out/",
	".git/",
}

// binaryExtensions contains file extensions that are typically binary
var binaryExtensions = []string{
	".png", ".jpg", ".jpeg", ".gif", ".ico", ".webp", ".svg",
	".woff", ".woff2", ".ttf", ".eot", ".otf",
	".pdf", ".zip", ".tar", ".gz", ".rar",
	".exe", ".dll", ".so", ".dylib",
	".mp3", ".mp4", ".wav", ".avi", ".mov",
	".pack", ".pack.gz", ".idx",
}

// shouldSkipFile returns true if the file should be skipped for diff generation
func shouldSkipFile(filePath string) bool {
	// Check skip patterns
	for _, pattern := range skipPatterns {
		if strings.Contains(filePath, pattern) {
			return true
		}
	}

	// Check binary extensions
	lowerPath := strings.ToLower(filePath)
	for _, ext := range binaryExtensions {
		if strings.HasSuffix(lowerPath, ext) {
			return true
		}
	}

	return false
}

// handleGetUncommittedDiffs handles a request for uncommitted file diffs (standalone, not tied to conversation).
func (a *Agent) handleGetUncommittedDiffs(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal get_uncommitted_diffs payload: %v", err)
		return
	}

	log.Printf("📥 Received get_uncommitted_diffs request for folder: %s", payload.FolderID)

	// Find the folder path
	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendUncommittedDiffsError(payload.FolderID, "Folder not found")
		return
	}

	if !git.IsGitRepo(folderPath) {
		log.Printf("❌ Not a git repository: %s", folderPath)
		a.sendUncommittedDiffsError(payload.FolderID, "Not a git repository")
		return
	}

	repo := git.NewRepository(folderPath)

	// Generate diffs for all uncommitted files
	diffs, err := repo.GenerateAllDiffs()
	if err != nil {
		log.Printf("❌ Failed to generate diffs: %v", err)
		a.sendUncommittedDiffsError(payload.FolderID, fmt.Sprintf("Failed to generate diffs: %v", err))
		return
	}

	// Convert to array format for response, with size limiting
	var filesArray []map[string]any
	totalSize := 0
	skippedCount := 0
	truncatedCount := 0

	for filePath, diff := range diffs {
		// Skip files in build directories or binary files
		if shouldSkipFile(filePath) {
			skippedCount++
			continue
		}

		// Check if adding this diff would exceed total size limit
		diffSize := len(diff)
		if totalSize+diffSize > maxTotalDiffsSize {
			log.Printf("⚠️  Skipping remaining files: total size limit reached (%d bytes)", totalSize)
			skippedCount += len(diffs) - len(filesArray) - skippedCount
			break
		}

		// Truncate individual large diffs
		truncated := false
		if diffSize > maxDiffSize {
			// Find a good truncation point (end of a line)
			truncateAt := maxDiffSize
			for i := maxDiffSize; i > maxDiffSize-1000 && i > 0; i-- {
				if diff[i] == '\n' {
					truncateAt = i
					break
				}
			}
			diff = diff[:truncateAt] + "\n\n... (diff truncated, file too large to display fully)"
			truncated = true
			truncatedCount++
			log.Printf("⚠️  Truncated large diff for %s: %d -> %d bytes", filePath, diffSize, len(diff))
		}

		// Parse additions/deletions from diff
		additions, deletions := 0, 0
		for _, line := range strings.Split(diff, "\n") {
			if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
				additions++
			} else if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				deletions++
			}
		}

		fileEntry := map[string]any{
			"file_path": filePath,
			"diff":      diff,
			"additions": additions,
			"deletions": deletions,
		}
		if truncated {
			fileEntry["truncated"] = true
		}

		filesArray = append(filesArray, fileEntry)
		totalSize += len(diff)
	}

	log.Printf("📤 Sending %d uncommitted diffs for folder: %s (skipped: %d, truncated: %d, total size: %d bytes)",
		len(filesArray), payload.FolderID, skippedCount, truncatedCount, totalSize)

	responsePayload, _ := json.Marshal(map[string]any{
		"folder_id":       payload.FolderID,
		"files":           filesArray,
		"skipped_count":   skippedCount,
		"truncated_count": truncatedCount,
	})

	responseMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "uncommitted_diffs",
		Payload:    responsePayload,
	}

	if err := a.wsClient.SendMessage(responseMsg); err != nil {
		log.Printf("❌ Failed to send uncommitted_diffs: %v", err)
	}
}

// sendUncommittedDiffsError sends an error response for uncommitted diffs request.
func (a *Agent) sendUncommittedDiffsError(folderID string, errMsg string) {
	payload, _ := json.Marshal(map[string]any{
		"folder_id": folderID,
		"error":     errMsg,
		"files":     []map[string]any{},
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "uncommitted_diffs",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send uncommitted_diffs error: %v", err)
	}
}

// handleStandaloneCommit handles a commit request not tied to a conversation.
func (a *Agent) handleStandaloneCommit(msg *ws.Message) {
	var payload struct {
		FolderID      string `json:"folder_id"`
		CommitMessage string `json:"commit_message"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal standalone_commit payload: %v", err)
		return
	}

	log.Printf("📥 Received standalone_commit request for folder: %s", payload.FolderID)

	// Find the folder path
	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendStandaloneCommitResponse(payload.FolderID, false, "Folder not found", nil)
		return
	}

	if !git.IsGitRepo(folderPath) {
		log.Printf("❌ Not a git repository: %s", folderPath)
		a.sendStandaloneCommitResponse(payload.FolderID, false, "Not a git repository", nil)
		return
	}

	repo := git.NewRepository(folderPath)

	// Check if there are changes to commit
	hasChanges, err := repo.HasChanges()
	if err != nil {
		log.Printf("❌ Failed to check for changes: %v", err)
		a.sendStandaloneCommitResponse(payload.FolderID, false, fmt.Sprintf("Failed to check for changes: %v", err), nil)
		return
	}

	if !hasChanges {
		log.Printf("⚠️ No changes to commit in: %s", folderPath)
		a.sendStandaloneCommitResponse(payload.FolderID, false, "No changes to commit", nil)
		return
	}

	// Use provided message or default
	commitMsg := payload.CommitMessage
	if commitMsg == "" {
		commitMsg = "Changes committed via Finn"
	}

	// First, commit the changes
	if err := repo.Commit(commitMsg); err != nil {
		log.Printf("❌ Failed to commit: %v", err)
		a.sendStandaloneCommitResponse(payload.FolderID, false, fmt.Sprintf("Commit failed: %v", err), nil)
		return
	}

	log.Printf("✅ Commit successful in: %s", folderPath)

	// Then, try to push (but don't fail the whole operation if push fails)
	pushFailed := false
	pushError := ""
	if err := repo.Push(); err != nil {
		// Check if it's a "no remote" error - that's OK
		errStr := err.Error()
		if strings.Contains(errStr, "No configured push destination") ||
			strings.Contains(errStr, "no upstream branch") {
			log.Printf("ℹ️  No remote repository configured - changes committed locally only")
		} else {
			// Real push error - note it but don't fail
			log.Printf("⚠️ Push failed (commit succeeded): %v", err)
			pushFailed = true
			pushError = err.Error()
		}
	} else {
		log.Printf("✅ Changes pushed to remote repository")
	}

	// Get the latest commit info for the response
	latestCommit, err := repo.GetCommits(1)
	var commitInfo map[string]any
	if err == nil && len(latestCommit) > 0 {
		commit := latestCommit[0]
		commitInfo = map[string]any{
			"commit_hash":   commit.FullHash,
			"short_hash":    commit.Hash,
			"message":       commit.Message,
			"author":        commit.Author,
			"author_email":  commit.Email,
			"committed_at":  time.Unix(commit.Timestamp, 0).Format(time.RFC3339),
			"additions":     commit.Stats.Additions,
			"deletions":     commit.Stats.Deletions,
			"files_changed": commit.Stats.FilesChanged,
			"push_failed":   pushFailed,
			"push_error":    pushError,
		}
	}

	// Build appropriate success message
	var successMessage string
	if pushFailed {
		successMessage = fmt.Sprintf("Committed locally, but push failed: %s", pushError)
	} else {
		successMessage = "Changes committed and pushed successfully"
	}

	a.sendStandaloneCommitResponse(payload.FolderID, true, successMessage, commitInfo)

	// Send updated folder list
	a.sendFolderListUpdate()
}

// sendStandaloneCommitResponse sends the result of a standalone commit operation.
func (a *Agent) sendStandaloneCommitResponse(folderID string, success bool, message string, commit map[string]any) {
	responseData := map[string]any{
		"folder_id": folderID,
		"success":   success,
		"message":   message,
	}

	if commit != nil {
		responseData["commit"] = commit
	}

	payload, _ := json.Marshal(responseData)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "standalone_commit_response",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send standalone_commit_response: %v", err)
	} else {
		log.Printf("📤 Sent standalone_commit_response: success=%v, message=%s", success, message)
	}
}

// handleBranchSwitch handles a request to switch git branches.
func (a *Agent) handleBranchSwitch(msg *ws.Message) {
	var payload struct {
		FolderID   string `json:"folder_id"`
		BranchName string `json:"branch_name"`
		CreateNew  bool   `json:"create_new,omitempty"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal branch_switch payload: %v", err)
		a.sendBranchSwitchedResponse(payload.FolderID, "", false, "Invalid request")
		return
	}

	log.Printf("📥 Received branch_switch request: folder=%s branch=%s create=%v",
		payload.FolderID, payload.BranchName, payload.CreateNew)

	// Find the folder path
	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendBranchSwitchedResponse(payload.FolderID, payload.BranchName, false, "Folder not found")
		return
	}

	if !git.IsGitRepo(folderPath) {
		log.Printf("❌ Not a git repository: %s", folderPath)
		a.sendBranchSwitchedResponse(payload.FolderID, payload.BranchName, false, "Not a git repository")
		return
	}

	repo := git.NewRepository(folderPath)

	// Get current branch before switching (for the response)
	previousBranch, _ := repo.GetCurrentBranch()

	// Check for uncommitted changes
	hasChanges, err := repo.HasChanges()
	if err != nil {
		log.Printf("❌ Failed to check for changes: %v", err)
		a.sendBranchSwitchedResponse(payload.FolderID, payload.BranchName, false,
			fmt.Sprintf("Failed to check for changes: %v", err))
		return
	}

	if hasChanges {
		log.Printf("⚠️ Uncommitted changes detected - cannot switch branches")
		a.sendBranchSwitchedResponse(payload.FolderID, payload.BranchName, false,
			"Cannot switch branches with uncommitted changes. Please commit or discard your changes first.")
		return
	}

	// Switch to the branch
	if err := repo.SwitchBranch(payload.BranchName, payload.CreateNew); err != nil {
		log.Printf("❌ Failed to switch branch: %v", err)
		a.sendBranchSwitchedResponse(payload.FolderID, payload.BranchName, false,
			fmt.Sprintf("Failed to switch branch: %v", err))
		return
	}

	log.Printf("✅ Switched to branch: %s", payload.BranchName)
	a.sendBranchSwitchedResponseFull(payload.FolderID, payload.BranchName, true, "", previousBranch)

	// Send updated folder list with new current branch
	a.sendFolderListUpdate()
}

// sendBranchSwitchedResponse sends a branch switch result with standard fields.
func (a *Agent) sendBranchSwitchedResponse(folderID, branchName string, success bool, errMsg string) {
	a.sendBranchSwitchedResponseFull(folderID, branchName, success, errMsg, "")
}

// sendBranchSwitchedResponseFull sends a branch switch result with all fields.
func (a *Agent) sendBranchSwitchedResponseFull(folderID, branchName string, success bool, errMsg, previousBranch string) {
	responseData := map[string]any{
		"folder_id":   folderID,
		"branch_name": branchName,
		"success":     success,
	}

	if errMsg != "" {
		responseData["error"] = errMsg
	}
	if previousBranch != "" {
		responseData["previous_branch"] = previousBranch
	}

	payload, _ := json.Marshal(responseData)

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeBranchSwitched,
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send branch_switched: %v", err)
	} else {
		log.Printf("📤 Sent branch_switched: success=%v, branch=%s", success, branchName)
	}
}

// handleGetBranches handles a request for the list of git branches.
func (a *Agent) handleGetBranches(msg *ws.Message) {
	var payload struct {
		FolderID string `json:"folder_id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Printf("Failed to unmarshal get_branches payload: %v", err)
		return
	}

	log.Printf("📥 Received get_branches request for folder: %s", payload.FolderID)

	// Find the folder path
	var folderPath string
	for _, folder := range a.cfg.ApprovedFolders {
		if folder.ID == payload.FolderID {
			folderPath = folder.Path
			break
		}
	}

	if folderPath == "" {
		log.Printf("❌ Folder not found: %s", payload.FolderID)
		a.sendBranchListError(payload.FolderID, "Folder not found")
		return
	}

	if !git.IsGitRepo(folderPath) {
		log.Printf("❌ Not a git repository: %s", folderPath)
		a.sendBranchListError(payload.FolderID, "Not a git repository")
		return
	}

	repo := git.NewRepository(folderPath)

	branches, err := repo.ListBranches()
	if err != nil {
		log.Printf("❌ Failed to list branches: %v", err)
		a.sendBranchListError(payload.FolderID, fmt.Sprintf("Failed to list branches: %v", err))
		return
	}

	currentBranch, _ := repo.GetCurrentBranch()

	responsePayload, _ := json.Marshal(map[string]any{
		"folder_id":      payload.FolderID,
		"branches":       branches,
		"current_branch": currentBranch,
	})

	responseMsg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeBranchList,
		Payload:    responsePayload,
	}

	if err := a.wsClient.SendMessage(responseMsg); err != nil {
		log.Printf("❌ Failed to send branch_list: %v", err)
	} else {
		log.Printf("📤 Sent %d branches for folder %s (current: %s)", len(branches), payload.FolderID, currentBranch)
	}
}

// sendBranchListError sends an error response for branch list request.
func (a *Agent) sendBranchListError(folderID, errMsg string) {
	payload, _ := json.Marshal(map[string]any{
		"folder_id": folderID,
		"error":     errMsg,
		"branches":  []string{},
	})

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       ws.MessageTypeBranchList,
		Payload:    payload,
	}

	a.wsClient.SendMessage(msg)
}
