//go:build darwin || windows

package ui

import (
	_ "embed"
	"log"
	"os"
	"os/exec"
	"runtime"

	"github.com/getlantern/systray"
	"github.com/getfinn/finn/internal/config"
	"github.com/sqweek/dialog"
)

// Embed the tray icon at compile time
// Place your 22x22 PNG icon at internal/ui/assets/icon.png
//
//go:embed assets/icon.png
var iconData []byte

// TrayUI manages the system tray UI
type TrayUI struct {
	cfg            *config.Config
	isConnected    bool
	statusItem     *systray.MenuItem
	foldersMenu    *systray.MenuItem
	onFolderAdd    func(path string)
	onFolderRemove func(path string)
	onQuit         func()
}

// NewTrayUI creates a new system tray UI
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

// Start starts the system tray
func (t *TrayUI) Start() {
	systray.Run(t.onReady, t.onExit)
}

// onReady is called when the tray is ready
func (t *TrayUI) onReady() {
	log.Println("🎨 System tray initializing...")

	// Set the embedded icon
	systray.SetIcon(iconData)

	// Don't set title on macOS - it takes up menu bar space
	// Only the icon should show. Title is for Windows tooltip.
	if runtime.GOOS == "windows" {
		systray.SetTitle("Finn")
	}
	systray.SetTooltip("Finn - Voice-driven coding assistant")

	log.Println("✅ System tray ready - check your menu bar!")

	// Status item (disabled, just for display)
	t.statusItem = systray.AddMenuItem("Status: Connecting...", "Connection status")
	t.statusItem.Disable()

	systray.AddSeparator()

	// Dashboard link (primary action - like Tailscale)
	dashboardItem := systray.AddMenuItem("Open Dashboard", "Manage folders and view conversations")

	systray.AddSeparator()

	// Add folder (quick action)
	addFolderItem := systray.AddMenuItem("Add Project Folder...", "Select a folder to approve")

	systray.AddSeparator()

	// Quit
	quitItem := systray.AddMenuItem("Quit Finn", "Exit Finn daemon")

	// Handle events
	go func() {
		for {
			select {
			case <-addFolderItem.ClickedCh:
				t.handleAddFolder()

			case <-dashboardItem.ClickedCh:
				t.openDashboard()

			case <-quitItem.ClickedCh:
				log.Println("Quit requested from tray")
				if t.onQuit != nil {
					t.onQuit()
				}
				systray.Quit()
			}
		}
	}()
}

// onExit is called when the tray is exiting
func (t *TrayUI) onExit() {
	log.Println("System tray exiting")
}

// UpdateConnectionStatus updates the connection status in the tray
func (t *TrayUI) UpdateConnectionStatus(connected bool) {
	t.isConnected = connected

	if t.statusItem != nil {
		if connected {
			t.statusItem.SetTitle("🟢 Status: Connected!")
		} else {
			t.statusItem.SetTitle("🔴 Status: Offline")
		}
	}
}

// updateFoldersList updates the folders submenu
func (t *TrayUI) updateFoldersList() {
	// Note: systray doesn't support dynamic menu updates easily
	// We'd need to recreate the submenu or use a different approach
	// For now, this is a placeholder

	if len(t.cfg.ApprovedFolders) == 0 {
		noFoldersItem := t.foldersMenu.AddSubMenuItem("(No folders approved)", "")
		noFoldersItem.Disable()
	} else {
		for _, folder := range t.cfg.ApprovedFolders {
			folderItem := t.foldersMenu.AddSubMenuItem("✓ "+folder.Name, folder.Path)
			folderItem.Disable() // Just for display
		}
	}
}

// handleAddFolder handles adding a new folder
func (t *TrayUI) handleAddFolder() {
	// Open folder picker dialog
	folderPath, err := dialog.Directory().Title("Select Project Folder").Browse()
	if err != nil {
		if err.Error() != "Cancelled" {
			log.Printf("Error selecting folder: %v", err)
		}
		return
	}

	log.Printf("Selected folder: %s", folderPath)

	if t.onFolderAdd != nil {
		t.onFolderAdd(folderPath)
	}
}

// getDashboardURL returns the dashboard URL from environment or default
func getDashboardURL() string {
	if url := os.Getenv("FINN_DASHBOARD_URL"); url != "" {
		return url
	}
	// Default to production
	return "https://tryfinn.ai"
}

// openDashboard opens the web dashboard in the default browser
func (t *TrayUI) openDashboard() {
	url := getDashboardURL() + "/dashboard"

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		log.Printf("Unsupported platform: %s", runtime.GOOS)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Failed to open browser: %v", err)
	}
}

// ShowNotification shows a system notification (placeholder)
func (t *TrayUI) ShowNotification(title, message string) {
	// This would use platform-specific notification APIs
	// For now, just log it
	log.Printf("Notification: %s - %s", title, message)
}

// SelectFolder opens a folder picker and returns the selected path
// Returns empty string if cancelled or error
func (t *TrayUI) SelectFolder() string {
	// Open folder picker dialog
	folderPath, err := dialog.Directory().Title("Select Project Folder").Browse()
	if err != nil {
		if err.Error() != "Cancelled" {
			log.Printf("Error selecting folder: %v", err)
		}
		return ""
	}

	log.Printf("Selected folder: %s", folderPath)
	return folderPath
}
