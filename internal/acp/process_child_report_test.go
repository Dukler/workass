package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// The daemon's half of the reporting contract (docs/specs/fleet-runtime.md
// §13 P4, rewritten 2026-10-30 after the first live two-machine test).
//
// What is asserted here is what the live test proved was missing: the daemon
// learns a job's whole life from REPORTS, acknowledges each one by seq, never
// discovers a job by looking at a file, and cannot be made to look healthy
// about a supervisor it has stopped hearing from.

func TestTheDaemonAcknowledgesEachReportedEventBySeqAndAppliesItOnlyOnce(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, err := procchild.IDForOperation("op-ack")
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	batch := []procchild.Event{
		{Seq: 1, At: time.Now().UTC(), Kind: procchild.KindCreated, Payload: map[string]any{"label": "training run"}},
		{Seq: 2, At: time.Now().UTC(), Kind: procchild.KindStarted, Payload: map[string]any{"pid": 4242, "pgid": 4242}},
	}
	options := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "training run", SupervisorPID: &pid,
		OutputFile: liveOutputPath(t, manager.opts.StateDir, childID),
		Events:     batch,
	}
	first, err := manager.RegisterProcessChild(options)
	if err != nil {
		t.Fatal(err)
	}
	if first["ackThrough"] != uint64(2) {
		t.Fatalf("the daemon must acknowledge through seq 2, got %#v", first["ackThrough"])
	}
	if first["created"] != true {
		t.Fatalf("the first report creates the row, got %#v", first["created"])
	}
	if item := itemFor(t, manager, childID); item.PID == nil || *item.PID != 4242 {
		t.Fatalf("the started event must give the row its pid, got %#v", item.PID)
	}

	// The same batch again, which is exactly what a supervisor that never saw
	// the ack re-sends. Nothing may be applied twice.
	options.Events = append(batch, procchild.Event{
		Seq: 3, At: time.Now().UTC(), Kind: procchild.KindSettled,
		Payload: map[string]any{"outcome": procchild.OutcomeSettled, "exitCode": 0, "summary": "Finished with exit 0"},
	})
	second, err := manager.RegisterProcessChild(options)
	if err != nil {
		t.Fatal(err)
	}
	if second["ackThrough"] != uint64(3) {
		t.Fatalf("the ack must advance to 3, got %#v", second["ackThrough"])
	}
	if second["created"] != false {
		t.Fatalf("a repeat must resolve to the row that exists, got %#v", second["created"])
	}
	if got := lifecycle.kinds(); got != "exit:settled" {
		// One settle, however many times the batch arrived.
		t.Fatalf("lifecycle = %q; the settle must be delivered exactly once", got)
	}
	if receipt := receiptFor(t, manager, childID); receipt.Outcome != procchild.OutcomeSettled {
		t.Fatalf("outcome = %q, want settled", receipt.Outcome)
	}
}

func TestAMalformedReportIsRefusedWholeRatherThanAppliedInPart(t *testing.T) {
	manager := processChildManager(t, nil)
	childID, err := procchild.IDForOperation("op-malformed")
	if err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	options := ProcessChildRegistrationOptions{
		OwnerKey: "owner-proc", ParentChatID: "chat-proc", ParentTabID: "tab-proc",
		WorkID: childID, Label: "training run", SupervisorPID: &pid,
		OutputFile: liveOutputPath(t, manager.opts.StateDir, childID),
	}
	for name, batch := range map[string][]procchild.Event{
		"an event with no seq": {
			{Seq: 1, Kind: procchild.KindCreated},
			{Seq: 0, Kind: procchild.KindStarted},
		},
		"an event with no kind": {
			{Seq: 1, Kind: procchild.KindCreated},
			{Seq: 2, Kind: "  "},
		},
		"one seq twice": {
			{Seq: 1, Kind: procchild.KindCreated},
			{Seq: 1, Kind: procchild.KindStarted},
		},
	} {
		options.Events = batch
		if _, err := manager.RegisterProcessChild(options); err == nil {
			// The events are a SEQUENCE. Applying the readable half would
			// advance the cursor past something nobody looked at.
			t.Fatalf("%s must be refused whole", name)
		}
	}
}

func TestTheBatchCeilingIsOneNumberOnBothSides(t *testing.T) {
	// The supervisor states it, the daemon enforces it, and the tool schema
	// declares it. Two of them disagreeing would be a refusal with no visible
	// cause: a backlog the supervisor thought was legal and the daemon refused
	// whole, forever.
	if procchild.MaxReportBatch != MaxReportedEventsPerCall {
		t.Fatalf("the supervisor sends batches of %d and this daemon accepts %d",
			procchild.MaxReportBatch, MaxReportedEventsPerCall)
	}
}

func TestADeadSupervisorsQueuedOutcomeIsRecoveredFromTheOutboxBeforeLostIsReached(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	state := manager.opts.StateDir
	// A supervisor that reported its start and then died with its settle in
	// the queue. The pid is one nothing is listening on, which is what makes
	// it indistinguishable from a crash.
	childID, _ := seedProcessChild(t, manager, "op-drain", unusedPIDForTest(t), nil)
	outbox, err := procchild.OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Append(childID, procchild.KindCreated, map[string]any{"label": "drained job"}); err != nil {
		t.Fatal(err)
	}
	if _, err := outbox.Append(childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeFailed, "exitCode": 9, "summary": "Failed with exit 9 after 2s",
	}); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Close(); err != nil {
		t.Fatal(err)
	}

	// Past the grace in one pass: the supervisor is gone, so the only question
	// is whether this daemon reads what it left or invents `lost`.
	manager.reconcileProcessChildren(time.Now().UTC())
	manager.reconcileProcessChildren(time.Now().UTC().Add(supervisorMissingGrace + time.Minute))
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeFailed {
		// A supervisor that crashed between recording its settle and
		// delivering it is NOT lost, and reading the queue it left is the
		// difference between the truth and a guess.
		t.Fatalf("outcome = %q, want the failure the dead supervisor recorded", receipt.Outcome)
	}
	if receipt.ExitCode == nil || *receipt.ExitCode != 9 {
		t.Fatalf("the recovered exit code = %#v, want the child's own 9", receipt.ExitCode)
	}
}

func TestASupervisorThatIsGoneWithAnEmptyQueueIsLostAndNotRunning(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-lost", unusedPIDForTest(t), nil)
	manager.reconcileProcessChildren(time.Now().UTC())
	if item := itemFor(t, manager, childID); item.Status != "running" {
		t.Fatalf("a vanished supervisor gets its grace first, got %q", item.Status)
	}
	manager.reconcileProcessChildren(time.Now().UTC().Add(supervisorMissingGrace + time.Minute))
	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeLost {
		// Absence is evidence. This is the inverse of the marker-file design
		// where absence meant "still running" and a finished job sat unnoticed
		// for five hours.
		t.Fatalf("outcome = %q, want lost", receipt.Outcome)
	}
	if !strings.Contains(receipt.Summary, "reported no outcome") {
		t.Fatalf("the summary must say what is unknown and why, got %q", receipt.Summary)
	}
}

func TestASupervisorThatStopsReportingIsSaidToBeSilentExactlyOnceAndClearsOnTheNextReport(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-silent", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})

	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	if item := itemFor(t, manager, childID); item.Reporting != "" {
		t.Fatalf("a job that just reported is not silent, got %q", item.Reporting)
	}
	// Two beats missed is not an incident.
	manager.reconcileProcessChildren(start.Add(2 * procchild.HeartbeatInterval))
	if item := itemFor(t, manager, childID); item.Reporting != "" {
		t.Fatalf("one lost report must not raise an alarm, got %q", item.Reporting)
	}
	silentAt := start.Add(procchild.SilenceThreshold + time.Minute)
	manager.reconcileProcessChildren(silentAt)
	item := itemFor(t, manager, childID)
	if item.Reporting != processReportingSilent {
		// A row that looked healthy while nothing had heard from it was the
		// operator's objection to the old design.
		t.Fatalf("reporting = %q, want silent", item.Reporting)
	}
	if item.Status != "running" {
		t.Fatalf("silence settles nothing: status = %q, want running", item.Status)
	}
	notices := lifecycle.kinds()
	if !strings.Contains(notices, "attention") {
		t.Fatalf("silence must be news, got %q", notices)
	}
	// And a reader sees it, which is the whole requirement: the row must not
	// READ as healthy.
	rows := manager.ListProcessChildren()
	if len(rows) != 1 || rows[0].Reporting != processReportingSilent {
		t.Fatalf("the list must show the silence: %#v", rows)
	}
	manager.reconcileProcessChildren(silentAt.Add(procchild.SilenceThreshold * 4))
	if lifecycle.kinds() != notices {
		t.Fatal("silence must be said once, not once per reconcile tick")
	}

	// And it clears the moment the supervisor is heard from again.
	appendSpool(t, manager, childID, procchild.KindHeartbeat, nil)
	if item := itemFor(t, manager, childID); item.Reporting != "" {
		t.Fatalf("a report must clear silence, got %q", item.Reporting)
	}
}

func TestAHeartbeatIsNeitherANoticeNorAChangeToTheRowsTimestamp(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	childID, _ := seedProcessChild(t, manager, "op-beat", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	before := itemFor(t, manager, childID)
	notices := lifecycle.kinds()

	appendSpool(t, manager, childID, procchild.KindHeartbeat, nil)
	after := itemFor(t, manager, childID)
	if lifecycle.kinds() != notices {
		t.Fatalf("a heartbeat must never be a notice, got %q", lifecycle.kinds())
	}
	if after.UpdatedAt != before.UpdatedAt {
		// A row whose timestamp moved every minute would make every quiet job
		// look busy, in every surface that sorts or ages by it.
		t.Fatalf("a heartbeat must not touch updatedAt: %q -> %q", before.UpdatedAt, after.UpdatedAt)
	}
}

// unusedPIDForTest is a pid that is certainly not running: the test's own
// child, reaped, so the number is real and the process is gone.
func unusedPIDForTest(t *testing.T) int {
	t.Helper()
	process, err := os.StartProcess("/usr/bin/true", []string{"true"}, &os.ProcAttr{})
	if err != nil {
		t.Skipf("cannot start a throwaway process on this host: %v", err)
	}
	if _, err := process.Wait(); err != nil {
		t.Fatal(err)
	}
	return process.Pid
}
