package acp

import (
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

// The owner's notice delay (Ajustes · Trabajos) moves when the chat is TOLD
// that a job stopped reporting, never when its row says so: a record must not
// look healthy while nothing has been heard, whatever was chosen about
// interruptions.
func TestALaterSilenceNoticeStillMarksTheRowAtTheWindow(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	manager.SetProcessSilenceNoticeAfter(10 * time.Minute)
	childID, _ := seedProcessChild(t, manager, "op-silent-later", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})

	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	marked := len(lifecycle.snapshot())
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + time.Minute))
	if item := itemFor(t, manager, childID); item.Reporting != processReportingSilent {
		t.Fatalf("the row is marked at the window whatever the notice waits for, got %q", item.Reporting)
	}
	if strings.Contains(lifecycle.kinds(), "not_reporting") {
		t.Fatalf("the chat is not told before the owner's delay, got %q", lifecycle.kinds())
	}
	events := lifecycle.snapshot()[marked:]
	if len(events) != 1 || events[0].Kind != ProcessLifecycleState || events[0].State == nil || events[0].State.Reporting != processReportingSilent {
		t.Fatalf("the mark must still travel to the owner's row as state, got %#v", events)
	}

	manager.reconcileProcessChildren(start.Add(5 * time.Minute))
	if strings.Contains(lifecycle.kinds(), "not_reporting") {
		t.Fatalf("still inside the owner's delay, got %q", lifecycle.kinds())
	}
	manager.reconcileProcessChildren(start.Add(10*time.Minute + time.Second))
	notices := lifecycle.kinds()
	if strings.Count(notices, "attention:not_reporting") != 1 {
		t.Fatalf("one notice once the delay has passed, got %q", notices)
	}
	manager.reconcileProcessChildren(start.Add(40 * time.Minute))
	if lifecycle.kinds() != notices {
		t.Fatalf("the notice is one-shot, got %q", lifecycle.kinds())
	}

	appendSpool(t, manager, childID, procchild.KindHeartbeat, nil)
	if item := itemFor(t, manager, childID); item.Reporting != "" {
		t.Fatalf("a report clears the mark, got %q", item.Reporting)
	}
}

// A delay shorter than the window cannot make the notice earlier than the
// mark: one lost report is still not an incident.
func TestASilenceNoticeShorterThanTheWindowChangesNothing(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	manager.SetProcessSilenceNoticeAfter(time.Minute)
	childID, _ := seedProcessChild(t, manager, "op-silent-short", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})

	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	manager.reconcileProcessChildren(start.Add(2 * procchild.HeartbeatInterval))
	if item := itemFor(t, manager, childID); item.Reporting != "" || strings.Contains(lifecycle.kinds(), "not_reporting") {
		t.Fatalf("inside the window nothing is said: reporting=%q notices=%q", item.Reporting, lifecycle.kinds())
	}
	manager.reconcileProcessChildren(start.Add(procchild.SilenceThreshold + time.Minute))
	if item := itemFor(t, manager, childID); item.Reporting != processReportingSilent {
		t.Fatalf("reporting = %q, want silent", item.Reporting)
	}
	if strings.Count(lifecycle.kinds(), "attention:not_reporting") != 1 {
		t.Fatalf("mark and notice together, as with no choice at all, got %q", lifecycle.kinds())
	}
}

// After a restart the notice waits for the later of the re-attach grace and
// the owner's delay.
func TestARestoredJobWaitsForTheLaterOfTheGraceAndTheOwnersDelay(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	manager.SetProcessSilenceNoticeAfter(30 * time.Minute)
	childID, _ := seedProcessChild(t, manager, "op-silent-restored", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	manager.spawnedWorkMu.Lock()
	for _, rec := range manager.spawnedWork {
		rec.LastReportAt = time.Time{}
	}
	manager.spawnedWorkMu.Unlock()

	start := time.Now().UTC()
	manager.reconcileProcessChildren(start)
	manager.reconcileProcessChildren(start.Add(2*procchild.SilenceThreshold + time.Minute))
	if item := itemFor(t, manager, childID); item.Reporting != processReportingSilent {
		t.Fatalf("the grace still marks the row when it ends, got %q", item.Reporting)
	}
	if strings.Contains(lifecycle.kinds(), "not_reporting") {
		t.Fatalf("the owner's delay is longer than the grace, got %q", lifecycle.kinds())
	}
	manager.reconcileProcessChildren(start.Add(30*time.Minute + time.Second))
	if strings.Count(lifecycle.kinds(), "attention:not_reporting") != 1 {
		t.Fatalf("told once the owner's delay has passed, got %q", lifecycle.kinds())
	}
}

func TestTheSilenceNoticeDelayIsBounded(t *testing.T) {
	manager := processChildManager(t, nil)
	manager.SetProcessSilenceNoticeAfter(-time.Minute)
	if got := manager.ProcessSilenceNoticeAfter(); got != 0 {
		t.Fatalf("a negative delay is no delay, got %s", got)
	}
	manager.SetProcessSilenceNoticeAfter(72 * time.Hour)
	if got := manager.ProcessSilenceNoticeAfter(); got != MaxProcessSilenceNoticeAfter {
		t.Fatalf("delay = %s, want the %s ceiling", got, MaxProcessSilenceNoticeAfter)
	}
}
