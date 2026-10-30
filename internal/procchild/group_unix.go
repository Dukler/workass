//go:build !windows

package procchild

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
)

// group is the platform handle for one isolated child. On unix it holds no OS
// handle at all: the isolation is the process group id, which is the child's
// own pid, and there is nothing to keep open or to leak.
type group struct {
	kind GroupKind
	unit string
	pgid int
}

// scopeAvailable, wrapForScope and scopeStop are the three Linux-only hooks.
// They are variables rather than build-tagged duplicates of this whole file so
// that the common unix path is written once; group_linux.go replaces them in
// its own init.
var (
	scopeAvailable = func() bool { return false }
	wrapForScope   = func(unit string, command []string) []string { return command }
	scopeStop      = func(unit string, kill bool) error {
		return errors.New("transient scopes are a Linux facility")
	}
)

// scopeUnitName derives the transient unit from the child id, so a human on
// the machine can find it with `systemctl --user status`.
func scopeUnitName(childID string) string {
	return "workass-run-" + strings.TrimSpace(childID)
}

// newGroup decides how this child will be isolated and rewrites argv if the
// platform needs a launcher. It runs before the process exists.
func newGroup(childID string, command []string) (*group, []string) {
	if scopeAvailable() {
		unit := scopeUnitName(childID)
		return &group{kind: GroupSystemdScope, unit: unit}, wrapForScope(unit, command)
	}
	return &group{kind: GroupProcessGroup}, command
}

// prepare puts the child in its own process group before it starts. setpgid
// happens in the child between fork and exec, so there is no window in which
// the child is in the supervisor's group.
func (g *group) prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// adopt records the group id once the child exists. Nothing is assigned here:
// unix isolation is already complete by the time the process runs.
func (g *group) adopt(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("the child did not start")
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		g.pgid = pgid
	} else {
		// setpgid makes the pid its own group leader, so the pid IS the group.
		g.pgid = cmd.Process.Pid
	}
	return nil
}

// Kind is how this child was isolated.
func (g *group) Kind() GroupKind {
	if g == nil {
		return GroupNone
	}
	return g.kind
}

// GroupID is the process group, 0 where the platform has no such number.
func (g *group) GroupID() int {
	if g == nil {
		return 0
	}
	return g.pgid
}

// stop asks the whole group to end, or kills it.
//
// The negative pid is the point: kill(-pgid) reaches every process in the
// group, which is how a shell script's children stop too. A stop that
// signalled only the pid on the record left orphans behind every time the
// child was a wrapper — npx, uv run, a Makefile — which is most of the time.
func (g *group) stop(pid int, kill bool) error {
	if g == nil {
		return errors.New("no group to stop")
	}
	if g.kind == GroupSystemdScope {
		if err := scopeStop(g.unit, kill); err == nil {
			return nil
		}
		// A scope that will not answer is still a process group underneath.
	}
	signal := syscall.SIGTERM
	if kill {
		signal = syscall.SIGKILL
	}
	if g.pgid > 1 {
		if err := syscall.Kill(-g.pgid, signal); err == nil {
			return nil
		}
	}
	if pid > 1 {
		return syscall.Kill(pid, signal)
	}
	return errors.New("no process to signal")
}

// close releases any OS handle the group held. Unix holds none.
func (g *group) close() {}

// PIDAlive reports whether one pid is still running.
//
// EPERM counts as alive: a process owned by another user answers that way, and
// reading it as dead would free a resource hold somebody is using. This is the
// rule internal/acp's external-work probe already applies, kept identical so
// the two never disagree about the same pid.
func PIDAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// DetachProcessGroup puts a supervisor this daemon starts into its own process
// group, so the job it supervises is not in the daemon's group and a signal to
// the daemon's group never reaches it. It is the same primitive newGroup uses
// for the child itself, exported so `process.start` does not reimplement the
// platform knowledge beside it.
func DetachProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
