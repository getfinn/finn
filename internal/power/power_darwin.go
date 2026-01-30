//go:build darwin

package power

import (
	"log"
	"os/exec"
)

type darwinInhibitor struct {
	cmd *exec.Cmd
}

func preventSleep(_ string) (Inhibitor, error) {
	// caffeinate -i: prevent idle sleep
	// This runs until we kill it
	// Note: macOS caffeinate doesn't support custom reason strings
	cmd := exec.Command("caffeinate", "-i")

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	log.Printf("Started caffeinate (PID %d) to prevent sleep", cmd.Process.Pid)
	return &darwinInhibitor{cmd: cmd}, nil
}

func (d *darwinInhibitor) Release() error {
	if d.cmd != nil && d.cmd.Process != nil {
		log.Printf("Stopping caffeinate (PID %d)", d.cmd.Process.Pid)
		return d.cmd.Process.Kill()
	}
	return nil
}
