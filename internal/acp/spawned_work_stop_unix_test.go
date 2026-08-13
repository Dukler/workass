//go:build darwin || linux

package acp

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The end-to-end receipt, with no probe or signal injected: a real process that
// deliberately ignores SIGTERM, discovered through the real open-file probe and
// killed for real. Everything above this test asserts the decision; this one
// asserts the effect, because a stop button that leaves the process running is
// the same lie as no button at all.
func TestStopSpawnedWorkKillsARealProcessThatIgnoresSIGTERM(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("no lsof: the open-file probe this test exercises is unavailable")
	}
	dir := t.TempDir()
	output := filepath.Join(dir, "stubborn.output")
	handle, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	// Holds the output file open (so the probe finds it) and refuses SIGTERM.
	cmd := exec.Command("/bin/sh", "-c", "trap '' TERM; echo up; while true; do sleep 1; done")
	cmd.Stdout, cmd.Stderr = handle, handle
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-stop", "chat-stop", "tab-stop", "mock")
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-stop", ParentChatID: "chat-stop", ParentTabID: "tab-stop",
		Label: "stubborn lane", OutputFile: output,
	}); err != nil {
		t.Fatalf("register lane: %v", err)
	}
	items := manager.ListSpawnedWork("tab-stop", "chat-stop")
	if len(items) != 1 {
		t.Fatalf("registered rows = %#v", items)
	}

	// Give the shell a moment to actually open the file before probing for it.
	for waited := time.Duration(0); waited < 2*time.Second; waited += 50 * time.Millisecond {
		if pids, ok := spawnedWorkPIDsForOutputs([]string{output}); ok && len(pids[output]) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	reply := manager.StopSpawnedWork("tab-stop", "chat-stop", items[0].ID)
	if reply["ok"] != true {
		t.Fatalf("stop reply = %#v", reply)
	}
	forced, _ := reply["forced"].([]int)
	if len(forced) == 0 {
		t.Fatalf("a process that ignores SIGTERM must be forced: %#v", reply)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Fatalf("reaping the stopped process: %v", err)
	}
	if externalPIDAlive(cmd.Process.Pid) {
		t.Fatalf("pid %d survived the stop", cmd.Process.Pid)
	}
	if got := manager.ListSpawnedWork("tab-stop", "chat-stop"); len(got) != 0 {
		t.Fatalf("terminal work remained in the liveness cache: %#v", got)
	}
	if got := stopTestReceipt(t, manager, items[0].ID); got.Status != "exited" {
		t.Fatalf("accepted stop receipt = %#v", got)
	}
}
