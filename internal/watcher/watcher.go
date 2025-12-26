package watcher

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/getfinn/finn/internal/claude"
)

// SessionInfo represents a detected Claude Code session
type SessionInfo struct {
	SessionID    string    `json:"session_id"`
	ProjectPath  string    `json:"project_path"`
	FilePath     string    `json:"file_path"`
	Title        string    `json:"title"`
	Model        string    `json:"model"`
	MessageCount int       `json:"message_count"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	LastActivity time.Time `json:"last_activity"`
	IsActive     bool      `json:"is_active"` // Deprecated: use GetStatus() instead
}

// Session status constants
const (
	SessionStatusActive   = "active"   // Activity within the last hour
	SessionStatusInactive = "inactive" // No activity for 1+ hours
	SessionStatusStale    = "stale"    // No activity for 24+ hours
)

// GetStatus returns the session status based on last activity time
func (s *SessionInfo) GetStatus() string {
	elapsed := time.Since(s.LastActivity)

	if elapsed < time.Hour {
		return SessionStatusActive
	} else if elapsed < 24*time.Hour {
		return SessionStatusInactive
	}
	return SessionStatusStale
}

// IsRecentlyActive returns true if the session had activity within the last hour
func (s *SessionInfo) IsRecentlyActive() bool {
	return time.Since(s.LastActivity) < time.Hour
}

// SessionCallbacks for session events
type SessionCallbacks struct {
	OnNewSession       func(session *SessionInfo)
	OnSessionUpdated   func(session *SessionInfo)                      // Debounced: called when session metadata changes
	OnNewMessage       func(sessionID string, msg *claude.StoredMessage) // DEPRECATED: use OnSessionUpdated instead
	OnSessionEnd       func(sessionID string)
	ShouldWatchProject func(projectPath string) bool // Filter: return true to watch this project
}

// Watcher monitors ~/.claude/projects for session changes
type Watcher struct {
	claudePath    string
	fsWatcher     *fsnotify.Watcher
	sessions      map[string]*SessionInfo
	filePositions map[string]int64
	callbacks     SessionCallbacks
	pollInterval  time.Duration

	// Debouncing for session updates
	pendingUpdates map[string]bool      // Sessions with pending updates
	updateDebounce time.Duration        // How long to wait before sending update
	updateTimers   map[string]*time.Timer // Debounce timers per session

	mu       sync.RWMutex
	stopChan chan struct{}
	wg       sync.WaitGroup
}

// NewWatcher creates a new session watcher
func NewWatcher(callbacks SessionCallbacks) (*Watcher, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	claudePath := filepath.Join(home, ".claude", "projects")

	// Create directory if it doesn't exist
	os.MkdirAll(claudePath, 0755)

	w := &Watcher{
		claudePath:     claudePath,
		sessions:       make(map[string]*SessionInfo),
		filePositions:  make(map[string]int64),
		callbacks:      callbacks,
		pollInterval:   2 * time.Second,
		pendingUpdates: make(map[string]bool),
		updateDebounce: 500 * time.Millisecond, // Debounce updates by 500ms
		updateTimers:   make(map[string]*time.Timer),
		stopChan:       make(chan struct{}),
	}

	// Initialize fsnotify
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("⚠️ fsnotify unavailable, using poll-only mode: %v", err)
	} else {
		w.fsWatcher = fsWatcher
		w.setupWatchers()
	}

	log.Printf("✅ Session watcher initialized (path: %s, OS: %s)", claudePath, runtime.GOOS)
	return w, nil
}

// setupWatchers adds fsnotify watches to existing directories
func (w *Watcher) setupWatchers() {
	if w.fsWatcher == nil {
		return
	}

	// Watch main projects directory
	if err := w.fsWatcher.Add(w.claudePath); err != nil {
		log.Printf("⚠️ Failed to watch %s: %v", w.claudePath, err)
	}

	// Watch existing project subdirectories
	entries, _ := os.ReadDir(w.claudePath)
	for _, entry := range entries {
		if entry.IsDir() {
			projPath := filepath.Join(w.claudePath, entry.Name())
			if err := w.fsWatcher.Add(projPath); err != nil {
				log.Printf("⚠️ Failed to watch %s: %v", projPath, err)
			}
		}
	}
}

// Start begins watching for session changes
func (w *Watcher) Start() {
	// Scan existing sessions
	w.scanExistingSessions()

	// Start fsnotify listener
	if w.fsWatcher != nil {
		w.wg.Add(1)
		go w.fsnotifyLoop()
	}

	// Start polling loop (always, as backup)
	w.wg.Add(1)
	go w.pollLoop()

	log.Println("🔍 Session watcher started")
}

// Stop stops the watcher
func (w *Watcher) Stop() {
	close(w.stopChan)
	if w.fsWatcher != nil {
		w.fsWatcher.Close()
	}
	// Stop all pending update timers
	w.mu.Lock()
	for _, timer := range w.updateTimers {
		timer.Stop()
	}
	w.mu.Unlock()
	w.wg.Wait()
	log.Println("🛑 Session watcher stopped")
}

// scheduleSessionUpdate schedules a debounced update notification for a session
func (w *Watcher) scheduleSessionUpdate(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Mark session as having pending update
	w.pendingUpdates[sessionID] = true

	// Cancel existing timer if any
	if timer, exists := w.updateTimers[sessionID]; exists {
		timer.Stop()
	}

	// Schedule new debounced update
	w.updateTimers[sessionID] = time.AfterFunc(w.updateDebounce, func() {
		w.sendSessionUpdate(sessionID)
	})
}

// sendSessionUpdate sends the actual update notification
func (w *Watcher) sendSessionUpdate(sessionID string) {
	w.mu.Lock()
	session, exists := w.sessions[sessionID]
	if !exists {
		w.mu.Unlock()
		return
	}
	// Clear pending flag
	delete(w.pendingUpdates, sessionID)
	delete(w.updateTimers, sessionID)
	// Make a copy to avoid holding lock during callback
	sessionCopy := *session
	w.mu.Unlock()

	if w.callbacks.OnSessionUpdated != nil {
		w.callbacks.OnSessionUpdated(&sessionCopy)
	}
}

// fsnotifyLoop handles fsnotify events
func (w *Watcher) fsnotifyLoop() {
	defer w.wg.Done()

	for {
		select {
		case <-w.stopChan:
			return

		case event, ok := <-w.fsWatcher.Events:
			if !ok {
				return
			}

			if event.Op&fsnotify.Create == fsnotify.Create {
				if strings.HasSuffix(event.Name, ".jsonl") {
					w.handleNewFile(event.Name, true) // Broadcast new sessions from fsnotify
				} else if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					w.fsWatcher.Add(event.Name)
				}
			}

			if event.Op&fsnotify.Write == fsnotify.Write {
				if strings.HasSuffix(event.Name, ".jsonl") {
					w.handleFileModified(event.Name)
				}
			}

		case err, ok := <-w.fsWatcher.Errors:
			if !ok {
				return
			}
			log.Printf("⚠️ fsnotify error: %v", err)
		}
	}
}

// pollLoop periodically checks for changes
func (w *Watcher) pollLoop() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopChan:
			return
		case <-ticker.C:
			w.pollForChanges()
		}
	}
}

// pollForChanges scans for new content in tracked files
func (w *Watcher) pollForChanges() {
	w.mu.Lock()
	positions := make(map[string]int64)
	for k, v := range w.filePositions {
		positions[k] = v
	}
	w.mu.Unlock()

	// Check each tracked file for growth
	for filePath, lastPos := range positions {
		info, err := os.Stat(filePath)
		if err != nil {
			continue
		}

		if info.Size() > lastPos {
			w.readNewLines(filePath, lastPos)
		}
	}

	// Scan for new session files
	w.scanForNewSessions()
}

// scanExistingSessions finds all existing sessions on startup
func (w *Watcher) scanExistingSessions() {
	entries, err := os.ReadDir(w.claudePath)
	if err != nil {
		log.Printf("⚠️ Failed to read %s: %v", w.claudePath, err)
		return
	}

	totalFiles := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		projPath := filepath.Join(w.claudePath, entry.Name())
		files, _ := os.ReadDir(projPath)

		for _, file := range files {
			if strings.HasSuffix(file.Name(), ".jsonl") {
				totalFiles++
				filePath := filepath.Join(projPath, file.Name())
				w.handleNewFile(filePath, false) // Don't broadcast existing sessions
			}
		}
	}

	log.Printf("📁 Indexed %d sessions in approved folders (skipped %d in other folders)",
		len(w.sessions), totalFiles-len(w.sessions))
}

// scanForNewSessions looks for new .jsonl files
func (w *Watcher) scanForNewSessions() {
	entries, _ := os.ReadDir(w.claudePath)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		projPath := filepath.Join(w.claudePath, entry.Name())
		files, _ := os.ReadDir(projPath)

		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".jsonl") {
				continue
			}

			filePath := filepath.Join(projPath, file.Name())
			sessionID := extractSessionID(filePath)

			w.mu.RLock()
			_, exists := w.sessions[sessionID]
			w.mu.RUnlock()

			if !exists {
				w.handleNewFile(filePath, true) // Broadcast new sessions found after startup
			}
		}
	}
}

// handleNewFile processes a newly discovered session file
// broadcastNew controls whether to call OnNewSession callback (false for initial scan)
func (w *Watcher) handleNewFile(filePath string, broadcastNew bool) {
	sessionID := extractSessionID(filePath)

	// Skip subagent sessions (agent-XXXX files)
	// These are internal Claude Code Task tool sessions that don't have proper titles
	// and shouldn't be shown as user-facing conversations
	if strings.HasPrefix(sessionID, "agent-") {
		return
	}

	// Decode project path from directory name BEFORE parsing
	projectPath := decodeProjectPath(filepath.Base(filepath.Dir(filePath)))

	// Check filter - skip if project is not in approved folders
	if w.callbacks.ShouldWatchProject != nil && !w.callbacks.ShouldWatchProject(projectPath) {
		return // Skip - not an approved folder, don't parse or track
	}

	w.mu.Lock()
	if _, exists := w.sessions[sessionID]; exists {
		w.mu.Unlock()
		return
	}

	session := &SessionInfo{
		SessionID:    sessionID,
		ProjectPath:  projectPath,
		FilePath:     filePath,
		LastActivity: time.Time{}, // Zero time - will be updated from message timestamps
		IsActive:     true,
	}

	// Parse file to get metadata (this updates LastActivity from actual message timestamps)
	w.parseSessionFile(filePath, session)

	// If no messages had timestamps, use file modification time as fallback
	if session.LastActivity.IsZero() {
		if info, err := os.Stat(filePath); err == nil {
			session.LastActivity = info.ModTime()
		} else {
			session.LastActivity = time.Now()
		}
	}

	// Track file position for tailing
	if info, err := os.Stat(filePath); err == nil {
		w.filePositions[filePath] = info.Size()
	}

	w.sessions[sessionID] = session
	w.mu.Unlock()

	if broadcastNew {
		log.Printf("🆕 New session detected: %s (title: %q, messages: %d)",
			sessionID, session.Title, session.MessageCount)

		if w.callbacks.OnNewSession != nil {
			go w.callbacks.OnNewSession(session)
		}
	}
}

// handleFileModified handles file write events
func (w *Watcher) handleFileModified(filePath string) {
	w.mu.RLock()
	lastPos, tracked := w.filePositions[filePath]
	w.mu.RUnlock()

	// Only process files we're already tracking (approved folders)
	// This prevents flooding from unapproved folder sessions
	if !tracked {
		return
	}

	w.readNewLines(filePath, lastPos)
}

// readNewLines reads new content from a file
func (w *Watcher) readNewLines(filePath string, startPos int64) {
	file, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer file.Close()

	file.Seek(startPos, io.SeekStart)

	sessionID := extractSessionID(filePath)
	reader := bufio.NewReader(file)
	messagesProcessed := 0

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var msg claude.StoredMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			log.Printf("⚠️ Failed to parse message: %v", err)
			continue
		}

		// Update session metadata
		w.updateSessionFromMessage(sessionID, &msg)
		messagesProcessed++

		// DEPRECATED: OnNewMessage callback (kept for backwards compatibility)
		// New code should use OnSessionUpdated instead
		if w.callbacks.OnNewMessage != nil {
			go w.callbacks.OnNewMessage(sessionID, &msg)
		}
	}

	// Schedule debounced session update if any messages were processed
	if messagesProcessed > 0 {
		w.scheduleSessionUpdate(sessionID)
	}

	// Update file position (only for files we're already tracking)
	if newPos, err := file.Seek(0, io.SeekCurrent); err == nil {
		w.mu.Lock()
		if _, exists := w.filePositions[filePath]; exists {
			w.filePositions[filePath] = newPos
		}
		w.mu.Unlock()
	}
}

// updateSessionFromMessage updates session metadata
func (w *Watcher) updateSessionFromMessage(sessionID string, msg *claude.StoredMessage) {
	w.mu.Lock()
	defer w.mu.Unlock()

	session, exists := w.sessions[sessionID]
	if !exists {
		return
	}

	session.MessageCount++
	session.LastActivity = time.Now()
	session.TotalCostUSD += msg.CostUSD

	if msg.Type == "summary" && msg.Summary != "" {
		session.Title = msg.Summary
	}

	if model := msg.GetModel(); model != "" {
		session.Model = model
	}
}

// parseSessionFile reads all messages for initial metadata
func (w *Watcher) parseSessionFile(filePath string, session *SessionInfo) {
	file, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	var firstUserMessage string    // For generating title if no summary
	var firstAssistantText string  // Fallback: use first assistant response
	var anyMessageContent string   // Last resort: any message with content

	for scanner.Scan() {
		var msg claude.StoredMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		session.MessageCount++
		session.TotalCostUSD += msg.CostUSD

		// Get text content once for efficiency
		content := msg.GetTextContent()

		// Capture first user message for title generation (highest priority)
		if firstUserMessage == "" && msg.Type == "user" {
			if content != "" {
				firstUserMessage = content
			}
		}

		// Capture first assistant message as fallback
		if firstAssistantText == "" && msg.Type == "assistant" {
			if content != "" {
				firstAssistantText = content
			}
		}

		// Capture any message content as last resort
		if anyMessageContent == "" && content != "" {
			anyMessageContent = content
		}

		// Summary messages take highest precedence for title
		if msg.Type == "summary" && msg.Summary != "" {
			session.Title = msg.Summary
		}

		if model := msg.GetModel(); model != "" {
			session.Model = model
		}

		if msg.Timestamp.After(session.LastActivity) {
			session.LastActivity = msg.Timestamp
		}
	}

	// Generate title with fallback chain
	if session.Title == "" {
		if firstUserMessage != "" {
			session.Title = generateTitleFromMessage(firstUserMessage)
		} else if firstAssistantText != "" {
			// Use assistant's first response as title hint
			session.Title = generateTitleFromMessage(firstAssistantText)
		} else if anyMessageContent != "" {
			// Last resort: use any message content
			session.Title = generateTitleFromMessage(anyMessageContent)
		}
	}
}

// generateTitleFromMessage creates a short title from a user message
func generateTitleFromMessage(message string) string {
	// Strip PocketVibe security preamble if present
	if idx := strings.Index(message, "User request:"); idx != -1 {
		message = strings.TrimSpace(message[idx+len("User request:"):])
	}

	// Truncate to reasonable title length
	maxLen := 60
	if len(message) <= maxLen {
		return message
	}

	// Find a good break point (space) near maxLen
	truncated := message[:maxLen]
	if lastSpace := strings.LastIndex(truncated, " "); lastSpace > maxLen/2 {
		truncated = truncated[:lastSpace]
	}

	return truncated + "..."
}

// GetSession returns a session by ID
func (w *Watcher) GetSession(sessionID string) *SessionInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.sessions[sessionID]
}

// GetSessions returns all sessions
func (w *Watcher) GetSessions() []*SessionInfo {
	w.mu.RLock()
	defer w.mu.RUnlock()

	result := make([]*SessionInfo, 0, len(w.sessions))
	for _, s := range w.sessions {
		result = append(result, s)
	}
	return result
}

// ClearProjectSessions removes all sessions for a specific project path.
// This should be called when a folder is removed from approved folders.
func (w *Watcher) ClearProjectSessions(projectPath string) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	count := 0
	for sessionID, session := range w.sessions {
		if session.ProjectPath == projectPath {
			// Remove file tracking
			delete(w.filePositions, session.FilePath)
			// Remove pending update timer if any
			if timer, exists := w.updateTimers[sessionID]; exists {
				timer.Stop()
				delete(w.updateTimers, sessionID)
			}
			delete(w.pendingUpdates, sessionID)
			// Remove session
			delete(w.sessions, sessionID)
			count++
		}
	}

	if count > 0 {
		log.Printf("🗑️ Cleared %d sessions for removed folder %s", count, projectPath)
	}
	return count
}

// ScanProjectSessions scans a specific project folder and returns all sessions.
// This is used when a new folder is added to batch-discover sessions instead of
// trickling them one by one through the polling loop.
// Thread-safe: holds lock for entire operation to prevent race with polling loop.
func (w *Watcher) ScanProjectSessions(projectPath string) []*SessionInfo {
	encoded := EncodeProjectPath(projectPath)
	projDir := filepath.Join(w.claudePath, encoded)

	files, err := os.ReadDir(projDir)
	if err != nil {
		log.Printf("⚠️ Failed to read project dir %s: %v", projDir, err)
		return nil
	}

	// Hold lock for entire operation to prevent race with polling loop
	w.mu.Lock()
	defer w.mu.Unlock()

	var sessions []*SessionInfo

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}

		filePath := filepath.Join(projDir, file.Name())
		sessionID := extractSessionID(filePath)

		// Skip subagent sessions (agent-XXXX files)
		if strings.HasPrefix(sessionID, "agent-") {
			continue
		}

		// Check if already tracked (under lock)
		if _, exists := w.sessions[sessionID]; exists {
			continue
		}

		// Create and parse session
		session := &SessionInfo{
			SessionID:    sessionID,
			ProjectPath:  projectPath,
			FilePath:     filePath,
			LastActivity: time.Time{}, // Zero time - will be updated from message timestamps
			IsActive:     true,
		}

		// parseSessionFile updates LastActivity from actual message timestamps
		w.parseSessionFile(filePath, session)

		// If no messages had timestamps, use file modification time as fallback
		if session.LastActivity.IsZero() {
			if info, err := os.Stat(filePath); err == nil {
				session.LastActivity = info.ModTime()
			} else {
				session.LastActivity = time.Now()
			}
		}

		// Track file position and session (already under lock)
		if info, err := os.Stat(filePath); err == nil {
			w.filePositions[filePath] = info.Size()
		}
		w.sessions[sessionID] = session

		sessions = append(sessions, session)
	}

	if len(sessions) > 0 {
		log.Printf("📁 Discovered %d sessions for project %s", len(sessions), projectPath)
	}

	return sessions
}

// GetSessionMessages reads all messages from a session file
func (w *Watcher) GetSessionMessages(sessionID string) ([]*claude.StoredMessage, error) {
	w.mu.RLock()
	session, exists := w.sessions[sessionID]
	w.mu.RUnlock()

	if !exists {
		return nil, nil
	}

	file, err := os.Open(session.FilePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var messages []*claude.StoredMessage
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		var msg claude.StoredMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		messages = append(messages, &msg)
	}

	return messages, nil
}

// Helper functions

func extractSessionID(filePath string) string {
	base := filepath.Base(filePath)
	return strings.TrimSuffix(base, ".jsonl")
}

func decodeProjectPath(encoded string) string {
	// -Users-jrk-myproject -> /Users/jrk/myproject
	if len(encoded) > 0 && encoded[0] == '-' {
		encoded = encoded[1:]
	}
	return "/" + strings.ReplaceAll(encoded, "-", "/")
}

// EncodeProjectPath converts a path to Claude's encoded format
func EncodeProjectPath(path string) string {
	// /Users/jrk/myproject -> -Users-jrk-myproject
	return strings.ReplaceAll(path, "/", "-")
}
