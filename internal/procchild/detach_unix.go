//go:build !windows

package procchild

import (
	"os"
	"syscall"
)

// LeaveCallerProcessGroup takes a `workass run` out of the group it was started
// in, which for an agent's shell is the provider's: that group goes when the
// provider does, and a provider ends with the daemon. A run with a terminal stays
// where it is, so Ctrl-C still reaches the job a person is watching.
func LeaveCallerProcessGroup() {
	if terminal, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0); err == nil {
		_ = terminal.Close()
		return
	}
	_ = syscall.Setpgid(0, 0)
}
