// Package devserver handles automatic detection and starting of local dev servers
package devserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ProjectType represents the detected project type
type ProjectType string

const (
	ProjectTypeNextJS  ProjectType = "nextjs"
	ProjectTypeVite    ProjectType = "vite"
	ProjectTypeCRA     ProjectType = "cra" // Create React App
	ProjectTypeNode    ProjectType = "node"
	ProjectTypeUnknown ProjectType = "unknown"
)

// ServerState represents the current state of a dev server
type ServerState string

const (
	StateStarting ServerState = "starting"
	StateRunning  ServerState = "running"
	StateStopping ServerState = "stopping"
	StateStopped  ServerState = "stopped"
	StateFailed   ServerState = "failed"
)

// DevServer represents a running dev server process
type DevServer struct {
	FolderID    string
	FolderPath  string
	Port        int
	ProjectType ProjectType
	State       ServerState
	Error       error
	Cmd         *exec.Cmd
	ctx         context.Context
	cancel      context.CancelFunc
	output      strings.Builder
	mu          sync.RWMutex
	onStateChange func(folderID string, state ServerState, err error)
}

// Manager manages dev server processes
type Manager struct {
	servers       map[string]*DevServer // folderID -> DevServer
	mu            sync.RWMutex
	onStateChange func(folderID string, state ServerState, err error)
}

// NewManager creates a new dev server manager
func NewManager() *Manager {
	return &Manager{
		servers: make(map[string]*DevServer),
	}
}

// SetStateChangeCallback sets a callback for state changes (useful for notifying mobile)
func (m *Manager) SetStateChangeCallback(cb func(folderID string, state ServerState, err error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onStateChange = cb
}

// PackageJSON represents the structure of package.json
type PackageJSON struct {
	Name         string            `json:"name"`
	Scripts      map[string]string `json:"scripts"`
	Dependencies map[string]string `json:"dependencies"`
	DevDeps      map[string]string `json:"devDependencies"`
}

// DetectProjectType detects the project type from the folder
func DetectProjectType(folderPath string) (ProjectType, error) {
	packageJSONPath := filepath.Join(folderPath, "package.json")

	data, err := os.ReadFile(packageJSONPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ProjectTypeUnknown, fmt.Errorf("no package.json found")
		}
		return ProjectTypeUnknown, err
	}

	var pkg PackageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ProjectTypeUnknown, err
	}

	// Check dependencies to determine project type
	allDeps := make(map[string]bool)
	for dep := range pkg.Dependencies {
		allDeps[dep] = true
	}
	for dep := range pkg.DevDeps {
		allDeps[dep] = true
	}

	// Check for Next.js
	if allDeps["next"] {
		return ProjectTypeNextJS, nil
	}

	// Check for Vite
	if allDeps["vite"] {
		return ProjectTypeVite, nil
	}

	// Check for Create React App
	if allDeps["react-scripts"] {
		return ProjectTypeCRA, nil
	}

	// Generic Node project with dev script
	if _, ok := pkg.Scripts["dev"]; ok {
		return ProjectTypeNode, nil
	}
	if _, ok := pkg.Scripts["start"]; ok {
		return ProjectTypeNode, nil
	}

	return ProjectTypeUnknown, nil
}

// CheckDependencies checks if node_modules exists
func CheckDependencies(folderPath string) error {
	nodeModulesPath := filepath.Join(folderPath, "node_modules")
	if _, err := os.Stat(nodeModulesPath); os.IsNotExist(err) {
		return fmt.Errorf("node_modules not found - run 'npm install' first")
	}
	return nil
}

// GetDevCommand returns the command to start the dev server
func GetDevCommand(projectType ProjectType, folderPath string, port int) (string, []string, error) {
	// First check for package manager (prefer npm for simplicity)
	packageManager := "npm"
	if _, err := os.Stat(filepath.Join(folderPath, "yarn.lock")); err == nil {
		packageManager = "yarn"
	} else if _, err := os.Stat(filepath.Join(folderPath, "pnpm-lock.yaml")); err == nil {
		packageManager = "pnpm"
	}

	switch projectType {
	case ProjectTypeNextJS:
		// Next.js: npm run dev -- -p PORT
		if packageManager == "npm" {
			return packageManager, []string{"run", "dev", "--", "-p", fmt.Sprintf("%d", port)}, nil
		}
		return packageManager, []string{"run", "dev", "-p", fmt.Sprintf("%d", port)}, nil

	case ProjectTypeVite:
		// Vite: npm run dev -- --port PORT
		if packageManager == "npm" {
			return packageManager, []string{"run", "dev", "--", "--port", fmt.Sprintf("%d", port)}, nil
		}
		return packageManager, []string{"run", "dev", "--port", fmt.Sprintf("%d", port)}, nil

	case ProjectTypeCRA:
		// CRA uses PORT env variable
		return packageManager, []string{"run", "start"}, nil

	case ProjectTypeNode:
		// Try dev script first, then start
		return packageManager, []string{"run", "dev"}, nil

	default:
		return "", nil, fmt.Errorf("unknown project type")
	}
}

// IsPortInUse checks if a port is already in use
func IsPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// WaitForPort waits for a port to become available with context support for cancellation
func WaitForPort(port int, timeout time.Duration) error {
	return WaitForPortWithContext(context.Background(), port, timeout)
}

// WaitForPortWithContext waits for a port with cancellation support
func WaitForPortWithContext(ctx context.Context, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if IsPortInUse(port) {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timeout waiting for port %d", port)
			}
		}
	}
}

// StartResult contains the result of starting a dev server
type StartResult struct {
	Server       *DevServer
	AlreadyRunning bool
	Error        error
}

// Start starts a dev server for the given folder
// Returns immediately - use WaitForPortWithContext to wait for it to be ready
func (m *Manager) Start(folderID, folderPath string, port int) (*DevServer, error) {
	m.mu.Lock()

	// Check if already running
	if existing, ok := m.servers[folderID]; ok {
		existing.mu.RLock()
		state := existing.State
		existing.mu.RUnlock()

		if state == StateRunning || state == StateStarting {
			m.mu.Unlock()
			log.Printf("⚠️  Dev server already running/starting for folder %s", folderID)
			return existing, nil
		}
		// Clean up failed/stopped server
		delete(m.servers, folderID)
	}
	m.mu.Unlock()

	// Check if port is already in use (maybe user started it manually)
	if IsPortInUse(port) {
		log.Printf("✅ Port %d already in use - assuming dev server is running", port)
		server := &DevServer{
			FolderID:   folderID,
			FolderPath: folderPath,
			Port:       port,
			State:      StateRunning,
		}
		m.mu.Lock()
		m.servers[folderID] = server
		m.mu.Unlock()
		return server, nil
	}

	// Check for node_modules
	if err := CheckDependencies(folderPath); err != nil {
		return nil, err
	}

	// Detect project type
	projectType, err := DetectProjectType(folderPath)
	if err != nil {
		return nil, fmt.Errorf("failed to detect project type: %w", err)
	}

	if projectType == ProjectTypeUnknown {
		return nil, fmt.Errorf("unknown project type - no package.json with dev/start script found")
	}

	log.Printf("📦 Detected project type: %s", projectType)

	// Get the command
	cmdName, args, err := GetDevCommand(projectType, folderPath, port)
	if err != nil {
		return nil, fmt.Errorf("failed to get dev command: %w", err)
	}

	log.Printf("🚀 Starting dev server: %s %s", cmdName, strings.Join(args, " "))

	// Create context with cancel
	ctx, cancel := context.WithCancel(context.Background())

	// Create command
	cmd := exec.CommandContext(ctx, cmdName, args...)
	cmd.Dir = folderPath

	// Set up process group for proper cleanup (Unix only)
	if runtime.GOOS != "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	// Set environment for CRA port
	cmd.Env = os.Environ()
	if projectType == ProjectTypeCRA {
		cmd.Env = append(cmd.Env, fmt.Sprintf("PORT=%d", port))
	}
	// Disable interactive prompts
	cmd.Env = append(cmd.Env, "CI=true")

	// Create server struct
	server := &DevServer{
		FolderID:      folderID,
		FolderPath:    folderPath,
		Port:          port,
		ProjectType:   projectType,
		State:         StateStarting,
		Cmd:           cmd,
		ctx:           ctx,
		cancel:        cancel,
		onStateChange: m.onStateChange,
	}

	// Capture output
	cmd.Stdout = &logWriter{prefix: "📤 [dev] ", server: server}
	cmd.Stderr = &logWriter{prefix: "📤 [dev] ", server: server, isErr: true}

	// Start the process
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start dev server: %w", err)
	}

	log.Printf("✅ Dev server process started (PID: %d)", cmd.Process.Pid)

	// Store the server
	m.mu.Lock()
	m.servers[folderID] = server
	m.mu.Unlock()

	// Monitor the process in background
	go m.monitorProcess(server)

	return server, nil
}

// monitorProcess watches a dev server process and handles cleanup
func (m *Manager) monitorProcess(server *DevServer) {
	err := server.Cmd.Wait()

	server.mu.Lock()
	wasRunning := server.State == StateRunning
	if err != nil {
		server.State = StateFailed
		server.Error = err
	} else {
		server.State = StateStopped
	}
	state := server.State
	server.mu.Unlock()

	m.mu.Lock()
	delete(m.servers, server.FolderID)
	m.mu.Unlock()

	if err != nil {
		log.Printf("⚠️  Dev server exited with error: %v", err)
	} else {
		log.Printf("📴 Dev server stopped")
	}

	// Notify callback if the server crashed while running (not during shutdown)
	if wasRunning && server.onStateChange != nil {
		server.onStateChange(server.FolderID, state, err)
	}
}

// Stop stops a dev server for the given folder
func (m *Manager) Stop(folderID string) {
	m.mu.Lock()
	server, ok := m.servers[folderID]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	server.mu.Lock()
	if server.State == StateStopping || server.State == StateStopped {
		server.mu.Unlock()
		return
	}
	server.State = StateStopping
	server.mu.Unlock()

	log.Printf("🛑 Stopping dev server for folder %s", folderID)

	// Cancel context first
	if server.cancel != nil {
		server.cancel()
	}

	if server.Cmd == nil || server.Cmd.Process == nil {
		return
	}

	// Try graceful shutdown first
	done := make(chan struct{})
	go func() {
		server.Cmd.Wait()
		close(done)
	}()

	// Send appropriate signal based on OS
	if runtime.GOOS == "windows" {
		// Windows: just kill it
		server.Cmd.Process.Kill()
	} else {
		// Unix: try SIGTERM to the process group first
		pgid, err := syscall.Getpgid(server.Cmd.Process.Pid)
		if err == nil {
			syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			server.Cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	// Wait up to 5 seconds for graceful shutdown
	select {
	case <-done:
		log.Printf("✅ Dev server stopped gracefully")
	case <-time.After(5 * time.Second):
		log.Printf("⚠️  Dev server didn't stop gracefully, killing...")
		if runtime.GOOS != "windows" {
			// Kill the process group
			pgid, err := syscall.Getpgid(server.Cmd.Process.Pid)
			if err == nil {
				syscall.Kill(-pgid, syscall.SIGKILL)
			}
		}
		server.Cmd.Process.Kill()
	}

	m.mu.Lock()
	delete(m.servers, folderID)
	m.mu.Unlock()
}

// StopAll stops all running dev servers
func (m *Manager) StopAll() {
	m.mu.RLock()
	folderIDs := make([]string, 0, len(m.servers))
	for id := range m.servers {
		folderIDs = append(folderIDs, id)
	}
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, id := range folderIDs {
		wg.Add(1)
		go func(folderID string) {
			defer wg.Done()
			m.Stop(folderID)
		}(id)
	}
	wg.Wait()
}

// IsRunning checks if a dev server is running for the folder
func (m *Manager) IsRunning(folderID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server, ok := m.servers[folderID]
	if !ok {
		return false
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.State == StateRunning
}

// GetState returns the current state of a dev server
func (m *Manager) GetState(folderID string) (ServerState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server, ok := m.servers[folderID]
	if !ok {
		return StateStopped, nil
	}
	server.mu.RLock()
	defer server.mu.RUnlock()
	return server.State, server.Error
}

// MarkRunning marks the server as running (call this after port is ready)
func (m *Manager) MarkRunning(folderID string) {
	m.mu.RLock()
	server, ok := m.servers[folderID]
	m.mu.RUnlock()
	if !ok {
		return
	}
	server.mu.Lock()
	if server.State == StateStarting {
		server.State = StateRunning
		log.Printf("✅ Dev server marked as running for folder %s", folderID)
	}
	server.mu.Unlock()
}

// logWriter implements io.Writer to capture dev server output
type logWriter struct {
	prefix string
	server *DevServer
	isErr  bool
}

func (w *logWriter) Write(p []byte) (n int, err error) {
	w.server.mu.Lock()
	w.server.output.Write(p)
	w.server.mu.Unlock()

	// Log first few lines to help debugging
	lines := strings.Split(string(p), "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			// Only log important lines to avoid spam
			lower := strings.ToLower(line)
			if strings.Contains(lower, "ready") ||
				strings.Contains(lower, "started") ||
				strings.Contains(lower, "compiled") ||
				strings.Contains(lower, "error") ||
				strings.Contains(lower, "failed") ||
				strings.Contains(lower, "localhost") ||
				strings.Contains(lower, "local:") ||
				strings.Contains(lower, "listening") {
				log.Printf("%s%s", w.prefix, line)
			}
		}
	}
	return len(p), nil
}
