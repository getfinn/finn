//go:build windows

package agent

import (
	"os"
	"os/signal"
)

// notifyShutdownSignals registers for shutdown signals.
// On Windows, we only have os.Interrupt (Ctrl+C).
// SIGTERM doesn't exist on Windows.
func notifyShutdownSignals(c chan<- os.Signal) {
	signal.Notify(c, os.Interrupt)
}
