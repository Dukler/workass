//go:build !windows

package httpserve

import (
	"runtime"
	"syscall"
)

// getrusage reports Maxrss in bytes on Darwin and in kilobytes on Linux.
func peakRSSBytes() (uint64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	if usage.Maxrss <= 0 {
		return 0, false
	}
	peak := uint64(usage.Maxrss)
	if runtime.GOOS != "darwin" {
		peak *= 1024
	}
	return peak, true
}
