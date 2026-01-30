// Package power provides cross-platform sleep prevention.
// When the Finn daemon is running, the system should not go to sleep.
package power

import "log"

// Inhibitor represents an active sleep prevention.
// Call Release() when the daemon is shutting down.
type Inhibitor interface {
	Release() error
}

// PreventSleep prevents the system from sleeping.
// Returns an Inhibitor that must be released when the daemon exits.
// The reason is shown in system UI (e.g., Activity Monitor on macOS).
//
// This is a global operation - the system will not sleep while the
// inhibitor is active, regardless of what tasks are running.
func PreventSleep(reason string) (Inhibitor, error) {
	inhibitor, err := preventSleep(reason)
	if err != nil {
		log.Printf("Warning: could not prevent sleep: %v", err)
		// Return a no-op inhibitor so callers don't need to check for nil
		return &noopInhibitor{}, nil
	}
	log.Printf("Sleep prevention enabled: %s", reason)
	return inhibitor, nil
}

// noopInhibitor is returned when sleep prevention is not available.
type noopInhibitor struct{}

func (n *noopInhibitor) Release() error {
	return nil
}
