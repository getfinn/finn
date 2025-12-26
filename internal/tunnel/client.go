package tunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

// ConnectionState represents the tunnel connection state
type ConnectionState string

const (
	StateConnected    ConnectionState = "connected"
	StateReconnecting ConnectionState = "reconnecting"
	StateDisconnected ConnectionState = "disconnected"
)

// StateChangeCallback is called when connection state changes
type StateChangeCallback func(folderID string, state ConnectionState, attempt int, maxAttempts int)

// TunnelRequest represents an HTTP request from the relay
type TunnelRequest struct {
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"body"`
}

// TunnelResponse represents an HTTP response to send back
type TunnelResponse struct {
	ID         string            `json:"id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"`
	Error      string            `json:"error,omitempty"`
}

// Client manages a tunnel connection to the relay server
type Client struct {
	relayURL  string // Base relay URL (e.g., "wss://relay.finn.dev")
	token     string
	userID    string
	deviceID  string
	folderID  string
	localPort int

	conn   *websocket.Conn
	connMu sync.Mutex

	// HTTP client for local requests
	httpClient *http.Client

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// Reconnection
	autoReconnect   bool
	maxReconnects   int
	reconnectCount  int
	onStateChange   StateChangeCallback
	state           ConnectionState
	stateMu         sync.RWMutex
	reconnectCtx    context.Context
	reconnectCancel context.CancelFunc
}

// NewClient creates a new tunnel client
func NewClient(relayURL, token, userID, deviceID, folderID string, localPort int) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	reconnectCtx, reconnectCancel := context.WithCancel(ctx)

	return &Client{
		relayURL:  relayURL,
		token:     token,
		userID:    userID,
		deviceID:  deviceID,
		folderID:  folderID,
		localPort: localPort,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			// Don't follow redirects - let the client handle them
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ctx:             ctx,
		cancel:          cancel,
		done:            make(chan struct{}),
		autoReconnect:   true,  // Enable by default
		maxReconnects:   5,     // 5 attempts before giving up
		state:           StateDisconnected,
		reconnectCtx:    reconnectCtx,
		reconnectCancel: reconnectCancel,
	}
}

// SetStateChangeCallback sets a callback for connection state changes
func (c *Client) SetStateChangeCallback(cb StateChangeCallback) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.onStateChange = cb
}

// SetAutoReconnect enables or disables auto-reconnection
func (c *Client) SetAutoReconnect(enabled bool, maxAttempts int) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.autoReconnect = enabled
	if maxAttempts > 0 {
		c.maxReconnects = maxAttempts
	}
}

// setState updates the connection state and notifies callback
func (c *Client) setState(state ConnectionState, attempt int) {
	c.stateMu.Lock()
	c.state = state
	cb := c.onStateChange
	maxAttempts := c.maxReconnects
	c.stateMu.Unlock()

	if cb != nil {
		cb(c.folderID, state, attempt, maxAttempts)
	}
}

// State returns the current connection state
func (c *Client) State() ConnectionState {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

// Connect establishes the tunnel connection
func (c *Client) Connect() error {
	return c.connectInternal(false)
}

// connectInternal handles the actual connection logic
func (c *Client) connectInternal(isReconnect bool) error {
	// Build tunnel URL
	tunnelURL, err := c.buildTunnelURL()
	if err != nil {
		return fmt.Errorf("failed to build tunnel URL: %w", err)
	}

	if isReconnect {
		log.Printf("🔄 Reconnecting tunnel to %s", tunnelURL)
	} else {
		log.Printf("🔗 Connecting tunnel to %s", tunnelURL)
	}

	// Connect to relay
	conn, _, err := websocket.Dial(c.reconnectCtx, tunnelURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionContextTakeover,
	})
	if err != nil {
		return fmt.Errorf("failed to connect tunnel: %w", err)
	}

	// Set large read limit for tunnel traffic
	conn.SetReadLimit(10 * 1024 * 1024) // 10MB

	c.connMu.Lock()
	c.conn = conn
	c.done = make(chan struct{}) // Reset done channel for new connection
	c.connMu.Unlock()

	// Reset reconnect count on successful connection
	c.stateMu.Lock()
	c.reconnectCount = 0
	c.stateMu.Unlock()

	// Update state
	c.setState(StateConnected, 0)

	log.Printf("✅ Tunnel connected: folder=%s port=%d", c.folderID, c.localPort)

	// Start message pump
	go c.readPump()

	return nil
}

// buildTunnelURL constructs the tunnel WebSocket URL
func (c *Client) buildTunnelURL() (string, error) {
	// Parse base URL
	base, err := url.Parse(c.relayURL)
	if err != nil {
		return "", err
	}

	// Change scheme to ws/wss
	if base.Scheme == "https" {
		base.Scheme = "wss"
	} else if base.Scheme == "http" {
		base.Scheme = "ws"
	}

	// Set path
	base.Path = "/tunnel"

	// Add query parameters
	q := base.Query()
	q.Set("token", c.token)
	q.Set("folder_id", c.folderID)
	q.Set("device_id", c.deviceID)
	q.Set("user_id", c.userID) // Fallback for dev mode
	q.Set("local_port", fmt.Sprintf("%d", c.localPort))
	base.RawQuery = q.Encode()

	return base.String(), nil
}

// readPump reads messages from the tunnel and handles requests
func (c *Client) readPump() {
	defer close(c.done)

	for {
		select {
		case <-c.reconnectCtx.Done():
			return
		default:
		}

		c.connMu.Lock()
		conn := c.conn
		c.connMu.Unlock()

		if conn == nil {
			return
		}

		_, data, err := conn.Read(c.reconnectCtx)
		if err != nil {
			// Check if we were intentionally closed
			if c.reconnectCtx.Err() != nil {
				return
			}

			log.Printf("❌ Tunnel read error: %v", err)

			// Clear connection
			c.connMu.Lock()
			if c.conn != nil {
				c.conn.Close(websocket.StatusAbnormalClosure, "read error")
				c.conn = nil
			}
			c.connMu.Unlock()

			// Try to reconnect
			c.handleDisconnect()
			return
		}

		// Parse request
		var req TunnelRequest
		if err := json.Unmarshal(data, &req); err != nil {
			log.Printf("⚠️  Failed to parse tunnel request: %v", err)
			continue
		}

		// Handle request in goroutine (don't block the read pump)
		go c.handleRequest(req)
	}
}

// handleDisconnect attempts to reconnect with exponential backoff
func (c *Client) handleDisconnect() {
	c.stateMu.RLock()
	autoReconnect := c.autoReconnect
	maxReconnects := c.maxReconnects
	c.stateMu.RUnlock()

	if !autoReconnect {
		c.setState(StateDisconnected, 0)
		return
	}

	// Exponential backoff: 1s, 2s, 4s, 8s, 16s
	backoffs := []time.Duration{
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
	}

	for attempt := 1; attempt <= maxReconnects; attempt++ {
		// Check if we've been cancelled
		select {
		case <-c.reconnectCtx.Done():
			c.setState(StateDisconnected, attempt)
			return
		default:
		}

		// Notify that we're reconnecting
		c.setState(StateReconnecting, attempt)
		log.Printf("🔄 Reconnection attempt %d/%d for folder %s", attempt, maxReconnects, c.folderID)

		// Wait before attempting
		backoffIndex := attempt - 1
		if backoffIndex >= len(backoffs) {
			backoffIndex = len(backoffs) - 1
		}
		backoff := backoffs[backoffIndex]

		select {
		case <-c.reconnectCtx.Done():
			c.setState(StateDisconnected, attempt)
			return
		case <-time.After(backoff):
		}

		// Attempt reconnection
		err := c.connectInternal(true)
		if err == nil {
			log.Printf("✅ Tunnel reconnected successfully on attempt %d", attempt)
			return
		}

		log.Printf("⚠️  Reconnection attempt %d failed: %v", attempt, err)
	}

	// All attempts exhausted
	log.Printf("❌ Failed to reconnect tunnel after %d attempts", maxReconnects)
	c.setState(StateDisconnected, maxReconnects)
}

// handleRequest proxies an HTTP request to the local dev server
func (c *Client) handleRequest(req TunnelRequest) {
	log.Printf("📥 Tunnel request: %s %s", req.Method, req.Path)

	// Build local URL
	localURL := fmt.Sprintf("http://localhost:%d%s", c.localPort, req.Path)

	// Create HTTP request
	var bodyReader io.Reader
	if len(req.Body) > 0 {
		bodyReader = strings.NewReader(string(req.Body))
	}

	httpReq, err := http.NewRequestWithContext(c.ctx, req.Method, localURL, bodyReader)
	if err != nil {
		c.sendErrorResponse(req.ID, fmt.Sprintf("failed to create request: %v", err))
		return
	}

	// Copy headers (skip hop-by-hop headers)
	hopByHop := map[string]bool{
		"Connection":          true,
		"Keep-Alive":          true,
		"Proxy-Authenticate":  true,
		"Proxy-Authorization": true,
		"Te":                  true,
		"Trailers":            true,
		"Transfer-Encoding":   true,
		"Upgrade":             true,
	}
	for key, value := range req.Headers {
		if !hopByHop[key] {
			httpReq.Header.Set(key, value)
		}
	}

	// Make request to local server
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Provide more helpful error message for common failures
		errMsg := err.Error()
		if strings.Contains(errMsg, "context deadline exceeded") || strings.Contains(errMsg, "Timeout") {
			errMsg = fmt.Sprintf("Dev server at localhost:%d not responding - make sure it's running (npm run dev)", c.localPort)
		} else if strings.Contains(errMsg, "connection refused") {
			errMsg = fmt.Sprintf("Cannot connect to localhost:%d - dev server may not be running", c.localPort)
		}
		c.sendErrorResponse(req.ID, errMsg)
		return
	}
	defer resp.Body.Close()

	// Read response body
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024)) // 10MB limit
	if err != nil {
		c.sendErrorResponse(req.ID, fmt.Sprintf("failed to read response: %v", err))
		return
	}

	// Build response headers
	headers := make(map[string]string)
	for key, values := range resp.Header {
		if len(values) > 0 && !hopByHop[key] {
			headers[key] = values[0]
		}
	}

	// Send response
	tunnelResp := TunnelResponse{
		ID:         req.ID,
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Body:       body,
	}

	c.sendResponse(tunnelResp)

	log.Printf("📤 Tunnel response: %s %s → %d (%d bytes)",
		req.Method, req.Path, resp.StatusCode, len(body))
}

// sendResponse sends a response back through the tunnel
func (c *Client) sendResponse(resp TunnelResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		log.Printf("⚠️  Failed to marshal tunnel response: %v", err)
		return
	}

	c.connMu.Lock()
	defer c.connMu.Unlock()

	if c.conn == nil {
		log.Printf("⚠️  Tunnel connection closed, cannot send response")
		return
	}

	ctx, cancel := context.WithTimeout(c.reconnectCtx, 10*time.Second)
	defer cancel()

	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		log.Printf("❌ Failed to send tunnel response: %v", err)
	}
}

// sendErrorResponse sends an error response back through the tunnel
func (c *Client) sendErrorResponse(requestID, errorMsg string) {
	log.Printf("⚠️  Tunnel error for %s: %s", requestID, errorMsg)

	c.sendResponse(TunnelResponse{
		ID:         requestID,
		StatusCode: 502,
		Error:      errorMsg,
	})
}

// Close closes the tunnel connection
func (c *Client) Close() {
	// Cancel reconnection attempts first
	c.reconnectCancel()
	c.cancel()

	c.connMu.Lock()
	if c.conn != nil {
		c.conn.Close(websocket.StatusNormalClosure, "tunnel closing")
		c.conn = nil
	}
	c.connMu.Unlock()

	// Wait for read pump to finish
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		log.Printf("⚠️  Tunnel close timed out")
	}

	// Update state (don't notify - this is intentional close)
	c.stateMu.Lock()
	c.state = StateDisconnected
	c.stateMu.Unlock()

	log.Printf("🔌 Tunnel closed: folder=%s", c.folderID)
}

// IsConnected returns whether the tunnel is connected
func (c *Client) IsConnected() bool {
	c.connMu.Lock()
	defer c.connMu.Unlock()
	return c.conn != nil
}

// FolderID returns the folder ID this tunnel is for
func (c *Client) FolderID() string {
	return c.folderID
}

// LocalPort returns the local port this tunnel proxies to
func (c *Client) LocalPort() int {
	return c.localPort
}
