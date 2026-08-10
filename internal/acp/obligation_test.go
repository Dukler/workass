package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func obligationManager(t *testing.T, stateDir string, probe func([]string) (map[string][]int, bool)) *Manager {
	t.Helper()
	if stateDir == "" {
		stateDir = t.TempDir()
	}
	opts := Options{StateDir: stateDir, RuntimeProfile: "dev", SpawnedWorkReconcileInterval: time.Hour}
	if probe != nil {
		opts.SpawnedWorkPIDProbe = probe
	}
	manager := NewManager(opts)
	t.Cleanup(func() { manager.Reset() })
	return manager
}

func obligationState(t *testing.T, manager *Manager) string {
	t.Helper()
	rec := manager.ObligationFor("tab-1", "chat-1")
	if rec == nil {
		return ""
	}
	state, _ := rec["state"].(string)
	return state
}

// seedLiveLane installs one running background lane whose output file exists,
// so the evidence probe has something real to be asked about.
func seedLiveLane(t *testing.T, manager *Manager, taskID string) string {
	t.Helper()
	output := spawnedWorkTestOutput(t, taskID)
	seedBackgroundServiceBash(manager, taskID, output, "npm run build")
	return output
}

func TestHumanPromptOpensAndSupersedes(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	if got := obligationState(t, manager); got != obligationWorking {
		t.Fatalf("state = %q, want working", got)
	}
	manager.openObligation("tab-1", "chat-1", "msg-2")

	manager.obligationMu.Lock()
	receipts := manager.obligationReceipts[obligationKey("tab-1", "chat-1")]
	open := manager.obligations[obligationKey("tab-1", "chat-1")]
	manager.obligationMu.Unlock()
	if len(receipts) != 1 || receipts[0].CloseReason != obligationCloseSuperseded {
		t.Fatalf("first obligation should be kept as a superseded receipt: %#v", receipts)
	}
	if open.PromptID != "msg-2" {
		t.Fatalf("open obligation promptId = %q, want msg-2", open.PromptID)
	}
}

func TestUserCancelLeavesNoPhantomWorkingRecord(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionCancelled, Source: dispositionSourceNative,
	}, false)

	// A "working" record on a chat with no running turn is the phantom-row
	// shape that once vetoed a stuck chat's recovery. Cancelling must close it,
	// not leave it looking busy forever.
	if got := obligationState(t, manager); got != "" {
		t.Fatalf("state after cancel = %q, want no open obligation", got)
	}
	manager.obligationMu.Lock()
	receipts := manager.obligationReceipts[obligationKey("tab-1", "chat-1")]
	manager.obligationMu.Unlock()
	if len(receipts) != 1 || receipts[0].CloseReason != obligationCloseCancelled {
		t.Fatalf("cancel should keep a receipt: %#v", receipts)
	}
}

func TestUnknownWithNothingLiveReadsAsDone(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionUnknown, Source: dispositionSourceNative,
	}, false)
	if got := obligationState(t, manager); got != obligationDone {
		t.Fatalf("state = %q, want done: an undeclared provider must behave exactly as today", got)
	}
}

// The headline: this is the false-"Listo" the whole mechanism exists to fix.
func TestUnknownWithLiveWorkIsParkedNotDone(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionUnknown, Source: dispositionSourceNative,
	}, true)
	if got := obligationState(t, manager); got != obligationParked {
		t.Fatalf("state = %q, want parked: a turn that yielded while work is alive has not finished the request", got)
	}
}

func TestDeclaredDoneIsDemotedByLiveEvidence(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionDone, Source: dispositionSourceNative,
	}, true)
	if got := obligationState(t, manager); got != obligationParked {
		t.Fatalf("state = %q, want parked: the model cannot un-arm a wake that is genuinely pending", got)
	}
}

func TestDeclaredDoneWithNothingLiveIsDone(t *testing.T) {
	manager := obligationManager(t, "", nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionDone, Source: dispositionSourceNative,
	}, false)
	if got := obligationState(t, manager); got != obligationDone {
		t.Fatalf("state = %q, want done", got)
	}
}

func TestStaleRunningRowNeverVetoesADone(t *testing.T) {
	// The stuck-queue pin. A supported probe that ran and found nothing IS
	// evidence of absence; a row that merely still says "running" is not
	// allowed to hold the chat hostage.
	manager := obligationManager(t, "", func([]string) (map[string][]int, bool) {
		return map[string][]int{}, true
	})
	seedLiveLane(t, manager, "bash-stale")
	if manager.chatHasLiveParkEvidence("tab-1", "chat-1") {
		t.Fatal("a probed row whose supported probe found no pid must not count as evidence")
	}
}

func TestUnsupportedProbeKeepsTheLaneAsEvidence(t *testing.T) {
	// The Windows path. spawned_work_probe_other.go returns (nil, false), so
	// demanding a confirmed pid there would stall every parked lane on the
	// production laptop while it genuinely runs.
	manager := obligationManager(t, "", func([]string) (map[string][]int, bool) {
		return nil, false
	})
	seedLiveLane(t, manager, "bash-winlane")
	if !manager.chatHasLiveParkEvidence("tab-1", "chat-1") {
		t.Fatal("an unverifiable probe is not evidence of absence; the lane must still count")
	}
}

func TestParkedWithoutEvidenceStalls(t *testing.T) {
	manager := obligationManager(t, "", func([]string) (map[string][]int, bool) {
		return map[string][]int{}, true
	})
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{Disposition: DispositionParked}, false)

	manager.obligationMu.Lock()
	manager.obligations[obligationKey("tab-1", "chat-1")].ParkedSince =
		time.Now().Add(-stalledGrace - time.Minute).UTC().Format(time.RFC3339Nano)
	manager.obligationMu.Unlock()

	manager.sweepStalledObligations(time.Now())
	if got := obligationState(t, manager); got != obligationStalled {
		t.Fatalf("state = %q, want stalled: a park with nothing behind it is news, not silence", got)
	}
}

func TestParkedWithLiveEvidenceDoesNotStall(t *testing.T) {
	manager := obligationManager(t, "", func([]string) (map[string][]int, bool) {
		return nil, false
	})
	seedLiveLane(t, manager, "bash-alive")
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.settleObligation("tab-1", "chat-1", CompletionSignal{Disposition: DispositionParked}, false)
	manager.obligationMu.Lock()
	manager.obligations[obligationKey("tab-1", "chat-1")].ParkedSince =
		time.Now().Add(-stalledGrace - time.Minute).UTC().Format(time.RFC3339Nano)
	manager.obligationMu.Unlock()

	manager.sweepStalledObligations(time.Now())
	if got := obligationState(t, manager); got != obligationParked {
		t.Fatalf("state = %q, want parked: a probe that cannot answer must not raise a false alarm", got)
	}
}

func TestSelfResumeRescindsAClose(t *testing.T) {
	for _, closed := range []TurnDisposition{DispositionDone, DispositionParked} {
		manager := obligationManager(t, "", nil)
		manager.openObligation("tab-1", "chat-1", "msg-1")
		manager.settleObligation("tab-1", "chat-1", CompletionSignal{Disposition: closed}, false)
		manager.resumeObligation("tab-1", "chat-1")
		if got := obligationState(t, manager); got != obligationWorking {
			t.Fatalf("after self-resume from %q: state = %q, want working", closed, got)
		}
		if rec := manager.ObligationFor("tab-1", "chat-1"); rec["promptId"] != "msg-1" {
			t.Fatalf("self-resume must keep the original request, got promptId %v", rec["promptId"])
		}
	}
}

func TestBootStallsWorkingAndUnrecoverableParks(t *testing.T) {
	stateDir := t.TempDir()
	first := obligationManager(t, stateDir, nil)
	first.openObligation("tab-1", "chat-1", "msg-1") // stays working
	first.openObligation("tab-1", "chat-2", "msg-2")
	first.settleObligation("tab-1", "chat-2", CompletionSignal{Disposition: DispositionParked}, false)
	first.Reset()

	// A turn cannot survive the daemon, and a park whose evidence did not
	// reload died with the old process. Both are news the user has no other
	// way to discover.
	second := obligationManager(t, stateDir, func([]string) (map[string][]int, bool) {
		return map[string][]int{}, true
	})
	for _, chatID := range []string{"chat-1", "chat-2"} {
		rec := second.ObligationFor("tab-1", chatID)
		if rec == nil || rec["state"] != obligationStalled {
			t.Fatalf("%s after restart = %#v, want stalled", chatID, rec)
		}
	}
}

func TestObligationSurvivesASnapshotRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	first := obligationManager(t, stateDir, nil)
	first.openObligation("tab-1", "chat-1", "msg-1")
	first.settleObligation("tab-1", "chat-1", CompletionSignal{
		Disposition: DispositionNeedsInput, Source: dispositionSourceNative, Note: "waiting on you",
	}, false)
	first.Reset()

	second := obligationManager(t, stateDir, nil)
	rec := second.ObligationFor("tab-1", "chat-1")
	if rec == nil || rec["state"] != obligationNeedsInput || rec["source"] != dispositionSourceNative {
		t.Fatalf("reloaded obligation = %#v", rec)
	}
	if note, _ := rec["note"].(string); !strings.Contains(note, "waiting on you") {
		t.Fatalf("note lost across restart: %#v", rec)
	}
}

func TestDeletedChatKeepsNoObligation(t *testing.T) {
	stateDir := t.TempDir()
	manager := obligationManager(t, stateDir, nil)
	manager.openObligation("tab-1", "chat-1", "msg-1")
	manager.DropObligationsForChat("tab-1", "chat-1")
	if got := obligationState(t, manager); got != "" {
		t.Fatalf("state = %q, want none: a deleted chat cannot still owe an answer", got)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "obligations", "tab-1.json"))
	if err == nil && strings.Contains(string(data), "chat-1") {
		t.Fatalf("deleted chat still present on disk: %s", data)
	}
}
