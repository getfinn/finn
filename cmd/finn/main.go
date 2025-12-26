package main

import (
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
	flag.Parse()

	// Handle version flag
	if *version {
		fmt.Printf("Finn Desktop Daemon\n")
		fmt.Printf("  Version:    %s\n", Version)
		fmt.Printf("  Build Time: %s\n", BuildTime)
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
