//go:build !windows

package acp

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// processTreeHandle names the provider's private process group. The native
// host (node) is its leader and the provider CLI it spawns (claude, codex, …)
// inherits it. Killing only the leader orphaned that CLI under launchd: the
// SDK's own deferred SIGTERM lives in an unref'd timer and an exit hook, and
// SIGKILL runs neither. An orphaned Claude CLI kept running its agent loop on
// background-task notifications and appended to the same session transcript
// as the next resumed attachment, forking the conversation (2026-10-10).
// Claude Code puts each background shell in a group of its own, so signalling
// this group never reaches the user's long-running commands.
type processTreeHandle struct {
	pgid int
}

// processTreeKillGrace is how long the group gets to honour SIGTERM — the
// provider CLI flushing its transcript — before survivors are SIGKILLed.
var processTreeKillGrace = 5 * time.Second

const processTreePollInterval = 20 * time.Millisecond

func startProcessTree(cmd *exec.Cmd) (processTreeHandle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return processTreeHandle{}, err
	}
	return processTreeHandle{pgid: cmd.Process.Pid}, nil
}

func stopProcessTree(process *os.Process, tree processTreeHandle) error {
	if process == nil {
		return nil
	}
	if tree.pgid > 1 {
		_ = syscall.Kill(-tree.pgid, syscall.SIGTERM)
	}
	err := process.Kill()
	terminateProcessGroup(tree)
	return err
}

// releaseProcessTree runs after the leader was reaped. Like Windows'
// KILL_ON_JOB_CLOSE it still tears down descendants, so a host that died on
// its own cannot leave its provider CLI behind either.
func releaseProcessTree(tree processTreeHandle) error {
	terminateProcessGroup(tree)
	return nil
}

// terminateProcessGroup returns only once the group is gone. Teardown is
// synchronous on purpose: a deferred SIGKILL died with the daemon when it
// exited inside the grace, and a teardown receipt published before the group
// was gone let a resumed attachment overlap the old provider CLI.
func terminateProcessGroup(tree processTreeHandle) {
	if tree.pgid <= 1 {
		return
	}
	pgid := tree.pgid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); errors.Is(err, syscall.ESRCH) {
		return
	}
	deadline := time.Now().Add(processTreeKillGrace)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(processTreePollInterval)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}
