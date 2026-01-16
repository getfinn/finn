package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/getfinn/finn/internal/llm/providers/claude"
	"github.com/getfinn/finn/internal/config"
	"github.com/getfinn/finn/internal/devserver"
	"github.com/getfinn/finn/internal/llm"
	"github.com/getfinn/finn/internal/tunnel"
	"github.com/getfinn/finn/internal/ui"
	"github.com/getfinn/finn/internal/watcher"
	ws "github.com/getfinn/finn/internal/websocket"
)

// ConversationState tracks state for an ongoing conversation.
type ConversationState struct {
	executor     claude.TaskRunner       // Claude executor (legacy)
	llmExecutor  llm.InteractiveExecutor // LLM executor (for Gemini, etc.)
	provider     llm.Provider            // Which LLM provider is being used (for reprompts)
	pendingDiffs map[string]bool         // file_path -> approved
	totalDiffs   int
	folderPath   string   // Track folder path for reprompts
	folderID     string   // Track folder ID for commit tracking
	files        []string // Files modified in this conversation (for selective discard)

	// TTL-based cleanup: conversations are kept for a grace period after completion
	// to handle mobile reconnection and late approvals
	completedAt  *time.Time            // When the task completed (nil if still running)
	diffContents map[string]string     // file_path -> diff content (for resending on reconnect)
	isCompleted  bool                  // Whether the task has completed
}

// Agent is the main daemon agent that orchestrates all operations.
// It manages WebSocket connections, folder approvals, Claude/Gemini execution,
// git operations, session watching, and live preview tunnels.
type Agent struct {
	cfg                *config.Config
	wsClient           *ws.Client
	tray               *ui.TrayUI
	isRunning          bool
	headless           bool
	executors          map[string]claude.TaskRunner  // conversation_id -> Claude executor
	conversationStates map[string]*ConversationState // conversation_id -> state
	sessionWatcher     *watcher.Watcher              // Watches ~/.claude/projects for external sessions

	// LLM executors (for Gemini, etc.)
	llmExecutors            map[string]llm.Executor            // conversation_id -> one-shot LLM executor
	llmInteractiveExecutors map[string]llm.InteractiveExecutor // conversation_id -> interactive LLM executor
	executorsMu             sync.RWMutex                       // Protects executors, llmExecutors, llmInteractiveExecutors

	// Client presence tracking (for skipping broadcasts when no listeners)
	mobileOnline bool
	webOnline    bool

	// Live Preview tunnels (folderID -> tunnel client)
	tunnels   map[string]*tunnel.Client
	tunnelsMu sync.Mutex

	// Dev server manager for auto-starting dev servers
	devServers *devserver.Manager

	// Spreadsheet file watcher for live updates
	spreadsheetWatcher   *devserver.SpreadsheetWatcher
	spreadsheetWatcherMu sync.Mutex

	// Git sync tracking (folderID -> last known HEAD hash)
	lastKnownHeads   map[string]string
	lastKnownHeadsMu sync.RWMutex
	gitSyncStop      chan struct{} // Signal to stop git sync goroutine

	// Conversation state TTL cleanup
	conversationStatesMu sync.RWMutex     // Protects conversationStates map
	stateCleanupStop     chan struct{}    // Signal to stop state cleanup goroutine
}

// New creates a new agent instance.
func New(headless bool, dev bool) (*Agent, error) {
	cfg, err := config.Load(dev)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	return &Agent{
		cfg:                     cfg,
		isRunning:               false,
		headless:                headless,
		executors:               make(map[string]claude.TaskRunner),
		conversationStates:      make(map[string]*ConversationState),
		tunnels:                 make(map[string]*tunnel.Client),
		devServers:              devserver.NewManager(),
		lastKnownHeads:          make(map[string]string),
		gitSyncStop:             make(chan struct{}),
		stateCleanupStop:        make(chan struct{}),
		llmExecutors:            make(map[string]llm.Executor),
		llmInteractiveExecutors: make(map[string]llm.InteractiveExecutor),
	}, nil
}

// Start starts the agent and all its subsystems.
func (a *Agent) Start() error {
	log.Println("🚀 PocketVibe Desktop Daemon starting...")

	// Set up dev server crash callback to notify mobile and cleanup tunnel when dev server dies
	a.devServers.SetStateChangeCallback(func(folderID string, state devserver.ServerState, err error) {
		if state == devserver.StateFailed {
			errMsg := "Dev server crashed"
			if err != nil {
				errMsg = fmt.Sprintf("Dev server crashed: %v", err)
			}
			log.Printf("💥 Dev server crash detected for folder %s: %s", folderID, errMsg)

			// Close the tunnel since it's now pointing to a dead port
			a.tunnelsMu.Lock()
			if client, ok := a.tunnels[folderID]; ok {
				log.Printf("🔌 Closing tunnel for crashed dev server: folder=%s", folderID)
				client.Close()
				delete(a.tunnels, folderID)
			}
			a.tunnelsMu.Unlock()

			a.sendPreviewStatus(folderID, "error", errMsg)
		}
	})

	// Check if we have auth token for current relay
	if a.cfg.GetToken(a.cfg.RelayURL) == "" {
		log.Printf("🔐 No auth token found for relay: %s", a.cfg.RelayURL)
		log.Println("🔐 Starting OAuth flow...")

		token, err := a.authenticateViaOAuth()
		if err != nil {
			log.Printf("❌ OAuth authentication failed: %v", err)
			return fmt.Errorf("authentication required: please sign in to continue. Error: %w", err)
		}

		log.Println("✅ OAuth authentication successful!")
		a.cfg.SetToken(a.cfg.RelayURL, token)
		a.cfg.Save()
	} else {
		log.Printf("✅ Using cached token for relay: %s", a.cfg.RelayURL)
	}

	// Ensure we have a device ID
	if a.cfg.DeviceID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "desktop"
		}
		a.cfg.DeviceID = fmt.Sprintf("%s-%d", hostname, time.Now().Unix())
		a.cfg.Save()
	}

	// Create WebSocket client with token for current relay
	a.wsClient = ws.NewClient(
		a.cfg.RelayURL,
		a.cfg.GetToken(a.cfg.RelayURL),
		a.cfg.UserID,
		a.cfg.DeviceID,
		a.handleMessage,
	)

	// Create system tray UI (only if not headless)
	if !a.headless {
		a.tray = ui.NewTrayUI(a.cfg)
		a.tray.SetCallbacks(a.handleFolderAdd, a.handleFolderRemove, a.handleQuit)
	}

	// Connect to relay server in background
	go func() {
		a.wsClient.ConnectWithRetry()

		if a.wsClient.IsConnected() {
			// Wait for connection to stabilize before sending folder list
			time.Sleep(100 * time.Millisecond)
			a.sendFolderListUpdate()
		}
	}()

	// Start background subsystems
	go a.monitorConnection()
	go a.startGitSyncChecker()
	go a.startConversationStateCleanup()

	// Initialize session watcher for external Claude Code sessions
	a.initSessionWatcher()

	a.isRunning = true

	// Start system tray (blocks until quit) or wait for signal in headless mode
	if a.headless {
		log.Println("✅ Running in headless mode - press Ctrl+C to stop")
		a.waitForShutdown()
	} else {
		a.tray.Start()
	}

	return nil
}

// monitorConnection monitors the WebSocket connection status.
func (a *Agent) monitorConnection() {
	// Placeholder for connection monitoring logic
}

// waitForShutdown blocks until a shutdown signal is received (for headless mode).
func (a *Agent) waitForShutdown() {
	sigChan := make(chan os.Signal, 1)
	notifyShutdownSignals(sigChan)

	sig := <-sigChan
	log.Printf("Received signal: %v", sig)

	a.handleQuit()
}

// handleQuit handles quit request and cleans up all resources.
func (a *Agent) handleQuit() {
	log.Println("Shutting down...")

	// Stop git sync checker
	close(a.gitSyncStop)

	// Stop conversation state cleanup
	close(a.stateCleanupStop)

	// Close all tunnel connections
	a.closeAllTunnels()

	if a.wsClient != nil {
		a.wsClient.Close()
	}

	a.isRunning = false
}

// ConversationStateTTL is how long to keep completed conversation state for late approvals.
const ConversationStateTTL = 10 * time.Minute

// startConversationStateCleanup runs a background goroutine that cleans up
// completed conversation states after the TTL expires.
func (a *Agent) startConversationStateCleanup() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-a.stateCleanupStop:
			log.Println("🛑 Conversation state cleanup goroutine stopped")
			return
		case <-ticker.C:
			a.cleanupExpiredConversationStates()
		}
	}
}

// cleanupExpiredConversationStates removes conversation states that have been
// completed for longer than the TTL.
func (a *Agent) cleanupExpiredConversationStates() {
	a.conversationStatesMu.Lock()
	defer a.conversationStatesMu.Unlock()

	now := time.Now()
	var toDelete []string

	for convID, state := range a.conversationStates {
		if state.isCompleted && state.completedAt != nil {
			age := now.Sub(*state.completedAt)
			if age > ConversationStateTTL {
				toDelete = append(toDelete, convID)
				log.Printf("🧹 Cleaning up expired conversation state: %s (age: %v)", convID, age)
			}
		}
	}

	if len(toDelete) > 0 {
		a.executorsMu.Lock()
		for _, convID := range toDelete {
			delete(a.conversationStates, convID)
			delete(a.executors, convID)
			delete(a.llmExecutors, convID)
			delete(a.llmInteractiveExecutors, convID)
		}
		a.executorsMu.Unlock()
	}
}

// syncPendingConversationsToMobile sends all pending (completed but not approved)
// conversations to mobile. Called when mobile reconnects.
func (a *Agent) syncPendingConversationsToMobile() {
	a.conversationStatesMu.RLock()
	defer a.conversationStatesMu.RUnlock()

	var pendingConversations []map[string]interface{}

	for convID, state := range a.conversationStates {
		// Only sync conversations that are completed and have pending diffs
		if state.isCompleted && len(state.diffContents) > 0 {
			// Build diff list for this conversation
			var diffs []map[string]interface{}
			for filePath, content := range state.diffContents {
				diffs = append(diffs, map[string]interface{}{
					"file_path": filePath,
					"diff":      content,
				})
			}

			pendingConversations = append(pendingConversations, map[string]interface{}{
				"conversation_id": convID,
				"folder_id":       state.folderID,
				"folder_path":     state.folderPath,
				"files":           state.files,
				"diffs":           diffs,
				"total_diffs":     state.totalDiffs,
			})

			log.Printf("📤 Syncing pending conversation to mobile: %s (%d diffs)", convID, len(diffs))
		}
	}

	if len(pendingConversations) == 0 {
		log.Println("📤 No pending conversations to sync to mobile")
		return
	}

	// Send pending_conversations message to mobile
	payload, err := json.Marshal(map[string]interface{}{
		"conversations": pendingConversations,
	})
	if err != nil {
		log.Printf("❌ Failed to marshal pending conversations: %v", err)
		return
	}

	msg := &ws.Message{
		UserID:     a.cfg.UserID,
		DeviceType: "desktop",
		Type:       "pending_conversations",
		Payload:    payload,
	}

	if err := a.wsClient.SendMessage(msg); err != nil {
		log.Printf("❌ Failed to send pending_conversations: %v", err)
	} else {
		log.Printf("✅ Sent pending_conversations to mobile (%d conversations)", len(pendingConversations))
	}
}
