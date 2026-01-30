//go:build windows

package updater

import (
	"log"
	"os"
	"os/exec"
)

// Restart starts a new instance and exits the current process.
// On Windows, we can't replace the running process, so we start a new one
// and exit. The new process inherits the same arguments.
func Restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}

	log.Printf("Starting new daemon instance: %s", exe)

	// Start new process with same arguments
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Start(); err != nil {
		return err
	}

	log.Printf("New instance started (PID %d), exiting current process", cmd.Process.Pid)

	// Exit current process - the new one takes over
	os.Exit(0)

	// Never reached
	return nil
}
