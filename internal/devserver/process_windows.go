//go:build windows

package devserver

import (
	"os/exec"
)

// setPlatformSysProcAttr sets Windows-specific process attributes.
// Windows doesn't have process groups like Unix, so this is a no-op.
func setPlatformSysProcAttr(cmd *exec.Cmd) {
	// No-op on Windows
	// Note: For better child process handling on Windows, we could use
	// CREATE_NEW_PROCESS_GROUP or Job Objects in the future.
}

// terminateProcess gracefully terminates a process.
// On Windows, we just kill the process directly.
// TODO: Consider using taskkill /T for tree termination.
func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killProcess forcefully kills a process.
// On Windows, this is the same as terminateProcess.
func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
