//go:build !darwin && !linux && !windows

package scheduledmessage

// defaultPIDAlive cannot observe other platforms; assume the process lives so
// a pid scheduled message waits for its expiry instead of firing early.
func defaultPIDAlive(int) bool { return true }
