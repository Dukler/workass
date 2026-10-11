package chat

import (
	"testing"

	"workass/internal/provider"
)

// 2026-10-11: agent messages promoted into the provider FIFO behind a
// long-running turn showed in the queue with a delete button that did
// nothing — the replacement only rewrote staged rows, so they came back.
func TestQueueReplacementDeletesPromotedRowsAndNeverRestagesThem(t *testing.T) {
	f := newProviderTurnFixture(t)
	f.state, _ = apply(t, f.state, Submit{OperationID: "running", Text: "long orchestration", Presentation: provider.TurnPresentation{Origin: "human"}})
	f.state, _ = apply(t, f.state, TurnAdmitted{OperationID: "running", Accepted: true, Turn: provider.TurnRef{OperationID: "running", NativeID: "job-run"}})
	for _, id := range []string{"a", "b"} {
		f.state, _ = apply(t, f.state, Submit{
			OperationID: provider.OperationID("agent-" + id), Text: "report " + id,
			Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "q:" + id},
		})
	}
	f.state, _ = apply(t, f.state, Submit{OperationID: "direct", Text: "typed while busy", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(f.state.Queue) != 3 {
		t.Fatalf("fixture queue = %#v", f.state.Queue)
	}

	// Echoing the visible queue back (an edit or reorder) re-stages nothing.
	f.state, _ = apply(t, f.state, ReplaceStagedQueue{
		OperationID: "echo", Digest: "d-echo", ExpectedRevision: f.state.Presentation.AgentQueueRevision,
		Entries: []StagedQueueEntry{{ID: "q:a", Text: "report a"}, {ID: "q:b", Text: "report b"}, {ID: "direct", Text: "typed while busy"}},
	})
	if len(f.state.StagedQueue) != 0 || len(f.state.Queue) != 3 {
		t.Fatalf("echoed promoted rows were re-staged: staged=%#v queue=%d", f.state.StagedQueue, len(f.state.Queue))
	}

	// Deleting "q:a" removes it from the provider FIFO.
	f.state, _ = apply(t, f.state, ReplaceStagedQueue{
		OperationID: "delete-a", Digest: "d-del", ExpectedRevision: f.state.Presentation.AgentQueueRevision,
		Entries: []StagedQueueEntry{{ID: "q:b", Text: "report b"}},
	})
	ids := []provider.OperationID{}
	for _, queued := range f.state.Queue {
		ids = append(ids, queued.OperationID)
	}
	if len(ids) != 2 || ids[0] != "agent-b" || ids[1] != "direct" {
		t.Fatalf("delete must remove exactly the omitted queue row (direct submits stay): %v", ids)
	}

	// A replacement that never saw a newer promotion cannot delete it.
	stale := f.state.Presentation.AgentQueueRevision
	f.state, _ = apply(t, f.state, Submit{OperationID: "agent-c", Text: "report c", Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "q:c"}})
	if _, _, err := Reduce(f.state, ReplaceStagedQueue{OperationID: "stale", Digest: "d-stale", ExpectedRevision: stale}); err == nil {
		t.Fatal("a stale replacement deleted a row it never saw")
	}

	// When the long turn ends, the deleted row never runs.
	effects := []Effect{}
	next, effects := apply(t, f.state, ProviderEventReceived{ConnectionGeneration: 1, Event: f.event("running", "job-run", completed())})
	f.state = next
	if len(effects) != 1 {
		t.Fatalf("next queued turn did not start: %#v", effects)
	}
	if start, ok := effects[0].(StartTurnEffect); !ok || start.Input.OperationID != "agent-b" {
		t.Fatalf("the deleted row or a wrong row started: %#v", effects[0])
	}
}
