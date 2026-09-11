package chat

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"workass/internal/provider"
)

func deltaFixture(t *testing.T) (State, provider.LaneIdentity, provider.LaneIdentity) {
	t.Helper()
	state, _ := NewState("chat")
	first, other := testLane("chat", "first"), testLane("chat", "other")
	for i, identity := range []provider.LaneIdentity{first, other} {
		state, _ = apply(t, state, SelectLane{Identity: identity})
		state, _ = apply(t, state, LaneOpened{LaneID: identity.ID,
			Thread:               provider.ThreadRef{ProviderID: identity.Realm.ProviderID, RootID: string(identity.ID), HeadID: string(identity.ID), Lineage: 1},
			ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
			Attachment: &provider.LaneAttachmentSnapshot{ConnectionID: string(identity.ID), ProviderID: identity.Realm.ProviderID},
		})
		op := provider.OperationID([]string{"first-input", "other-input"}[i])
		state, _ = apply(t, state, Submit{OperationID: op, Text: string(op), Presentation: provider.TurnPresentation{Origin: "human"}})
		state, _ = apply(t, state, TurnCompleted{OperationID: op, Assistant: string(op) + "-answer"})
	}
	state, effects := apply(t, state, SelectLane{Identity: first})
	if len(effects) != 0 || state.ActiveLaneID != other.ID {
		t.Fatal("idle selection must wait for a real request")
	}
	return state, first, other
}

func submitDelta(t *testing.T, state State, op string) (State, StartTurnEffect) {
	t.Helper()
	state, effects := apply(t, state, Submit{OperationID: provider.OperationID(op), Text: op, Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 1 {
		t.Fatalf("expected one delivery, got %#v", effects)
	}
	start, ok := effects[0].(StartTurnEffect)
	if !ok {
		t.Fatalf("expected exact-lane start, got %T", effects[0])
	}
	return state, start
}

func TestContextDeltaReturnsToExactThreadWithOnlyMissingMessages(t *testing.T) {
	state, first, _ := deltaFixture(t)
	thread := state.Lanes[first.ID].Thread
	state, start := submitDelta(t, state, "back")
	if len(start.Seed.Messages) != 2 || start.Seed.Messages[0].Text != "other-input" || start.Seed.Messages[1].Text != "other-input-answer" || start.Input.InitialSeedDigest != "" || start.Input.DeltaDigest == "" {
		t.Fatalf("delta included covered history or lost missing messages: %#v", start)
	}
	if state.Lanes[first.ID].Thread != thread || state.Lanes[first.ID].CoveredThrough != 2 {
		t.Fatal("dispatch changed thread or prematurely claimed coverage")
	}
	// Persisted reconstruction must retain the immutable batch rather than
	// recomputing it from whatever history is current after recovery.
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var restored State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil {
		t.Fatal(err)
	}
	entry := restored.Outbox[len(restored.Outbox)-1]
	rebuilt, err := effectFromOutbox(restored, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(start, rebuilt) {
		t.Fatal("persisted delivery changed its batch")
	}
	state, _ = apply(t, restored, InputConsumed{OperationID: "back"})
	state, _ = apply(t, state, InputConsumed{OperationID: "back"})
	if state.Lanes[first.ID].Coverage[3].Status != CoveragePromptSeen || state.Lanes[first.ID].CoveredThrough != 5 {
		t.Fatal("consumption did not commit exact delta coverage once")
	}
	state, _ = apply(t, state, TurnCompleted{OperationID: "back", Assistant: "back-answer"})
	_, next := submitDelta(t, state, "same-provider")
	if len(next.Seed.Messages) != 0 || next.Input.DeltaDigest != "" {
		t.Fatal("same-provider continuation replayed history")
	}
}

func TestContextDeltaAmbiguousDeliveryNeverReplaysMissingMessages(t *testing.T) {
	for _, boundary := range []string{"admission", "host-loss"} {
		t.Run(boundary, func(t *testing.T) {
			state, first, _ := deltaFixture(t)
			state, _ = submitDelta(t, state, "uncertain")
			if boundary == "admission" {
				state, _ = apply(t, state, TurnAdmitted{OperationID: "uncertain", Ambiguous: true})
			} else {
				state, _ = apply(t, state, TurnAdmitted{OperationID: "uncertain", Accepted: true, Turn: provider.TurnRef{OperationID: "uncertain", NativeID: "turn"}})
				state, _ = apply(t, state, HostLost{LaneID: first.ID, ConnectionGeneration: state.Lanes[first.ID].ConnectionGeneration})
			}
			if state.Lanes[first.ID].Coverage[3].Status != CoverageUncertain || state.Lanes[first.ID].Coverage[4].Status != CoverageUncertain {
				t.Fatal("ambiguous delta has no durable delivery fence")
			}
			state, effects := apply(t, state, Submit{OperationID: "next", Text: "next", Presentation: provider.TurnPresentation{Origin: "human"}})
			if len(effects) != 1 {
				t.Fatal("next request must resume original thread")
			}
			resume, ok := effects[0].(ResumeLaneEffect)
			if !ok || resume.Thread != state.Lanes[first.ID].Thread {
				t.Fatal("changed native identity")
			}
			state, effects = apply(t, state, LaneOpened{LaneID: first.ID, Thread: resume.Thread, ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported)})
			if len(effects) != 1 || len(effects[0].(StartTurnEffect).Seed.Messages) != 0 {
				t.Fatal("uncertain delta was resent")
			}
		})
	}
}

func TestContextDeltaDefiniteRejectionLeavesHistoryForNextDistinctRequest(t *testing.T) {
	state, first, _ := deltaFixture(t)
	state, _ = submitDelta(t, state, "rejected")
	state, _ = apply(t, state, TurnAdmissionFailed{OperationID: "rejected", Kind: provider.ErrorAdmissionRejected})
	if _, exists := state.Lanes[first.ID].Coverage[3]; exists {
		t.Fatal("rejection falsely marked context consumed")
	}
	state, effects := apply(t, state, Submit{OperationID: "new-request", Text: "new request", Presentation: provider.TurnPresentation{Origin: "human"}})
	resume := effects[0].(ResumeLaneEffect)
	_, effects = apply(t, state, LaneOpened{LaneID: first.ID, Thread: resume.Thread, ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported)})
	start := effects[0].(StartTurnEffect)
	if len(start.Seed.Messages) != 2 || start.Seed.Messages[0].Text != "other-input" {
		t.Fatal("rejected delta lost messages or included failed request")
	}
}

func TestContextDeltaBoundsNeverSilentlyDropMissingHistory(t *testing.T) {
	state, first, _ := deltaFixture(t)
	state.Ledger[2].Text = strings.Repeat("x", initialSeedMaxBytes)
	state, effects := apply(t, state, Submit{OperationID: "oversize", Text: "next", Presentation: provider.TurnPresentation{Origin: "human"}})
	if len(effects) != 0 || len(state.Queue) != 1 || state.Lanes[first.ID].LastError != provider.ErrorContextLimitReached || state.Lanes[first.ID].CoveredThrough != 2 {
		t.Fatal("oversize delta was dispatched, dropped, or claimed consumed")
	}
}

func TestContextDeltaRevivesPreviouslyBlockedEstablishedLane(t *testing.T) {
	state, first, _ := deltaFixture(t)
	lane := state.Lanes[first.ID]
	lane.Phase, lane.LastError = LaneBlocked, provider.ErrorUnsupportedCapability
	state.Lanes[first.ID] = lane
	state, _ = apply(t, state, SelectLane{Identity: first})
	_, start := submitDelta(t, state, "resume-blocked")
	if start.Input.DeltaDigest == "" {
		t.Fatal("existing blocked lane was not revived")
	}
}

func TestContextDeltaCrashBoundaryAndTamperRejection(t *testing.T) {
	for _, status := range []OutboxStatus{OutboxPending, OutboxDispatched, OutboxConsumed} {
		t.Run(string(status), func(t *testing.T) {
			state, first, _ := deltaFixture(t)
			state, _ = submitDelta(t, state, "crash")
			if status == OutboxConsumed {
				state, _ = apply(t, state, InputConsumed{OperationID: "crash"})
			} else {
				state.Outbox[len(state.Outbox)-1].Status = status
			}
			state, _ = apply(t, state, RecoverOutbox{})
			coverage, exists := state.Lanes[first.ID].Coverage[3]
			if status == OutboxPending {
				if exists || state.Foreground == nil {
					t.Fatal("never-dispatched delta was lost or claimed delivered")
				}
			} else {
				want := CoverageUncertain
				if status == OutboxConsumed {
					want = CoveragePromptSeen
				}
				if !exists || coverage.Status != want || state.Foreground != nil {
					t.Fatal("crash lost confirmed/uncertain delivery classification")
				}
			}
		})
	}
	state, _, _ := deltaFixture(t)
	state, _ = submitDelta(t, state, "tamper")
	state.Outbox[len(state.Outbox)-1].Batch.Messages[0].Text = "changed"
	if state.Validate() == nil {
		t.Fatal("durable delta payload tampering was accepted")
	}
}

func TestContextDeltaSkipsCoverageBeyondFrontierAndInternalNotices(t *testing.T) {
	state, first, _ := deltaFixture(t)
	lane := state.Lanes[first.ID]
	if err := setCoverage(&lane, &state, 4, CoverageNativeSeen, "already-seen"); err != nil {
		t.Fatal(err)
	}
	state.Lanes[first.ID] = lane
	batch, err := buildContextDelta(state, lane)
	if err != nil || len(batch.Messages) != 1 || batch.Messages[0].LedgerSequence != 3 {
		t.Fatal("replayed covered event beyond coverage frontier")
	}
	state.Ledger[2].ContextExcluded = true
	batch, err = buildContextDelta(state, lane)
	if err != nil || len(batch.Messages) != 0 {
		t.Fatal("internal notice entered delta")
	}
}
