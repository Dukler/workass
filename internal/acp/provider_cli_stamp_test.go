package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pretendStartedFromOtherCLI makes the bridge believe it was started from a
// different executable, as if the CLI on disk was replaced since.
func pretendStartedFromOtherCLI(t *testing.T, bridge *Bridge) {
	t.Helper()
	old := filepath.Join(t.TempDir(), "old-cli")
	if err := os.WriteFile(old, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.cliStamp == nil {
		t.Fatal("a started bridge did not stamp its CLI")
	}
	bridge.cliStamp = stampProviderCLI(old)
}

func TestReplacedCLIRecyclesAnIdleEngine(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           time.Hour,
		LifecycleCheckInterval: time.Hour,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "cli-idle-tab")
	job := startAppChatJob(t, manager, session.SessionID, "cli-idle-tab", "turn on the old CLI")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateIdle, 500*time.Millisecond)

	// The same CLI on disk: nothing happens.
	manager.SweepLifecycle()
	time.Sleep(50 * time.Millisecond)
	if procHasState(manager, StateHibernated) {
		t.Fatalf("an unchanged CLI recycled the engine: %#v", manager.Processes())
	}

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("bridge missing")
	}
	pretendStartedFromOtherCLI(t, bridge)
	manager.SweepLifecycle()
	_ = waitProcState(t, manager, StateHibernated, time.Second)
}

func TestReplacedCLIWaitsForTheRunningTurn(t *testing.T) {
	t.Parallel()
	manager, events := newFakeManager(t, "slow-prompt", Options{
		HibernateTTL:           time.Hour,
		LifecycleCheckInterval: time.Hour,
		RSSSampleInterval:      time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "cli-busy-tab")
	job := startAppChatJob(t, manager, session.SessionID, "cli-busy-tab", "slow turn while the CLI is replaced")
	_ = waitProcState(t, manager, StateActive, 500*time.Millisecond)

	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{SessionID: session.SessionID})
	if bridge == nil {
		t.Fatal("bridge missing")
	}
	pretendStartedFromOtherCLI(t, bridge)
	manager.SweepLifecycle()
	if procHasState(manager, StateHibernated) {
		t.Fatalf("a replaced CLI stopped a running turn: %#v", manager.Processes())
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	_ = waitProcState(t, manager, StateHibernated, time.Second)
}

func TestProviderCLIStampFollowsAReplacedExecutable(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"1", "2"} {
		if err := os.WriteFile(filepath.Join(versions, v), []byte("cli"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	launcher := filepath.Join(dir, "cli")
	if err := os.Symlink(filepath.Join(versions, "1"), launcher); err != nil {
		t.Fatal(err)
	}
	before := stampProviderCLI(launcher)
	if !before.same(stampProviderCLI(launcher)) {
		t.Fatal("the same executable stamped differently")
	}
	// A native install swaps the launcher's symlink.
	_ = os.Remove(launcher)
	if err := os.Symlink(filepath.Join(versions, "2"), launcher); err != nil {
		t.Fatal(err)
	}
	if before.same(stampProviderCLI(launcher)) {
		t.Fatal("a swapped symlink was not seen")
	}
	// A package manager writes a new file in place, same size, same mtime.
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	first := stampProviderCLI(plain)
	staged := plain + ".new"
	if err := os.WriteFile(staged, []byte("cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staged, first.modTime, first.modTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, plain); err != nil {
		t.Fatal(err)
	}
	if first.same(stampProviderCLI(plain)) {
		t.Fatal("a rewritten file with the same size and mtime was not seen")
	}
	// A CLI missing mid-install is never a reason to recycle.
	_ = os.Remove(plain)
	if !first.same(stampProviderCLI(plain)) {
		t.Fatal("a missing CLI counted as replaced")
	}
}
