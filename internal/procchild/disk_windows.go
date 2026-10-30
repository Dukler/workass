package procchild

import (
	"syscall"
	"unsafe"
)

// freeDiskBytes on Windows asks GetDiskFreeSpaceExW through the stdlib's own
// lazy-DLL loader. The production machine has no package registry, so every
// platform call in this product is made this way rather than through a
// dependency.
func freeDiskBytes(dir string) (uint64, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getDiskFreeSpaceEx := kernel32.NewProc("GetDiskFreeSpaceExW")
	path, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var freeToCaller, total, free uint64
	ret, _, callErr := getDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(path)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&free)),
	)
	if ret == 0 {
		return 0, callErr
	}
	// FreeBytesAvailableToCaller, for the same reason as the unix side: a quota
	// the caller cannot exceed is not free space.
	return freeToCaller, nil
}
