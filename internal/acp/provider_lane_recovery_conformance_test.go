package acp

import (
	"strings"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestUnregisterCrashedProviderLaneFencesOperationWithoutRebinding(t *testing.T) {
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

	// A host crash unregisters the disposable attachment and permanently fences
	// that operation. A later distinct prompt may resume the native thread, but
	// this accepted input must never be rebound or replayed.
	manager.unregisterProviderLane(crashed)
	if !manager.providerLaneClosedJob(jobID) {
		t.Fatal("crashed lane unregister omitted the terminal operation fence")
	}
	if !manager.providerLaneManagedJob(jobID) {
		t.Fatal("crashed lane unregister forgot the identity before its UI receipt")
	}
	if manager.providerLaneForJob(jobID) != nil {
		t.Fatal("crashed lane unregister left a stale live job attachment")
	}

	resumed := newUnopenedManagerLaneForTest(t, manager, chatID, sessionID)
	if attached := <-resumed.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("resumed lane first event = %q, want lane attached", attached.Kind)
	}
	manager.bindProviderLaneJob(resumed, jobID, "unregister-recovery-operation")
	if !manager.providerLaneClosedJob(jobID) || manager.providerLaneForJob(jobID) != nil {
		t.Fatalf("closed operation rebound to a replacement attachment: closed=%v lane=%p", manager.providerLaneClosedJob(jobID), manager.providerLaneForJob(jobID))
	}

	for _, late := range []map[string]any{
		{"type": "data", "id": jobID, "stream": "stdout", "chunk": "late"},
		{"type": "start", "job": map[string]any{"id": jobID, "sessionId": sessionID, "status": "running"}},
		{"type": "acp", "id": jobID, "event": map[string]any{"kind": "late"}},
		{"type": "end", "job": map[string]any{"id": jobID, "sessionId": sessionID, "status": "done"}},
	} {
		err := manager.observeProviderLaneJobEvent(late)
		if err == nil || !strings.Contains(err.Error(), "closed provider job") {
			t.Fatalf("post-terminal callback was not rejected by the terminal fence: event=%#v err=%v", late, err)
		}
	}
	if err := manager.observeProviderLaneJobEvent(map[string]any{
		"type": "end", "job": map[string]any{
			"id": jobID, "sessionId": sessionID, "status": "failed", "crashInterrupted": true,
		},
	}); err != nil {
		t.Fatalf("crash UI terminal receipt was rejected: %v", err)
	}
	if manager.providerLaneManagedJob(jobID) {
		t.Fatal("crash UI terminal receipt retained the old managed operation")
	}
}
