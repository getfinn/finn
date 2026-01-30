//go:build !windows

package updater

import (
	"log"
	"os"
	"syscall"
)

// Restart replaces the current process with a new instance.
// On Unix systems, this uses syscall.Exec which replaces the current process
// entirely - no new PID is created.
func Restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	log.Printf("Restarting daemon: %s", exe)

	// syscall.Exec replaces the current process
	// This never returns on success
	return syscall.Exec(exe, os.Args, os.Environ())
}
