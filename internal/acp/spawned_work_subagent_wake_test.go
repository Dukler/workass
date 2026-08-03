package acp

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// A coordinator must be able to end its turn after spawning a tracked subagent
// and trust that the completion comes back to it, exactly as a registered
// external lane does. These tests own that guarantee end to end: it fires when
// the chat is idle, it waits instead of interrupting a live turn, it still
// fires after that turn ends, it never fires twice, a storm collapses into one
// notice, a cancellation is never news, and a result the coordinator already
// read is not repeated back to it.

func newSubagentWakeManagerForTest(t *testing.T) *Manager {
	t.Helper()
	manager := NewManager(Options{StateDir: t.TempDir(), SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	return manager
}

type wakeRecorder struct {
	mu      sync.Mutex
	batches [][]SpawnedWorkItem
}

func (r *wakeRecorder) install(manager *Manager) {
	manager.SetSpawnedWorkWakeFunc(func(tabID, chatID string, items []SpawnedWorkItem) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.batches = append(r.batches, append([]SpawnedWorkItem(nil), items...))
		return nil
	})
}

func (r *wakeRecorder) snapshot() [][]SpawnedWorkItem {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]SpawnedWorkItem(nil), r.batches...)
}

func startTrackedSubagentForTest(manager *Manager, tabID, chatID, id, label string, startedAt time.Time) {
	manager.registerSubagentSpawnedWork(tabID, chatID, SubagentRun{
		ID: id, Label: label, ProviderID: "claude", Status: "running",
		Phase: "working", LatestActivity: "Running delegated task",
		StartedAt: startedAt.UTC().Format(time.RFC3339Nano),
	})
}

func finishTrackedSubagentForTest(manager *Manager, tabID, chatID, id, status, result string) {
	activity := map[string]string{"done": "Completed", "failed": "Failed", "cancelled": "Cancelled"}[status]
	manager.settleSubagentSpawnedWork(tabID, chatID, SubagentRun{
		ID: id, Status: status, Phase: status, LatestActivity: activity,
		ModelLabel: "Opus4.8-xhigh", Result: result,
		FinishedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func setRunningTurnForTest(manager *Manager, tabID, chatID, jobID string) {
	manager.mu.Lock()
	manager.jobs[jobID] = &Job{
		ID: jobID, Kind: "chat", Status: "running", TabID: tabID, ChatID: chatID,
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	manager.mu.Unlock()
}

func endRunningTurnForTest(manager *Manager, jobID string) {
	manager.mu.Lock()
	delete(manager.jobs, jobID)
	manager.mu.Unlock()
}

func TestTrackedSubagentCompletionWakesIdleChatExactlyOnce(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID, id = "tab-sub-wake", "chat-sub-wake", "wa-subagent-idle-1"
	recorder := &wakeRecorder{}
	recorder.install(manager)

	startTrackedSubagentForTest(manager, tabID, chatID, id, "probe subagent", time.Now().Add(-time.Minute))
	finishTrackedSubagentForTest(manager, tabID, chatID, id, "done", "the answer is 42")

	batches := recorder.snapshot()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].TaskID != id {
		t.Fatalf("idle subagent completion did not wake the owning chat exactly once: %#v", batches)
	}
	if batches[0][0].Kind != trackedSubagentSpawnedWorkKind || batches[0][0].Status != "exited" {
		t.Fatalf("woken item = %#v", batches[0][0])
	}
	item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))[id]
	if item.Wake != "delivered" {
		t.Fatalf("wake state after delivery = %#v", item)
	}

	// Every later dispatch trigger (ticker, another chat's commit, restart)
	// must not re-notify a completion that was already delivered.
	manager.dispatchSpawnedWorkWake()
	manager.dispatchSpawnedWorkWake()
	if batches := recorder.snapshot(); len(batches) != 1 {
		t.Fatalf("delivered subagent wake was re-dispatched: %#v", batches)
	}
}

func TestSpawnedWorkWakeDefersWhileTurnRunningAndFiresOnceIdle(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID = "tab-sub-defer", "chat-sub-defer"
	const subagentID, jobID = "wa-subagent-defer-1", "job-defer-1"
	recorder := &wakeRecorder{}
	recorder.install(manager)

	setRunningTurnForTest(manager, tabID, chatID, jobID)
	startTrackedSubagentForTest(manager, tabID, chatID, subagentID, "deferred subagent", time.Now().Add(-time.Minute))
	addExternalRecordForTest(manager, tabID, chatID, "xw-defer", "deferred lane", nil, "", "", "running", "")
	finishTrackedSubagentForTest(manager, tabID, chatID, subagentID, "done", "deferred result")
	settleSpawnedWorkRecordForTest(t, manager, tabID, chatID, "xw-defer", "exited")
	manager.dispatchSpawnedWorkWake()

	if batches := recorder.snapshot(); len(batches) != 0 {
		t.Fatalf("wake interrupted a live turn: %#v", batches)
	}
	items := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))
	if items[subagentID].Wake != "pending" || items["xw-defer"].Wake != "pending" {
		t.Fatalf("deferred wake was dropped instead of held pending: %#v", items)
	}

	endRunningTurnForTest(manager, jobID)
	manager.dispatchSpawnedWorkWake()

	batches := recorder.snapshot()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("deferred wake did not fire once the turn ended: %#v", batches)
	}
	items = spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))
	if items[subagentID].Wake != "delivered" || items["xw-defer"].Wake != "delivered" {
		t.Fatalf("wake state after deferred delivery = %#v", items)
	}
}

func TestTrackedSubagentWakeStormUnderOneTurnCoalescesToOneNotice(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID, jobID = "tab-sub-storm", "chat-sub-storm", "job-storm-1"
	recorder := &wakeRecorder{}
	recorder.install(manager)

	setRunningTurnForTest(manager, tabID, chatID, jobID)
	ids := []string{"wa-subagent-storm-1", "wa-subagent-storm-2", "wa-subagent-storm-3", "wa-subagent-storm-4"}
	for i, id := range ids {
		startTrackedSubagentForTest(manager, tabID, chatID, id, "fan-out "+id, time.Now().Add(-time.Duration(len(ids)-i)*time.Minute))
	}
	for _, id := range ids {
		finishTrackedSubagentForTest(manager, tabID, chatID, id, "done", "result for "+id)
	}
	endRunningTurnForTest(manager, jobID)
	manager.dispatchSpawnedWorkWake()

	batches := recorder.snapshot()
	if len(batches) != 1 || len(batches[0]) != len(ids) {
		t.Fatalf("a fan-out storm did not coalesce into one notice: %#v", batches)
	}
	seen := map[string]int{}
	for _, item := range batches[0] {
		seen[item.TaskID]++
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("subagent %s appeared %d times in the coalesced notice", id, seen[id])
		}
	}
}

func TestCancelledTrackedSubagentDoesNotWake(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID, id = "tab-sub-cancel", "chat-sub-cancel", "wa-subagent-cancel-1"
	recorder := &wakeRecorder{}
	recorder.install(manager)

	startTrackedSubagentForTest(manager, tabID, chatID, id, "cancelled subagent", time.Now().Add(-time.Minute))
	finishTrackedSubagentForTest(manager, tabID, chatID, id, "cancelled", "")

	if batches := recorder.snapshot(); len(batches) != 0 {
		t.Fatalf("a cancellation the coordinator itself caused was reported as news: %#v", batches)
	}
	item := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))[id]
	if item.Wake != "" || item.Status != "exited" || item.Summary != "Cancelled" {
		t.Fatalf("cancelled subagent row = %#v", item)
	}
}

func TestCoordinatorObservedSubagentResultCancelsDeferredWake(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID = "tab-sub-observed", "chat-sub-observed"
	const observedID, unobservedID, jobID = "wa-subagent-observed", "wa-subagent-unobserved", "job-observed-1"
	recorder := &wakeRecorder{}
	recorder.install(manager)
	bindExternalWorkOwnerForTest(manager, "owner-observed", chatID, tabID, "claude")

	setRunningTurnForTest(manager, tabID, chatID, jobID)
	for _, id := range []string{observedID, unobservedID} {
		startTrackedSubagentForTest(manager, tabID, chatID, id, id, time.Now().Add(-time.Minute))
		manager.mu.Lock()
		manager.subagents[id] = &SubagentRun{
			ID: id, Label: id, Status: "running", ProviderID: "claude",
			Adopted: true, parentChatID: chatID, parentTabID: tabID,
			StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		manager.mu.Unlock()
	}
	finishTrackedSubagentForTest(manager, tabID, chatID, observedID, "done", "observed result")
	finishTrackedSubagentForTest(manager, tabID, chatID, unobservedID, "done", "unobserved result")
	items := spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))
	if items[observedID].Wake != "pending" || items[unobservedID].Wake != "pending" {
		t.Fatalf("subagent completions under a live turn were not held pending: %#v", items)
	}

	// The coordinator reads exactly one child's terminal result inside its own
	// turn; only that one stops being news.
	manager.mu.Lock()
	manager.subagents[observedID].Status = "done"
	manager.subagents[observedID].Result = "observed result"
	manager.mu.Unlock()
	if runs := manager.ListSubagents("owner-observed", chatID, tabID); len(runs) != 2 {
		t.Fatalf("owner listing = %#v", runs)
	}

	items = spawnedWorkItemsByTaskID(manager.ListSpawnedWork(tabID, chatID))
	if items[observedID].Wake == "pending" {
		t.Fatalf("an already-read subagent result still had a pending wake: %#v", items[observedID])
	}
	if items[unobservedID].Wake != "pending" {
		t.Fatalf("an unread subagent result lost its pending wake: %#v", items[unobservedID])
	}

	endRunningTurnForTest(manager, jobID)
	manager.dispatchSpawnedWorkWake()
	batches := recorder.snapshot()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0][0].TaskID != unobservedID {
		t.Fatalf("wake after the turn ended = %#v", batches)
	}
}

func TestTrackedSubagentSettleCarriesModelAndResultOnTheWire(t *testing.T) {
	manager := newSubagentWakeManagerForTest(t)
	const tabID, chatID, id = "tab-sub-wire", "chat-sub-wire", "wa-subagent-wire-1"
	bindExternalWorkOwnerForTest(manager, "owner-wire", chatID, tabID, "claude")

	startTrackedSubagentForTest(manager, tabID, chatID, id, "wire subagent", time.Now().Add(-time.Minute))
	finishTrackedSubagentForTest(manager, tabID, chatID, id, "done", "the answer is 42 token=sk-should-not-survive")

	rows, err := manager.ListSpawnedWorkForOwner("owner-wire", chatID, tabID, chatID, tabID, 0)
	if err != nil {
		t.Fatalf("owner spawned-work listing: %v", err)
	}
	var row map[string]any
	for _, candidate := range rows {
		if asString(candidate["taskId"]) == id {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("subagent row missing from owner listing: %#v", rows)
	}
	if asString(row["modelLabel"]) != "Opus4.8-xhigh" {
		t.Fatalf("subagent row carried no model label for the woken coordinator: %#v", row)
	}
	excerpt := asString(row["resultExcerpt"])
	if !strings.Contains(excerpt, "the answer is 42") {
		t.Fatalf("subagent row carried no result excerpt: %#v", row)
	}
	if strings.Contains(excerpt, "sk-should-not-survive") || !strings.Contains(strings.ToLower(excerpt), "[redacted]") {
		t.Fatalf("subagent result excerpt was not redacted: %q", excerpt)
	}
	// Spelled as a literal, not maxSpawnedWorkResultExcerpt, so this file also
	// compiles against the pre-fix tree and can produce a fail-before receipt.
	if len(excerpt) > 600 {
		t.Fatalf("subagent result excerpt is unbounded: %d bytes", len(excerpt))
	}
}
