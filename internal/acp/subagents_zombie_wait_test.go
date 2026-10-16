package acp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestTrackedSubagentZombieWaitStillDeliversCompletion reproduces the prod
// swallowed-wake incident (2026-10-16, chat "forking + subsessions"): the parent turn's background tool
// shell issued workass_wait_subagent with timeout_ms -1 as its turn's last
// act, the parent turn then ended normally, and the surviving zombie waiter
// silently consumed the child's settle so the parent chat was never woken
// even though the receipt was durably written. A waiter registered from a
// turn that has since settled must neither block nor consume the completion
// notice.
func TestTrackedSubagentZombieWaitStillDeliversCompletion(t *testing.T) {
	t.Parallel()
	manager, _, session, ownerKey, root, _ := newSubagentLifecycleFixture(t, "zombie-wait-delivers")
	calls := 0
	if err := manager.InstallSubagentCompletionObserver(func(tabID, chatID string, receipt SubagentReceipt) error {
		calls++
		if tabID != session.TabID || chatID != session.ChatID {
			t.Errorf("completion delivered to wrong owner: tab=%q chat=%q", tabID, chatID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	job := startSubagentParent(t, manager, session, root, "[mock:slow] zombie wait repro parent")
	parentJobID := asString(job["id"])
	if parentJobID == "" {
		t.Fatal("started parent job has no id")
	}

	run := &SubagentRun{
		ID:           "zombie-wait-child",
		Label:        "zombie-wait-child",
		Status:       "running",
		ProviderID:   session.Info.ProviderID,
		ParentJobID:  parentJobID,
		Adopted:      true,
		parentChatID: session.ChatID,
		parentTabID:  session.TabID,
		done:         make(chan struct{}),
	}
	manager.mu.Lock()
	manager.subagents[run.ID] = run
	manager.mu.Unlock()

	// Hold the exact event-only wait the prod background shell used as the
	// parent turn's last act, and require it to register while the parent
	// turn is still running so the waiter really is a turn-owned zombie once
	// the turn ends.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan SubagentRun, 1)
	waitErr := make(chan error, 1)
	go func() {
		snapshot, err := manager.WaitSubagent(ctx, ownerKey, session.ChatID, session.TabID, run.ID, -1)
		if err != nil {
			waitErr <- err
			return
		}
		waitDone <- snapshot
	}()
	registerDeadline := time.Now().Add(10 * time.Second)
	for {
		time.Sleep(10 * time.Millisecond)
		manager.mu.Lock()
		registered := run.completionWaiters == 1
		turnRunning := manager.jobs[parentJobID] != nil && manager.jobs[parentJobID].Status == "running"
		manager.mu.Unlock()
		if registered {
			if !turnRunning {
				t.Fatal("zombie wait registered after the parent turn already ended; reproduce requires the wait to race the live turn")
			}
			break
		}
		if time.Now().After(registerDeadline) {
			t.Fatal("subagent wait never registered within the deadline")
		}
	}

	// The parent turn settles normally, which is exactly when prod deleted
	// the job from m.jobs and left the background-shell waiter behind.
	endDeadline := time.Now().Add(10 * time.Second)
	for {
		manager.mu.Lock()
		ended := manager.jobs[parentJobID] == nil
		manager.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(endDeadline) {
			t.Fatal("parent turn did not end within the deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The child settles after the parent turn is gone. The zombie waiter is
	// still latched, so the settle must still be delivered exactly once and
	// must not stay pending.
	finishDone := make(chan struct{})
	go func() {
		manager.finishSubagent(run, nil, nil)
		close(finishDone)
	}()
	select {
	case <-finishDone:
	case <-time.After(5 * time.Second):
		t.Fatal("finishSubagent did not return after the zombie-wait child settled")
	}
	select {
	case snapshot := <-waitDone:
		if snapshot.ID != run.ID || snapshot.Status != "done" {
			t.Fatalf("zombie wait returned unexpected snapshot: %#v", snapshot)
		}
	case err := <-waitErr:
		t.Fatalf("zombie wait failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("zombie wait did not return after the child settled")
	}
	if calls != 1 || len(manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("zombie wait swallowed the settle notice: calls=%d pending=%#v", calls, manager.pendingSubagentCompletions())
	}
}

// zombieWaitHarness drives WaitSubagent against hand-made turns so a test can
// decide exactly which parent turn is alive when each waiter registers and
// returns. Fake turns are plain running jobs in m.jobs, which is all the
// waiter bookkeeping looks at.
type zombieWaitHarness struct {
	t        *testing.T
	manager  *Manager
	session  subagentParentSession
	ownerKey string
	run      *SubagentRun
	calls    atomic.Int32
}

func newZombieWaitHarness(t *testing.T, name string) *zombieWaitHarness {
	t.Helper()
	manager, _, session, ownerKey, _, _ := newSubagentLifecycleFixture(t, name)
	h := &zombieWaitHarness{t: t, manager: manager, session: session, ownerKey: ownerKey}
	if err := manager.InstallSubagentCompletionObserver(func(tabID, chatID string, receipt SubagentReceipt) error {
		h.calls.Add(1)
		if tabID != session.TabID || chatID != session.ChatID {
			t.Errorf("completion delivered to wrong owner: tab=%q chat=%q", tabID, chatID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.run = &SubagentRun{
		ID: name + "-child", Label: name + "-child", Status: "running",
		ProviderID: session.Info.ProviderID, Adopted: true,
		parentChatID: session.ChatID, parentTabID: session.TabID,
		done: make(chan struct{}),
	}
	manager.mu.Lock()
	manager.subagents[h.run.ID] = h.run
	manager.mu.Unlock()
	return h
}

func (h *zombieWaitHarness) startTurn(id string) {
	h.manager.mu.Lock()
	h.manager.jobs[id] = &Job{ID: id, ChatID: h.session.ChatID, TabID: h.session.TabID, Status: "running"}
	h.manager.mu.Unlock()
	h.t.Cleanup(func() { h.endTurn(id) })
}

func (h *zombieWaitHarness) endTurn(id string) {
	h.manager.mu.Lock()
	delete(h.manager.jobs, id)
	h.manager.mu.Unlock()
}

// waitAsync runs a WaitSubagent and returns once it has registered as a
// waiter, so the registering turn is the one that was running at that point.
func (h *zombieWaitHarness) waitAsync(timeout time.Duration) <-chan error {
	h.t.Helper()
	h.manager.mu.Lock()
	before := h.run.completionWaiters
	h.manager.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := h.manager.WaitSubagent(context.Background(), h.ownerKey, h.session.ChatID, h.session.TabID, h.run.ID, timeout)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.manager.mu.Lock()
		registered := h.run.completionWaiters > before
		h.manager.mu.Unlock()
		if registered {
			return done
		}
		select {
		case err := <-done:
			h.t.Fatalf("wait returned before registering: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			h.t.Fatal("subagent wait never registered within the deadline")
		}
	}
}

func (h *zombieWaitHarness) finish() {
	h.t.Helper()
	finished := make(chan struct{})
	go func() {
		h.manager.finishSubagent(h.run, nil, nil)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		h.t.Fatal("finishSubagent did not return")
	}
}

func (h *zombieWaitHarness) requireReturned(name string, done <-chan error) {
	h.t.Helper()
	select {
	case err := <-done:
		if err != nil {
			h.t.Fatalf("%s failed: %v", name, err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatalf("%s did not return after the child settled", name)
	}
}

func (h *zombieWaitHarness) requireNoWaiters() {
	h.t.Helper()
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if h.run.completionWaiters != 0 || len(h.run.waiterTurns) != 0 {
		h.t.Fatalf("waiter bookkeeping leaked: waiters=%d turns=%#v", h.run.completionWaiters, h.run.waiterTurns)
	}
}

// An expired live-turn wait must release its own turn attribution, otherwise
// the stale entry keeps the next turn's gate shut forever.
func TestSubagentWaitTimeoutReleasesItsOwnTurnAttribution(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-timeout-release")
	h.startTurn("turn-live")
	if err := <-h.waitAsync(30 * time.Millisecond); err == nil {
		t.Fatal("expected the timed wait to expire")
	}
	h.requireNoWaiters()

	ids := []string{h.run.ID}
	result, err := h.manager.WaitSubagents(context.Background(), h.ownerKey, h.session.ChatID, h.session.TabID, ids, "all", 30*time.Millisecond)
	if err != nil || result["timedOut"] != true {
		t.Fatalf("expected the fan-out wait to time out: result=%#v err=%v", result, err)
	}
	h.requireNoWaiters()
	h.finish() // settle the child so fixture cleanup does not wait on it
}

// A zombie from an ended turn plus a later turn's expired wait: the expired
// wait's entry must not make the zombie look live, so the notice is delivered
// exactly once and the zombie consumes nothing.
func TestTrackedSubagentZombieWaitWithExpiredLiveWaitStillDelivers(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-expired-live")
	h.startTurn("turn-old")
	zombie := h.waitAsync(-1)
	h.endTurn("turn-old")
	h.startTurn("turn-new")
	if err := <-h.waitAsync(30 * time.Millisecond); err == nil {
		t.Fatal("expected the timed wait to expire")
	}

	h.finish()
	h.requireReturned("zombie wait", zombie)
	if got := h.calls.Load(); got != 1 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("expected exactly one delivered notice: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.requireNoWaiters()
}

// Consumption is per waiter: with a zombie and a live waiter both registered,
// the live waiter's result reaches the model, so no notice is delivered.
func TestTrackedSubagentLiveWaiterConsumesEvenWithZombiePresent(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-live-consumes")
	h.startTurn("turn-old")
	zombie := h.waitAsync(-1)
	h.endTurn("turn-old")
	h.startTurn("turn-new")
	live := h.waitAsync(-1)

	h.finish()
	h.requireReturned("live wait", live)
	h.requireReturned("zombie wait", zombie)
	if got := h.calls.Load(); got != 0 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("live waiter should have consumed the notice: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.requireNoWaiters()
}

// The plain foreground wait keeps suppressing the notice (no duplicate).
func TestTrackedSubagentForegroundWaitStillSuppressesNotice(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-foreground")
	h.startTurn("turn-live")
	live := h.waitAsync(-1)

	h.finish()
	h.requireReturned("foreground wait", live)
	if got := h.calls.Load(); got != 0 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("foreground wait should have consumed the notice: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.requireNoWaiters()
}

// A zombie alone consumes nothing; the notice is delivered exactly once.
func TestTrackedSubagentZombieAloneDeliversNoticeOnce(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-alone")
	h.startTurn("turn-old")
	zombie := h.waitAsync(-1)
	h.endTurn("turn-old")

	h.finish()
	h.requireReturned("zombie wait", zombie)
	if got := h.calls.Load(); got != 1 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("expected exactly one delivered notice: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.manager.tryDeliverSubagentCompletion(h.run)
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("a second delivery attempt re-sent the notice: calls=%d", got)
	}
	h.requireNoWaiters()
}

// registerBookkeepingWaiter registers a waiter attributed to turnID without a
// goroutine, standing in for a wait whose caller will release (cancelled
// context, ended turn) at a moment the test chooses.
func (h *zombieWaitHarness) registerBookkeepingWaiter(turnID string) {
	h.manager.mu.Lock()
	h.run.completionWaiters++
	h.run.addCompletionWaiterTurnLocked(turnID)
	h.manager.mu.Unlock()
}

// A registered zombie keeps completionWaiters above zero forever. When the
// last LIVE waiter releases without consuming (its turn ended or its wait
// context was cancelled at the settle), the pending notice must still be
// retried and delivered exactly once.
func TestTrackedSubagentLastLiveWaiterReleaseRetriesDespiteZombie(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-live-release-retry")
	h.startTurn("turn-old")
	h.registerBookkeepingWaiter("turn-old")
	h.endTurn("turn-old")
	h.startTurn("turn-new")
	h.registerBookkeepingWaiter("turn-new")

	h.finish()
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("a live waiter's turn is running; the notice must wait: calls=%d", got)
	}
	h.endTurn("turn-new")
	h.manager.releaseSubagentCompletionWaiterFor(h.run.ID, "turn-new")
	if got := h.calls.Load(); got != 1 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("last live waiter released but the notice was stuck: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.manager.releaseSubagentCompletionWaiterFor(h.run.ID, "turn-old")
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("the zombie's later exit re-sent the notice: calls=%d", got)
	}
	h.requireNoWaiters()
}

// Consumption is per waiter: a zombie returning while ANOTHER waiter's turn is
// still running must not consume the notice. Once that live waiter releases
// without consuming, the notice is delivered exactly once.
func TestTrackedSubagentZombieReturnDoesNotConsumeWhileOtherTurnLive(t *testing.T) {
	t.Parallel()
	h := newZombieWaitHarness(t, "zombie-wait-per-waiter")
	h.startTurn("turn-old")
	zombie := h.waitAsync(-1)
	h.endTurn("turn-old")
	h.startTurn("turn-new")
	h.registerBookkeepingWaiter("turn-new")

	h.finish()
	h.requireReturned("zombie wait", zombie)
	h.manager.mu.Lock()
	pending, suppressed := h.run.completionPending, h.run.completionSuppressed
	h.manager.mu.Unlock()
	if !pending || suppressed || h.calls.Load() != 0 {
		t.Fatalf("zombie return consumed another turn's notice: pending=%v suppressed=%v calls=%d", pending, suppressed, h.calls.Load())
	}
	h.endTurn("turn-new")
	h.manager.releaseSubagentCompletionWaiterFor(h.run.ID, "turn-new")
	if got := h.calls.Load(); got != 1 || len(h.manager.pendingSubagentCompletions()) != 0 {
		t.Fatalf("expected exactly one delivered notice: calls=%d pending=%#v", got, h.manager.pendingSubagentCompletions())
	}
	h.requireNoWaiters()
}
