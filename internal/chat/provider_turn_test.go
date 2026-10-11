package chat

import (
	"testing"

	"workass/internal/provider"
)

// providerTurnFixture is an idle chat on an established, attached lane — the
// state a Claude chat is in when a background task finishes after its turn.
type providerTurnFixture struct {
	t        *testing.T
	state    State
	lane     provider.LaneIdentity
	sequence uint64
}

func newProviderTurnFixture(t *testing.T) *providerTurnFixture {
	t.Helper()
	state, _ := NewState("chat")
	lane := testLane("chat", "claude")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "claude", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	f := &providerTurnFixture{t: t, state: state, lane: lane}
	// The turn that launched the background work: delivered, consumed, settled.
	f.state, _ = apply(t, f.state, Submit{OperationID: "launch", Text: "start the waiter", Presentation: provider.TurnPresentation{Origin: "human"}})
	f.state, _ = apply(t, f.state, TurnAdmitted{OperationID: "launch", Accepted: true, Turn: provider.TurnRef{OperationID: "launch", NativeID: "launch-job"}})
	f.emit("launch", "launch-job", provider.Event{Kind: provider.EventInputConsumed, Input: &provider.InputEvent{OperationID: "launch"}})
	f.emit("launch", "launch-job", chunk("waiting in the background"))
	f.emit("launch", "launch-job", completed())
	if got := f.state.Lanes[lane.ID]; got.Phase != LaneReady || got.InitialSeedPending || len(f.state.Ledger) != 2 {
		t.Fatalf("fixture is not an idle established chat: lane=%#v ledger=%d", got, len(f.state.Ledger))
	}
	return f
}

func (f *providerTurnFixture) event(operationID provider.OperationID, turnID string, event provider.Event) provider.Event {
	f.sequence++
	event.Identity = provider.EventIdentity{
		ChatID: "chat", LaneID: f.lane.ID, OperationID: operationID, TurnID: turnID,
		Sequence: f.sequence, ObservedAtUnixMS: 1_791_677_100_000 + int64(f.sequence),
	}
	return event
}

func (f *providerTurnFixture) emit(operationID provider.OperationID, turnID string, event provider.Event) []Effect {
	f.t.Helper()
	next, effects := apply(f.t, f.state, ProviderEventReceived{ConnectionGeneration: 1, Event: f.event(operationID, turnID, event)})
	f.state = next
	return effects
}

func (f *providerTurnFixture) start(nativeID string) provider.OperationID {
	f.t.Helper()
	operationID := provider.ProviderTurnOperationID(nativeID)
	effects := f.emit(operationID, nativeID, provider.Event{
		Kind:      provider.EventProviderTurnStarted,
		Admission: &provider.TurnAdmission{Turn: provider.TurnRef{OperationID: operationID, NativeID: nativeID}, Accepted: true},
	})
	if len(effects) != 0 {
		f.t.Fatalf("a provider-started turn must never produce a delivery: %#v", effects)
	}
	return operationID
}

func chunk(text string) provider.Event {
	return provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: text}}
}

func completed() provider.Event {
	return provider.Event{Kind: provider.EventTurnTerminal, Terminal: &provider.TerminalEvent{Status: "completed", StopReason: "end_turn"}}
}

func TestProviderStartedTurnIsOwnedAndSettlesLikeAnyTurn(t *testing.T) {
	f := newProviderTurnFixture(t)
	operationID := f.start("harness-turn-1")
	foreground := f.state.Foreground
	if foreground == nil || foreground.OperationID != operationID || foreground.Status != ForegroundRunning || !foreground.UserConsumed || foreground.Turn.NativeID != "harness-turn-1" {
		t.Fatalf("provider-started turn did not become the running foreground: %#v", foreground)
	}
	if f.state.Lanes[f.lane.ID].Phase != LaneRunning || f.state.Obligation == nil || f.state.Obligation.State != "working" {
		t.Fatalf("lane/obligation did not enter the running turn: lane=%q obligation=%#v", f.state.Lanes[f.lane.ID].Phase, f.state.Obligation)
	}
	f.emit(operationID, "harness-turn-1", chunk("HG2 reached 4000. "))
	f.emit(operationID, "harness-turn-1", provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "tool", Status: "running", Title: "Read progress"}})
	f.emit(operationID, "harness-turn-1", chunk("Numbers below."))
	if effects := f.emit(operationID, "harness-turn-1", completed()); len(effects) != 0 {
		t.Fatalf("settling a provider-started turn scheduled work: %#v", effects)
	}
	if f.state.Foreground != nil {
		t.Fatalf("terminal left the provider-started turn running: %#v", f.state.Foreground)
	}
	if len(f.state.Ledger) != 3 {
		t.Fatalf("a provider-started turn must add exactly its assistant row, got %#v", f.state.Ledger)
	}
	row := f.state.Ledger[2]
	if row.Role != "assistant" || row.Text != "HG2 reached 4000. Numbers below." || row.Status != "done" || row.OperationID != operationID || row.NativeTurnID != "harness-turn-1" {
		t.Fatalf("assistant row = %#v", row)
	}
	if len(row.Timeline) != 1 || row.Timeline[0].Tool == nil || row.Timeline[0].Tool.Status != "completed" {
		t.Fatalf("tool activity did not settle with the turn: %#v", row.Timeline)
	}
	lane := f.state.Lanes[f.lane.ID]
	if lane.Phase != LaneReady || lane.CoveredThrough != 3 || lane.Coverage[3].Status != CoverageNativeSeen {
		t.Fatalf("provider's own reply must count as already seen by its lane: %#v", lane)
	}

	// The next prompt goes to the same live session with nothing replayed.
	_, effects := apply(t, f.state, Submit{OperationID: "next", Text: "thanks", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("next prompt did not start: %#v", effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok || start.Input.OperationID != "next" || start.Input.DeltaThrough != 0 || len(start.Seed.Messages) != 0 {
		t.Fatalf("next prompt must not replay the provider's own reply: %#v", effects[0])
	}
}

func TestHumanPromptQueuesBehindProviderStartedTurn(t *testing.T) {
	f := newProviderTurnFixture(t)
	operationID := f.start("harness-turn-1")
	next, effects := apply(t, f.state, Submit{OperationID: "human", Text: "status?", Presentation: provider.TurnPresentation{Origin: "human"}})
	f.state = next
	if len(effects) != 0 || len(f.state.Queue) != 1 || f.state.Foreground.OperationID != operationID {
		t.Fatalf("human prompt must wait for the running provider turn: effects=%#v queue=%#v", effects, f.state.Queue)
	}
	effects = f.emit(operationID, "harness-turn-1", completed())
	if len(effects) != 1 {
		t.Fatalf("queued prompt did not start at the provider turn's boundary: %#v", effects)
	}
	if start, ok := effects[0].(StartTurnEffect); !ok || start.Input.OperationID != "human" {
		t.Fatalf("queued start = %#v", effects[0])
	}
}

func TestBusyChatDeclinesProviderStartedTurnWithoutProtocolFailure(t *testing.T) {
	f := newProviderTurnFixture(t)
	f.state, _ = apply(t, f.state, Submit{OperationID: "human", Text: "go", Presentation: provider.TurnPresentation{Origin: "human"}})
	f.state, _ = apply(t, f.state, TurnAdmitted{OperationID: "human", Accepted: true, Turn: provider.TurnRef{OperationID: "human", NativeID: "human-job"}})

	operationID := f.start("harness-turn-1")
	if f.state.Foreground.OperationID != "human" {
		t.Fatalf("provider-started turn displaced the running prompt: %#v", f.state.Foreground)
	}
	if _, reserved := f.state.Operations[operationID]; reserved {
		t.Fatal("a declined provider turn must not reserve its operation")
	}
	// Its output is observed but owned by nobody — never a reason to break the lane.
	f.emit(operationID, "harness-turn-1", chunk("declined output"))
	f.emit(operationID, "harness-turn-1", completed())
	if f.state.Foreground == nil || f.state.Foreground.OperationID != "human" || f.state.Foreground.AssistantContent != "" {
		t.Fatalf("declined provider output leaked into the running prompt: %#v", f.state.Foreground)
	}
	if len(f.state.Ledger) != 2 || f.state.Lanes[f.lane.ID].Phase != LaneRunning {
		t.Fatalf("declined provider turn changed the ledger or lane: ledger=%#v lane=%q", f.state.Ledger, f.state.Lanes[f.lane.ID].Phase)
	}

	// The streamed-event fast path must agree with the full reducer.
	for _, event := range []provider.Event{
		chunk("more declined output"),
		{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "declined-tool", Status: "running"}},
		{Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: "thinking"}},
	} {
		next, err := reduceJournaledProviderEvent(f.state, ProviderEventReceived{ConnectionGeneration: 1, Event: f.event(operationID, "harness-turn-1", event)})
		if err != nil {
			t.Fatalf("journaled %s for a declined provider turn failed: %v", event.Kind, err)
		}
		f.state = next
	}
	if _, owned := f.state.Tools["declined-tool"]; owned {
		t.Fatal("a declined provider turn acquired tool ownership")
	}
}

func TestProviderStartedTurnIsDeclinedOffTheActiveEstablishedLane(t *testing.T) {
	f := newProviderTurnFixture(t)
	f.state.ActiveLaneID = ""
	f.start("harness-turn-1")
	if f.state.Foreground != nil {
		t.Fatalf("a provider turn on an inactive lane must not take over the chat: %#v", f.state.Foreground)
	}
}

func TestProviderStartedTurnOperationIsNeverReused(t *testing.T) {
	f := newProviderTurnFixture(t)
	operationID := f.start("harness-turn-1")
	f.emit(operationID, "harness-turn-1", completed())
	f.start("harness-turn-1")
	if f.state.Foreground != nil {
		t.Fatalf("a settled provider turn identity was admitted twice: %#v", f.state.Foreground)
	}
}

func TestHostLossDuringProviderStartedTurnInterruptsWithoutResend(t *testing.T) {
	f := newProviderTurnFixture(t)
	operationID := f.start("harness-turn-1")
	f.emit(operationID, "harness-turn-1", chunk("partial"))
	next, effects := apply(t, f.state, HostLost{LaneID: f.lane.ID, ConnectionGeneration: 1})
	for _, effect := range effects {
		if _, resend := effect.(StartTurnEffect); resend {
			t.Fatalf("host loss resent a turn nobody delivered: %#v", effects)
		}
	}
	if next.Foreground != nil {
		t.Fatalf("host loss left the provider turn running: %#v", next.Foreground)
	}
	if len(next.Ledger) == 0 || next.Ledger[len(next.Ledger)-1].Text != "partial" {
		t.Fatalf("partial provider output was not preserved: %#v", next.Ledger)
	}
}

// 2026-10-10 19:52 local: the first turn of a deferred Claude lane ended with
// no consumption receipt. Terminal marked the still-provisional lane ready, an
// invalid state, and the chat tore the session down.
func TestTerminalWithoutReceiptKeepsDeferredCandidateAndDeliversNextInputToIt(t *testing.T) {
	state, _ := NewState("chat")
	lane := testLane("chat", "claude")
	creation := provider.CreationCapabilities{DeferredUntilInput: true}
	delivery := provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
	candidate := provider.ThreadRef{ProviderID: "claude", RootID: "candidate", HeadID: "candidate", Lineage: 1}
	state, _ = apply(t, state, SelectLane{Identity: lane, Creation: creation})
	state, _ = apply(t, state, Submit{OperationID: "first", Text: "hello", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, LaneProvisioned{
		LaneID: lane.ID, Identity: lane, Candidate: candidate, ConnectionGeneration: 1,
		Context: exactContext(provider.ContextImportUnsupported), Delivery: delivery, Creation: creation,
	})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "first", Accepted: true, Turn: provider.TurnRef{OperationID: "first", NativeID: "job-1"}})
	state, _ = apply(t, state, ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind:     provider.EventTurnTerminal,
		Identity: provider.EventIdentity{ChatID: "chat", LaneID: lane.ID, OperationID: "first", TurnID: "job-1", Sequence: 1, ObservedAtUnixMS: 1},
		Terminal: &provider.TerminalEvent{Status: "failed", Error: "boom"},
	}})
	settled := state.Lanes[lane.ID]
	if state.Foreground != nil || settled.Provision == nil || !settled.Provision.Equal(candidate) || !settled.Thread.IsZero() || settled.Phase != LaneCreating {
		t.Fatalf("receiptless terminal must leave the attached candidate creating: foreground=%#v lane=%#v", state.Foreground, settled)
	}
	_, effects := apply(t, state, Submit{OperationID: "second", Text: "again", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("next input did not start: %#v", effects)
	}
	if start, ok := effects[0].(StartTurnEffect); !ok || start.Input.OperationID != "second" || start.LaneID != lane.ID {
		t.Fatalf("next input must go to the same candidate lane: %#v", effects[0])
	}
}
