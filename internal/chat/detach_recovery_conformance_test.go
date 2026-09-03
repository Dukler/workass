package chat

import (
	"context"
	"strings"
	"testing"

	"workass/internal/provider"
)

func detachOutboxEntry(t *testing.T, state State, operationID provider.OperationID) OutboxEntry {
	t.Helper()
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane && entry.OperationID == provider.NormalizeOperationID(string(operationID)) {
			return entry
		}
	}
	t.Fatalf("detach operation %q was not journaled: %#v", operationID, state.Outbox)
	return OutboxEntry{}
}

func TestDispatchedDetachWithoutDurableReceiptIsAmbiguousWithAttachmentUnchanged(t *testing.T) {
	state, identity, owner, thread, connectionID := newDetachTestState(t)
	operationID := DetachOperationID(state.ChatID, identity.ID, connectionID, 7)

	var err error
	state, _, err = Reduce(state, DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: owner,
		ConnectionID: connectionID, ConnectionGeneration: 7,
	})
	if err != nil {
		t.Fatalf("journal detach: %v", err)
	}
	state, _, err = Reduce(state, ClaimEffect{EffectID: string(operationID)})
	if err != nil {
		t.Fatalf("dispatch detach: %v", err)
	}

	// The saved attachment still looks live, but a provider Detach has no
	// idempotency or readback contract. A restart therefore cannot infer that
	// the call did not escape the process.
	recovered, effects, err := Reduce(state, RecoverOutbox{})
	if err != nil {
		t.Fatalf("recover dispatched detach: %v", err)
	}
	entry := detachOutboxEntry(t, recovered, operationID)
	if entry.Status != OutboxAmbiguous || entry.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("unreceipted detach was made retryable: %#v", entry)
	}
	if len(effects) != 0 {
		t.Fatalf("ambiguous detach emitted recovery effects: %#v", effects)
	}
	lane := recovered.Lanes[identity.ID]
	if !lane.Thread.Equal(thread) {
		t.Fatalf("ambiguous detach changed immutable ThreadRef: got=%#v want=%#v", lane.Thread, thread)
	}
	if lane.Attachment == nil || lane.Attachment.ConnectionID != connectionID {
		t.Fatalf("ambiguous detach did not preserve the saved attachment evidence: %#v", lane.Attachment)
	}
	if _, _, err := Reduce(recovered, ClaimEffect{EffectID: string(operationID)}); err == nil {
		t.Fatal("ambiguous detach was claimable after recovery")
	}

	recoveredAgain, effects, err := Reduce(recovered, RecoverOutbox{})
	if err != nil {
		t.Fatalf("repeat detach recovery: %v", err)
	}
	if len(effects) != 0 {
		t.Fatalf("repeat ambiguous detach recovery emitted effects: %#v", effects)
	}
	entry = detachOutboxEntry(t, recoveredAgain, operationID)
	if entry.Status != OutboxAmbiguous || entry.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("repeat detach recovery changed the fail-closed result: %#v", entry)
	}
	if !recoveredAgain.Lanes[identity.ID].Thread.Equal(thread) {
		t.Fatalf("repeat detach recovery changed immutable ThreadRef: %#v", recoveredAgain.Lanes[identity.ID].Thread)
	}
}

func TestLaneDetachedReceiptCompletesDetachAndExactResumeKeepsThread(t *testing.T) {
	state, identity, owner, thread, connectionID := newDetachTestState(t)
	operationID := DetachOperationID(state.ChatID, identity.ID, connectionID, 7)

	var err error
	state, _, err = Reduce(state, DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: owner,
		ConnectionID: connectionID, ConnectionGeneration: 7,
	})
	if err != nil {
		t.Fatalf("journal detach: %v", err)
	}
	state, _, err = Reduce(state, ClaimEffect{EffectID: string(operationID)})
	if err != nil {
		t.Fatalf("dispatch detach: %v", err)
	}

	state, effects, err := Reduce(state, ProviderEventReceived{
		ConnectionGeneration: 7,
		Event: provider.Event{
			Kind:     provider.EventLaneDetached,
			Identity: provider.EventIdentity{ChatID: state.ChatID, LaneID: identity.ID, Sequence: 1},
		},
	})
	if err != nil {
		t.Fatalf("durable lane-detached receipt: %v", err)
	}
	if len(effects) != 0 {
		t.Fatalf("lane-detached receipt emitted unexpected effects: %#v", effects)
	}
	entry := detachOutboxEntry(t, state, operationID)
	if entry.Status != OutboxCompleted || entry.LastError != "" {
		t.Fatalf("durable lane-detached receipt did not settle detach: %#v", entry)
	}
	lane := state.Lanes[identity.ID]
	if lane.Attachment != nil || lane.Phase != LaneDetached || lane.ConnectionGeneration != 8 {
		t.Fatalf("lane-detached receipt changed attachment lifecycle incorrectly: %#v", lane)
	}
	if !lane.Thread.Equal(thread) {
		t.Fatalf("completed detach deleted or replaced ThreadRef: got=%#v want=%#v", lane.Thread, thread)
	}

	state, effects, err = Reduce(state, SelectLane{Identity: identity, Owner: owner})
	if err != nil {
		t.Fatalf("select detached lane for exact resume: %v", err)
	}
	if len(effects) != 1 {
		t.Fatalf("exact resume effects = %#v", effects)
	}
	resume, ok := effects[0].(ResumeLaneEffect)
	if !ok {
		t.Fatalf("detached lane recovery effect = %T, want ResumeLaneEffect", effects[0])
	}
	if !resume.Thread.Equal(thread) || resume.Thread.IsZero() {
		t.Fatalf("exact resume changed the native thread: got=%#v want=%#v", resume.Thread, thread)
	}
	if _, _, err := Reduce(state, RecoverOutbox{}); err != nil {
		t.Fatalf("recover completed detach before resume: %v", err)
	}
}

func TestRunningTurnHostLossEndsImmediatelyWithoutResumeOrResend(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("host-loss-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	laneIdentity := testLane("host-loss-chat", "codex")
	openReadyDurableLane(t, engine, laneIdentity)
	if err := engine.Apply(Submit{OperationID: "host-loss-turn", Text: "send once", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim running turn: ok=%v err=%v", ok, err)
	}
	thread := engine.Snapshot().Lanes[laneIdentity.ID].Thread
	turn := provider.TurnRef{OperationID: "host-loss-turn", NativeID: "native-host-loss-turn"}
	if err := engine.Apply(TurnAdmitted{OperationID: "host-loss-turn", Accepted: true, Turn: turn}); err != nil {
		t.Fatal(err)
	}

	if err := engine.Apply(HostLost{LaneID: laneIdentity.ID, ConnectionGeneration: 1}); err != nil {
		t.Fatalf("host loss: %v", err)
	}
	state := engine.Snapshot()
	assertInterruptedTurnTerminalized(t, state, laneIdentity.ID, "host-loss-turn", provider.ErrorTransientTransport)
	if state.Lanes[laneIdentity.ID].Phase != LaneDetached || !state.Lanes[laneIdentity.ID].Thread.Equal(thread) {
		t.Fatalf("host loss changed the native ThreadRef: got=%#v want=%#v", state.Lanes[laneIdentity.ID].Thread, thread)
	}
	if effect, claimed, err := engine.ClaimNext(); err != nil || claimed {
		t.Fatalf("host loss manufactured work: effect=%#v claimed=%v err=%v", effect, claimed, err)
	}
	for _, entry := range state.Outbox {
		if entry.Kind == EffectStartTurn && entry.OperationID == "host-loss-turn" && entry.Status == OutboxPending {
			t.Fatal("host loss made the original turn replayable")
		}
	}
}

func TestNextDistinctPromptResumesSavedThreadAfterHostLoss(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("host-loss-next-prompt-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	laneIdentity := testLane("host-loss-next-prompt-chat", "codex")
	openReadyDurableLane(t, engine, laneIdentity)
	thread := engine.Snapshot().Lanes[laneIdentity.ID].Thread
	if err := engine.Apply(Submit{OperationID: "interrupted-turn", Text: "first", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim running turn: ok=%v err=%v", ok, err)
	}
	turn := provider.TurnRef{OperationID: "interrupted-turn", NativeID: "native-interrupted-turn"}
	if err := engine.Apply(TurnAdmitted{OperationID: "interrupted-turn", Accepted: true, Turn: turn}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(HostLost{LaneID: laneIdentity.ID, ConnectionGeneration: 1}); err != nil {
		t.Fatalf("host loss: %v", err)
	}
	if err := engine.Apply(Submit{OperationID: "next-turn", Text: "continue", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatalf("submit next turn: %v", err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim next attachment: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("next prompt did not exact-resume saved thread: %#v", effect)
	}
}

func TestStartupRetiresLegacyTurnReadbackState(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("legacy-readback-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	laneIdentity := testLane("legacy-readback-chat", "qwen")
	openReadyDurableLane(t, engine, laneIdentity)
	if err := engine.Apply(Submit{OperationID: "legacy-readback-turn", Text: "send once", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim running turn: ok=%v err=%v", ok, err)
	}
	turn := provider.TurnRef{OperationID: "legacy-readback-turn", NativeID: "native-legacy-readback-turn"}
	if err := engine.Apply(TurnAdmitted{OperationID: "legacy-readback-turn", Accepted: true, Turn: turn}); err != nil {
		t.Fatal(err)
	}

	legacy := engine.Snapshot()
	legacy.Foreground.Status = ForegroundUncertain
	lane := legacy.Lanes[laneIdentity.ID]
	lane.Phase = LaneBlocked
	lane.LastError = provider.ErrorProtocolViolation
	lane.Delivery = provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
	legacy.Lanes[laneIdentity.ID] = lane
	legacy.Outbox = append(legacy.Outbox, OutboxEntry{
		ID: "turn-reconcile:legacy-readback-turn", Kind: EffectReconcileTurn, Status: OutboxPending,
		LaneID: laneIdentity.ID, OperationID: "legacy-readback-turn", Turn: turn, LastError: provider.ErrorProtocolViolation,
	})
	if err := store.Save(legacy); err != nil {
		t.Fatalf("save legacy actor fixture: %v", err)
	}

	restarted, err := NewDurableEngine("legacy-readback-chat", store)
	if err != nil {
		t.Fatalf("restart legacy actor: %v", err)
	}
	state := restarted.Snapshot()
	assertInterruptedTurnTerminalized(t, state, laneIdentity.ID, "legacy-readback-turn", provider.ErrorTransientTransport)
	if !outboxHas(&state, "turn-reconcile:legacy-readback-turn", OutboxFailed) {
		t.Fatalf("legacy turn readback entry was not retired: %#v", state.Outbox)
	}
	if _, claimed, err := restarted.ClaimNext(); err != nil || claimed {
		t.Fatalf("legacy state replayed provider work: claimed=%v err=%v", claimed, err)
	}
}

func assertInterruptedTurnTerminalized(t *testing.T, state State, laneID provider.LaneID, operationID provider.OperationID, kind provider.ErrorKind) {
	t.Helper()
	if state.Foreground != nil {
		t.Fatalf("interrupted turn retained live foreground ownership: %#v", state.Foreground)
	}
	lane := state.Lanes[laneID]
	if lane.Phase != LaneDetached && lane.Phase != LaneAbsent {
		t.Fatalf("interrupted turn left lane phase %q", lane.Phase)
	}
	if lane.LastError != kind {
		t.Fatalf("interrupted turn lane error = %q, want %q", lane.LastError, kind)
	}
	if len(state.Ledger) < 2 {
		t.Fatalf("interrupted turn did not persist both visible owners: %#v", state.Ledger)
	}
	user, assistant := state.Ledger[len(state.Ledger)-2], state.Ledger[len(state.Ledger)-1]
	if user.OperationID != operationID || user.Role != "user" || user.Status != "done" {
		t.Fatalf("interrupted user row = %#v", user)
	}
	if assistant.OperationID != operationID || assistant.Role != "assistant" || assistant.Status != "failed" || !assistant.Interrupted {
		t.Fatalf("interrupted assistant row = %#v", assistant)
	}
	if assistant.RetryPrompt != "" {
		t.Fatalf("interrupted turn exposed retry input: %#v", assistant)
	}
	if assistant.Terminal == nil || assistant.Terminal.Status != "failed" || !assistant.Terminal.Interrupted || assistant.Terminal.Error != string(kind) {
		t.Fatalf("interrupted terminal receipt = %#v", assistant.Terminal)
	}
	if strings.Contains(strings.ToLower(assistant.Text), "recovery") {
		t.Fatalf("interrupted terminal explanation exposed retired recovery state: %q", assistant.Text)
	}
	if state.Obligation != nil && state.Obligation.State == "working" {
		t.Fatalf("interrupted turn left its obligation working: %#v", state.Obligation)
	}
}

func TestStartupDoesNotReplayDispatchedExternalMutation(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("external-recovery-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(InitializeChat{
		Presentation: PresentationState{TabID: "external-tab"},
		OperationID:  "create:external-recovery", Digest: "external-recovery-create",
	}); err != nil {
		t.Fatal(err)
	}
	operationID := provider.OperationID("external-mutation-once")
	if err := engine.Apply(RecordExternalMutation{
		OperationID: operationID, Kind: "browser", Method: "click", TabID: "external-tab",
		Digest: strings.Repeat("a", 64),
	}); err != nil {
		t.Fatal(err)
	}

	effect, claimed, err := engine.ClaimEffect(externalMutationEffectID(operationID))
	if err != nil || !claimed {
		t.Fatalf("claim external mutation: claimed=%v err=%v", claimed, err)
	}
	if _, ok := effect.(ExternalMutationEffect); !ok {
		t.Fatalf("claimed external effect = %T, want ExternalMutationEffect", effect)
	}

	restarted, err := NewDurableEngine("external-recovery-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	state := restarted.Snapshot()
	entry := externalMutationEntryForOperation(state, operationID)
	if entry == nil || entry.Status != OutboxAmbiguous || entry.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("startup did not fail closed dispatched external mutation: %#v", entry)
	}
	if _, claimed, err := restarted.ClaimEffect(externalMutationEffectID(operationID)); err != nil || claimed {
		t.Fatalf("startup replayed dispatched external mutation: claimed=%v err=%v", claimed, err)
	}
}

type detachErrorLane struct {
	provider.Lane
	attachment provider.LaneAttachmentSnapshot
	detachErr  error
}

func (l *detachErrorLane) Detach(context.Context) error { return l.detachErr }

func (l *detachErrorLane) AttachmentSnapshot() provider.LaneAttachmentSnapshot { return l.attachment }

type detachErrorFactory struct {
	base      coordinatorFactory
	detachErr error
}

func (f *detachErrorFactory) wrap(lane provider.Lane) provider.Lane {
	return &detachErrorLane{
		Lane: lane,
		attachment: provider.LaneAttachmentSnapshot{
			ConnectionID: "unreceipted-detach-connection", ProviderID: "custom",
		},
		detachErr: f.detachErr,
	}
}

func (f *detachErrorFactory) Create(ctx context.Context, request provider.CreateLaneRequest) (provider.Lane, provider.ThreadRef, error) {
	lane, thread, err := f.base.Create(ctx, request)
	if err != nil {
		return nil, provider.ThreadRef{}, err
	}
	return f.wrap(lane), thread, nil
}

func (f *detachErrorFactory) Resume(ctx context.Context, request provider.ResumeLaneRequest) (provider.Lane, error) {
	lane, err := f.base.Resume(ctx, request)
	if err != nil {
		return nil, err
	}
	return f.wrap(lane), nil
}

func TestDispatchedDetachErrorWithoutReceiptIsAcceptanceAmbiguous(t *testing.T) {
	factory := &detachErrorFactory{detachErr: &provider.Error{
		Kind: provider.ErrorTransientTransport, Message: "detach transport failed before receipt",
	}}
	registry := provider.NewRegistry()
	if err := registry.Register(provider.Definition{
		Identity:       provider.ProviderIdentity{ID: "custom", DisplayName: "Custom"},
		Realm:          coordinatorRealm{id: "custom"},
		Runtime:        factory,
		Authentication: coordinatorAuthentication{},
	}); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine("unreceipted-detach-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewCoordinator(engine, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	identity := coordinatorLaneIdentity("unreceipted-detach-chat", "custom")
	owner := provider.AttachmentOwner{TabID: "unreceipted-detach-tab"}
	if err := engine.Apply(SelectLane{Identity: identity, Owner: owner, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("create exact lane: executed=%v err=%v", executed, err)
	}
	state := engine.Snapshot()
	lane := state.Lanes[identity.ID]
	if lane.Attachment == nil {
		t.Fatal("test lane did not expose an exact attachment")
	}
	operationID := DetachOperationID(state.ChatID, identity.ID, lane.Attachment.ConnectionID, lane.ConnectionGeneration)
	if err := engine.Apply(DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: lane.Owner,
		ConnectionID: lane.Attachment.ConnectionID, ConnectionGeneration: lane.ConnectionGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteDetach(context.Background(), operationID); !executed || err == nil {
		t.Fatalf("unreceipted detach error: executed=%v err=%v", executed, err)
	}
	entry := detachOutboxEntry(t, engine.Snapshot(), operationID)
	if entry.Status != OutboxAmbiguous || entry.LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("dispatched detach error was not fail-closed ambiguous: %#v", entry)
	}
}
