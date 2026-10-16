//go:build darwin || linux

package scheduledmessage

import (
	"errors"
	"syscall"
)

func defaultPIDAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
