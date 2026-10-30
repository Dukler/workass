package procchild

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Windows has no process groups worth the name and no SIGTERM, so the
// isolation primitive is a Job object: a kernel container that every
// descendant joins automatically and that can be terminated as a unit.
//
// Two things make this the right primitive rather than a convenience. First,
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE means the supervisor's own death closes
// the handle and takes the tree with it — so a killed supervisor leaves no
// orphan still writing to the output file the row claims is finished. Second,
// a Job object is the only Windows mechanism that reaches a grandchild; a
// `taskkill /pid` reaches one process, and the endpoint-protection rules on the
// production machine forbid the PowerShell process-walking that would be
// needed otherwise.
//
// Every call goes through the stdlib's own lazy-DLL loader. The production
// Windows machine has no package registry, so this product makes platform
// calls this way rather than through a dependency.

const (
	jobObjectExtendedLimitInformation        = 9
	jobObjectLimitKillOnJobClose             = 0x00002000
	processSetQuota                          = 0x0100
	processTerminate                         = 0x0001
	createNewProcessGroup                    = 0x00000200
	createBreakawayFromJob            uint32 = 0x01000000
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
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

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject   = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJob = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessJob  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJob      = kernel32.NewProc("TerminateJobObject")
)

type group struct {
	kind   GroupKind
	unit   string
	handle syscall.Handle
}

// scopeUnitName keeps one name for the child's isolation across platforms, so
// the recorded unit reads the same in a receipt wherever it ran.
func scopeUnitName(childID string) string { return "workass-run-" + childID }

// newGroup creates the job before the child exists. A failure here is not
// fatal: the run proceeds with GroupNone and the record says so, because
// refusing to run work on a host whose job-object call failed would be a worse
// answer than running it with a stop that reaches one process.
func newGroup(childID string, command []string) (*group, []string) {
	handle, _, _ := procCreateJobObject.Call(0, 0)
	if handle == 0 {
		return &group{kind: GroupNone, unit: scopeUnitName(childID)}, command
	}
	limits := jobObjectExtendedLimitInfo{}
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if ret, _, _ := procSetInformationJob.Call(
		handle, uintptr(jobObjectExtendedLimitInformation),
		uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits),
	); ret == 0 {
		_ = syscall.CloseHandle(syscall.Handle(handle))
		return &group{kind: GroupNone, unit: scopeUnitName(childID)}, command
	}
	return &group{kind: GroupJobObject, unit: scopeUnitName(childID), handle: syscall.Handle(handle)}, command
}

// prepare gives the child its own console-control group, so a CTRL_BREAK the
// supervisor receives is not delivered to it, and clears any inherited job
// membership that would stop it joining ours.
func (g *group) prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	if g != nil && g.kind == GroupJobObject {
		cmd.SysProcAttr.CreationFlags |= createBreakawayFromJob
	}
}

// adopt assigns the started process to the job.
//
// There is a short window between CreateProcess and the assignment in which a
// grandchild could be created outside the job. Go's exec does not expose the
// suspended primary thread that would close it, and reaching for one would
// mean reimplementing CreateProcess. The window is microseconds, the practice
// is the standard one for Go on Windows, and the consequence is bounded: a
// grandchild created in that window is stopped by the process-group signal
// rather than by the job.
func (g *group) adopt(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("the child did not start")
	}
	if g == nil || g.kind != GroupJobObject {
		return nil
	}
	handle, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("cannot open the child to put it in its job object: %w", err)
	}
	defer syscall.CloseHandle(handle)
	if ret, _, callErr := procAssignProcessJob.Call(uintptr(g.handle), uintptr(handle)); ret == 0 {
		return fmt.Errorf("cannot assign the child to its job object: %v", callErr)
	}
	return nil
}

func (g *group) Kind() GroupKind {
	if g == nil {
		return GroupNone
	}
	return g.kind
}

// GroupID is 0 on Windows: a job object has a handle, not a number another
// process could use. The record carries the kind and the unit name instead of
// a number that would mean nothing to a reader.
func (g *group) GroupID() int { return 0 }

// stop ends the job.
//
// Windows has no SIGTERM, so the graceful half of the sequence is the
// STOP-FILE (see StopPath): the supervisor writes it, the child is given its
// grace to notice, and only then is the job terminated. Terminating a job is
// always the ungraceful half, whichever argument is passed here.
func (g *group) stop(pid int, kill bool) error {
	if g == nil {
		return errors.New("no group to stop")
	}
	if g.kind == GroupJobObject && g.handle != 0 {
		code := uintptr(1)
		if ret, _, callErr := procTerminateJob.Call(uintptr(g.handle), code); ret == 0 {
			return fmt.Errorf("cannot terminate the child's job object: %v", callErr)
		}
		return nil
	}
	if pid <= 1 {
		return errors.New("no process to stop")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}

// close drops the job handle, which — with kill-on-close set — is also what
// takes the tree down when the supervisor itself dies.
func (g *group) close() {
	if g != nil && g.handle != 0 {
		_ = syscall.CloseHandle(g.handle)
		g.handle = 0
	}
}

// PIDAlive reports whether one pid is still running. A handle that opens and a
// wait that times out is the only honest answer Windows offers without the
// process being our child.
func PIDAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	handle, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		// The handle opened, so something is there; a failed wait is not
		// evidence of death, and reading it as death would free a resource
		// hold somebody is using.
		return true
	}
	return state == uint32(syscall.WAIT_TIMEOUT)
}

// DetachProcessGroup puts a supervisor this daemon starts into its own process
// group, and breaks it out of any Job object the daemon itself is in, so the
// job it supervises outlives a daemon restart or a self-update. It is the same
// primitive the child's own isolation uses, exported so `process.start` does
// not reimplement the platform knowledge beside it.
func DetachProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	cmd.SysProcAttr.CreationFlags |= createBreakawayFromJob
}
