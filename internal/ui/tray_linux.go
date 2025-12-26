//go:build linux

package ui

import (
	"log"

	"github.com/getfinn/finn/internal/config"
)

// TrayUI manages the system tray UI (stub for Linux/WSL)
type TrayUI struct {
	cfg            *config.Config
	isConnected    bool
	onFolderAdd    func(path string)
	onFolderRemove func(path string)
	onQuit         func()
}

// NewTrayUI creates a new system tray UI (stub for Linux/WSL)
func NewTrayUI(cfg *config.Config) *TrayUI {
	return &TrayUI{
		cfg:         cfg,
		isConnected: false,
	}
}

// SetCallbacks sets the callback functions
func (t *TrayUI) SetCallbacks(onFolderAdd, onFolderRemove func(string), onQuit func()) {
	t.onFolderAdd = onFolderAdd
	t.onFolderRemove = onFolderRemove
	t.onQuit = onQuit
}

// Start starts the system tray (no-op on Linux/WSL)
func (t *TrayUI) Start() {
	log.Println("System tray not available on Linux/WSL - running in headless mode")
	// This should never be called when headless=true, but if it is, just return
}

// UpdateConnectionStatus updates the connection status in the tray (no-op on Linux/WSL)
func (t *TrayUI) UpdateConnectionStatus(connected bool) {
	t.isConnected = connected
	// No GUI to update on Linux/WSL
}

// ShowNotification shows a system notification (no-op on Linux/WSL)
func (t *TrayUI) ShowNotification(title, message string) {
	// Just log it since we have no GUI
	log.Printf("Notification: %s - %s", title, message)
}

// SelectFolder opens a folder picker (not available on Linux/WSL)
// Returns empty string to indicate not supported
func (t *TrayUI) SelectFolder() string {
	log.Println("⚠️ Folder picker not available on Linux/WSL - use CLI to add folders")
	return ""
}
