package acp

import (
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestUnregisterCrashedProviderLaneLeavesRunningJobRebindableButTerminalFenceIntact(t *testing.T) {
	manager := NewManager(Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	const (
		chatID    = "unregister-recovery-chat"
		sessionID = "unregister-recovery-thread"
		jobID     = "unregister-recovery-job"
	)
	crashed := newUnopenedManagerLaneForTest(t, manager, chatID, sessionID)
	if attached := <-crashed.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("crashed lane first event = %q, want lane attached", attached.Kind)
	}
	manager.bindProviderLaneJob(crashed, jobID, "unregister-recovery-operation")

	// A host crash unregisters the disposable attachment without terminally
	// settling the foreground job. The exact native thread can be resumed and
	// the same operation can be rebound to the replacement attachment.
	manager.unregisterProviderLane(crashed)
	if manager.providerLaneClosedJob(jobID) {
		t.Fatal("crashed lane unregister created a terminal closed-job tombstone")
	}
	if !manager.providerLaneManagedJob(jobID) {
		t.Fatal("crashed lane unregister forgot the running job's recovery identity")
	}
	if manager.providerLaneForJob(jobID) != nil {
		t.Fatal("crashed lane unregister left a stale live job attachment")
	}

	resumed := newUnopenedManagerLaneForTest(t, manager, chatID, sessionID)
	if attached := <-resumed.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("resumed lane first event = %q, want lane attached", attached.Kind)
	}
	manager.bindProviderLaneJob(resumed, jobID, "unregister-recovery-operation")
	if manager.providerLaneClosedJob(jobID) || manager.providerLaneForJob(jobID) != resumed {
		t.Fatalf("running job did not rebind to exact resumed lane: closed=%v lane=%p want=%p", manager.providerLaneClosedJob(jobID), manager.providerLaneForJob(jobID), resumed)
	}

	dataDone := make(chan error, 1)
	go func() {
		dataDone <- manager.observeProviderLaneJobEvent(map[string]any{
			"type": "data", "id": jobID, "stream": "stdout", "chunk": "readback remains attached",
		})
	}()
	dataEvent := <-resumed.Events()
	if dataEvent.Kind != providercontract.EventAssistantChunk {
		t.Fatalf("rebound job event = %q, want assistant chunk", dataEvent.Kind)
	}
	resumed.AcknowledgeDurableEvent(dataEvent.Identity.Sequence, nil)
	if err := <-dataDone; err != nil {
		t.Fatalf("rebound running job event was rejected: %v", err)
	}

	terminalDone := make(chan error, 1)
	go func() {
		terminalDone <- manager.observeProviderLaneJobEvent(map[string]any{
			"type": "end",
			"job": map[string]any{
				"id": jobID, "sessionId": sessionID, "status": "done", "result": "finished",
			},
		})
	}()
	terminalEvent := <-resumed.Events()
	if terminalEvent.Kind != providercontract.EventTurnTerminal {
		t.Fatalf("terminal rebound event = %q, want turn terminal", terminalEvent.Kind)
	}
	resumed.AcknowledgeDurableEvent(terminalEvent.Identity.Sequence, nil)
	if err := <-terminalDone; err != nil {
		t.Fatalf("terminal rebound event was rejected: %v", err)
	}
	if manager.providerLaneForJob(jobID) != nil || manager.providerLaneManagedJob(jobID) || !manager.providerLaneClosedJob(jobID) {
		t.Fatal("terminal job did not move from recoverable to closed exactly once")
	}

	for _, late := range []map[string]any{
		{"type": "data", "id": jobID, "stream": "stdout", "chunk": "late"},
		{"type": "start", "job": map[string]any{"id": jobID, "sessionId": sessionID, "status": "running"}},
		{"type": "acp", "id": jobID, "event": map[string]any{"kind": "late"}},
	} {
		err := manager.observeProviderLaneJobEvent(late)
		if err == nil || !strings.Contains(err.Error(), "closed provider job") {
			t.Fatalf("post-terminal callback was not rejected by the terminal fence: event=%#v err=%v", late, err)
		}
	}
}
