//go:build !windows

package acp

import (
	"bufio"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startTreeWithChildren starts `sh -c script` as a process tree. The script
// prints one pid per line for every descendant the test must observe.
func startTreeWithChildren(t *testing.T, script string, children int) (*exec.Cmd, processTreeHandle, []int) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	tree, err := startProcessTree(cmd)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)
	pids := make([]int, 0, children)
	for len(pids) < children {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read child pid: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("parse child pid %q: %v", line, err)
		}
		pids = append(pids, pid)
	}
	return cmd, tree, pids
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitProcessGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		// The test is not the parent of these grandchildren; launchd reaps them.
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d survived its process tree teardown", pid)
}

func killForCleanup(t *testing.T, pids ...int) {
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

func TestStopProcessTreeTerminatesProviderChildOfTheHost(t *testing.T) {
	// The host (sh) spawns the provider CLI (sleep) the way node spawns claude:
	// same process group, no detach. Killing the host alone used to orphan it.
	cmd, tree, pids := startTreeWithChildren(t, `sleep 60 & echo $!; wait`, 1)
	killForCleanup(t, pids...)
	if tree.pgid != cmd.Process.Pid {
		t.Fatalf("process tree pgid = %d, want host pid %d", tree.pgid, cmd.Process.Pid)
	}
	stopAndReap(t, cmd, tree)
	waitProcessGone(t, pids[0], 200*time.Millisecond)
}

// stopAndReap mirrors the bridge: its waitChild goroutine reaps the host while
// Close is inside stopProcessTree.
func stopAndReap(t *testing.T, cmd *exec.Cmd, tree processTreeHandle) {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- stopProcessTree(cmd.Process, tree) }()
	_ = cmd.Wait()
	if err := <-stopped; err != nil {
		t.Fatalf("stopProcessTree: %v", err)
	}
}

func TestReleaseProcessTreeTerminatesChildrenOfAHostThatDiedOnItsOwn(t *testing.T) {
	// The host exits first (crash, OOM kill) and leaves the provider CLI behind.
	cmd, tree, pids := startTreeWithChildren(t, `sleep 60 & echo $!`, 1)
	killForCleanup(t, pids...)
	_ = cmd.Wait()
	if !processAlive(pids[0]) {
		t.Fatal("fixture child exited before release; the test proves nothing")
	}
	if err := releaseProcessTree(tree); err != nil {
		t.Fatalf("releaseProcessTree: %v", err)
	}
	waitProcessGone(t, pids[0], 2*time.Second)
}

func TestStopProcessTreeEscalatesWhenAChildIgnoresSIGTERM(t *testing.T) {
	previous := processTreeKillGrace
	processTreeKillGrace = 100 * time.Millisecond
	t.Cleanup(func() { processTreeKillGrace = previous })
	// SIG_IGN survives exec, so the sleep ignores SIGTERM like a wedged CLI.
	cmd, tree, pids := startTreeWithChildren(t, `trap "" TERM; sleep 60 & echo $!; wait`, 1)
	killForCleanup(t, pids...)
	stopAndReap(t, cmd, tree)
	// Teardown is synchronous: when stop returns the stubborn child is gone,
	// so a daemon exiting right after cannot leave it behind (no timer to lose).
	waitProcessGone(t, pids[0], 200*time.Millisecond)
}

func TestStopProcessTreeSparesChildrenInTheirOwnProcessGroup(t *testing.T) {
	// Claude Code starts every background shell as its own group leader. A
	// training run launched from the chat must not die with the engine.
	cmd, tree, pids := startTreeWithChildren(t, `perl -e 'setpgrp(0,0); sleep 60' & echo $!; wait`, 1)
	killForCleanup(t, pids...)
	deadline := time.Now().Add(2 * time.Second)
	for {
		pgid, err := syscall.Getpgid(pids[0])
		if err == nil && pgid == pids[0] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture child never became its own group leader (pgid %d, err %v)", pgid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopAndReap(t, cmd, tree)
	time.Sleep(300 * time.Millisecond)
	if !processAlive(pids[0]) {
		t.Fatal("a child in its own process group was killed with the provider tree")
	}
}
