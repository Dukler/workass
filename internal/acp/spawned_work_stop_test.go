package acp

import (
	"os"
	"testing"
	"time"
)

// stopTestManager records every signal instead of sending one, so the pid set a
// stop decides on is asserted directly rather than inferred from what died.
func stopTestManager(t *testing.T, pidsByPath map[string][]int, signals *[][2]int) *Manager {
	t.Helper()
	manager := NewManager(Options{
		StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
		SpawnedWorkPIDProbe: func(paths []string) (map[string][]int, bool) {
			out := map[string][]int{}
			for _, path := range paths {
				out[path] = pidsByPath[path]
			}
			return out, true
		},
		SpawnedWorkSignal: func(pid int, force bool) bool {
			forced := 0
			if force {
				forced = 1
			}
			*signals = append(*signals, [2]int{pid, forced})
			return true
		},
	})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-stop", "chat-stop", "tab-stop", "mock")
	return manager
}

func stopTestItem(t *testing.T, manager *Manager) SpawnedWorkItem {
	t.Helper()
	items := manager.ListSpawnedWork("tab-stop", "chat-stop")
	if len(items) != 1 {
		t.Fatalf("want exactly one registered row, got %#v", items)
	}
	return items[0]
}

// The lane's processes are discovered from its output file, not from the one
// pid the registration carried: `npx expo start` registers a wrapper that binds
// nothing, and the node child that actually holds the port is only reachable
// this way. Killing the wrapper alone leaves the server running and the port
// held, which is the failure this button exists to prevent.
func TestStopSpawnedWorkSignalsEveryProcessHoldingTheLaneOpen(t *testing.T) {
	output := externalWorkTestPath(t, "lane.output")
	signals := [][2]int{}
	manager := stopTestManager(t, map[string][]int{output: {4101, 4102, 4103}}, &signals)
	pid := 4101
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-stop", ParentChatID: "chat-stop", ParentTabID: "tab-stop",
		Label: "expo start", Role: "service", PID: &pid, OutputFile: output,
	}); err != nil {
		t.Fatalf("register lane: %v", err)
	}

	item := stopTestItem(t, manager)
	reply := manager.StopSpawnedWork("tab-stop", "chat-stop", item.ID)
	if reply["ok"] != true || reply["stopped"] != true {
		t.Fatalf("stop reply = %#v", reply)
	}
	if len(signals) != 3 {
		t.Fatalf("want one signal per process holding the lane open, got %#v", signals)
	}
	seen := map[int]bool{}
	for _, signal := range signals {
		if signal[1] != 0 {
			t.Fatalf("a process that exits on SIGTERM must never be forced: %#v", signals)
		}
		seen[signal[0]] = true
	}
	// The registration's own pid is inside the probe result here; it must be
	// signalled once, not twice.
	if !seen[4101] || !seen[4102] || !seen[4103] {
		t.Fatalf("signalled pids = %#v", signals)
	}

	stopped := stopTestItem(t, manager)
	if stopped.Status != "exited" {
		t.Fatalf("a stop settles the row as exited, not %q (this card carries no failure wording)", stopped.Status)
	}
	if stopped.FinishedAt == "" || stopped.Summary == "" {
		t.Fatalf("stopped row = %#v", stopped)
	}
	// No wake: the human who pressed stop does not need the chat woken to be
	// told what they just did. Same reasoning tracked-subagent cancels follow.
	if stopped.Wake == "pending" {
		t.Fatalf("a user-initiated stop must not arm a wake: %#v", stopped)
	}
}

// The ghost this button most needs to answer: a lane whose process died without
// writing its done-file reads "running" forever. There is nothing to signal, so
// the stop is the settle — and it must still report ok, or the row stays.
func TestStopSpawnedWorkSettlesALaneWithNoLiveProcess(t *testing.T) {
	output := externalWorkTestPath(t, "ghost.output")
	signals := [][2]int{}
	manager := stopTestManager(t, map[string][]int{output: {}}, &signals)
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-stop", ParentChatID: "chat-stop", ParentTabID: "tab-stop",
		Label: "promote lane", OutputFile: output,
	}); err != nil {
		t.Fatalf("register lane: %v", err)
	}

	item := stopTestItem(t, manager)
	reply := manager.StopSpawnedWork("tab-stop", "chat-stop", item.ID)
	if reply["ok"] != true {
		t.Fatalf("stop reply = %#v", reply)
	}
	if len(signals) != 0 {
		t.Fatalf("nothing was alive; nothing may be signalled: %#v", signals)
	}
	if got := stopTestItem(t, manager); got.Status != "exited" {
		t.Fatalf("ghost row status = %q, want exited", got.Status)
	}

	// Idempotent: a second press, or a slow client still drawing the old row,
	// is not an error the user did anything about.
	again := manager.StopSpawnedWork("tab-stop", "chat-stop", item.ID)
	if again["ok"] != true || again["alreadyFinished"] != true {
		t.Fatalf("second stop = %#v", again)
	}
	if len(signals) != 0 {
		t.Fatalf("a settled row must not signal anything: %#v", signals)
	}
}

// The daemon spawns these lanes from its own session, so a probe that catches
// the daemon mid-read of the lane's output file would otherwise hand the stop
// its own pid. Killing the daemon to stop one row would take every chat with
// it, so this exclusion is the property the whole feature rests on.
func TestStopSpawnedWorkNeverSignalsTheDaemonOrInit(t *testing.T) {
	output := externalWorkTestPath(t, "self.output")
	signals := [][2]int{}
	manager := stopTestManager(t, map[string][]int{output: {os.Getpid(), os.Getppid(), 1, 0}}, &signals)
	if _, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-stop", ParentChatID: "chat-stop", ParentTabID: "tab-stop",
		Label: "lane", OutputFile: output,
	}); err != nil {
		t.Fatalf("register lane: %v", err)
	}

	item := stopTestItem(t, manager)
	if reply := manager.StopSpawnedWork("tab-stop", "chat-stop", item.ID); reply["ok"] != true {
		t.Fatalf("stop reply = %#v", reply)
	}
	if len(signals) != 0 {
		t.Fatalf("the daemon, its parent and init are never signalled: %#v", signals)
	}
}

func TestStopSpawnedWorkRejectsUnknownRows(t *testing.T) {
	signals := [][2]int{}
	manager := stopTestManager(t, map[string][]int{}, &signals)
	if reply := manager.StopSpawnedWork("tab-stop", "chat-stop", "nope"); reply["ok"] != false {
		t.Fatalf("unknown row = %#v", reply)
	}
	if reply := manager.StopSpawnedWork("", "", ""); reply["ok"] != false {
		t.Fatalf("empty request = %#v", reply)
	}
}
