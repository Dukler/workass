//go:build windows

package acp

import (
	"context"
	"errors"
	"strconv"
	"syscall"
	"unsafe"
)

// Resident memory is read in-process through the documented Win32 API.
// Spawning tasklist.exe every sampling interval made the unsigned daemon look
// like a process-discovery loop to endpoint protection.
var procK32GetProcessMemInfo = kernel32.NewProc("K32GetProcessMemoryInfo")

const (
	processQueryLimitedInformation = 0x1000
)

// processMemoryCounters is PROCESS_MEMORY_COUNTERS from psapi.h.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

func sampleProcessRSS(ctx context.Context, pid int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, errors.New("invalid process id")
	}
	if err := procK32GetProcessMemInfo.Find(); err != nil {
		return 0, err
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, err
	}
	defer syscall.CloseHandle(handle)
	counters := processMemoryCounters{CB: uint32(unsafe.Sizeof(processMemoryCounters{}))}
	ok, _, callErr := procK32GetProcessMemInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ok == 0 {
		if callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, errors.New("process memory counters are unavailable")
	}
	return int(counters.WorkingSetSize / 1024), nil
}

func pidString(pid int) string {
	return strconv.Itoa(pid)
}
