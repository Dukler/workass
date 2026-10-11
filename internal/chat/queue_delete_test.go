package chat

import (
	"errors"
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

// 2026-10-11 QA: every failed background action read only "background action
// failed"; an agent could not tell a fan-out cap from a bad argument.
func TestFailedBackgroundActionKeepsABoundedReason(t *testing.T) {
	state, _ := NewState("chat")
	laneID := testLane("chat", "codex")
	state, _ = apply(t, state, InitializeChat{Presentation: PresentationState{TabID: "tab"}, OperationID: "create:test", Digest: "create-test"})
	state, _ = apply(t, state, SelectLane{Identity: laneID, Owner: provider.AttachmentOwner{TabID: "tab"}})
	state, _ = apply(t, state, LaneOpened{
		LaneID: laneID.ID, Thread: provider.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "coordinate", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"}})
	state, _ = apply(t, state, RequestBackgroundAction{Action: BackgroundAction{
		Kind: BackgroundSpawnAgent, OperationID: "spawn-op", TabID: "tab", ChatID: "chat",
		Owner: ProviderActivityOwner{LaneID: laneID.ID, OperationID: "turn", TurnID: "native-turn", ConnectionGeneration: 1},
		Spawn: &SpawnAgentAction{Prompt: "review"},
	}})
	reason := "subagent fan-out limit reached (8 running for this turn) " + strings.Repeat("x", 500)
	state, _ = apply(t, state, BackgroundActionFailed{OperationID: "spawn-op", Kind: provider.ErrorTransientTransport, Reason: reason})
	for _, entry := range state.Outbox {
		if entry.Kind != EffectBackground || entry.OperationID != "spawn-op" {
			continue
		}
		if entry.Status != OutboxFailed || !strings.HasPrefix(entry.FailureReason, "subagent fan-out limit reached") || len([]rune(entry.FailureReason)) > maxBackgroundFailureReason+1 {
			t.Fatalf("failed background action lost or did not bound its reason: %#v", entry)
		}
		return
	}
	t.Fatal("background action outbox entry missing")
}

// 2026-10-11 QA: an image sent to an agent without image support rendered as
// "connection lost", inviting a retry that can never succeed.
func TestUnsupportedInputRejectionIsAFailedTurnNotAConnectionLoss(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "mock")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Identity: lane, Thread: provider.ThreadRef{ProviderID: "mock", RootID: "t", HeadID: "t", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{OperationID: "img", Text: "look", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmissionFailed{OperationID: "img", Kind: provider.ErrorUnsupportedCapability})
	row := state.Ledger[len(state.Ledger)-1]
	if row.Interrupted || row.Terminal == nil || row.Terminal.Interrupted || !strings.Contains(row.Text, "does not support") {
		t.Fatalf("unsupported input rejection row = %#v", row)
	}
}

// 2026-10-11 QA chat-core B7 / lanes B-10: a failed start said only
// "transient_transport."; the provider's reason lived in the daemon log.
func TestFailedTurnAdmissionShowsTheProviderReason(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "mock")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Identity: lane, Thread: provider.ThreadRef{ProviderID: "mock", RootID: "t", HeadID: "t", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{OperationID: "big", Text: "look", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmissionFailed{OperationID: "big", Kind: provider.ErrorTransientTransport, Reason: "too many attached images; the maximum is 6"})
	row := state.Ledger[len(state.Ledger)-1]
	if !strings.Contains(row.Text, "too many attached images") || strings.Contains(row.Text, "transient_transport") {
		t.Fatalf("admission failure row = %q", row.Text)
	}
}

func TestCoordinatorFailureReasonIsRedactedAndBounded(t *testing.T) {
	coordinator := &Coordinator{}
	if got := coordinator.failureReason(errors.New("token=abc")); got != "" {
		t.Fatalf("reason without a redactor = %q, want none", got)
	}
	if err := coordinator.SetFailureReasonRedactor(func(text string) string { return strings.ReplaceAll(text, "abc", "[redacted]") }); err != nil {
		t.Fatal(err)
	}
	if got := coordinator.failureReason(errors.New("token=abc")); got != "token=[redacted]" {
		t.Fatalf("redacted reason = %q", got)
	}
	if got := coordinator.failureReason(errors.New(strings.Repeat("x", 5000))); len([]rune(got)) > maxBackgroundFailureReason+1 {
		t.Fatalf("reason not bounded: %d runes", len([]rune(got)))
	}
}

// 2026-10-11 QA lanes B-9: a switch whose target provider died at boot left
// the chat's selected provider on the dead target.
func TestFailedProviderSwitchReturnsToTheEstablishedLane(t *testing.T) {
	state, _ := NewState("chat")
	state, _, err := Reduce(state, InitializeChat{Presentation: PresentationState{TabID: "tab", ProviderID: "mock"}, OperationID: "create", Digest: "d"})
	if err != nil {
		t.Fatal(err)
	}
	established := testLane("chat", "mock")
	state, _ = apply(t, state, SelectLane{Identity: established, ModelID: "mock-model", ModeID: "ask"})
	state, _ = apply(t, state, LaneOpened{
		LaneID: established.ID, Identity: established, Thread: provider.ThreadRef{ProviderID: "mock", RootID: "t", HeadID: "t", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, UpdateRuntimeControls{ProviderID: "dead", ModelID: "dead-model", ReplaceModelID: true})
	target := testLane("chat", "dead")
	state, _ = apply(t, state, SelectLane{Identity: target})
	state, _ = apply(t, state, ClaimEffect{EffectID: createEffectID(target.ID, 1)})
	state, _ = apply(t, state, LaneOpenFailed{LaneID: target.ID, Kind: provider.ErrorTransientTransport, Reason: "exited at boot"})
	if state.DesiredLaneID != established.ID || state.Presentation.ProviderID != "mock" || state.Presentation.CurrentModelID != "mock-model" {
		t.Fatalf("failed switch half-switched the chat: desired=%s provider=%s model=%s", state.DesiredLaneID, state.Presentation.ProviderID, state.Presentation.CurrentModelID)
	}
}
