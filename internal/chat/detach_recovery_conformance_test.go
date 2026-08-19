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
	state, _, err = Reduce(state, DetachLane{
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
	state, _, err = Reduce(state, DetachLane{
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

func TestRunningTurnHostLossResumesExactThreadThenReadsBackWithoutResend(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("host-loss-recovery-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	laneIdentity := testLane("host-loss-recovery-chat", "codex")
	openReadyDurableLane(t, engine, laneIdentity)
	if err := engine.Apply(Submit{OperationID: "host-loss-turn", Text: "send once"}); err != nil {
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
	if state.Foreground == nil || state.Foreground.Status != ForegroundReconciling || state.Lanes[laneIdentity.ID].Phase != LaneResuming {
		t.Fatalf("running host loss did not enter exact-recovery state: %#v", state)
	}
	if !state.Lanes[laneIdentity.ID].Thread.Equal(thread) {
		t.Fatalf("host loss changed the native ThreadRef: got=%#v want=%#v", state.Lanes[laneIdentity.ID].Thread, thread)
	}

	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim exact resume after host loss: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok || !resume.Thread.Equal(thread) {
		t.Fatalf("host loss recovery claimed %#v, want exact ResumeLaneEffect", effect)
	}
	if err := engine.Apply(LaneOpened{
		LaneID: laneIdentity.ID, Identity: laneIdentity, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported),
	}); err != nil {
		t.Fatalf("open exact resumed lane: %v", err)
	}
	effect, ok, err = engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim provider turn readback: ok=%v err=%v", ok, err)
	}
	reconcile, ok := effect.(ReconcileTurnEffect)
	if !ok || reconcile.OperationID != "host-loss-turn" || reconcile.Turn != turn {
		t.Fatalf("host loss recovery dispatched %#v, want exact readback", effect)
	}

	if err := engine.Apply(TurnReconciled{
		OperationID: "host-loss-turn", Turn: turn, Found: false,
	}); err != nil {
		t.Fatalf("apply authoritative not-found readback: %v", err)
	}
	state = engine.Snapshot()
	if state.Foreground == nil || state.Foreground.Status != ForegroundUncertain || state.Lanes[laneIdentity.ID].Phase != LaneBlocked {
		t.Fatalf("missing native turn did not block uncertain recovery: %#v", state)
	}
	for _, entry := range state.Outbox {
		if entry.Kind == EffectStartTurn && entry.OperationID == "host-loss-turn" && entry.Status == OutboxPending {
			t.Fatal("host-loss readback made the original turn replayable")
		}
	}
}

func TestFailedTurnReadbackCanRetryAfterExactHostReattach(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("failed-readback-recovery-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	laneIdentity := testLane("failed-readback-recovery-chat", "codex")
	openReadyDurableLane(t, engine, laneIdentity)
	if err := engine.Apply(Submit{OperationID: "failed-readback-turn", Text: "send once"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim running turn: ok=%v err=%v", ok, err)
	}
	turn := provider.TurnRef{OperationID: "failed-readback-turn", NativeID: "native-failed-readback-turn"}
	if err := engine.Apply(TurnAdmitted{OperationID: "failed-readback-turn", Accepted: true, Turn: turn}); err != nil {
		t.Fatal(err)
	}

	if err := engine.Apply(HostLost{LaneID: laneIdentity.ID, ConnectionGeneration: 1}); err != nil {
		t.Fatalf("first host loss: %v", err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim first exact resume: ok=%v err=%v", ok, err)
	}
	resume, ok := effect.(ResumeLaneEffect)
	if !ok {
		t.Fatalf("first recovery claimed %T, want ResumeLaneEffect", effect)
	}
	if err := engine.Apply(LaneOpened{
		LaneID: laneIdentity.ID, Identity: laneIdentity, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported),
	}); err != nil {
		t.Fatalf("open first exact resume: %v", err)
	}
	if effect, ok, err = engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim first readback: ok=%v err=%v", ok, err)
	} else if _, ok := effect.(ReconcileTurnEffect); !ok {
		t.Fatalf("first post-resume effect = %T, want ReconcileTurnEffect", effect)
	}
	if err := engine.Apply(TurnReconciled{OperationID: "failed-readback-turn", Turn: turn, Found: false}); err != nil {
		t.Fatalf("record failed readback: %v", err)
	}
	state := engine.Snapshot()
	if state.Foreground == nil || state.Foreground.Status != ForegroundUncertain || !outboxHas(&state, reconcileTurnEffectID("failed-readback-turn"), OutboxFailed) {
		t.Fatalf("failed readback did not remain uncertain and non-replayable: %#v", state)
	}
	if _, claimed, err := engine.ClaimNext(); err != nil || claimed {
		t.Fatalf("failed readback retried without a new attachment: claimed=%v err=%v", claimed, err)
	}

	generation := state.Lanes[laneIdentity.ID].ConnectionGeneration
	if err := engine.Apply(HostLost{LaneID: laneIdentity.ID, ConnectionGeneration: generation}); err != nil {
		t.Fatalf("second host loss: %v", err)
	}
	effect, ok, err = engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim second exact resume: ok=%v err=%v", ok, err)
	}
	resume, ok = effect.(ResumeLaneEffect)
	if !ok {
		t.Fatalf("second recovery claimed %T, want ResumeLaneEffect", effect)
	}
	if err := engine.Apply(LaneOpened{
		LaneID: laneIdentity.ID, Identity: laneIdentity, Thread: resume.Thread,
		ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported),
	}); err != nil {
		t.Fatalf("open second exact resume: %v", err)
	}
	if effect, ok, err = engine.ClaimNext(); err != nil || !ok {
		t.Fatalf("claim retried readback: ok=%v err=%v", ok, err)
	} else if retry, ok := effect.(ReconcileTurnEffect); !ok || retry.OperationID != "failed-readback-turn" || retry.Turn != turn {
		t.Fatalf("retried effect = %#v, want same immutable readback", effect)
	}
	state = engine.Snapshot()
	count := 0
	for _, entry := range state.Outbox {
		if entry.Kind == EffectReconcileTurn && entry.OperationID == "failed-readback-turn" {
			count++
		}
		if entry.Kind == EffectStartTurn && entry.OperationID == "failed-readback-turn" && entry.Status == OutboxPending {
			t.Fatal("retry made the original user input replayable")
		}
	}
	if count != 1 {
		t.Fatalf("reconciliation retry appended duplicate receipts: count=%d", count)
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
	if err := engine.Apply(DetachLane{
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
