package procchild

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// The reporting contract, which is the correction the first live two-machine
// test forced (operator, 2026-10-30: "we are sending events THROUGH FILES
// INSTEAD OF A TOOL?").
//
// Four properties, one test each, and every one of them is a failure that
// actually happened or that the old design could not have detected:
//
//   - The daemon-up path delivers over the API and reads no file.
//   - A daemon that was down and comes back drains the backlog in order,
//     exactly once.
//   - A supervisor that cannot reach its daemon is LOUD — on stderr and in its
//     receipt — and keeps trying.
//   - A state directory that is wrong or missing cannot swallow events
//     quietly: it is a refusal before anything runs.

func TestTheDaemonUpPathDeliversOverTheAPIAndDrainsTheQueue(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "exit 0")
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reporting.Delivered || result.Reporting.Pending != 0 {
		t.Fatalf("a healthy run must be fully delivered, got %+v", result.Reporting)
	}
	applied := kindsOf(spoolOf(t, reporter))
	if got := strings.Join(applied, ","); got != "created,started,settled" {
		t.Fatalf("the daemon must be TOLD the whole lifecycle, got %s", got)
	}
	// The queue is empty because the daemon acknowledged, not because nothing
	// was written: the outbox still holds the delivered rows for the retention
	// window, which is the only record of what the supervisor said.
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	pending, err := outbox.PendingCount(result.ChildID)
	if err != nil || pending != 0 {
		t.Fatalf("the queue must be empty after a full delivery, got %d / %v", pending, err)
	}
	if events, err := outbox.All(result.ChildID, 0, 0); err != nil || len(events) == 0 {
		t.Fatalf("the delivered events must still be readable, got %d / %v", len(events), err)
	}
}

func TestADaemonThatWasDownAndComesBackDrainsTheBacklogInOrderExactlyOnce(t *testing.T) {
	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "exit 3")
	// Down for the first three reports — the creation, the start, and at least
	// one retry — then back. This is a daemon restart or a self-update, which
	// is precisely the window the old design was built around and the new one
	// still has to survive.
	reporter.failFor = 3
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutcomeFailed || result.ExitCode != 3 {
		t.Fatalf("the outage must not change the outcome, got %q / %d", result.Outcome, result.ExitCode)
	}
	if !result.Reporting.Delivered {
		t.Fatalf("the backlog must drain once the daemon answers, got %+v", result.Reporting)
	}
	if result.Reporting.Gap <= 0 {
		// The gap outlives the outage, so a row that healed still shows that
		// it happened. A run that silently recovered would hide the window in
		// which nothing could have been told.
		t.Fatalf("a run that was unable to report must remember the gap, got %+v", result.Reporting)
	}
	applied := spoolOf(t, reporter)
	if got := strings.Join(kindsOf(applied), ","); got != "created,started,settled" {
		// In order, and each exactly once: the replay re-sends everything
		// unacknowledged, and the seq cursor is what makes the re-send free.
		t.Fatalf("the drained backlog = %s; want created,started,settled exactly once", got)
	}
	for index, event := range applied {
		if event.Seq != uint64(index+1) {
			t.Fatalf("the backlog must arrive in seq order, got %+v", kindsOf(applied))
		}
	}
	// The daemon was asked more than once, which is the retry working.
	if reporter.reportCount() < 4 {
		t.Fatalf("an unreachable daemon must be retried, got %d reports", reporter.reportCount())
	}
}

func TestACrashBetweenTheInsertAndTheSendReplaysExactlyOnce(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	// The crash, staged exactly: the event is in the outbox and nothing
	// acknowledged it, which is what a supervisor killed between its INSERT
	// and its send leaves behind.
	crashed, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crashed.Append(childID, KindCreated, map[string]any{"label": "before the crash"}); err != nil {
		t.Fatal(err)
	}
	if _, err := crashed.Append(childID, KindStarted, map[string]any{"pid": 1234}); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}

	reporter := &recordingReporter{}
	resumed, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	record := newJournal(childID, resumed, reporter, &bytes.Buffer{}, time.Now,
		func() Report { return Report{ChildID: childID} }, time.Millisecond, 50*time.Millisecond)
	record.flush()
	// And again, because a replay that is not idempotent is worse than no
	// replay: the supervisor cannot know whether the ack it never saw arrived.
	record.flush()
	applied := reporter.applied()
	if len(applied) != 2 {
		t.Fatalf("the queued events must be delivered exactly once across two flushes, got %v", kindsOf(applied))
	}
	if applied[0].Seq != 1 || applied[1].Seq != 2 {
		t.Fatalf("the replay must be in seq order, got %v", applied)
	}
	if pending, err := resumed.PendingCount(childID); err != nil || pending != 0 {
		t.Fatalf("an acknowledged replay must empty the queue, got %d / %v", pending, err)
	}
}

func TestADaemonThatAcknowledgesNothingIsTreatedAsAFailureAndNotAsAnAck(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if _, err := outbox.Append(childID, KindSettled, map[string]any{"outcome": OutcomeSettled}); err != nil {
		t.Fatal(err)
	}
	diagnostics := &bytes.Buffer{}
	reporter := &recordingReporter{ackNothing: true}
	record := newJournal(childID, outbox, reporter, diagnostics, time.Now,
		func() Report { return Report{ChildID: childID} }, time.Millisecond, 20*time.Millisecond)
	health := record.settle()
	if health.Delivered || health.Pending != 1 {
		// A daemon older than this contract answers ok and acknowledges
		// nothing. Reading that as an ack would retire the one copy of an
		// outcome nobody has been told, which is the exact loss the outbox
		// exists to prevent.
		t.Fatalf("an unacknowledged report must stay queued, got %+v", health)
	}
	if !strings.Contains(health.LastError, "acknowledging") {
		t.Fatalf("the reason must name the missing acknowledgement, got %q", health.LastError)
	}
	if !strings.Contains(diagnostics.String(), "NOT REPORTED") {
		t.Fatalf("an undelivered outcome must be loud, got %q", diagnostics.String())
	}
}

func TestAnUnreachableDaemonIsLoudOnStderrAndKeepsTrying(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	diagnostics := &bytes.Buffer{}
	reporter := &recordingReporter{fail: errors.New("connection refused")}
	// A clock the test moves, so the grace is crossed without waiting it out.
	clock := time.Now()
	record := newJournal(childID, outbox, reporter, diagnostics, func() time.Time { return clock },
		func() Report { return Report{ChildID: childID} }, time.Millisecond, time.Millisecond)
	if _, err := record.record(KindCreated, map[string]any{"label": "unreachable"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diagnostics.String(), "NOT REPORTING") {
		// Below the grace a restart is routine, and a warning would be noise.
		t.Fatalf("a brief outage must not complain, got %q", diagnostics.String())
	}
	clock = clock.Add(reportGrace + time.Second)
	record.flush()
	complaint := diagnostics.String()
	if !strings.Contains(complaint, "NOT REPORTING") {
		t.Fatalf("an outage past the grace must be loud, got %q", complaint)
	}
	if !strings.Contains(complaint, "The work continues") {
		// What it says matters: the job is fine, the REPORTING is not, and a
		// line that read like a failure would send somebody looking for a bug
		// in the work.
		t.Fatalf("the complaint must say the work continues, got %q", complaint)
	}
	before := strings.Count(complaint, "NOT REPORTING")
	record.flush()
	if strings.Count(diagnostics.String(), "NOT REPORTING") != before {
		t.Fatal("the complaint must be repeated on an interval, not once per retry")
	}
	clock = clock.Add(reportComplaintInterval + time.Second)
	record.flush()
	if strings.Count(diagnostics.String(), "NOT REPORTING") != before+1 {
		t.Fatal("a continuing outage must be said again after the complaint interval")
	}
	// And it never stopped trying: one report per flush.
	if reporter.reportCount() < 4 {
		t.Fatalf("an unreachable daemon must keep being tried, got %d reports", reporter.reportCount())
	}
	health := record.health()
	if health.Delivered || health.Pending != 1 || health.Gap <= 0 {
		t.Fatalf("the health must say undelivered with a gap, got %+v", health)
	}
}

func TestADaemonOlderThanThisContractIsNamedAsSuchAndNotWaitedOutForTwoMinutes(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if _, err := outbox.Append(childID, KindSettled, map[string]any{"outcome": OutcomeSettled}); err != nil {
		t.Fatal(err)
	}
	diagnostics := &bytes.Buffer{}
	// The exact refusal a v0.1.238 daemon answers with, observed live on this
	// Mac against the installed app on 2026-10-30.
	reporter := &recordingReporter{fail: errors.New("arguments.events is not a known argument")}
	// A deadline long enough that waiting it out would be the failure.
	record := newJournal(childID, outbox, reporter, diagnostics, time.Now,
		func() Report { return Report{ChildID: childID} }, time.Millisecond, time.Hour)
	started := time.Now()
	health := record.settle()
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		// A schema mismatch will not fix itself while a finished supervisor
		// waits, and the wait would delay every job's exit for the deadline.
		t.Fatalf("a refused report must not be retried to the deadline; it took %s", elapsed)
	}
	if !health.Refused || health.Delivered || health.Pending != 1 {
		t.Fatalf("a refusal must be reported as a refusal, got %+v", health)
	}
	complaint := diagnostics.String()
	if !strings.Contains(complaint, "older than the supervised-job reporting contract") {
		// "Unable to reach its daemon" would send somebody to look at the
		// network for a version skew.
		t.Fatalf("the complaint must name the version skew, got %q", complaint)
	}
	if !strings.Contains(complaint, "until that daemon is updated") {
		t.Fatalf("the complaint must name the action, got %q", complaint)
	}
	if strings.Contains(complaint, "of trying") {
		t.Fatalf("it must not claim a wait it did not perform, got %q", complaint)
	}
}

func TestAHeartbeatIsReportedOnTheDeclaredCadenceAndIsNotALifecycleEvent(t *testing.T) {
	state := t.TempDir()
	childID, err := IDForOperation("op-" + t.Name())
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenOutbox(state)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	clock := time.Now()
	reporter := &recordingReporter{}
	record := newJournal(childID, outbox, reporter, &bytes.Buffer{}, func() time.Time { return clock },
		func() Report { return Report{ChildID: childID} }, time.Millisecond, time.Millisecond)
	record.beat()
	if reporter.reportCount() != 0 {
		// The clock starts at construction: a beat due immediately would make
		// every run report liveness seconds after the event it just reported.
		t.Fatalf("a beat before the interval must do nothing, got %d reports", reporter.reportCount())
	}
	clock = clock.Add(HeartbeatInterval + time.Second)
	record.beat()
	applied := reporter.applied()
	if len(applied) != 1 || applied[0].Kind != KindHeartbeat {
		t.Fatalf("the beat must report a heartbeat, got %v", kindsOf(applied))
	}
	if SilenceThreshold != 3*HeartbeatInterval {
		// Three beats, so one lost report is not an incident.
		t.Fatalf("the silence threshold must be three heartbeats, got %s", SilenceThreshold)
	}
}
