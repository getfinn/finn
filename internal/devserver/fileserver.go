// Package devserver - fileserver.go provides static file serving for non-web projects
package devserver

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileServer represents a simple HTTP file server for static files
type FileServer struct {
	FolderID   string
	FolderPath string
	Port       int
	State      ServerState
	Error      error

	server *http.Server
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex

	onStateChange func(folderID string, state ServerState, err error)
}

// fileServers tracks active file servers
var (
	fileServers   = make(map[string]*FileServer) // folderID -> FileServer
	fileServersMu sync.RWMutex
)

// StartFileServer starts a static file server for the given folder
// This is used for non-web projects like Excel files, images, etc.
func (m *Manager) StartFileServer(folderID, folderPath string, port int) (*FileServer, error) {
	fileServersMu.Lock()

	// Check if already running
	if existing, ok := fileServers[folderID]; ok {
		existing.mu.RLock()
		state := existing.State
		existing.mu.RUnlock()

		if state == StateRunning || state == StateStarting {
			fileServersMu.Unlock()
			log.Printf("⚠️  File server already running/starting for folder %s", folderID)
			return existing, nil
		}
		// Clean up failed/stopped server
		delete(fileServers, folderID)
	}
	fileServersMu.Unlock()

	// Check if port is already in use
	if IsPortInUse(port) {
		return nil, fmt.Errorf("port %d is already in use", port)
	}

	ctx, cancel := context.WithCancel(context.Background())

	fs := &FileServer{
		FolderID:      folderID,
		FolderPath:    folderPath,
		Port:          port,
		State:         StateStarting,
		ctx:           ctx,
		cancel:        cancel,
		onStateChange: m.onStateChange,
	}

	// Create file server with CORS and proper MIME types
	fileHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Log the request
		log.Printf("📁 File server: %s %s", r.Method, r.URL.Path)

		// Set CORS headers for preview access
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Clean and validate the path
		cleanPath := filepath.Clean(r.URL.Path)
		if cleanPath == "/" || cleanPath == "." {
			cleanPath = "/"
		}

		// Get the full file path
		fullPath := filepath.Join(folderPath, cleanPath)

		// Security check: ensure path is within folder
		absFolder, _ := filepath.Abs(folderPath)
		absPath, _ := filepath.Abs(fullPath)
		if !strings.HasPrefix(absPath, absFolder) {
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}

		// Set proper MIME types for common file types
		ext := strings.ToLower(filepath.Ext(fullPath))
		switch ext {
		case ".xlsx":
			w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		case ".xls":
			w.Header().Set("Content-Type", "application/vnd.ms-excel")
		case ".csv":
			w.Header().Set("Content-Type", "text/csv")
		case ".json":
			w.Header().Set("Content-Type", "application/json")
		case ".html", ".htm":
			w.Header().Set("Content-Type", "text/html")
		case ".css":
			w.Header().Set("Content-Type", "text/css")
		case ".js":
			w.Header().Set("Content-Type", "application/javascript")
		case ".png":
			w.Header().Set("Content-Type", "image/png")
		case ".jpg", ".jpeg":
			w.Header().Set("Content-Type", "image/jpeg")
		case ".gif":
			w.Header().Set("Content-Type", "image/gif")
		case ".svg":
			w.Header().Set("Content-Type", "image/svg+xml")
		case ".pdf":
			w.Header().Set("Content-Type", "application/pdf")
		}

		// Serve the file
		http.ServeFile(w, r, fullPath)
	})

	// Create HTTP server
	fs.server = &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", port),
		Handler:      fileHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}

	// Start server in background
	go func() {
		log.Printf("📂 Starting file server on port %d for %s", port, folderPath)

		fs.mu.Lock()
		fs.State = StateRunning
		fs.mu.Unlock()

		err := fs.server.ListenAndServe()
		if err != nil && err != http.ErrServerClosed {
			fs.mu.Lock()
			fs.State = StateFailed
			fs.Error = err
			fs.mu.Unlock()
			log.Printf("❌ File server error: %v", err)
		} else {
			fs.mu.Lock()
			fs.State = StateStopped
			fs.mu.Unlock()
			log.Printf("📴 File server stopped")
		}

		fileServersMu.Lock()
		delete(fileServers, folderID)
		fileServersMu.Unlock()
	}()

	// Store the server
	fileServersMu.Lock()
	fileServers[folderID] = fs
	fileServersMu.Unlock()

	// Wait for server to be ready
	for i := 0; i < 20; i++ { // 2 seconds max
		if IsPortInUse(port) {
			log.Printf("✅ File server ready on port %d", port)
			return fs, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fs, nil
}

// StopFileServer stops a file server for the given folder
func (m *Manager) StopFileServer(folderID string) {
	fileServersMu.Lock()
	fs, ok := fileServers[folderID]
	if !ok {
		fileServersMu.Unlock()
		return
	}
	fileServersMu.Unlock()

	log.Printf("🛑 Stopping file server for folder %s", folderID)

	fs.mu.Lock()
	if fs.State == StateStopping || fs.State == StateStopped {
		fs.mu.Unlock()
		return
	}
	fs.State = StateStopping
	fs.mu.Unlock()

	// Cancel context
	if fs.cancel != nil {
		fs.cancel()
	}

	// Shutdown server gracefully
	if fs.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		fs.server.Shutdown(ctx)
	}

	fileServersMu.Lock()
	delete(fileServers, folderID)
	fileServersMu.Unlock()
}

// IsFileServerRunning checks if a file server is running for the folder
func (m *Manager) IsFileServerRunning(folderID string) bool {
	fileServersMu.RLock()
	defer fileServersMu.RUnlock()
	fs, ok := fileServers[folderID]
	if !ok {
		return false
	}
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.State == StateRunning
}

// GetFileServer returns the file server for a folder if it exists
func (m *Manager) GetFileServer(folderID string) *FileServer {
	fileServersMu.RLock()
	defer fileServersMu.RUnlock()
	return fileServers[folderID]
}

// StopAllFileServers stops all running file servers
func (m *Manager) StopAllFileServers() {
	fileServersMu.RLock()
	folderIDs := make([]string, 0, len(fileServers))
	for id := range fileServers {
		folderIDs = append(folderIDs, id)
	}
	fileServersMu.RUnlock()

	var wg sync.WaitGroup
	for _, id := range folderIDs {
		wg.Add(1)
		go func(folderID string) {
			defer wg.Done()
			m.StopFileServer(folderID)
		}(id)
	}
	wg.Wait()
}
