//go:build !windows

package agent

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyShutdownSignals registers for shutdown signals.
// On Unix, we listen for both SIGINT (Ctrl+C) and SIGTERM.
func notifyShutdownSignals(c chan<- os.Signal) {
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
}
