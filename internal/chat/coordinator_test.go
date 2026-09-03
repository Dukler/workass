package chat

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"workass/internal/provider"
)

type coordinatorRealm struct{ id provider.ID }

type coordinatorAuthentication struct{}

func (coordinatorAuthentication) IsAuthenticationFailure(error) bool { return false }
func (coordinatorAuthentication) LoginHint() string                  { return "" }

func (r coordinatorRealm) ResolveRealm(_ context.Context, request provider.RealmRequest) (provider.Realm, error) {
	return provider.Realm{
		ProviderID: r.id, MachineID: request.MachineID,
		AccountScope: "account", InstallScope: "install",
	}.Normalize(), nil
}

type coordinatorFactory struct {
	mu             sync.Mutex
	lanes          map[provider.LaneID]*coordinatorLane
	imports        bool
	importJournal  *coordinatorImportJournal
	fail           error
	canonicalRealm *provider.Realm
	createCalls    int
	reconcileCalls int
}

func (f *coordinatorFactory) Create(_ context.Context, request provider.CreateLaneRequest) (provider.Lane, provider.ThreadRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, provider.ThreadRef{}, f.fail
	}
	if f.lanes == nil {
		f.lanes = make(map[provider.LaneID]*coordinatorLane)
	}
	if request.Reconcile {
		f.reconcileCalls++
		for _, lane := range f.lanes {
			if lane.identity.ChatID == request.Identity.ChatID && lane.identity.Realm.ProviderID == request.Identity.Realm.ProviderID {
				return lane, lane.thread, nil
			}
		}
		return nil, provider.ThreadRef{}, &provider.Error{Kind: provider.ErrorAcceptanceAmbiguous, Message: "no durable create receipt"}
	}
	f.createCalls++
	identity := request.Identity
	if f.canonicalRealm != nil {
		identity.Realm = f.canonicalRealm.Normalize()
		identity.ID = ""
		identity = identity.Normalize()
	}
	if _, exists := f.lanes[identity.ID]; exists {
		return nil, provider.ThreadRef{}, &provider.Error{Kind: provider.ErrorNativeIdentityConflict, Message: "create twice"}
	}
	thread := provider.ThreadRef{
		ProviderID: identity.Realm.ProviderID,
		RootID:     "thread-" + string(identity.ID),
		HeadID:     "thread-" + string(identity.ID), Lineage: 1,
	}
	lane := &coordinatorLane{
		identity: identity, thread: thread, events: make(chan provider.Event, 32),
		context: coordinatorContext{imports: f.imports, journal: f.importJournal},
	}
	lane.delivery = coordinatorDelivery{lane: lane}
	f.lanes[identity.ID] = lane
	return lane, thread, nil
}

func (f *coordinatorFactory) Resume(_ context.Context, request provider.ResumeLaneRequest) (provider.Lane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lane := f.lanes[request.Identity.ID]
	if lane == nil || !lane.thread.Equal(request.Thread) {
		return nil, &provider.Error{Kind: provider.ErrorNativeThreadMissing, Message: "exact thread missing"}
	}
	return lane, nil
}

type coordinatorLane struct {
	identity provider.LaneIdentity
	thread   provider.ThreadRef
	delivery coordinatorDelivery
	context  coordinatorContext
	events   chan provider.Event
	closed   bool
}

func (l *coordinatorLane) Identity() provider.LaneIdentity     { return l.identity }
func (l *coordinatorLane) Thread() provider.ThreadRef          { return l.thread }
func (l *coordinatorLane) Delivery() provider.DeliveryStrategy { return l.delivery }
func (l *coordinatorLane) Context() provider.ContextStrategy   { return l.context }
func (l *coordinatorLane) Events() <-chan provider.Event       { return l.events }
func (l *coordinatorLane) Detach(context.Context) error {
	if !l.closed {
		close(l.events)
		l.closed = true
	}
	return nil
}

func (l *coordinatorLane) send(sequence uint64, operation provider.OperationID, kind provider.EventKind, mutate func(*provider.Event)) {
	event := provider.Event{
		Kind: kind,
		Identity: provider.EventIdentity{
			ChatID: l.identity.ChatID, LaneID: l.identity.ID, OperationID: operation,
			TurnID: "turn-" + string(operation), Sequence: sequence,
		},
	}
	mutate(&event)
	l.events <- event
}

type coordinatorDelivery struct {
	lane          *coordinatorLane
	startStarted  chan<- struct{}
	steerStarted  chan<- struct{}
	steerRelease  <-chan struct{}
	cancelStarted chan<- struct{}
}

func (d coordinatorDelivery) Capabilities() provider.DeliveryCapabilities {
	return provider.DeliveryCapabilities{StableInputIdentity: true, ConsumptionReceipt: true}
}
func (d coordinatorDelivery) StartTurn(_ context.Context, input provider.TurnInput) (provider.TurnAdmission, error) {
	if d.startStarted != nil {
		d.startStarted <- struct{}{}
	}
	turn := provider.TurnRef{OperationID: input.OperationID, NativeID: "turn-" + string(input.OperationID)}
	admission := provider.TurnAdmission{Turn: turn, Accepted: true}
	if input.CommitAdmission != nil {
		if err := input.CommitAdmission(admission); err != nil {
			return provider.TurnAdmission{}, err
		}
	}
	return admission, nil
}

func TestCoordinatorReplyAdmissionFencesGenericProviderClaim(t *testing.T) {
	engine, err := NewEngine("reply-admission-chat")
	if err != nil {
		t.Fatal(err)
	}
	factory := &coordinatorFactory{}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	identity := coordinatorLaneIdentity("reply-admission-chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	factory.mu.Lock()
	started := make(chan struct{}, 1)
	lane := factory.lanes[identity.ID]
	lane.delivery.startStarted = started
	factory.mu.Unlock()

	release := coordinator.BeginReplyAdmission()
	if err := engine.Apply(Submit{
		OperationID: "reply-admission-turn", Text: "question",
		Presentation: provider.TurnPresentation{Origin: "human"},
	}); err != nil {
		release()
		t.Fatal(err)
	}
	coordinator.Wake()
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || executed {
		release()
		t.Fatalf("provider effect crossed pending reply: executed=%v err=%v", executed, err)
	}
	select {
	case <-started:
		release()
		t.Fatal("provider start crossed pending reply")
	default:
	}

	release()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("provider start did not resume after reply release")
	}
}
func (d coordinatorDelivery) Steer(context.Context, provider.SteerInput) (provider.SteerReceipt, error) {
	if d.steerStarted != nil {
		d.steerStarted <- struct{}{}
	}
	if d.steerRelease != nil {
		<-d.steerRelease
		return provider.SteerReceipt{Accepted: true, AwaitConsumption: true}, nil
	}
	return provider.SteerReceipt{}, provider.Unsupported("", "not used")
}
func (d coordinatorDelivery) Cancel(context.Context, provider.TurnRef) error {
	if d.cancelStarted != nil {
		d.cancelStarted <- struct{}{}
	}
	return nil
}
func (d coordinatorDelivery) ResolvePermission(_ context.Context, decision provider.PermissionDecision) (provider.PermissionReceipt, error) {
	return provider.PermissionReceipt{
		OperationID: decision.OperationID, RequestID: decision.RequestID, OptionID: decision.OptionID, Accepted: true,
	}, nil
}

type coordinatorImportJournal struct {
	mu             sync.Mutex
	receipts       map[provider.OperationID]provider.ContextImportReceipt
	importCalls    int
	reconcileCalls int
}

type coordinatorContext struct {
	imports bool
	journal *coordinatorImportJournal
}

func (c coordinatorContext) Capabilities() provider.ContextCapabilities {
	capabilities := provider.ContextCapabilities{ExactResume: true, ImportMode: provider.ContextImportUnsupported}
	if c.imports {
		capabilities.ImportMode = provider.ContextImportNonSampling
		capabilities.ImportReadback = true
		capabilities.IdempotentImport = true
		capabilities.MaxImportEvents = 64
		capabilities.MaxImportBytes = 1 << 20
	}
	return capabilities
}
func (c coordinatorContext) Import(_ context.Context, request provider.ContextImportRequest) (provider.ContextImportReceipt, error) {
	if !c.imports {
		return provider.ContextImportReceipt{}, provider.Unsupported(request.OperationID, "no import")
	}
	receipt := provider.ContextImportReceipt{
		OperationID: request.OperationID, From: request.From, To: request.To,
		Digest: request.Digest, Found: true, Confirmed: true,
	}
	if c.journal != nil {
		c.journal.mu.Lock()
		defer c.journal.mu.Unlock()
		c.journal.importCalls++
		if c.journal.receipts == nil {
			c.journal.receipts = make(map[provider.OperationID]provider.ContextImportReceipt)
		}
		if previous, ok := c.journal.receipts[request.OperationID]; ok {
			if previous != receipt {
				return provider.ContextImportReceipt{}, &provider.Error{Kind: provider.ErrorNativeIdentityConflict, Message: "context import operation changed identity"}
			}
			return previous, nil
		}
		c.journal.receipts[request.OperationID] = receipt
	}
	return receipt, nil
}

func (c coordinatorContext) ReconcileImport(_ context.Context, request provider.ContextImportRequest) (provider.ContextImportReceipt, error) {
	if !c.imports {
		return provider.ContextImportReceipt{}, provider.Unsupported(request.OperationID, "context import unsupported")
	}
	missing := provider.ContextImportReceipt{
		OperationID: request.OperationID, From: request.From, To: request.To,
		Digest: request.Digest,
	}
	if c.journal == nil {
		missing.Found = true
		missing.Confirmed = true
		return missing, nil
	}
	c.journal.mu.Lock()
	defer c.journal.mu.Unlock()
	c.journal.reconcileCalls++
	receipt, ok := c.journal.receipts[request.OperationID]
	if !ok {
		return missing, nil
	}
	if receipt.From != request.From || receipt.To != request.To || receipt.Digest != request.Digest {
		return provider.ContextImportReceipt{}, &provider.Error{Kind: provider.ErrorNativeIdentityConflict, Message: "context import readback changed identity"}
	}
	return receipt, nil
}
func (coordinatorContext) Checkpoint(context.Context) (provider.ContextCheckpoint, error) {
	return provider.ContextCheckpoint{}, provider.Unsupported("", "not used")
}

func coordinatorRegistry(t *testing.T, factories map[provider.ID]*coordinatorFactory) *provider.Registry {
	t.Helper()
	registry := provider.NewRegistry()
	for id, factory := range factories {
		if err := registry.Register(provider.Definition{
			Identity:       provider.ProviderIdentity{ID: id, DisplayName: string(id)},
			Realm:          coordinatorRealm{id: id},
			Runtime:        factory,
			Authentication: coordinatorAuthentication{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return registry
}

func coordinatorLaneIdentity(chatID string, providerID provider.ID) provider.LaneIdentity {
	return provider.LaneIdentity{
		ChatID:         chatID,
		Realm:          provider.Realm{ProviderID: providerID, MachineID: "machine", AccountScope: "account", InstallScope: "install"},
		WorkspaceEpoch: "workspace",
	}.Normalize()
}

func waitCoordinatorState(t *testing.T, engine *Engine, predicate func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state := engine.Snapshot()
		if predicate(state) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	state := engine.Snapshot()
	t.Fatalf("timed out waiting for coordinator state: %#v", state)
	return State{}
}

func TestCoordinatorExecutesDurableProviderEffectsAndNormalizesOwnership(t *testing.T) {
	engine, err := NewEngine("chat")
	if err != nil {
		t.Fatal(err)
	}
	factoryA := &coordinatorFactory{imports: true}
	factoryB := &coordinatorFactory{imports: true}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factoryA, "beta": factoryB}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	alpha := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: alpha, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("execute create: executed=%v err=%v", executed, err)
	}
	if err := engine.Apply(Submit{OperationID: "op", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("execute turn: executed=%v err=%v", executed, err)
	}

	beta := coordinatorLaneIdentity("chat", "beta")
	if err := engine.Apply(SelectLane{Identity: beta, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	factoryA.mu.Lock()
	laneA := factoryA.lanes[alpha.ID]
	factoryA.mu.Unlock()
	laneA.send(1, "op", provider.EventInputConsumed, func(event *provider.Event) {
		event.Input = &provider.InputEvent{OperationID: "op", NativeTurnID: "turn-op"}
	})
	laneA.send(2, "op", provider.EventPermissionRequested, func(event *provider.Event) {
		event.Permission = &provider.PermissionEvent{RequestID: "permission", Status: "pending", Options: []string{"allow", "deny"}}
	})
	laneA.send(3, "op", provider.EventBackgroundWork, func(event *provider.Event) {
		event.Background = &provider.BackgroundEvent{WorkID: "work", Title: "background", Status: "running"}
	})
	laneA.send(4, "op", provider.EventAssistantChunk, func(event *provider.Event) {
		event.Assistant = &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "answer"}
	})
	laneA.send(5, "op", provider.EventTurnTerminal, func(event *provider.Event) {
		event.Terminal = &provider.TerminalEvent{Status: "completed", StopReason: "end_turn"}
	})

	state := waitCoordinatorState(t, engine, func(state State) bool {
		return state.Foreground == nil && len(state.Ledger) == 2 && state.ActiveLaneID == beta.ID &&
			state.Lanes[beta.ID].Phase == LaneReady && state.Lanes[beta.ID].CoveredThrough == state.LedgerHead()
	})
	if len(state.Permissions) != 0 || state.Background["work"].Owner.LaneID != alpha.ID {
		t.Fatalf("terminal permission cleanup/background origin = permissions=%#v background=%#v", state.Permissions, state.Background)
	}
}

func TestCoordinatorExactCancelPreemptsSlowSteerDrain(t *testing.T) {
	engine, err := NewEngine("chat")
	if err != nil {
		t.Fatal(err)
	}
	factory := &coordinatorFactory{imports: true}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	steerStarted := make(chan struct{}, 1)
	steerRelease := make(chan struct{})
	cancelStarted := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case <-steerRelease:
		default:
			close(steerRelease)
		}
		_ = coordinator.Close(context.Background())
	})

	identity := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	if err := engine.Apply(Submit{OperationID: "turn", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("start turn: executed=%v err=%v", executed, err)
	}
	factory.mu.Lock()
	lane := factory.lanes[identity.ID]
	lane.delivery.steerStarted = steerStarted
	lane.delivery.steerRelease = steerRelease
	lane.delivery.cancelStarted = cancelStarted
	factory.mu.Unlock()

	if err := engine.Apply(Steer{OperationID: "steer", Text: "redirect", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	drainDone := make(chan error, 1)
	go func() { drainDone <- coordinator.Drain(context.Background()) }()
	select {
	case <-steerStarted:
	case <-time.After(time.Second):
		t.Fatal("steer did not enter provider acknowledgement")
	}
	if err := engine.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if executed, err := coordinator.ExecuteCancel(context.Background(), "cancel"); err != nil || !executed {
		t.Fatalf("execute exact cancel: executed=%v err=%v", executed, err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("exact cancel waited behind slow steer for %s", elapsed)
	}
	select {
	case <-cancelStarted:
	default:
		t.Fatal("provider cancellation was not invoked")
	}
	state := engine.Snapshot()
	if !outboxHas(&state, cancelEffectID("cancel"), OutboxAccepted) {
		t.Fatalf("cancel did not durably acknowledge: outbox=%#v", state.Outbox)
	}
	close(steerRelease)
	if err := <-drainDone; err != nil {
		t.Fatalf("steer drain after cancellation: %v", err)
	}
}

func TestCoordinatorExactSteerPreemptsUnrelatedActorDrain(t *testing.T) {
	engine, err := NewEngine("chat")
	if err != nil {
		t.Fatal(err)
	}
	factory := &coordinatorFactory{imports: true}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	backgroundStarted := make(chan struct{}, 1)
	backgroundRelease := make(chan struct{})
	if err := coordinator.SetBackgroundExecutor(func(context.Context, BackgroundAction) (json.RawMessage, error) {
		backgroundStarted <- struct{}{}
		<-backgroundRelease
		return json.RawMessage(`{"ok":true}`), nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-backgroundRelease:
		default:
			close(backgroundRelease)
		}
		_ = coordinator.Close(context.Background())
	})

	identity := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(InitializeChat{
		Presentation: PresentationState{TabID: "tab"}, OperationID: "create", Digest: "create-digest",
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	if err := engine.Apply(Submit{OperationID: "turn", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("start turn: executed=%v err=%v", executed, err)
	}
	state := engine.Snapshot()
	owner := ProviderActivityOwner{
		LaneID: identity.ID, OperationID: "turn", TurnID: state.Foreground.Turn.NativeID,
		ConnectionGeneration: state.Lanes[identity.ID].ConnectionGeneration,
	}
	if err := engine.Apply(RequestBackgroundAction{Action: BackgroundAction{
		Kind: BackgroundSpawnAgent, OperationID: "background", TabID: "tab", ChatID: "chat", Owner: owner,
		Spawn: &SpawnAgentAction{Prompt: "blocked unrelated work"},
	}}); err != nil {
		t.Fatal(err)
	}
	drainDone := make(chan error, 1)
	go func() { drainDone <- coordinator.Drain(context.Background()) }()
	select {
	case <-backgroundStarted:
	case <-time.After(time.Second):
		t.Fatal("unrelated actor drain did not block")
	}

	steerStarted := make(chan struct{}, 1)
	steerRelease := make(chan struct{})
	close(steerRelease)
	factory.mu.Lock()
	lane := factory.lanes[identity.ID]
	lane.delivery.steerStarted = steerStarted
	lane.delivery.steerRelease = steerRelease
	factory.mu.Unlock()
	if err := engine.Apply(Steer{OperationID: "steer", Text: "redirect", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if executed, err := coordinator.ExecuteSteer(context.Background(), "steer"); err != nil || !executed {
		t.Fatalf("execute exact steer: executed=%v err=%v", executed, err)
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("exact steer waited behind unrelated actor work for %s", elapsed)
	}
	select {
	case <-steerStarted:
	default:
		t.Fatal("provider steer was not invoked")
	}

	close(backgroundRelease)
	if err := <-drainDone; err != nil {
		t.Fatalf("unrelated drain after steer: %v", err)
	}
}

func TestCoordinatorRetriesAmbiguousUnestablishedCreateOnlyAfterExplicitSelection(t *testing.T) {
	engine, err := NewEngine("chat")
	if err != nil {
		t.Fatal(err)
	}
	factory := &coordinatorFactory{fail: &provider.Error{Kind: provider.ErrorAcceptanceAmbiguous, Message: "unknown acceptance"}}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	identity := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); !executed || err == nil {
		t.Fatalf("ambiguous create execution: executed=%v err=%v", executed, err)
	}
	state := engine.Snapshot()
	if state.Lanes[identity.ID].Phase != LaneBlocked || state.Outbox[0].Status != OutboxAmbiguous {
		t.Fatalf("ambiguous unestablished create did not remain fail-closed: %#v", state)
	}
	if _, ok, err := engine.ClaimNext(); err != nil || ok {
		t.Fatalf("ambiguous create retried without explicit intent: ok=%v err=%v", ok, err)
	}
	factory.mu.Lock()
	factory.fail = nil
	factory.mu.Unlock()
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("explicit unestablished create retry: %v", err)
	}
	state = engine.Snapshot()
	if lane := state.Lanes[identity.ID]; lane.Phase != LaneReady || lane.Thread.IsZero() || lane.CreateGeneration != 2 {
		t.Fatalf("explicit retry did not establish one fresh lane: %#v", lane)
	}
	if len(state.Outbox) != 2 || state.Outbox[0].Status != OutboxAmbiguous || state.Outbox[1].Status != OutboxCompleted || state.Outbox[1].Reconcile {
		t.Fatalf("explicit retry lost audit evidence or reconciled the failed create: %#v", state.Outbox)
	}
}

func TestCoordinatorCanonicalizesProviderRealmOnlyBeforeFirstThread(t *testing.T) {
	engine, err := NewEngine("chat")
	if err != nil {
		t.Fatal(err)
	}
	canonicalRealm := provider.Realm{
		ProviderID: "alpha", MachineID: "machine", AccountScope: "account-attested",
		InstallScope: "install-attested", Verified: true,
	}.Normalize()
	factory := &coordinatorFactory{canonicalRealm: &canonicalRealm}
	coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	proposal := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: proposal, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(Submit{OperationID: "queued-before-create", Text: "keep my target", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("execute canonicalizing create: executed=%v err=%v", executed, err)
	}

	canonical := proposal
	canonical.Realm = canonicalRealm
	canonical.ID = ""
	canonical = canonical.Normalize()
	state := engine.Snapshot()
	if _, exists := state.Lanes[proposal.ID]; exists && proposal.ID != canonical.ID {
		t.Fatalf("provisional lane survived canonicalization: %#v", state.Lanes)
	}
	if state.DesiredLaneID != canonical.ID || state.Foreground == nil || state.Foreground.LaneID != canonical.ID {
		t.Fatalf("queued work was not atomically rekeyed to canonical lane: %#v", state)
	}
	if state.Lanes[canonical.ID].Identity != canonical {
		t.Fatalf("canonical identity was not committed: got %#v want %#v", state.Lanes[canonical.ID].Identity, canonical)
	}
}

func TestCrashAfterProviderCreateReconcilesBindingWithoutSecondCreate(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	identity := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: identity, Owner: provider.AttachmentOwner{TabID: "tab"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim create: ok=%v err=%v", ok, err)
	}
	create := effect.(CreateLaneEffect)
	factory := &coordinatorFactory{}
	if _, _, err := factory.Create(context.Background(), provider.CreateLaneRequest{
		Identity: create.Identity, Owner: create.Owner, CWD: create.CWD,
	}); err != nil {
		t.Fatalf("provider accepted initial create: %v", err)
	}
	// Crash here: provider + daemon-native binding committed, while the chat
	// actor never received LaneOpened.
	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	recovered := restarted.Snapshot()
	if recovered.Outbox[0].Status != OutboxPending || !recovered.Outbox[0].Reconcile {
		t.Fatalf("crashed create was not converted to reconcile-only: %#v", recovered.Outbox[0])
	}
	coordinator, err := NewCoordinator(restarted, coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factory}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("reconcile create: executed=%v err=%v", executed, err)
	}
	factory.mu.Lock()
	createCalls, reconcileCalls := factory.createCalls, factory.reconcileCalls
	factory.mu.Unlock()
	if createCalls != 1 || reconcileCalls != 1 {
		t.Fatalf("provider create calls=%d reconcile calls=%d, want exactly 1/1", createCalls, reconcileCalls)
	}
	if lane := restarted.Snapshot().Lanes[identity.ID]; lane.Phase != LaneReady || lane.Thread.IsZero() {
		t.Fatalf("reconciled native binding did not open exact lane: %#v", lane)
	}
}

func TestCrashAfterProviderContextImportReconcilesWithoutSecondImport(t *testing.T) {
	store := &memoryStateStore{}
	engine, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	journal := &coordinatorImportJournal{}
	factoryA := &coordinatorFactory{imports: true}
	factoryB := &coordinatorFactory{imports: true, importJournal: journal}
	registry := coordinatorRegistry(t, map[provider.ID]*coordinatorFactory{"alpha": factoryA, "beta": factoryB})
	coordinator, err := NewCoordinator(engine, registry)
	if err != nil {
		t.Fatal(err)
	}

	alpha := coordinatorLaneIdentity("chat", "alpha")
	if err := engine.Apply(SelectLane{Identity: alpha, Owner: provider.AttachmentOwner{TabID: "tab-a"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	if err := engine.Apply(Submit{OperationID: "alpha-turn", Text: "question", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("start alpha turn: executed=%v err=%v", executed, err)
	}
	factoryA.mu.Lock()
	laneA := factoryA.lanes[alpha.ID]
	factoryA.mu.Unlock()
	laneA.send(1, "alpha-turn", provider.EventInputConsumed, func(event *provider.Event) {
		event.Input = &provider.InputEvent{OperationID: "alpha-turn", NativeTurnID: "turn-alpha-turn"}
	})
	laneA.send(2, "alpha-turn", provider.EventAssistantChunk, func(event *provider.Event) {
		event.Assistant = &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "answer"}
	})
	laneA.send(3, "alpha-turn", provider.EventTurnTerminal, func(event *provider.Event) {
		event.Terminal = &provider.TerminalEvent{Status: "completed", StopReason: "end_turn"}
	})
	waitCoordinatorState(t, engine, func(state State) bool {
		return state.Foreground == nil && state.LedgerHead() == 2
	})

	beta := coordinatorLaneIdentity("chat", "beta")
	if err := engine.Apply(SelectLane{Identity: beta, Owner: provider.AttachmentOwner{TabID: "tab-b"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("create beta: executed=%v err=%v", executed, err)
	}
	effect, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim context import: ok=%v err=%v", ok, err)
	}
	importEffect, ok := effect.(ImportContextEffect)
	if !ok || importEffect.Reconcile {
		t.Fatalf("claimed effect = %#v, want initial context import", effect)
	}
	factoryB.mu.Lock()
	laneB := factoryB.lanes[beta.ID]
	factoryB.mu.Unlock()
	request := provider.ContextImportRequest{
		OperationID: importEffect.OperationID, From: importEffect.From, To: importEffect.To,
		Digest: importEffect.Batch.Digest, Messages: append([]provider.ContextMessage(nil), importEffect.Batch.Messages...),
	}
	if receipt, err := laneB.Context().Import(context.Background(), request); err != nil || !receipt.Confirmed {
		t.Fatalf("provider accepted import: receipt=%#v err=%v", receipt, err)
	}
	// Crash here: the provider durably accepted the immutable import operation,
	// but Workass did not yet persist ContextImported.
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewDurableEngine("chat", store)
	if err != nil {
		t.Fatal(err)
	}
	recovered := restarted.Snapshot()
	var recoveredImport *OutboxEntry
	for index := range recovered.Outbox {
		if recovered.Outbox[index].Kind == EffectImportContext {
			recoveredImport = &recovered.Outbox[index]
			break
		}
	}
	if recoveredImport == nil || recoveredImport.Status != OutboxPending || !recoveredImport.Reconcile {
		t.Fatalf("crashed import was not converted to readback-only: %#v", recoveredImport)
	}
	if recovered.Lanes[beta.ID].Phase != LaneDetached {
		t.Fatalf("provider attachment survived process restart: %#v", recovered.Lanes[beta.ID])
	}

	restartedCoordinator, err := NewCoordinator(restarted, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedCoordinator.Close(context.Background()) })
	if err := restarted.Apply(SelectLane{Identity: beta, Owner: provider.AttachmentOwner{TabID: "tab-after-restart"}, CWD: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if err := restartedCoordinator.Drain(context.Background()); err != nil {
		t.Fatalf("resume and reconcile beta: %v", err)
	}

	journal.mu.Lock()
	importCalls, reconcileCalls := journal.importCalls, journal.reconcileCalls
	journal.mu.Unlock()
	if importCalls != 1 || reconcileCalls != 1 {
		t.Fatalf("provider import calls=%d reconcile calls=%d, want exactly 1/1", importCalls, reconcileCalls)
	}
	state := restarted.Snapshot()
	if state.ActiveLaneID != beta.ID || state.Lanes[beta.ID].CoveredThrough != state.LedgerHead() || state.Lanes[beta.ID].PendingImport != nil {
		t.Fatalf("readback did not commit imported coverage: %#v", state)
	}
}

func TestProviderEventUnionRejectsWrongTypedPayload(t *testing.T) {
	event := provider.Event{
		Kind:      provider.EventTurnTerminal,
		Identity:  provider.EventIdentity{ChatID: "chat", LaneID: "lane", Sequence: 1},
		Assistant: &provider.AssistantEvent{Text: "wrong payload"},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("provider event accepted a payload belonging to another event kind")
	}
}

func TestCoordinatorRuntimeCallbacksReceiveExactActorOperation(t *testing.T) {
	t.Run("delete cleanup", func(t *testing.T) {
		engine, err := NewEngine("delete-callback-chat")
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

		var received provider.OperationID
		if err := coordinator.SetChatCleanup(func(_ context.Context, tabID, chatID string, operationID provider.OperationID) error {
			if tabID != "delete-callback-tab" || chatID != "delete-callback-chat" {
				t.Fatalf("cleanup target = tab=%q chat=%q", tabID, chatID)
			}
			received = operationID
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(InitializeChat{
			Presentation: PresentationState{TabID: "delete-callback-tab"},
			OperationID:  "delete-callback-create", Digest: "delete-callback-create-digest",
		}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(DeleteChat{OperationID: "delete-callback-operation", Force: true}); err != nil {
			t.Fatal(err)
		}
		if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
			t.Fatalf("execute delete cleanup: executed=%v err=%v", executed, err)
		}
		if received != "delete-callback-operation" {
			t.Fatalf("cleanup operation = %q, want exact actor operation", received)
		}
		state := engine.Snapshot()
		if len(state.Outbox) != 1 || state.Outbox[0].Status != OutboxCompleted {
			t.Fatalf("delete cleanup receipt = %#v", state.Outbox)
		}
		if err := engine.Apply(DeleteChat{OperationID: "delete-callback-operation", Force: true}); err != nil {
			t.Fatal("same delete operation retry should be idempotent: ", err)
		}
		if err := engine.Apply(DeleteChat{OperationID: "delete-callback-reused", Force: true}); err == nil {
			t.Fatal("changed delete operation was accepted against the tombstone")
		}
	})

	t.Run("checkpoint restore", func(t *testing.T) {
		engine, err := NewEngine("checkpoint-callback-chat")
		if err != nil {
			t.Fatal(err)
		}
		coordinator, err := NewCoordinator(engine, coordinatorRegistry(t, nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

		var received provider.OperationID
		publishedBeforeCommit := false
		if err := coordinator.SetCheckpointRestoreExecutor(func(_ context.Context, chatID string, turnSequence int, checkpoint json.RawMessage, checkpointDigest string, operationID provider.OperationID) (json.RawMessage, error) {
			if chatID != "checkpoint-callback-chat" || turnSequence != 1 {
				t.Fatalf("checkpoint target = chat=%q turn=%d", chatID, turnSequence)
			}
			if len(checkpoint) == 0 || checkpointDigest == "" {
				t.Fatal("checkpoint executor received no immutable target")
			}
			received = operationID
			return json.RawMessage(`{"ok":true,"chatId":"checkpoint-callback-chat","turnSeq":1}`), nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.SetLifecycleObserver(func(receipt LifecycleReceipt) {
			if receipt.Kind != LifecycleCheckpointRestored || receipt.OperationID != "checkpoint-callback-operation" {
				t.Fatalf("checkpoint lifecycle receipt = %#v", receipt)
			}
			state := engine.Snapshot()
			for _, entry := range state.Outbox {
				if entry.Kind == EffectCheckpointRestore && entry.OperationID == receipt.OperationID && entry.Status != OutboxCompleted {
					publishedBeforeCommit = true
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(InitializeFork{
			Presentation: PresentationState{TabID: "checkpoint-callback-tab"}, SourceChatID: "checkpoint-source",
			OperationID: "checkpoint-callback-create", Digest: "checkpoint-callback-create-digest",
			Messages: []LedgerEvent{{
				EventID: "checkpoint-callback-assistant-event", MessageID: "checkpoint-callback-assistant",
				Role: "assistant", Text: "completed", Status: "done", OperationID: "checkpoint-callback-turn",
			}},
		}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(UpdateEnvironment{
			ExpectedTabID: "checkpoint-callback-tab", Payload: json.RawMessage(`{"chatId":"checkpoint-callback-chat"}`),
			Checkpoints: json.RawMessage(`[{"turnSeq":1,"repos":[]}]`), Reference: json.RawMessage(`null`),
		}); err != nil {
			t.Fatal(err)
		}
		command := checkpointRestoreCommand(t, engine, "checkpoint-callback-operation", 1, 1000)
		if err := engine.Apply(command); err != nil {
			t.Fatal(err)
		}
		if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
			t.Fatalf("execute checkpoint restore: executed=%v err=%v", executed, err)
		}
		if received != "checkpoint-callback-operation" {
			t.Fatalf("checkpoint operation = %q, want exact actor operation", received)
		}
		if publishedBeforeCommit {
			t.Fatal("checkpoint lifecycle publication raced ahead of actor receipt commit")
		}
		state := engine.Snapshot()
		if len(state.Outbox) != 1 || state.Outbox[0].Status != OutboxCompleted {
			t.Fatalf("checkpoint restore receipt = %#v", state.Outbox)
		}
		changedCommand := command
		changedCommand.TurnSequence = 2
		changedCommand.ObservedAtUnixMS = 2000
		if err := engine.Apply(changedCommand); err == nil {
			t.Fatal("changed checkpoint request reused the original operation")
		}
	})
}
