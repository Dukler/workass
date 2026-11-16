package procchild

import (
	"os"
	"syscall"
)

// Windows never re-parents, so the parent pid alone would still name a caller
// that is long gone. A handle opened at start pins that exact process object,
// and a wait on it with no timeout says whether it has exited.
func watchCaller() func() bool {
	parent := os.Getppid()
	if parent <= 0 {
		return func() bool { return false }
	}
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(parent))
	if err != nil {
		return func() bool { return false }
	}
	return func() bool {
		event, err := syscall.WaitForSingleObject(handle, 0)
		return err == nil && event == syscall.WAIT_TIMEOUT
	}
}
