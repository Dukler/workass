//go:build windows

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processTerminate                  = 0x0001
	processSetQuota                   = 0x0100
	createSuspended                   = 0x00000004
	threadSuspendResume               = 0x0002
	threadSnapshot                    = 0x00000004
)

var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procOpenProcess              = kernel32.NewProc("OpenProcess")
	procCreateToolhelp32Snapshot = kernel32.NewProc("CreateToolhelp32Snapshot")
	procThread32First            = kernel32.NewProc("Thread32First")
	procThread32Next             = kernel32.NewProc("Thread32Next")
	procOpenThread               = kernel32.NewProc("OpenThread")
	procResumeThread             = kernel32.NewProc("ResumeThread")
)

type processTreeHandle struct {
	job syscall.Handle
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IOInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type threadEntry32 struct {
	Size           uint32
	UsageCount     uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePriority   int32
	DeltaPriority  int32
	Flags          uint32
}

// startProcessTree creates the provider suspended, assigns it to its private
// Job Object, and only then resumes its primary thread. No provider code can
// spawn a descendant in the gap before kernel tree ownership is established.
func startProcessTree(cmd *exec.Cmd) (processTreeHandle, error) {
	if cmd == nil {
		return processTreeHandle{}, fmt.Errorf("ACP command is unavailable")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createSuspended
	if err := cmd.Start(); err != nil {
		return processTreeHandle{}, err
	}
	tree, err := attachProcessTree(cmd.Process)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return processTreeHandle{}, err
	}
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		_ = stopProcessTree(cmd.Process, tree)
		_ = cmd.Wait()
		return processTreeHandle{}, err
	}
	return tree, nil
}

// attachProcessTree gives one ACP engine a kernel-owned Windows Job Object.
// Every descendant inherits membership, and closing the only Workass-owned job
// handle terminates the whole tree without launching a shell cleanup command.
func attachProcessTree(process *os.Process) (processTreeHandle, error) {
	if process == nil || process.Pid <= 0 {
		return processTreeHandle{}, fmt.Errorf("ACP process is unavailable for Windows job assignment")
	}
	jobValue, _, callErr := procCreateJobObjectW.Call(0, 0)
	if jobValue == 0 {
		return processTreeHandle{}, windowsProcessCallError("CreateJobObjectW", callErr)
	}
	job := syscall.Handle(jobValue)
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = syscall.CloseHandle(job)
		}
	}()

	limits := jobObjectExtendedLimitInfo{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	setOK, _, setErr := procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		unsafe.Sizeof(limits),
	)
	if setOK == 0 {
		return processTreeHandle{}, windowsProcessCallError("SetInformationJobObject", setErr)
	}

	processValue, _, openErr := procOpenProcess.Call(processTerminate|processSetQuota, 0, uintptr(uint32(process.Pid)))
	if processValue == 0 {
		return processTreeHandle{}, windowsProcessCallError("OpenProcess", openErr)
	}
	processHandle := syscall.Handle(processValue)
	defer syscall.CloseHandle(processHandle)
	assigned, _, assignErr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(processHandle))
	if assigned == 0 {
		return processTreeHandle{}, windowsProcessCallError("AssignProcessToJobObject", assignErr)
	}

	closeOnError = false
	return processTreeHandle{job: job}, nil
}

func resumeProcess(pid int) error {
	snapshotValue, _, snapshotErr := procCreateToolhelp32Snapshot.Call(threadSnapshot, 0)
	if snapshotValue == ^uintptr(0) {
		return windowsProcessCallError("CreateToolhelp32Snapshot", snapshotErr)
	}
	snapshot := syscall.Handle(snapshotValue)
	defer syscall.CloseHandle(snapshot)

	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	ok, _, firstErr := procThread32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		return windowsProcessCallError("Thread32First", firstErr)
	}
	for {
		if entry.OwnerProcessID == uint32(pid) {
			threadValue, _, openErr := procOpenThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
			if threadValue == 0 {
				return windowsProcessCallError("OpenThread", openErr)
			}
			thread := syscall.Handle(threadValue)
			resumed, _, resumeErr := procResumeThread.Call(uintptr(thread))
			closeErr := syscall.CloseHandle(thread)
			if resumed == ^uintptr(0) {
				return windowsProcessCallError("ResumeThread", resumeErr)
			}
			return closeErr
		}
		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		next, _, _ := procThread32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
		if next == 0 {
			break
		}
	}
	return fmt.Errorf("suspended ACP process %d has no resumable thread", pid)
}

func stopProcessTree(process *os.Process, tree processTreeHandle) error {
	if tree.job == 0 {
		if process == nil {
			return nil
		}
		return process.Kill()
	}
	terminated, _, terminateErr := procTerminateJobObject.Call(uintptr(tree.job), 1)
	closeErr := syscall.CloseHandle(tree.job)
	if terminated == 0 {
		return windowsProcessCallError("TerminateJobObject", terminateErr)
	}
	return closeErr
}

func releaseProcessTree(tree processTreeHandle) error {
	if tree.job == 0 {
		return nil
	}
	// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE makes this a descendant-tree cleanup
	// even when the root process exited before Workass observed it.
	return syscall.CloseHandle(tree.job)
}

func windowsProcessCallError(operation string, err error) error {
	if errno, ok := err.(syscall.Errno); ok && errno != 0 {
		return fmt.Errorf("%s: %w", operation, errno)
	}
	return fmt.Errorf("%s failed", operation)
}
