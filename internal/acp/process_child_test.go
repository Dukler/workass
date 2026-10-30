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

// The daemon's half of P4: the spool is ingested, the verdicts are reached, and
// a restart re-attaches instead of inventing an outcome.
//
// The projection-parity test at the end is the migration's gate: a row
// registered through the external-work adapter must encode byte for byte as it
// did before process children existed, because every rail row, card and
// receipt in the product reads that encoding.

type recordedLifecycle struct {
	mu     sync.Mutex
	events []ProcessLifecycleEvent
}

func (r *recordedLifecycle) observe(event ProcessLifecycleEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *recordedLifecycle) snapshot() []ProcessLifecycleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProcessLifecycleEvent(nil), r.events...)
}

func (r *recordedLifecycle) kinds() string {
	parts := make([]string, 0, 4)
	for _, event := range r.snapshot() {
		if event.Kind == ProcessLifecycleExit {
			parts = append(parts, event.Kind+":"+event.Outcome)
			continue
		}
		parts = append(parts, event.Kind+":"+event.AttentionKind)
	}
	return strings.Join(parts, ",")
}

// seedProcessChild registers one supervised child exactly as `workass run`
// does, with a supervisor pid the test controls.
func seedProcessChild(t *testing.T, manager *Manager, operationID string, supervisorPID int, mutate func(*ProcessChildRegistrationOptions)) (string, string) {
	t.Helper()
	childID, err := procchild.IDForOperation(operationID)
	if err != nil {
		t.Fatal(err)
	}
	outputDir := filepath.Join(manager.opts.StateDir, "external-work")
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outputFile := filepath.Join(outputDir, childID+".output")
	if err := os.WriteFile(outputFile, []byte("job output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := procchild.EnsureDir(manager.opts.StateDir, childID); err != nil {
		t.Fatal(err)
	}
	pid := supervisorPID
	opts := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "training run", Command: "python train.py",
		OutputFile: outputFile, SupervisorPID: &pid, Isolation: string(procchild.GroupProcessGroup),
		OriginLaneID: "lane-1", OriginOperationID: "op-1", OriginTurnID: "turn-1",
	}
	if mutate != nil {
		mutate(&opts)
	}
	if _, err := manager.RegisterProcessChild(opts); err != nil {
		t.Fatalf("register process child: %v", err)
	}
	return childID, outputFile
}

func processChildManager(t *testing.T, lifecycle *recordedLifecycle) *Manager {
	t.Helper()
	manager := NewManager(Options{
		StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-proc", "chat-proc", "tab-proc", "mock")
	if lifecycle != nil {
		manager.SetProcessLifecycleObserver(lifecycle.observe)
	}
	return manager
}

// appendSpool keeps its name and now does what a supervisor really does:
// queue the event in the outbox AND report it. The two together are one act —
// write-ahead, then deliver — and a test that only wrote the file would be
// testing the design the operator rejected.
func appendSpool(t *testing.T, manager *Manager, childID, kind string, payload map[string]any) {
	t.Helper()
	outbox, err := procchild.OpenOutbox(manager.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	event, err := outbox.Append(childID, kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.RegisterProcessChild(reportFor(t, manager, childID, event))
	if err != nil {
		t.Fatalf("report %s: %v", kind, err)
	}
	ack, _ := result["ackThrough"].(uint64)
	if ack < event.Seq {
		// The ack is the contract. A daemon that applied an event and did not
		// say so would leave the supervisor unable to retire it.
		t.Fatalf("the daemon must acknowledge through %d, got %d", event.Seq, ack)
	}
	if err := outbox.MarkDelivered(childID, ack); err != nil {
		t.Fatal(err)
	}
}

// reportFor is the registration facts a supervisor repeats on every report,
// plus one event.
func reportFor(t *testing.T, manager *Manager, childID string, events ...procchild.Event) ProcessChildRegistrationOptions {
	t.Helper()
	// The supervisor states its own pid on every report, so a test standing in
	// for one must keep the pid the row was seeded with — otherwise it quietly
	// re-points the row at the live test process and `lost` becomes
	// unreachable.
	pid := os.Getpid()
	manager.spawnedWorkMu.Lock()
	if rec := manager.spawnedWork[spawnedWorkKey("tab-proc", "chat-proc", childID)]; rec != nil && rec.Item.SupervisorPID != nil {
		pid = *rec.Item.SupervisorPID
	}
	manager.spawnedWorkMu.Unlock()
	return ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "test job", SupervisorPID: &pid,
		OutputFile:  filepath.Join(manager.opts.StateDir, "external-work", childID+".output"),
		HeartbeatMs: procchild.HeartbeatInterval.Milliseconds(),
		Events:      events,
	}
}

func itemFor(t *testing.T, manager *Manager, childID string) SpawnedWorkItem {
	t.Helper()
	manager.spawnedWorkMu.Lock()
	defer manager.spawnedWorkMu.Unlock()
	rec := manager.spawnedWork[spawnedWorkKey("tab-proc", "chat-proc", childID)]
	if rec == nil {
		t.Fatalf("no live record for %s", childID)
	}
	return rec.Item
}

// receiptFor is how a SETTLED process child is read.
//
// The live registry deliberately compacts a settled row once its chat actor
// has accepted it — that is pre-existing behaviour every surface relies on, and
// the durable receipt is the record from then on. So the outcome assertions
// below read the receipt, which is also exactly what a chat reads an hour
// later.
func receiptFor(t *testing.T, manager *Manager, childID string) SpawnedWorkReceipt {
	t.Helper()
	receipts, err := manager.ListSpawnedWorkReceipts("owner-proc", "chat-proc", "tab-proc", "chat-proc", "tab-proc", 64)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.TaskID == childID {
			return receipt
		}
	}
	t.Fatalf("no receipt for %s; have %d receipt(s)", childID, len(receipts))
	return SpawnedWorkReceipt{}
}

func TestTheSpoolIsIngestedOnceAndASettleBecomesOneOutcome(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-ingest", os.Getpid(), nil)

	appendSpool(t, manager, childID, procchild.KindCreated, map[string]any{"label": "training run"})
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242, "pgid": 4242, "group": "process_group"})
	manager.reconcileProcessChildren(time.Now().UTC())
	item := itemFor(t, manager, childID)
	if item.PID == nil || *item.PID != 4242 || item.ProcessGroup != 4242 || item.Status != "running" {
		t.Fatalf("a started event must land the child's pid and group, got %#v", item)
	}

	appendSpool(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0 after 2s",
	})
	manager.reconcileProcessChildren(time.Now().UTC())
	receipt := receiptFor(t, manager, childID)
	if receipt.Status != "exited" || receipt.Outcome != procchild.OutcomeSettled {
		t.Fatalf("status %q outcome %q; want exited / settled", receipt.Status, receipt.Outcome)
	}
	if receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		t.Fatalf("exit code = %#v, want 0", receipt.ExitCode)
	}
	// Re-reading the spool must change nothing: dedup is by seq, so a crash
	// between applying and persisting the cursor costs one re-application.
	manager.reconcileProcessChildren(time.Now().UTC())
	manager.reconcileProcessChildren(time.Now().UTC())
	if got := lifecycle.kinds(); got != "exit:settled" {
		t.Fatalf("lifecycle events = %q; one settle must produce exactly one outcome", got)
	}
}

func TestAVanishedSupervisorIsLostAndNeverRunning(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	// A pid that cannot be alive: the supervisor is gone and wrote no outcome.
	childID, _ := seedProcessChild(t, manager, "op-lost", 2, nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242, "pgid": 4242})

	now := time.Now().UTC()
	manager.reconcileProcessChildren(now)
	if item := itemFor(t, manager, childID); item.Status != "running" {
		t.Fatalf("a vanished supervisor gets a grace first, got %q", item.Status)
	}
	manager.reconcileProcessChildren(now.Add(supervisorMissingGrace + time.Second))
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeLost {
		t.Fatalf("outcome = %q, want lost: a pid that is gone with no settled event is a verdict, not a guess", receipt.Outcome)
	}
	if receipt.Status != "orphaned" {
		// `orphaned` is the word the rail has always shown for "no process",
		// so the row stays in the closed status vocabulary every surface reads.
		t.Fatalf("status = %q, want orphaned", receipt.Status)
	}
	if receipt.Status == "running" {
		t.Fatal("a lost job must never still read as running")
	}
	if !strings.Contains(receipt.Summary, "unknown") {
		t.Fatalf("a lost job's summary must say what it did is unknown, got %q", receipt.Summary)
	}
	if got := lifecycle.kinds(); got != "exit:lost" {
		t.Fatalf("lifecycle events = %q, want exactly one lost exit", got)
	}
}

func TestASpooledSettleOutranksAVanishedSupervisor(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-settle-then-die", 2, nil)
	appendSpool(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeFailed, "exitCode": 1, "summary": "Failed with exit 1 after 4s",
	})
	// The supervisor exited after writing its outcome, which is the ordinary
	// case: the spool is read to its end BEFORE absence is read as evidence.
	manager.reconcileProcessChildren(time.Now().UTC().Add(time.Hour))
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeFailed || receipt.Status != "failed" {
		t.Fatalf("outcome %q status %q; a recorded failure must not become lost", receipt.Outcome, receipt.Status)
	}
	if got := lifecycle.kinds(); got != "exit:failed" {
		t.Fatalf("lifecycle events = %q", got)
	}
}

func TestMaxRuntimeKeepsItsOwnOutcomeWordOnTheRow(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-timeout", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.MaxRuntimeMs = (3 * time.Hour).Milliseconds()
	})
	appendSpool(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeTimedOut, "exitCode": 124, "summary": "Stopped at its declared max_runtime after 3h0m0s",
	})
	manager.reconcileProcessChildren(time.Now().UTC())
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeTimedOut {
		t.Fatalf("outcome = %q, want timed_out", receipt.Outcome)
	}
	if receipt.Status != "failed" {
		// Two vocabularies: Status stays the closed presentation set every
		// surface already reads, Outcome carries the verdict a chat needs.
		t.Fatalf("status = %q; a timed-out job presents as failed in the closed vocabulary", receipt.Status)
	}
	if got := lifecycle.kinds(); got != "exit:timed_out" {
		t.Fatalf("lifecycle events = %q", got)
	}
}

func TestADeclaredStallNotifiesOnceAndDoesNotEndTheJob(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-stall", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.StaleAfterMs = (40 * time.Minute).Milliseconds()
	})
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	appendSpool(t, manager, childID, procchild.KindStalled, map[string]any{
		"evidence": "no output for 41m0s, past the declared 40m0s (still running)",
	})
	manager.reconcileProcessChildren(time.Now().UTC())
	item := itemFor(t, manager, childID)
	if !item.Stalled || item.Status != "running" {
		t.Fatalf("a stall is a flag on running, got stalled=%v status=%q", item.Stalled, item.Status)
	}
	// A second stalled event — a spool re-read after a restart, say — must not
	// produce a second notice.
	appendSpool(t, manager, childID, procchild.KindStalled, map[string]any{"evidence": "still quiet"})
	manager.reconcileProcessChildren(time.Now().UTC())
	if got := lifecycle.kinds(); got != "stalled:stalled" {
		t.Fatalf("lifecycle events = %q; a declared stall is said exactly once", got)
	}
}

func TestACancelledJobIsSuppressedButStillRecorded(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-cancel", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeCancelled, "exitCode": 130, "summary": "Stopped on request after 9s",
	})
	manager.reconcileProcessChildren(time.Now().UTC())
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeCancelled || receipt.Status != "exited" {
		// A stop is an outcome the user chose, which the rail has always shown
		// as exited rather than failed.
		t.Fatalf("outcome %q status %q; want cancelled / exited", receipt.Outcome, receipt.Status)
	}
	events := lifecycle.snapshot()
	if len(events) != 1 || !events[0].NoticeSuppressed || events[0].SuppressReason == "" {
		t.Fatalf("a cancel must be logged with this machine's own suppression verdict, got %#v", events)
	}
}

func TestAProcessChildReattachesAfterADaemonRestartWithoutLosingEvents(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	stateDir := manager.opts.StateDir
	childID, _ := seedProcessChild(t, manager, "op-restart", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242, "pgid": 4242})
	manager.reconcileProcessChildren(time.Now().UTC())
	if err := manager.persistSpawnedWorkSnapshot("tab-proc"); err != nil {
		t.Fatal(err)
	}

	// The daemon is replaced — a restart, or a self-update. The child is not
	// its child, so it is still running; what must survive is the row and the
	// acknowledgement cursor, so the supervisor's next report applies what is
	// new and re-applies nothing.
	restarted := NewManager(Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { restarted.Reset() })
	bindExternalWorkOwnerForTest(restarted, "owner-proc", "chat-proc", "tab-proc", "mock")
	restartedLifecycle := &recordedLifecycle{}
	restarted.SetProcessLifecycleObserver(restartedLifecycle.observe)

	item := itemFor(t, restarted, childID)
	if item.Status != "running" || !item.Supervised() {
		t.Fatalf("a supervised child must survive a daemon restart as running, got %#v", item)
	}
	appendSpool(t, restarted, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0 after 11s",
	})
	restarted.reconcileProcessChildren(time.Now().UTC())
	if receipt := receiptFor(t, restarted, childID); receipt.Outcome != procchild.OutcomeSettled {
		t.Fatalf("outcome after restart = %q, want settled", receipt.Outcome)
	}
	if got := restartedLifecycle.kinds(); got != "exit:settled" {
		t.Fatalf("the new daemon's lifecycle events = %q; the settle must be delivered exactly once", got)
	}
	// The cursor survived, so the events the first daemon already applied are
	// not replayed as new outcomes by the second.
	if got := lifecycle.kinds(); got != "" {
		t.Fatalf("the replaced daemon must have emitted nothing after its own pass, got %q", got)
	}
	if _, err := os.Stat(procchild.SpoolPath(stateDir, childID)); !os.IsNotExist(err) {
		t.Fatalf("a consumed spool must be removed once the outcome is durable: %v", err)
	}
}

func TestAnEvidencelessProcessChildRegistrationIsRefused(t *testing.T) {
	manager := processChildManager(t, nil)
	outputFile := filepath.Join(manager.opts.StateDir, "external-work", "xwevidence000.output")
	if err := os.MkdirAll(filepath.Dir(outputFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: "xwevidence000", Label: "no supervisor", OutputFile: outputFile,
	}
	if _, err := manager.RegisterProcessChild(base); err == nil {
		// Without a supervisor pid there is nothing that could ever settle the
		// row honestly, which is the stale `running` P4 exists to remove.
		t.Fatal("a registration with no supervisor pid must be refused")
	}
	pid := 1
	withPID1 := base
	withPID1.SupervisorPID = &pid
	if _, err := manager.RegisterProcessChild(withPID1); err == nil {
		t.Fatal("pid 1 is never a Workass supervisor and must be refused")
	}
	real := os.Getpid()
	badID := base
	badID.SupervisorPID = &real
	badID.WorkID = "not-a-work-id"
	if _, err := manager.RegisterProcessChild(badID); err == nil {
		t.Fatal("a work id the supervisor did not derive must be refused")
	}
	noLabel := base
	noLabel.SupervisorPID = &real
	noLabel.Label = ""
	if _, err := manager.RegisterProcessChild(noLabel); err == nil {
		// The label is the only name the chat will ever show for this job.
		t.Fatal("a child with no label must be refused")
	}
}

func TestRegisteringTheSameOperationTwiceIsOneChildAndOneRow(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-idempotent", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished",
	})
	// This is the supervisor's closing poke: the same registration, repeated
	// with the same operation id, which must be ACCEPTED and must ingest the
	// spool rather than answering with a cached first receipt.
	second, _ := seedProcessChild(t, manager, "op-idempotent", os.Getpid(), nil)
	if second != childID {
		t.Fatalf("a repeat produced a second child: %q then %q", childID, second)
	}
	if got := lifecycle.kinds(); got != "exit:settled" {
		t.Fatalf("lifecycle events = %q; the poke must deliver the outcome once, not twice", got)
	}
	receipts, err := manager.ListSpawnedWorkReceipts("owner-proc", "chat-proc", "tab-proc", "chat-proc", "tab-proc", 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 {
		t.Fatalf("receipts = %d; one job registered twice is still one job", len(receipts))
	}
}

func TestReadAndCancelOneProcessChild(t *testing.T) {
	manager := processChildManager(t, nil)
	childID, outputFile := seedProcessChild(t, manager, "op-read", os.Getpid(), func(opts *ProcessChildRegistrationOptions) {
		opts.Resource = "gpu0"
	})
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	manager.reconcileProcessChildren(time.Now().UTC())

	read, err := manager.ReadProcessChild(childID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tail, _ := read["output_tail"].(string); !strings.Contains(tail, "job output") {
		t.Fatalf("a read must return a bounded tail of the output FILE, got %q", tail)
	}
	transitions, _ := read["transitions"].([]map[string]any)
	if len(transitions) == 0 {
		t.Fatalf("a read must return the spooled transitions, got %#v", read["transitions"])
	}
	// A cursor the reader states means a re-read costs nothing.
	again, err := manager.ReadProcessChild(childID, 0, uint64(len(transitions)))
	if err != nil {
		t.Fatal(err)
	}
	if rest, _ := again["transitions"].([]map[string]any); len(rest) != 0 {
		t.Fatalf("a cursor past the head must return no transitions, got %#v", rest)
	}
	if _, err := manager.ReadProcessChild("xwnosuchjob01", 0, 0); err == nil {
		t.Fatal("reading a job this machine does not track must be refused, not answered with an empty row")
	}

	cancelled, err := manager.CancelProcessChild(childID, "the operator asked")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled["already"] != false || cancelled["supervisorAlive"] != true {
		t.Fatalf("cancel = %#v", cancelled)
	}
	stop, err := os.ReadFile(procchild.StopPath(manager.opts.StateDir, childID))
	if err != nil || !strings.Contains(string(stop), "the operator asked") {
		// A cancel is a wish, written where the supervisor will see it, with
		// the reason so whoever finds the file knows who asked.
		t.Fatalf("stop file = %q / %v", stop, err)
	}
	if !procchild.StopRequested(manager.opts.StateDir, childID) {
		t.Fatal("a pending stop must be visible on the row")
	}
	rows := manager.ListProcessChildren()
	if len(rows) != 1 || !rows[0].StopRequested || rows[0].Resource != "gpu0" {
		t.Fatalf("list = %#v", rows)
	}
	_ = outputFile
}

// TestExternalWorkRowEncodingIsUnchangedByProcessChildren is the migration's
// projection-parity gate.
//
// Every rail row, notice card and receipt in the product reads this encoding.
// P4 added eleven fields to SpawnedWorkItem; all of them are omitempty, so a
// row that no supervisor registered must serialize to exactly the bytes it
// serialized to before — otherwise the gate is "it looked the same", which is
// not a gate.
func TestExternalWorkRowEncodingIsUnchangedByProcessChildren(t *testing.T) {
	manager := NewManager(Options{
		StateDir: t.TempDir(), RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	bindExternalWorkOwnerForTest(manager, "owner-parity", "chat-parity", "tab-parity", "mock")
	registered, err := manager.RegisterExternalWork(ExternalWorkRegistrationOptions{
		OwnerKey: "owner-parity", ParentChatID: "chat-parity", ParentTabID: "tab-parity",
		Label: "detached lane", OriginLaneID: "lane-1", OriginOperationID: "op-1", OriginTurnID: "turn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	workID, _ := registered["workId"].(string)
	item := func() SpawnedWorkItem {
		manager.spawnedWorkMu.Lock()
		defer manager.spawnedWorkMu.Unlock()
		return manager.spawnedWork[spawnedWorkKey("tab-parity", "chat-parity", workID)].Item
	}()
	if item.Supervised() {
		t.Fatal("the external-work adapter must register an UNSUPERVISED row; every P4 behaviour hangs off that one test")
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, added := range []string{
		"outcome", "owner", "resource", "queuePosition", "supervisorPid",
		"processGroup", "isolation", "stalled", "maxRuntimeMs", "staleAfterMs", "maxHoldMs",
	} {
		if _, present := fields[added]; present {
			t.Fatalf("P4 field %q appears on an unsupervised row; the rail's encoding is not byte-identical", added)
		}
	}
	// The same for the durable snapshot and the receipt, which are the other
	// two encodings a surface reads.
	snapshot, err := json.Marshal(spawnedWorkSnapshotItem{SpawnedWorkItem: item, DoneFile: "/tmp/x.done"})
	if err != nil {
		t.Fatal(err)
	}
	for _, added := range []string{"spoolFile", "spoolSeq", "outcome", "supervisorPid"} {
		if strings.Contains(string(snapshot), `"`+added+`"`) {
			t.Fatalf("P4 field %q appears in an unsupervised snapshot row", added)
		}
	}
	receipt, err := json.Marshal(manager.spawnedWorkReceiptFromItem(item))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(receipt), "outcome") || strings.Contains(string(receipt), "supervisorPid") {
		t.Fatalf("the receipt encoding changed: %s", receipt)
	}
	if rows := manager.ListProcessChildren(); len(rows) != 0 {
		t.Fatalf("an unsupervised lane is not a process child, got %#v", rows)
	}
}
