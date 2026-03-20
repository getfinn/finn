package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/getfinn/finn/internal/agent"
)

// Version info - set by ldflags during build
var (
	Version   = "dev"
	BuildTime = "unknown"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// Parse command line flags
	wslDev := flag.Bool("wsl-dev", false, "Run in headless mode for WSL development (no GUI)")
	dev := flag.Bool("dev", false, "Run in development mode (connect to local relay server)")
	version := flag.Bool("version", false, "Print version information and exit")
	resetAuth := flag.Bool("reset-auth", false, "Clear saved auth token and re-authenticate")
	uninstall := flag.Bool("uninstall", false, "Remove all Finn data and configuration from this machine")
	flag.Parse()

	// Handle version flag
	if *version {
		fmt.Printf("Finn Desktop Daemon\n")
		fmt.Printf("  Version:    %s\n", Version)
		fmt.Printf("  Build Time: %s\n", BuildTime)
		os.Exit(0)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		if *uninstall || *resetAuth {
			log.Fatalf("Cannot determine home directory: %v", err)
		}
		log.Printf("Warning: cannot determine home directory: %v", err)
		homeDir = ""
	}
	finnDir := homeDir + "/.finn"
	configPath := finnDir + "/config.json"

	// Handle --uninstall flag
	if *uninstall {
		fmt.Println("Uninstalling Finn...")

		// Remove config directory
		if err := os.RemoveAll(finnDir); err != nil {
			fmt.Printf("  Warning: could not remove %s: %v\n", finnDir, err)
		} else {
			fmt.Printf("  Removed %s\n", finnDir)
		}

		// Remove LaunchAgent (macOS)
		launchAgent := homeDir + "/Library/LaunchAgents/com.pocketvibe.daemon.plist"
		if _, err := os.Stat(launchAgent); err == nil {
			os.Remove(launchAgent)
			fmt.Printf("  Removed %s\n", launchAgent)
		}

		// Remove logs (macOS)
		for _, logFile := range []string{
			homeDir + "/Library/Logs/pocketvibe.log",
			homeDir + "/Library/Logs/pocketvibe-error.log",
		} {
			if _, err := os.Stat(logFile); err == nil {
				os.Remove(logFile)
				fmt.Printf("  Removed %s\n", logFile)
			}
		}

		fmt.Println("\nFinn has been uninstalled.")
		fmt.Println("To also remove the binary: rm $(which finn)")
		os.Exit(0)
	}

	// Handle --reset-auth flag
	if *resetAuth {
		fmt.Println("Resetting Finn authentication...")
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			fmt.Println("  No config file found - nothing to reset.")
			os.Exit(0)
		}
		var configMap map[string]interface{}
		if jsonErr := json.Unmarshal(data, &configMap); jsonErr != nil {
			os.Remove(configPath)
			fmt.Println("  Config was corrupted - removed. Restart Finn to re-authenticate.")
			os.Exit(0)
		}
		delete(configMap, "auth_token")
		delete(configMap, "auth_tokens")
		newData, _ := json.MarshalIndent(configMap, "", "  ")
		os.WriteFile(configPath, newData, 0600)
		fmt.Println("  Auth tokens cleared. Your folders and settings are preserved.")
		fmt.Println("  Restart Finn to re-authenticate.")
		os.Exit(0)
	}

	log.Println("===========================================")
	log.Printf("   Finn Desktop Daemon %s", Version)
	log.Println("===========================================")

	if *wslDev {
		log.Println("🔧 Running in WSL development mode (headless)")
	}
	if *dev {
		log.Println("🔧 Running in development mode (local relay)")
	}

	// Create agent
	a, err := agent.New(*wslDev, *dev)
	if err != nil {
		log.Fatalf("Failed to create agent: %v", err)
	}

	// Start agent (blocks until quit)
	if err := a.Start(); err != nil {
		log.Fatalf("Failed to start agent: %v", err)
	}

	log.Println("Daemon stopped")
	os.Exit(0)
}
