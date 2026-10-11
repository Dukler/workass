package chat

import (
	"strings"
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

// A lane that fails to open must not strand queued rows: each failure settles
// the next waiting input, queued or not, and nothing retries on its own.
func TestFailedLaneOpenSettlesTheNextQueuedRowInsteadOfWedging(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "mock")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, Submit{OperationID: "a", Text: "first", Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "q:a"}})
	state, _ = apply(t, state, Submit{OperationID: "b", Text: "second", Presentation: provider.TurnPresentation{Origin: "agent", QueueID: "q:b"}})
	state, effects := apply(t, state, LaneOpenFailed{LaneID: lane.ID, Kind: provider.ErrorTransientTransport})
	if len(effects) != 0 {
		t.Fatalf("a failed open must not retry on its own: %#v", effects)
	}
	if len(state.Queue) != 1 || state.Queue[0].OperationID != "b" {
		t.Fatalf("the failed open did not settle the next queued row: %#v", state.Queue)
	}
	if len(state.Ledger) != 2 || state.Ledger[0].OperationID != "a" || state.Ledger[1].Status != "failed" {
		t.Fatalf("settled queued row is not visible as failed: %#v", state.Ledger)
	}
	_, effects = apply(t, state, Submit{OperationID: "c", Text: "user retries", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("the next send did not reopen the lane: %#v", effects)
	}
	if _, ok := effects[0].(CreateLaneEffect); !ok {
		t.Fatalf("reopen effect = %#v", effects[0])
	}
}

// 2026-10-11 QA: a second turn reusing a message id was admitted, then every
// projection of the chat failed with "duplicate message id" until it was
// force-deleted. Reuse is refused at admission instead.
func TestReusedMessageIDIsRefusedAtAdmission(t *testing.T) {
	f := newProviderTurnFixture(t)
	f.state, _ = apply(t, f.state, Submit{OperationID: "one", Text: "first", Presentation: provider.TurnPresentation{
		Origin: "human", UserMessageID: "dup-user", AssistantMessageID: "dup-assistant",
	}})
	for _, presentation := range []provider.TurnPresentation{
		{Origin: "human", UserMessageID: "dup-user"},
		{Origin: "human", UserMessageID: "fresh", AssistantMessageID: "dup-assistant"},
		{Origin: "human", UserMessageID: "dup-assistant"},
		{Origin: "human", UserMessageID: "same", AssistantMessageID: "same"},
	} {
		if _, _, err := Reduce(f.state, Submit{OperationID: "two", Text: "second", Presentation: presentation}); err == nil {
			t.Fatalf("reused message ids were admitted: %#v", presentation)
		}
	}
	if _, _, err := Reduce(f.state, Submit{OperationID: "two", Text: "second", Presentation: provider.TurnPresentation{
		Origin: "human", UserMessageID: "new-user", AssistantMessageID: "new-assistant",
	}}); err != nil {
		t.Fatalf("fresh ids were refused: %v", err)
	}
}

// 2026-10-11 QA: a Workass failure notice ("The provider could not start this
// turn…") was handed to the next provider as something its assistant said.
func TestWorkassNoticesAreNeverProviderContext(t *testing.T) {
	state, _ := NewState("chat")
	laneA := testLane("chat", "mock")
	state, _ = apply(t, state, SelectLane{Identity: laneA})
	state, _ = apply(t, state, Submit{OperationID: "a1", Text: "alpha request", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, LaneOpenFailed{LaneID: laneA.ID, Kind: provider.ErrorUnsupportedCapability})
	notices := 0
	for _, row := range state.Ledger {
		if row.WorkassNotice {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("the admission failure notice is not marked: %#v", state.Ledger)
	}
	laneB := testLane("chat", "mock-b")
	state, _ = apply(t, state, SelectLane{Identity: laneB})
	state, _ = apply(t, state, LaneOpened{
		LaneID: laneB.ID, Identity: laneB, Thread: provider.ThreadRef{ProviderID: "mock-b", RootID: "b", HeadID: "b", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	_, effects := apply(t, state, Submit{OperationID: "b1", Text: "beta request", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("turn on the second provider did not start: %#v", effects)
	}
	start := effects[0].(StartTurnEffect)
	for _, message := range start.Seed.Messages {
		if strings.Contains(message.Text, "could not start this turn") {
			t.Fatalf("a Workass notice was sent as provider context: %#v", start.Seed.Messages)
		}
	}
	found := false
	for _, message := range start.Seed.Messages {
		found = found || strings.Contains(message.Text, "alpha request")
	}
	if !found {
		t.Fatalf("the user's own earlier request was dropped from context: %#v", start.Seed.Messages)
	}
}
