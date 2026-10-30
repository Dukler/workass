//go:build !windows

package procchild

import "syscall"

// freeDiskBytes is the space available TO THIS USER, not the raw free space:
// a filesystem with a root reserve reports more free than a non-root process
// can actually write, and a preflight that passed on the difference would be a
// preflight that let the lane die on disk anyway.
func freeDiskBytes(dir string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
