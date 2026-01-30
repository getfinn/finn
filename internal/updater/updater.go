// Package updater provides automatic update functionality for the Finn daemon.
// It checks GitHub releases on startup and applies updates seamlessly.
package updater

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

const (
	// GitHub repository for releases
	repoOwner = "getfinn"
	repoName  = "finn"

	// Timeout for update check and download
	updateTimeout = 60 * time.Second
)

// Result contains information about the update check result.
type Result struct {
	Updated        bool
	CurrentVersion string
	NewVersion     string
	Error          error
}

// CheckAndUpdate checks for updates and applies them if available.
// Returns a Result indicating what happened.
//
// The update process:
// 1. Query GitHub releases for the latest version
// 2. Compare with current version
// 3. If newer, download the archive
// 4. Validate SHA256 checksum
// 5. Replace current binary
// 6. Return success (caller should restart)
func CheckAndUpdate(currentVersion string) Result {
	result := Result{CurrentVersion: currentVersion}

	// Don't check for updates in development
	if currentVersion == "dev" || currentVersion == "" {
		log.Println("Skipping update check (development mode)")
		return result
	}

	log.Printf("Checking for updates (current: %s)...", currentVersion)

	// Create updater source for GitHub
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{})
	if err != nil {
		result.Error = fmt.Errorf("failed to create GitHub source: %w", err)
		return result
	}

	// Create updater with SHA256 checksum validation
	// The validator looks for {asset}.sha256 files in the release
	updater, err := selfupdate.NewUpdater(selfupdate.Config{
		Source:    source,
		Validator: &selfupdate.ChecksumValidator{UniqueFilename: "sha256"},
	})
	if err != nil {
		result.Error = fmt.Errorf("failed to create updater: %w", err)
		return result
	}

	// Check for latest version
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeout)
	defer cancel()

	latest, found, err := updater.DetectLatest(ctx, selfupdate.NewRepositorySlug(repoOwner, repoName))
	if err != nil {
		result.Error = fmt.Errorf("failed to check for updates: %w", err)
		return result
	}

	if !found {
		log.Println("No releases found on GitHub")
		return result
	}

	// Compare versions (go-selfupdate handles semver comparison)
	if !latest.GreaterThan(currentVersion) {
		log.Printf("Already up to date (latest: %s)", latest.Version())
		return result
	}

	log.Printf("Update available: %s -> %s", currentVersion, latest.Version())
	result.NewVersion = latest.Version()

	// Get current executable path
	exe, err := os.Executable()
	if err != nil {
		result.Error = fmt.Errorf("failed to get executable path: %w", err)
		return result
	}

	// Download and apply update
	log.Printf("Downloading update...")
	if err := updater.UpdateTo(ctx, latest, exe); err != nil {
		result.Error = fmt.Errorf("failed to apply update: %w", err)
		return result
	}

	log.Printf("Update applied successfully: %s -> %s", currentVersion, latest.Version())
	result.Updated = true
	return result
}
