package agent

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/getfinn/finn/internal/claude"
	"github.com/getfinn/finn/internal/config"
	"github.com/getfinn/finn/internal/devserver"
	"github.com/getfinn/finn/internal/tunnel"
	"github.com/getfinn/finn/internal/ui"
	"github.com/getfinn/finn/internal/watcher"
	ws "github.com/getfinn/finn/internal/websocket"
)

// ConversationState tracks state for an ongoing conversation.
type ConversationState struct {
	executor     claude.TaskRunner
	pendingDiffs map[string]bool // file_path -> approved
	totalDiffs   int
	folderPath   string   // Track folder path for reprompts
	folderID     string   // Track folder ID for commit tracking
	files        []string // Files modified in this conversation (for selective discard)
}

// Agent is the main daemon agent that orchestrates all operations.
// It manages WebSocket connections, folder approvals, Claude execution,
// git operations, session watching, and live preview tunnels.
type Agent struct {
	cfg                *config.Config
	wsClient           *ws.Client
	tray               *ui.TrayUI
	isRunning          bool
	headless           bool
	executors          map[string]claude.TaskRunner  // conversation_id -> executor
	conversationStates map[string]*ConversationState // conversation_id -> state
	sessionWatcher     *watcher.Watcher              // Watches ~/.claude/projects for external sessions

	// Client presence tracking (for skipping broadcasts when no listeners)
	mobileOnline bool
	webOnline    bool

	// Live Preview tunnels (folderID -> tunnel client)
	tunnels   map[string]*tunnel.Client
	tunnelsMu sync.Mutex

	// Dev server manager for auto-starting dev servers
	devServers *devserver.Manager

	// Git sync tracking (folderID -> last known HEAD hash)
	lastKnownHeads   map[string]string
	lastKnownHeadsMu sync.RWMutex
	gitSyncStop      chan struct{} // Signal to stop git sync goroutine
}

// New creates a new agent instance.
func New(headless bool, dev bool) (*Agent, error) {
	cfg, err := config.Load(dev)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}

	return &Agent{
		cfg:                cfg,
		isRunning:          false,
		headless:           headless,
		executors:          make(map[string]claude.TaskRunner),
		conversationStates: make(map[string]*ConversationState),
		tunnels:            make(map[string]*tunnel.Client),
		devServers:         devserver.NewManager(),
		lastKnownHeads:     make(map[string]string),
		gitSyncStop:        make(chan struct{}),
	}, nil
}

// Start starts the agent and all its subsystems.
func (a *Agent) Start() error {
	log.Println("🚀 PocketVibe Desktop Daemon starting...")

	// Set up dev server crash callback to notify mobile when dev server dies
	a.devServers.SetStateChangeCallback(func(folderID string, state devserver.ServerState, err error) {
		if state == devserver.StateFailed {
			errMsg := "Dev server crashed"
			if err != nil {
				errMsg = fmt.Sprintf("Dev server crashed: %v", err)
			}
			log.Printf("💥 Dev server crash detected for folder %s: %s", folderID, errMsg)
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
			if a.tray != nil {
				a.tray.UpdateConnectionStatus(true)
			}

			// Wait for connection to stabilize before sending folder list
			time.Sleep(100 * time.Millisecond)
			a.sendFolderListUpdate()
		}
	}()

	// Start background subsystems
	go a.monitorConnection()
	go a.startGitSyncChecker()

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
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	sig := <-sigChan
	log.Printf("Received signal: %v", sig)

	a.handleQuit()
}

// handleQuit handles quit request and cleans up all resources.
func (a *Agent) handleQuit() {
	log.Println("Shutting down...")

	// Stop git sync checker
	close(a.gitSyncStop)

	// Close all tunnel connections
	a.closeAllTunnels()

	if a.wsClient != nil {
		a.wsClient.Close()
	}

	a.isRunning = false
}
