//go:build windows

package power

import (
	"log"
	"syscall"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	setThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
)

type windowsInhibitor struct{}

func preventSleep(_ string) (Inhibitor, error) {
	// ES_CONTINUOUS: keep the state until explicitly cleared
	// ES_SYSTEM_REQUIRED: prevent system sleep
	// Note: Windows API doesn't support custom reason strings
	ret, _, err := setThreadExecutionState.Call(uintptr(esContinuous | esSystemRequired))

	if ret == 0 {
		log.Printf("Warning: SetThreadExecutionState failed: %v", err)
		// Don't return error - continue without sleep prevention
		return &windowsInhibitor{}, nil
	}

	log.Println("Windows sleep prevention enabled via SetThreadExecutionState")
	return &windowsInhibitor{}, nil
}

func (w *windowsInhibitor) Release() error {
	// Clear the execution state by setting only ES_CONTINUOUS
	ret, _, err := setThreadExecutionState.Call(uintptr(esContinuous))
	if ret == 0 {
		log.Printf("Warning: Failed to clear SetThreadExecutionState: %v", err)
	} else {
		log.Println("Windows sleep prevention disabled")
	}
	return nil
}
