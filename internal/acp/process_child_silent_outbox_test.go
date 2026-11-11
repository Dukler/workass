package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// bug-20261111-dc171619. After an app update a supervisor kept supervising but
// never reached the daemon again. It honoured a cancel — stopped its job and
// queued cancel_requested and settled — and then held on for two minutes
// trying to deliver them, so to its owner it looked like a job that ignored
// cancel, with an outcome nobody knew.
//
// The report is still the path; this is what happens once that path has been
// declared missing: the daemon reads the live supervisor's queue by work id,
// applies it, acknowledges it so the supervisor stops waiting, and leaves the
// row marked silent because nothing it read is a report.

func queueWithoutReporting(t *testing.T, manager *Manager, childID, kind string, payload map[string]any) {
	t.Helper()
	outbox, err := procchild.OpenOutbox(manager.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if _, err := outbox.Append(childID, kind, payload); err != nil {
		t.Fatal(err)
	}
}

func pendingInOutbox(t *testing.T, manager *Manager, childID string) int {
	t.Helper()
	outbox, err := procchild.OpenOutbox(manager.opts.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	count, err := outbox.PendingCount(childID)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func TestASilentLiveSupervisorsQueuedOutcomeIsReadAndAcknowledged(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	// This test process is the supervisor: alive throughout.
	childID, _ := seedProcessChild(t, manager, "op-silent-cancel", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})

	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	if item := itemFor(t, manager, childID); item.Reporting == processReportingSilent {
		t.Fatalf("a supervisor heard a moment ago is not silent, got %q", item.Reporting)
	}

	// Its reports stop arriving, and it goes on writing its queue.
	queueWithoutReporting(t, manager, childID, procchild.KindHeartbeat, nil)
	manager.reconcileProcessChildren(start.Add(time.Minute))
	if pending := pendingInOutbox(t, manager, childID); pending != 1 {
		// Before the reports are declared missing, the queue is the
		// supervisor's to deliver: the daemon reads nothing.
		t.Fatalf("a supervisor inside its window must be left to report, got %d pending", pending)
	}

	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + time.Minute))
	if item := itemFor(t, manager, childID); item.Reporting != processReportingSilent {
		t.Fatalf("three missed beats from a live pid mark the row silent, got %q", item.Reporting)
	}

	// The owner cancels; the supervisor stops its job and queues the outcome,
	// and none of it is reported.
	queueWithoutReporting(t, manager, childID, procchild.KindCancelRequested, map[string]any{
		"by": "a stop was requested", "outcome": procchild.OutcomeCancelled,
	})
	queueWithoutReporting(t, manager, childID, procchild.KindSettled, map[string]any{
		"outcome": procchild.OutcomeCancelled, "exitCode": procchild.ExitCancelled,
		"summary": "Stopped on request after 3h25m11s",
	})
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + 2*time.Minute))

	receipt := receiptFor(t, manager, childID)
	if receipt.Outcome != procchild.OutcomeCancelled {
		t.Fatalf("the queued outcome must be read, got %q", receipt.Outcome)
	}
	if !strings.Contains(receipt.Summary, "Stopped on request") {
		t.Fatalf("the outcome is the supervisor's own, got %q", receipt.Summary)
	}
	if pending := pendingInOutbox(t, manager, childID); pending != 0 {
		// The supervisor's closing wait reads exactly this: an empty queue is
		// what lets it exit at once instead of waiting out its deadline.
		t.Fatalf("what the daemon applied must be acknowledged, got %d still pending", pending)
	}
	if got := strings.Count(lifecycle.kinds(), "exit:cancelled"); got != 1 {
		t.Fatalf("the owner is told once, got %q", lifecycle.kinds())
	}

	// A late report of the same events, which the supervisor may still send,
	// changes nothing.
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + 3*time.Minute))
	if got := strings.Count(lifecycle.kinds(), "exit:"); got != 1 {
		t.Fatalf("a second pass must not settle twice, got %q", lifecycle.kinds())
	}
}

func TestASilentLiveSupervisorStaysSilentWhenOnlyItsHeartbeatsAreRead(t *testing.T) {
	manager := processChildManager(t, nil)
	childID, _ := seedProcessChild(t, manager, "op-silent-beat", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + time.Minute))

	queueWithoutReporting(t, manager, childID, procchild.KindHeartbeat, nil)
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + 2*time.Minute))
	item := itemFor(t, manager, childID)
	if item.Status != "running" {
		t.Fatalf("a heartbeat settles nothing, got %q", item.Status)
	}
	if item.Reporting != processReportingSilent {
		// Reading the queue is not hearing a report: the row keeps saying
		// that the reports stopped, which is still true.
		t.Fatalf("the row must stay marked silent, got %q", item.Reporting)
	}
	if pending := pendingInOutbox(t, manager, childID); pending != 0 {
		t.Fatalf("the heartbeat read must be acknowledged, got %d pending", pending)
	}
}
