package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/getfinn/finn/internal/subscription"
)

// ExecutionMode represents how the daemon executes tasks
type ExecutionMode struct {
	InteractiveMode  bool   `json:"interactiveMode"`     // Enable interactive mode with decisions
	DiffApprovalMode string `json:"diff_approval_mode"` // "show-all", "show-on-error", "auto-approve"
}

// Config holds the daemon's configuration
type Config struct {
	UserID           string                      `json:"user_id"`
	DeviceID         string                      `json:"device_id"`
	AuthToken        string                      `json:"auth_token,omitempty"`  // Deprecated: kept for backward compatibility
	AuthTokens       map[string]string           `json:"auth_tokens,omitempty"` // New: tokens keyed by relay URL
	RelayURL         string                      `json:"-"`                     // Not saved: determined at runtime from --dev flag or env vars
	ApprovedFolders  []Folder                    `json:"approved_folders"`
	SelectedFolderID string                      `json:"selected_folder_id"`
	Subscription     *subscription.Subscription  `json:"subscription"`
	ExecutionMode    ExecutionMode               `json:"execution_mode"`
}

// Folder represents an approved project folder
type Folder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// GetToken retrieves the authentication token for the given relay URL
// Returns empty string if no token exists for this relay
func (c *Config) GetToken(relayURL string) string {
	if c.AuthTokens == nil {
		return ""
	}
	return c.AuthTokens[relayURL]
}

// SetToken stores the authentication token for the given relay URL
// This allows the daemon to maintain separate tokens for local and production relays
func (c *Config) SetToken(relayURL string, token string) {
	if c.AuthTokens == nil {
		c.AuthTokens = make(map[string]string)
	}
	c.AuthTokens[relayURL] = token
}

// Load loads the configuration from disk
func Load(dev bool) (*Config, error) {
	configPath := getConfigPath()

	// Create default config if doesn't exist
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return createDefaultConfig(dev)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Ensure subscription exists (default to standard if not set)
	if cfg.Subscription == nil {
		cfg.Subscription = subscription.NewSubscription(subscription.TierStandard)
	} else {
		// Always refresh MaxFolders based on tier (in case tier limits changed)
		cfg.Subscription.MaxFolders = subscription.GetMaxFolders(cfg.Subscription.Tier)
	}

	// Migrate folder IDs from legacy format (folder name) to UUIDs
	folderMigrated := cfg.migrateFolderIDs()

	// Migrate auth token from legacy single token to multi-token map
	tokenMigrated := cfg.migrateAuthToken()

	// Save config if any migration occurred
	if folderMigrated || tokenMigrated {
		if err := cfg.Save(); err != nil {
			// Log but don't fail - migration is best-effort
			fmt.Printf("Warning: failed to save migrated config: %v\n", err)
		}
	}

	// Override with environment variables and dev flag if set
	cfg.applyEnvironmentOverrides(dev)

	return &cfg, nil
}

// migrateFolderIDs converts legacy folder IDs (like "aoc-2024") to proper UUIDs
// Returns true if any migration occurred
func (c *Config) migrateFolderIDs() bool {
	migrated := false
	oldToNew := make(map[string]string) // Track old->new ID mapping

	for i, folder := range c.ApprovedFolders {
		// Check if ID is already a valid UUID
		if _, err := uuid.Parse(folder.ID); err != nil {
			// Not a UUID - migrate to a new UUID
			newID := uuid.New().String()
			oldToNew[folder.ID] = newID
			c.ApprovedFolders[i].ID = newID
			migrated = true
			fmt.Printf("🔄 Migrated folder ID: %s -> %s (%s)\n", folder.ID, newID, folder.Name)
		}
	}

	// Update selected folder ID if it was migrated
	if newID, ok := oldToNew[c.SelectedFolderID]; ok {
		c.SelectedFolderID = newID
	}

	return migrated
}

// migrateAuthToken converts legacy single auth_token to multi-token map
// Returns true if migration occurred
func (c *Config) migrateAuthToken() bool {
	// If already using new format, no migration needed
	if c.AuthTokens != nil && len(c.AuthTokens) > 0 {
		return false
	}

	// If no legacy token exists, nothing to migrate
	if c.AuthToken == "" {
		return false
	}

	// Migrate: move legacy token to new map
	// Legacy tokens were always for production relay (before multi-relay support)
	if c.AuthTokens == nil {
		c.AuthTokens = make(map[string]string)
	}

	// Assign to production relay (legacy default)
	productionRelay := "wss://api.tryfinn.ai/ws"
	c.AuthTokens[productionRelay] = c.AuthToken
	c.AuthToken = "" // Clear legacy field

	fmt.Printf("🔄 Migrated auth token to multi-token format (relay: %s)\n", productionRelay)
	return true
}

// applyEnvironmentOverrides applies environment variable overrides
// Since relay_url is not saved to disk, we always determine it from flags/env vars
func (c *Config) applyEnvironmentOverrides(dev bool) {
	// Always determine relay URL from environment (never use saved value)
	c.RelayURL = getDefaultRelayURL(dev)
}

// Save saves the configuration to disk
func (c *Config) Save() error {
	configPath := getConfigPath()

	// Ensure config directory exists
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0600) // 0600 = owner read/write only
}

// createDefaultConfig creates a default configuration
func createDefaultConfig(dev bool) (*Config, error) {
	// Default relay URL - production by default unless dev flag is set
	// Priority order:
	// 1. Dev flag (--dev, uses localhost)
	// 2. FINN_RELAY_URL env var
	// 3. RELAY_HOST env var (for easier domain switching)
	// 4. Hardcoded production IP (fallback)
	defaultRelayURL := getDefaultRelayURL(dev)

	cfg := &Config{
		DeviceID:        generateDeviceID(),
		RelayURL:        defaultRelayURL,
		ApprovedFolders: []Folder{},
		Subscription:    subscription.NewSubscription(subscription.TierStandard),
		ExecutionMode: ExecutionMode{
			InteractiveMode:  true,         // Default to interactive mode
			DiffApprovalMode: "show-all",   // Default to showing all diffs
		},
	}

	if err := cfg.Save(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// getConfigPath returns the path to the config file
func getConfigPath() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".finn", "config.json")
}

// generateDeviceID generates a unique device ID
func generateDeviceID() string {
	// Simple implementation - in production you might want MAC address or UUID
	hostname, _ := os.Hostname()
	return "desktop-" + hostname
}

// AddFolder adds a folder to the approved list
func (c *Config) AddFolder(name, path string) error {
	// Generate a proper UUID for the folder ID
	// This ensures uniqueness even if folder names collide (e.g., two "src" folders)
	id := uuid.New().String()

	// Check if already exists
	for _, f := range c.ApprovedFolders {
		if f.Path == path {
			return nil // Already exists, not an error
		}
	}

	// Check subscription limits
	currentCount := len(c.ApprovedFolders)
	if c.Subscription == nil {
		c.Subscription = subscription.NewSubscription(subscription.TierStandard)
	}

	if !c.Subscription.CanAddFolder(currentCount) {
		return fmt.Errorf("folder limit reached: %d/%d folders (tier: %s). Upgrade to add more",
			currentCount, c.Subscription.MaxFolders, c.Subscription.Tier)
	}

	c.ApprovedFolders = append(c.ApprovedFolders, Folder{
		ID:   id,
		Name: name,
		Path: path,
	})

	return nil
}

// RemoveFolder removes a folder from the approved list by path
func (c *Config) RemoveFolder(path string) {
	filtered := []Folder{}
	for _, f := range c.ApprovedFolders {
		if f.Path != path {
			filtered = append(filtered, f)
		}
	}
	c.ApprovedFolders = filtered

	// Clear selected folder if it was removed
	for _, f := range c.ApprovedFolders {
		if f.ID == c.SelectedFolderID {
			return // Selected folder still exists
		}
	}
	c.SelectedFolderID = "" // Selected folder was removed
}

// RemoveFolderByID removes a folder from the approved list by ID
func (c *Config) RemoveFolderByID(id string) error {
	filtered := []Folder{}
	found := false
	for _, f := range c.ApprovedFolders {
		if f.ID != id {
			filtered = append(filtered, f)
		} else {
			found = true
		}
	}

	if !found {
		return fmt.Errorf("folder with ID %s not found", id)
	}

	c.ApprovedFolders = filtered

	// Clear selected folder if it was removed
	if c.SelectedFolderID == id {
		c.SelectedFolderID = ""
	}

	return nil
}

// SelectFolder sets the selected folder ID
func (c *Config) SelectFolder(id string) error {
	// Verify folder exists
	found := false
	for _, f := range c.ApprovedFolders {
		if f.ID == id {
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("folder with ID %s not found", id)
	}

	c.SelectedFolderID = id
	return nil
}

// GetSelectedFolder returns the currently selected folder, or nil if none selected
func (c *Config) GetSelectedFolder() *Folder {
	if c.SelectedFolderID == "" {
		return nil
	}

	for _, f := range c.ApprovedFolders {
		if f.ID == c.SelectedFolderID {
			return &f
		}
	}

	return nil
}

// GetFolderByID returns a folder by its ID, or nil if not found
func (c *Config) GetFolderByID(id string) *Folder {
	for i := range c.ApprovedFolders {
		if c.ApprovedFolders[i].ID == id {
			return &c.ApprovedFolders[i]
		}
	}
	return nil
}

// IsFolderApproved checks if a folder is approved
func (c *Config) IsFolderApproved(path string) bool {
	for _, f := range c.ApprovedFolders {
		if f.Path == path {
			return true
		}
	}
	return false
}

// getDefaultRelayURL returns the default relay URL with fallback logic
func getDefaultRelayURL(dev bool) string {
	// 1. Check dev flag first (highest priority for local development)
	if dev {
		return "ws://localhost:8080/ws"
	}

	// 2. Check for full URL override (for local dev/testing)
	if url := os.Getenv("FINN_RELAY_URL"); url != "" {
		return url
	}

	// 3. Check for host-only override (easier domain switching)
	// This allows setting RELAY_HOST=relay.finn.com instead of full URL
	if host := os.Getenv("RELAY_HOST"); host != "" {
		return fmt.Sprintf("ws://%s/ws", host)
	}

	// 4. Fallback to hardcoded production relay
	return "wss://api.tryfinn.ai/ws"
}
