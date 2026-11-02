package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/procchild"
)

// Hold time on a real claim, read by a real reconcile pass. The live failure: a job
// queued on gpu0 for 2h26m was told it had held the GPU for all of it, under a
// minute after getting it, and a false stall is what sends somebody to kill a
// healthy job.

const holdTestMaxHold = 15 * time.Minute

// holdNotices is what a job has been told about its hold.
func holdNotices(lifecycle *recordedLifecycle) []ProcessLifecycleEvent {
	out := []ProcessLifecycleEvent{}
	for _, event := range lifecycle.snapshot() {
		if event.AttentionKind == "max_hold" {
			out = append(out, event)
		}
	}
	return out
}

// queueThenAcquire is a job that really waited behind another claim: it queues at
// queuedAt, and takes the hold at acquiredAt. The pid is this test's, because the
// daemon's reader probes real pids.
func queueThenAcquire(t *testing.T, stateDir, childID string, queuedAt, acquiredAt time.Time) *procchild.Lease {
	t.Helper()
	var mu sync.Mutex
	at := queuedAt
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return at
	}
	first, err := procchild.Acquire(procchild.AcquireRequest{
		StateDir: stateDir, Resource: "gpu0", ChildID: "xwaheadofit1", Poll: time.Millisecond,
		Now: func() time.Time { return queuedAt.Add(-time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan struct{})
	granted := make(chan *procchild.Lease, 1)
	go func() {
		lease, err := procchild.Acquire(procchild.AcquireRequest{
			StateDir: stateDir, Resource: "gpu0", ChildID: childID, MaxHold: holdTestMaxHold,
			Poll: time.Millisecond, Now: clock, OnQueued: func(int, int) { close(queued) },
		})
		if err != nil {
			t.Error(err)
		}
		granted <- lease
	}()
	<-queued
	mu.Lock()
	at = acquiredAt
	mu.Unlock()
	first.Release()
	lease := <-granted
	t.Cleanup(lease.Release)
	return lease
}

// seedHoldingChild registers the row a supervisor's reports would have left.
func seedHoldingChild(t *testing.T, manager *Manager, operation string, queuedAt, startedAt time.Time) string {
	t.Helper()
	childID, _ := seedProcessChild(t, manager, operation, os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.Resource, opts.MaxHoldMs = "gpu0", holdTestMaxHold.Milliseconds()
		opts.Events = []procchild.Event{
			{Seq: 1, At: queuedAt, Kind: procchild.KindCreated},
			{Seq: 2, At: queuedAt, Kind: procchild.KindQueued, Payload: map[string]any{"position": 2}},
			{Seq: 3, At: startedAt, Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4242}},
		}
	})
	return childID
}

func holdChildID(t *testing.T, operation string) string {
	t.Helper()
	childID, err := procchild.IDForOperation(operation)
	if err != nil {
		t.Fatal(err)
	}
	return childID
}

func TestAJobIsNotPastItsHoldBecauseItWaitedInTheQueue(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	acquiredAt := time.Now().UTC().Add(-time.Minute)
	queuedAt := acquiredAt.Add(-(2*time.Hour + 26*time.Minute))
	childID := holdChildID(t, "op-waited")
	queueThenAcquire(t, manager.opts.StateDir, childID, queuedAt, acquiredAt)
	seedHoldingChild(t, manager, "op-waited", queuedAt, acquiredAt)

	manager.reconcileProcessChildren(acquiredAt.Add(40 * time.Second))
	if notices := holdNotices(lifecycle); len(notices) != 0 {
		t.Fatalf("a job holding for 40s was told %q", notices[0].AttentionText)
	}
	if item := itemFor(t, manager, childID); item.Stalled {
		t.Fatal("a job within its hold reads as stalled")
	}
	queues := manager.ResourceQueues()
	if len(queues) != 1 || !queues[0].Holding || queues[0].HeldForMs > (5*time.Minute).Milliseconds() || queues[0].HeldForMs <= 0 {
		t.Fatalf("resource_queues must count from the grant, got %+v", queues)
	}
	if queues[0].Since != queuedAt.Format(time.RFC3339Nano) || queues[0].HeldSince != acquiredAt.Format(time.RFC3339Nano) {
		t.Fatalf("resource_queues must keep queue time and grant time apart, got %+v", queues[0])
	}
	if row := manager.ListProcessChildren()[0]; !row.Holding || row.QueuedMs != (2*time.Hour+26*time.Minute).Milliseconds() {
		t.Fatalf("the job's row must still show its wait, got %+v", row)
	}
}

func TestAJobThatTrulyHoldsTooLongIsToldOnceAndKeepsRunning(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	acquiredAt := time.Now().UTC().Add(-time.Minute)
	queuedAt := acquiredAt.Add(-time.Hour)
	childID := holdChildID(t, "op-held")
	queueThenAcquire(t, manager.opts.StateDir, childID, queuedAt, acquiredAt)
	seedHoldingChild(t, manager, "op-held", queuedAt, acquiredAt)

	manager.reconcileProcessChildren(acquiredAt.Add(holdTestMaxHold - time.Second))
	if notices := holdNotices(lifecycle); len(notices) != 0 {
		t.Fatalf("one second inside its hold, a job was told %q", notices[0].AttentionText)
	}
	manager.reconcileProcessChildren(acquiredAt.Add(16 * time.Minute))
	manager.reconcileProcessChildren(acquiredAt.Add(17 * time.Minute))
	notices := holdNotices(lifecycle)
	if len(notices) != 1 {
		t.Fatalf("hold notices = %d, want exactly one", len(notices))
	}
	if want := "still holding gpu0 after 16m0s, past the 15m0s it declared"; notices[0].AttentionText != want {
		t.Fatalf("notice = %q, want %q: the figure must leave out the hour it queued", notices[0].AttentionText, want)
	}
	if item := itemFor(t, manager, childID); !item.Stalled || item.Status != "running" {
		t.Fatalf("a long hold is reported, never ended, got stalled=%v status=%q", item.Stalled, item.Status)
	}
}

// A daemon restart: the claim belongs to the supervisor, so the grant is still on
// disk for the new daemon to read, and it is neither lost nor rebuilt from the queue.
func TestTheHoldClockSurvivesADaemonRestart(t *testing.T) {
	first := processChildManager(t, nil)
	stateDir := first.opts.StateDir
	acquiredAt := time.Now().UTC().Add(-time.Minute)
	queuedAt := acquiredAt.Add(-3 * time.Hour)
	childID := holdChildID(t, "op-restart-hold")
	queueThenAcquire(t, stateDir, childID, queuedAt, acquiredAt)
	seedHoldingChild(t, first, "op-restart-hold", queuedAt, acquiredAt)
	first.reconcileProcessChildren(acquiredAt.Add(time.Second))
	if err := first.persistSpawnedWorkSnapshot("tab-proc"); err != nil {
		t.Fatal(err)
	}

	restarted := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restarted.Reset() })
	bindExternalWorkOwnerForTest(restarted, "owner-proc", "chat-proc", "tab-proc", "mock")
	lifecycle := &recordedLifecycle{}
	restarted.SetProcessLifecycleObserver(lifecycle.observe)
	if item := itemFor(t, restarted, childID); item.Status != "running" || item.Stalled {
		t.Fatalf("restart changed the row: %+v", item)
	}

	restarted.reconcileProcessChildren(acquiredAt.Add(2 * time.Minute))
	if notices := holdNotices(lifecycle); len(notices) != 0 {
		t.Fatalf("after a restart, a job holding for 2m was told %q", notices[0].AttentionText)
	}
	restarted.reconcileProcessChildren(acquiredAt.Add(16 * time.Minute))
	notices := holdNotices(lifecycle)
	if len(notices) != 1 || !strings.Contains(notices[0].AttentionText, "after 16m0s") {
		t.Fatalf("a real long hold must still be reported after a restart, got %+v", notices)
	}
}

// A job started by the old binary survives an update of the daemon, with a claim
// that says only when it queued. Its command's start stands in for the grant.
func TestAClaimFromBeforeTheGrantStampIsMeasuredFromTheJobsStart(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	startedAt := time.Now().UTC().Add(-time.Minute)
	queuedAt := startedAt.Add(-2 * time.Hour)
	childID := holdChildID(t, "op-old-claim")
	dir := procchild.ResourceQueueDir(manager.opts.StateDir, "gpu0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old, err := json.Marshal(map[string]any{
		"ticket": "01", "pid": os.Getpid(), "childId": childID, "label": "old supervisor",
		"since": queuedAt.Format(time.RFC3339Nano), "maxHoldMs": holdTestMaxHold.Milliseconds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "q-01-"+childID+".claim"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	seedHoldingChild(t, manager, "op-old-claim", queuedAt, startedAt)

	manager.reconcileProcessChildren(startedAt.Add(time.Minute))
	if notices := holdNotices(lifecycle); len(notices) != 0 {
		t.Fatalf("an old claim was charged its queue time: %q", notices[0].AttentionText)
	}
	queues := manager.ResourceQueues()
	if len(queues) != 1 || queues[0].HeldSince != startedAt.Format(time.RFC3339Nano) || queues[0].HeldForMs <= 0 || queues[0].HeldForMs > (5*time.Minute).Milliseconds() {
		t.Fatalf("resource_queues must measure an old claim from the job's start, got %+v", queues)
	}
	manager.reconcileProcessChildren(startedAt.Add(16 * time.Minute))
	notices := holdNotices(lifecycle)
	if len(notices) != 1 || !strings.Contains(notices[0].AttentionText, "after 16m0s") {
		t.Fatalf("a real long hold on an old claim must still be reported, got %+v", notices)
	}
}

// The moment between reaching the head of the queue and the supervisor noticing.
func TestAHeadOfQueueWithNoStartIsNotReported(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	queuedAt := time.Now().UTC().Add(-3 * time.Hour)
	childID := holdChildID(t, "op-not-yet")
	dir := procchild.ResourceQueueDir(manager.opts.StateDir, "gpu0")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	claim, err := json.Marshal(map[string]any{
		"ticket": "01", "pid": os.Getpid(), "childId": childID,
		"since": queuedAt.Format(time.RFC3339Nano), "maxHoldMs": holdTestMaxHold.Milliseconds(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "q-01-"+childID+".claim"), claim, 0o600); err != nil {
		t.Fatal(err)
	}
	seedProcessChild(t, manager, "op-not-yet", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.Resource, opts.MaxHoldMs = "gpu0", holdTestMaxHold.Milliseconds()
		opts.Events = []procchild.Event{{Seq: 1, At: queuedAt, Kind: procchild.KindCreated}}
	})
	manager.reconcileProcessChildren(time.Now().UTC())
	if notices := holdNotices(lifecycle); len(notices) != 0 {
		t.Fatalf("a job that has not started was told %q", notices[0].AttentionText)
	}
}
