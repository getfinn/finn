//go:build !windows

package devserver

import (
	"os/exec"
	"syscall"
)

// setPlatformSysProcAttr sets Unix-specific process attributes.
// On Unix, we use process groups so we can kill the entire tree.
func setPlatformSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcess gracefully terminates a process and its children.
// On Unix, we send SIGTERM to the process group.
func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// Try to kill the entire process group
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		// Negative PID means kill the process group
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}

	// Fallback: signal just the process
	return cmd.Process.Signal(syscall.SIGTERM)
}

// killProcess forcefully kills a process and its children.
// On Unix, we send SIGKILL to the process group.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	// Try to kill the entire process group
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		return syscall.Kill(-pgid, syscall.SIGKILL)
	}

	// Fallback: kill just the process
	return cmd.Process.Kill()
}
