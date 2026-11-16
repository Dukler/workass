package chat

import (
	"testing"

	"workass/internal/provider"
)

// A Workass notice folded into a running turn (bug-20261116-7ab17943) is owed
// to the agent: it is never lost to a turn that ends around it, and it goes
// with a turn only when that turn is its own work's turn and is being stopped.

func runningNoticeChat(t *testing.T) State {
	t.Helper()
	state, _ := NewState("chat")
	lane := testLane("chat", "alpha")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	return state
}

func noticeSteer(origin string) Steer {
	return Steer{OperationID: "notice", Text: "<task-notification>done</task-notification>", Presentation: provider.TurnPresentation{
		Origin: "agent", QueueID: "notice", UserMessageID: "notice-user", AssistantMessageID: "notice-assistant",
		NoticeKind: "subagent", NoticeTitle: "deploy", NoticeStatus: "done", NoticeOrigin: origin,
	}}
}

func TestAConsumedNoticeSteerIsStillDrawnAsANotice(t *testing.T) {
	state := runningNoticeChat(t)
	state, _ = apply(t, state, noticeSteer(""))
	state, _ = apply(t, state, SteerAdmitted{OperationID: "notice", Accepted: true, Consumed: true})
	for _, event := range state.Ledger {
		if event.OperationID == "notice" {
			if event.NoticeKind != "subagent" || event.NoticeTitle != "deploy" || event.SteerState != "applied" {
				t.Fatalf("consumed notice row = %#v", event)
			}
			return
		}
	}
	t.Fatalf("no row for the consumed notice: %#v", state.Ledger)
}

func TestARefusedNoticeSteerGoesWithItsOwnStoppedTurnOnly(t *testing.T) {
	t.Run("its own turn is being stopped", func(t *testing.T) {
		state := runningNoticeChat(t)
		state, _ = apply(t, state, noticeSteer("turn"))
		state, _ = apply(t, state, ClaimEffect{EffectID: steerEffectID("notice")})
		state, _ = apply(t, state, CancelTurn{OperationID: "cancel"})
		state, _ = apply(t, state, SteerFailed{OperationID: "notice", Kind: provider.ErrorAdmissionRejected})
		if len(state.Queue) != 0 {
			t.Fatalf("a stopped turn's own notice outlived the stop: %#v", state.Queue)
		}
	})
	t.Run("an unrelated turn is being stopped", func(t *testing.T) {
		state := runningNoticeChat(t)
		state, _ = apply(t, state, noticeSteer("another-turn"))
		state, _ = apply(t, state, ClaimEffect{EffectID: steerEffectID("notice")})
		state, _ = apply(t, state, CancelTurn{OperationID: "cancel"})
		state, _ = apply(t, state, SteerFailed{OperationID: "notice", Kind: provider.ErrorAdmissionRejected})
		if len(state.Queue) != 1 || state.Queue[0].OperationID != "notice" {
			t.Fatalf("a notice about other work is still owed: %#v", state.Queue)
		}
	})
}

func TestANoticeSteerThatNeverLeftTheOutboxTakesTheNextTurn(t *testing.T) {
	t.Run("the turn finished first", func(t *testing.T) {
		state := runningNoticeChat(t)
		state, _ = apply(t, state, noticeSteer("turn"))
		state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "done"})
		if state.Foreground == nil || state.Foreground.OperationID != "notice" {
			t.Fatalf("an undelivered notice must start the next turn: foreground=%#v queue=%#v", state.Foreground, state.Queue)
		}
		if state.Foreground.Input.Presentation.NoticeKind == "" {
			t.Fatalf("the requeued notice lost its presentation: %#v", state.Foreground.Input)
		}
	})
	t.Run("the provider may have it", func(t *testing.T) {
		state := runningNoticeChat(t)
		state, _ = apply(t, state, noticeSteer(""))
		state, _ = apply(t, state, ClaimEffect{EffectID: steerEffectID("notice")})
		state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "done"})
		if state.Foreground != nil || len(state.Queue) != 0 {
			t.Fatalf("a notice the provider may already hold must never be sent twice: foreground=%#v queue=%#v", state.Foreground, state.Queue)
		}
	})
	t.Run("its own turn was stopped", func(t *testing.T) {
		state := runningNoticeChat(t)
		state, _ = apply(t, state, noticeSteer("turn"))
		state, _ = apply(t, state, CancelTurn{OperationID: "cancel"})
		state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "cancelled"})
		if state.Foreground != nil || len(state.Queue) != 0 {
			t.Fatalf("a stopped turn's own notice outlived the stop: foreground=%#v queue=%#v", state.Foreground, state.Queue)
		}
	})
}
