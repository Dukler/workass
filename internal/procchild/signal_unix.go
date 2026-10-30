//go:build !windows

package procchild

import (
	"os/exec"
	"syscall"
)

// signalledExitCode is the shell's own answer for a child that was killed
// rather than exited: 128 + the signal number. A job killed by the OOM killer
// therefore reads 137, which is the number every log reader already knows.
func signalledExitCode(err *exec.ExitError) int {
	if status, ok := err.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return ExitSupervisorFailed
}
