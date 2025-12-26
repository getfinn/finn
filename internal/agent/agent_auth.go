package agent

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/getfinn/finn/internal/auth"
)

// authenticateViaOAuth starts the OAuth flow to authenticate with the dashboard.
// It opens the user's browser to the dashboard login page and waits for the
// callback with the authentication token.
func (a *Agent) authenticateViaOAuth() (string, error) {
	// Start OAuth callback server on port 47923
	oauthServer := auth.NewOAuthServer(47923)
	if err := oauthServer.Start(); err != nil {
		return "", fmt.Errorf("failed to start OAuth server: %w", err)
	}
	defer oauthServer.Stop()

	// Build auth URL - use environment variable or default to localhost for development
	dashboardURL := os.Getenv("FINN_DASHBOARD_URL")
	if dashboardURL == "" {
		dashboardURL = "http://localhost:3000"
	}
	authURL := fmt.Sprintf("%s/auth/daemon?port=47923&device_id=%s",
		dashboardURL, a.cfg.DeviceID)

	log.Printf("🌐 Opening browser for authentication...")
	log.Printf("   If browser doesn't open, visit: %s", authURL)

	// Open browser
	if err := a.openBrowser(authURL); err != nil {
		log.Printf("⚠️  Could not open browser: %v", err)
		log.Printf("   Please manually visit: %s", authURL)
	}

	// Wait for token (5 minute timeout)
	log.Println("⏳ Waiting for authentication...")
	token, err := oauthServer.WaitForToken(5 * time.Minute)
	if err != nil {
		return "", fmt.Errorf("authentication timeout: %w", err)
	}

	return token, nil
}

// openBrowser opens a URL in the default browser.
// Supports macOS, Windows, and Linux.
func (a *Agent) openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return cmd.Start()
}
