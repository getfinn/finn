//go:build linux

package power

import (
	"log"
	"os/exec"
)

type linuxInhibitor struct {
	cmd *exec.Cmd
}

func preventSleep(reason string) (Inhibitor, error) {
	// Use systemd-inhibit if available
	// --what=idle:sleep: prevent both idle and sleep
	// --who: application name shown in systemd
	// --why: reason shown in systemd
	// --mode=block: block sleep entirely (not just delay)
	// "sleep infinity": command that runs forever (we kill it to release)
	cmd := exec.Command("systemd-inhibit",
		"--what=idle:sleep",
		"--who=Finn",
		"--why="+reason,
		"--mode=block",
		"sleep", "infinity",
	)

	if err := cmd.Start(); err != nil {
		// systemd-inhibit might not be available (non-systemd distros)
		log.Printf("systemd-inhibit not available: %v (sleep prevention disabled)", err)
		return &noopInhibitor{}, nil
	}

	log.Printf("Started systemd-inhibit (PID %d) to prevent sleep", cmd.Process.Pid)
	return &linuxInhibitor{cmd: cmd}, nil
}

func (l *linuxInhibitor) Release() error {
	if l.cmd != nil && l.cmd.Process != nil {
		log.Printf("Stopping systemd-inhibit (PID %d)", l.cmd.Process.Pid)
		return l.cmd.Process.Kill()
	}
	return nil
}
