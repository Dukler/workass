package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"workass/internal/provider"
)

func handoffNotice(operationID provider.OperationID) Submit {
	return Submit{
		OperationID: operationID, Text: "<task-notification>job ended</task-notification>",
		Presentation: provider.TurnPresentation{
			Origin: "agent", NoticeKind: "subagent", NoticeTitle: "job", NoticeStatus: "done",
			UserMessageID: "u-" + string(operationID), AssistantMessageID: "a-" + string(operationID), QueueID: string(operationID),
		},
	}
}

func admitForeground(t *testing.T, engine *Engine, operationID provider.OperationID) {
	t.Helper()
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim %s: ok=%v err=%v", operationID, ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: operationID, Accepted: true,
		Turn: provider.TurnRef{OperationID: operationID, NativeID: "native-" + string(operationID)},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestartRedeliversAnInterruptedNoticeAndStopsAtTheLimit(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(handoffNotice("notice")); err != nil {
		t.Fatal(err)
	}
	operation := provider.OperationID("notice")
	for attempt := 1; attempt <= noticeRedeliveryLimit; attempt++ {
		admitForeground(t, engine, operation)
		if engine, err = NewDurableEngine("chat", store); err != nil {
			t.Fatal(err)
		}
		state := engine.Snapshot()
		want := provider.OperationID("notice" + noticeRedeliverySuffix + string(rune('0'+attempt)))
		// The lane resumes before the copy can start, so it waits as the head.
		if state.Foreground != nil {
			t.Fatalf("attempt %d: interrupted turn kept its foreground: %#v", attempt, state.Foreground)
		}
		if len(state.Queue) != 1 || state.Queue[0].OperationID != want {
			t.Fatalf("attempt %d: queue = %#v, want head %s", attempt, state.Queue, want)
		}
		if state.Queue[0].Text != handoffNotice("notice").Text {
			t.Fatalf("attempt %d: notice text changed: %q", attempt, state.Queue[0].Text)
		}
		openRedeliveryLane(t, engine, lane)
		operation = want
	}
	admitForeground(t, engine, operation)
	if engine, err = NewDurableEngine("chat", store); err != nil {
		t.Fatal(err)
	}
	if state := engine.Snapshot(); len(state.Queue) != 0 {
		t.Fatalf("redelivery passed its limit: %#v", state.Queue)
	}
}

// openRedeliveryLane executes the resume recovery asked for and leaves the lane
// ready, the way the restarted coordinator would.
func openRedeliveryLane(t *testing.T, engine *Engine, lane provider.LaneIdentity) {
	t.Helper()
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim resume: ok=%v err=%v", ok, err)
	}
	resume, isResume := effect.(ResumeLaneEffect)
	if !isResume {
		t.Fatalf("expected the exact resume first, got %T", effect)
	}
	if err := engine.Apply(LaneOpened{
		LaneID: lane.ID, Identity: resume.Identity, Thread: resume.Thread, ConnectionGeneration: resume.Generation,
		Context:  provider.ContextCapabilities{ExactResume: true},
		Delivery: provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAPersonsInterruptedTurnIsNotRepeated(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	openReadyDurableLane(t, engine, testLane("chat", "codex"))
	if err := engine.Apply(Submit{OperationID: "op", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	admitForeground(t, engine, "op")
	if engine, err = NewDurableEngine("chat", store); err != nil {
		t.Fatal(err)
	}
	if state := engine.Snapshot(); len(state.Queue) != 0 || state.Foreground != nil {
		t.Fatalf("a person's turn was repeated: %#v", state.Queue)
	}
}

func TestAStoppedNoticeIsNotRedelivered(t *testing.T) {
	state := &State{
		Operations: map[provider.OperationID]struct{}{},
		Foreground: &ForegroundTurn{
			OperationID: "notice", Turn: provider.TurnRef{OperationID: "notice", NativeID: "native"},
			Input: QueueEntry{OperationID: "notice", Presentation: handoffNotice("notice").Presentation},
		},
		Outbox: []OutboxEntry{{Kind: EffectCancelTurn, Turn: provider.TurnRef{NativeID: "native"}, Status: OutboxCompleted}},
	}
	requeueInterruptedNotice(state)
	if len(state.Queue) != 0 {
		t.Fatalf("a stopped notice was sent again: %#v", state.Queue)
	}
	state.Outbox = nil
	requeueInterruptedNotice(state)
	if len(state.Queue) != 1 || !strings.HasSuffix(string(state.Queue[0].OperationID), noticeRedeliverySuffix+"1") {
		t.Fatalf("the same notice without a Stop was not sent again: %#v", state.Queue)
	}
}

func TestADeferredStartIsSentOnceAfterRestartWithNoFailedRows(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	openReadyDurableLane(t, engine, testLane("chat", "codex"))
	if err := engine.Apply(handoffNotice("notice")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnStartDeferred{OperationID: "notice"}); err != nil {
		t.Fatal(err)
	}
	if engine, err = NewDurableEngine("chat", store); err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	if state.Foreground == nil || state.Foreground.OperationID != "notice" || len(state.Queue) != 0 {
		t.Fatalf("deferred start lost its owner: %#v queue %#v", state.Foreground, state.Queue)
	}
	for _, row := range state.Ledger {
		if row.OperationID == "notice" {
			t.Fatalf("a turn that never started left a row: %#v", row)
		}
	}
	if !outboxHas(&state, startTurnEffectID("notice"), OutboxPending) {
		t.Fatalf("deferred start is not pending: %#v", state.Outbox)
	}
}

func handoffCoordinator(t *testing.T) (*Engine, *Coordinator, *coordinatorLane) {
	t.Helper()
	engine, err := NewEngine("handoff-chat")
	if err != nil {
		t.Fatal(err)
	}
	factory := &coordinatorFactory{}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	identity := coordinatorLaneIdentity("handoff-chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return engine, coordinator, factory.lanes[identity.ID]
}

func TestAnUpdateHandoffHoldsATurnInsteadOfFailingIt(t *testing.T) {
	engine, coordinator, lane := handoffCoordinator(t)
	held := true
	coordinator.SetTurnStartHold(func() bool { return held })
	started := make(chan struct{}, 1)
	lane.delivery.startStarted = started
	if err := engine.Apply(handoffNotice("notice")); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || executed {
		t.Fatalf("a held turn was claimed: executed=%v err=%v", executed, err)
	}
	held = false
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("a released turn did not start: executed=%v err=%v", executed, err)
	}
	<-started
}

func TestARefusalForTheHandoffOrDuringCloseSettlesNothing(t *testing.T) {
	for name, setup := range map[string]func(*Coordinator, *coordinatorLane){
		"refused for the handoff": func(_ *Coordinator, lane *coordinatorLane) {
			lane.delivery.startErr = &provider.Error{Kind: provider.ErrorTurnStartDeferred, Message: "handoff"}
		},
		"session closed under an effect already in flight": func(coordinator *Coordinator, lane *coordinatorLane) {
			lane.delivery.startErr = errors.New("service-tier session is not attached")
			lane.delivery.startHook = func() { coordinator.stopping.Store(true) }
		},
	} {
		t.Run(name, func(t *testing.T) {
			engine, coordinator, lane := handoffCoordinator(t)
			setup(coordinator, lane)
			if err := engine.Apply(handoffNotice("notice")); err != nil {
				t.Fatal(err)
			}
			if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
				t.Fatalf("execute: executed=%v err=%v", executed, err)
			}
			state := engine.Snapshot()
			if state.Foreground == nil || state.Foreground.OperationID != "notice" {
				t.Fatalf("the turn was settled: %#v", state.Foreground)
			}
			if !outboxHas(&state, startTurnEffectID("notice"), OutboxPending) {
				t.Fatalf("the start did not go back to pending: %#v", state.Outbox)
			}
			for _, row := range state.Ledger {
				if row.OperationID == "notice" {
					t.Fatalf("a refused start left a failed row: %#v", row)
				}
			}
		})
	}
}

func TestACloseEndedTurnIsRecordedAsTheDaemonStoppingNotAFailure(t *testing.T) {
	engine, coordinator, lane := handoffCoordinator(t)
	if err := engine.Apply(handoffNotice("notice")); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("start: executed=%v err=%v", executed, err)
	}
	coordinator.stopping.Store(true)
	lane.send(1, "notice", provider.EventTurnTerminal, func(event *provider.Event) {
		event.Terminal = &provider.TerminalEvent{Status: "failed", Error: "ACP session closed"}
	})
	for i := 0; i < 200; i++ {
		if state := engine.Snapshot(); state.Foreground == nil {
			found := false
			for _, entry := range state.Queue {
				found = found || strings.HasPrefix(string(entry.OperationID), "notice"+noticeRedeliverySuffix)
			}
			if !found {
				t.Fatalf("the notice ended by Close was not queued again: %#v", state.Queue)
			}
			return
		} else if state.Foreground.OperationID != "notice" {
			return // already promoted to the redelivery
		}
		sleepBriefly()
	}
	t.Fatal("terminal was never applied")
}

func sleepBriefly() { time.Sleep(5 * time.Millisecond) }
