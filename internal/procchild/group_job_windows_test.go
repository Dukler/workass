package procchild

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// The Windows Job object, asserted on Windows. A Job object is the only
// Windows mechanism that reaches a grandchild — taskkill /pid reaches one
// process, and the production machine's endpoint-protection rules forbid the
// PowerShell process-walking that would be needed otherwise.

func TestNewGroupCreatesAJobObjectWithKillOnClose(t *testing.T) {
	group, command := newGroup("xwabc123456", []string{"cmd.exe", "/c", "echo hi"})
	t.Cleanup(group.close)
	if group.Kind() != GroupJobObject {
		t.Fatalf("isolation = %q, want a job object", group.Kind())
	}
	if group.handle == 0 {
		t.Fatal("a job-object group must hold its handle; kill-on-close is what takes the tree down with the supervisor")
	}
	if strings.Join(command, " ") != "cmd.exe /c echo hi" {
		t.Fatalf("Windows needs no launcher, so the command must be untouched, got %q", strings.Join(command, " "))
	}
	if group.GroupID() != 0 {
		t.Fatal("a job object has a handle, not a number another process could use")
	}
}

func TestPrepareFlags(t *testing.T) {
	group, _ := newGroup("xwabc123456", []string{"cmd.exe"})
	t.Cleanup(group.close)
	cmd := exec.Command("cmd.exe")
	group.prepare(cmd)
	attr := cmd.SysProcAttr
	if attr == nil || attr.CreationFlags&createNewProcessGroup == 0 {
		// Without its own console-control group, a CTRL_BREAK the supervisor
		// receives would be delivered to the child as well.
		t.Fatalf("the child must get its own process group, flags = %x", attr.CreationFlags)
	}
	if group.Kind() == GroupJobObject && attr.CreationFlags&createBreakawayFromJob == 0 {
		// An inherited job membership would stop the child joining ours.
		t.Fatalf("a job-object child must break away from an inherited job, flags = %x", attr.CreationFlags)
	}
}

func TestPIDAliveAnswersForThisProcessAndNotForAnImpossibleOne(t *testing.T) {
	if !PIDAlive(syscall.Getpid()) {
		t.Fatal("this process must read as alive")
	}
	if PIDAlive(1) {
		t.Fatal("pid 1 is never a Workass supervisor and must never read as one")
	}
}
