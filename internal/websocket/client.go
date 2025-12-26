package websocket

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// MessageType represents different message types
type MessageType string

const (
	MessageTypePrompt         MessageType = "prompt"
	MessageTypeDecision       MessageType = "decision"
	MessageTypeChoice         MessageType = "choice"
	MessageTypeThinking       MessageType = "thinking"
	MessageTypeToolUse        MessageType = "tool_use"
	MessageTypeProgress       MessageType = "progress"
	MessageTypeDiff           MessageType = "diff"
	MessageTypeApproval       MessageType = "approval"
	MessageTypeDiffApproved   MessageType = "diff_approved"   // Mobile approves specific diff
	MessageTypeReprompt       MessageType = "reprompt"        // Mobile sends reprompt to revise changes
	MessageTypeSettingsUpdate MessageType = "settings_update" // Mobile sends execution mode changes
	MessageTypeComplete       MessageType = "complete"
	MessageTypeUsage          MessageType = "usage"    // Desktop → Relay/Mobile: Token usage data
	MessageTypeError          MessageType = "error"
	MessageTypePresence       MessageType = "presence"
	MessageTypeRollAgain      MessageType = "roll_again"
	MessageTypeCommitSuccess    MessageType = "commit_success"     // Desktop → Mobile: Commit completed
	MessageTypeGetCommits       MessageType = "get_commits"        // Mobile → Desktop: Request commit list
	MessageTypeCommitsList      MessageType = "commits_list"       // Desktop → Mobile: Commit list response
	MessageTypeGetCommitDetail  MessageType = "get_commit_detail"  // Mobile → Desktop: Request single commit details
	MessageTypeCommitDetail     MessageType = "commit_detail"      // Desktop → Mobile: Single commit details response
	MessageTypeSessionLinked    MessageType = "session_linked"     // Desktop → Relay: Link conversation_id with session_id

	// Live Preview (Pro/Max only)
	MessageTypePreviewStart  MessageType = "preview_start"  // Mobile/Web → Desktop: Start preview for folder
	MessageTypePreviewReady  MessageType = "preview_ready"  // Desktop → Mobile/Web: Preview URL is ready
	MessageTypePreviewStop   MessageType = "preview_stop"   // Mobile/Web → Desktop: Stop preview
	MessageTypePreviewStatus MessageType = "preview_status" // Desktop → Mobile/Web: Preview status update

	// maxMessageSize is the maximum message size allowed (512 KB)
	maxMessageSize = 512 * 1024

	// pingInterval is how often we send pings to keep connection alive
	pingInterval = 30 * time.Second

	// pingTimeout is how long we wait for pong response
	pingTimeout = 10 * time.Second

	// writeTimeout is max time to write a message
	writeTimeout = 10 * time.Second
)

// Message represents a message sent/received via WebSocket
type Message struct {
	UserID     string          `json:"user_id"`
	DeviceType string          `json:"device_type"`
	Type       MessageType     `json:"type"`
	Payload    json.RawMessage `json:"payload"`
}

// MessageHandler is called when a message is received
type MessageHandler func(msg *Message)

// Client manages the WebSocket connection to the relay server
type Client struct {
	url            string
	token          string
	userID         string
	deviceID       string
	conn           *websocket.Conn
	mu             sync.Mutex
	reconnectDelay time.Duration
	maxReconnect   time.Duration
	onMessage      MessageHandler

	// Main context (cancelled when Close() is called)
	ctx    context.Context
	cancel context.CancelFunc

	// Connection lifecycle management
	connMu       sync.Mutex    // Protects connection state transitions
	connected    atomic.Bool   // Atomic flag for connection status
	reconnecting atomic.Bool   // Prevents multiple concurrent reconnection attempts

	// Pump synchronization
	pumpCtx    context.Context
	pumpCancel context.CancelFunc
	pumpWg     sync.WaitGroup // Wait for pumps to exit before reconnecting
}

// NewClient creates a new WebSocket client
func NewClient(url, token, userID, deviceID string, onMessage MessageHandler) *Client {
	ctx, cancel := context.WithCancel(context.Background())

	return &Client{
		url:            url,
		token:          token,
		userID:         userID,
		deviceID:       deviceID,
		reconnectDelay: 1 * time.Second,  // Start with 1 second
		maxReconnect:   30 * time.Second, // Max 30 seconds (consistent with web/mobile)
		onMessage:      onMessage,
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Connect establishes a WebSocket connection
func (c *Client) Connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	return c.connectLocked()
}

// connectLocked establishes connection (must be called with connMu held)
func (c *Client) connectLocked() error {
	// Check if client is shutting down
	select {
	case <-c.ctx.Done():
		return fmt.Errorf("client shutting down")
	default:
	}

	// Cancel any existing pumps and wait for them to exit
	if c.pumpCancel != nil {
		c.pumpCancel()
		// Wait for pumps to actually exit
		// The goroutine here is intentional - it allows us to timeout the wait
		// while the WaitGroup eventually completes (pumps will exit since context is cancelled)
		waitDone := make(chan struct{})
		go func() {
			c.pumpWg.Wait()
			close(waitDone)
		}()

		select {
		case <-waitDone:
			// Pumps exited cleanly
		case <-c.ctx.Done():
			// Client is shutting down
			return fmt.Errorf("client shutting down")
		case <-time.After(2 * time.Second):
			// Timeout - pumps haven't exited yet but we'll proceed
			// The old pumps will eventually exit since their context is cancelled
			log.Println("⚠️  Timeout waiting for pumps to exit, proceeding anyway")
		}
	}

	// Close any existing connection
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "reconnecting")
		c.conn = nil
	}
	c.connected.Store(false)

	// Create new context for pumps
	c.pumpCtx, c.pumpCancel = context.WithCancel(c.ctx)

	// Build URL with auth params
	urlWithParams := fmt.Sprintf("%s?token=%s&device_type=desktop&device_id=%s", c.url, c.token, c.deviceID)

	// Dial with compression matching relay server
	conn, _, err := websocket.Dial(c.ctx, urlWithParams, &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}

	// Set read limit
	conn.SetReadLimit(maxMessageSize)

	c.conn = conn
	c.connected.Store(true)

	log.Println("✅ Connected to relay server")

	// Start pump goroutines with WaitGroup tracking
	c.pumpWg.Add(2)
	go c.readPump()
	go c.writePump()

	// Reset reconnect delay on successful connection
	c.reconnectDelay = 1 * time.Second

	return nil
}

// ConnectWithRetry connects with automatic retry logic
func (c *Client) ConnectWithRetry() {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	delay := c.reconnectDelay

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		err := c.connectLocked()
		if err == nil {
			return // Successfully connected
		}

		log.Printf("Failed to connect: %v. Retrying in %v...", err, delay)

		c.connMu.Unlock()
		time.Sleep(delay)
		c.connMu.Lock()

		// Exponential backoff
		delay = time.Duration(float64(delay) * 1.5)
		if delay > c.maxReconnect {
			delay = c.maxReconnect
		}
	}
}

// readPump reads messages from the WebSocket
func (c *Client) readPump() {
	defer c.pumpWg.Done()
	defer c.triggerReconnect("readPump exited")

	for {
		select {
		case <-c.pumpCtx.Done():
			return // Normal shutdown, no log needed
		default:
		}

		// Get connection reference safely
		c.mu.Lock()
		conn := c.conn
		c.mu.Unlock()

		if conn == nil {
			return
		}

		// Read with pump context - will be cancelled on reconnect
		_, data, err := conn.Read(c.pumpCtx)
		if err != nil {
			// Check if this is expected shutdown
			select {
			case <-c.pumpCtx.Done():
				return // Normal shutdown, no log needed
			default:
			}
			log.Printf("WebSocket read error: %v", err)
			return
		}

		// Parse message
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("Failed to parse WebSocket message: %v", err)
			continue
		}

		// Call message handler (no per-message log - too noisy)
		if c.onMessage != nil {
			c.onMessage(&msg)
		}
	}
}

// writePump handles ping/pong to keep connection alive
func (c *Client) writePump() {
	defer c.pumpWg.Done()
	defer c.triggerReconnect("writePump exited")

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.pumpCtx.Done():
			return // Normal shutdown, no log needed

		case <-ticker.C:
			// Get connection reference safely
			c.mu.Lock()
			conn := c.conn
			c.mu.Unlock()

			if conn == nil {
				return
			}

			// Send ping with timeout
			ctx, cancel := context.WithTimeout(c.pumpCtx, pingTimeout)
			err := conn.Ping(ctx)
			cancel()

			if err != nil {
				// Check if this is expected shutdown
				select {
				case <-c.pumpCtx.Done():
					return // Normal shutdown, no log needed
				default:
				}
				log.Printf("WebSocket ping failed: %v", err)
				return
			}
		}
	}
}

// triggerReconnect handles disconnection and triggers reconnect
// Uses atomic flag to ensure only ONE reconnection attempt happens
func (c *Client) triggerReconnect(reason string) {
	// Check if we're shutting down
	select {
	case <-c.ctx.Done():
		return // Shutting down, no reconnect
	default:
	}

	// Atomic check-and-set to prevent multiple concurrent reconnections
	if !c.reconnecting.CompareAndSwap(false, true) {
		return // Reconnection already in progress
	}

	log.Println("❌ Disconnected from relay server, reconnecting...")

	// Start reconnection in background
	go c.reconnectLoop()
}

// reconnectLoop handles the actual reconnection with exponential backoff
func (c *Client) reconnectLoop() {
	defer c.reconnecting.Store(false)

	delay := c.reconnectDelay
	attempt := 0

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		attempt++

		// Wait before reconnecting
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(delay):
		}

		// Attempt reconnection
		c.connMu.Lock()
		err := c.connectLocked()
		c.connMu.Unlock()

		if err == nil {
			// Success log only on reconnect (not initial connect)
			if attempt > 1 {
				log.Printf("✅ Reconnected after %d attempt(s)", attempt)
			}
			return
		}

		// Only log every 5th attempt to reduce noise during extended outages
		if attempt <= 3 || attempt%5 == 0 {
			log.Printf("Reconnection attempt %d failed: %v", attempt, err)
		}

		// Exponential backoff: 1s -> 1.5s -> 2.25s -> ... -> max 30s
		delay = time.Duration(math.Min(float64(delay)*1.5, float64(c.maxReconnect)))
	}
}

// SendMessage sends a message to the relay server
func (c *Client) SendMessage(msg *Message) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil || !c.connected.Load() {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
	defer cancel()

	return conn.Write(ctx, websocket.MessageText, data)
}

// IsConnected returns whether the client is connected
func (c *Client) IsConnected() bool {
	return c.connected.Load()
}

// Close closes the WebSocket connection
func (c *Client) Close() {
	// Cancel main context - stops all operations
	c.cancel()

	// Cancel pump context
	c.connMu.Lock()
	if c.pumpCancel != nil {
		c.pumpCancel()
	}
	c.connMu.Unlock()

	// Wait for pumps to exit
	c.pumpWg.Wait()

	// Close connection
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "client closed")
		c.conn = nil
	}
	c.connected.Store(false)
	c.mu.Unlock()
}
