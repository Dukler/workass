package acp

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestPhaseCManagerLanePreservesBurstAndTerminalUnderBoundedBackpressure(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	lane := newUnopenedManagerLaneForTest(t, manager, "phase-c-burst-chat", "phase-c-burst-session")
	if attached := <-lane.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("first lane event = %q, want lane attached", attached.Kind)
	}

	const burst = 512
	collected := make(chan []providercontract.Event, 1)
	go func() {
		events := make([]providercontract.Event, 0, burst+1)
		for event := range lane.Events() {
			// Deliberately keep the actor side slower than the adapter queue. The
			// producer below uses the exact bounded channel path and must block,
			// never overwrite or discard a semantic event.
			time.Sleep(100 * time.Microsecond)
			events = append(events, event)
			lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
		collected <- events
	}()

	producerDone := make(chan error, 1)
	go func() {
		lane.emitMu.Lock()
		defer lane.emitMu.Unlock()
		for index := 0; index < burst; index++ {
			if err := lane.emitLocked(providercontract.Event{
				Kind:  providercontract.EventUsageUpdated,
				Usage: &providercontract.UsageEvent{Used: index + 1, Size: burst},
			}, false); err != nil {
				producerDone <- err
				return
			}
		}
		if err := lane.emitLocked(providercontract.Event{
			Kind: providercontract.EventTurnTerminal,
			Identity: providercontract.EventIdentity{
				OperationID: "phase-c-operation", TurnID: "phase-c-turn",
			},
			Terminal: &providercontract.TerminalEvent{Status: "completed", Result: "done"},
		}, false); err != nil {
			producerDone <- err
			return
		}
		producerDone <- nil
	}()
	select {
	case err := <-producerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded provider producer did not finish")
	}

	// The terminal event is admitted through the same saturated queue, then
	// normal attachment cleanup appends a final detach receipt.
	lane.attachmentClosed()
	var events []providercontract.Event
	select {
	case events = <-collected:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded provider event stream did not close")
	}
	if len(events) != burst+2 { // burst, terminal, detach; attach was consumed above
		t.Fatalf("received %d normalized events, want %d", len(events), burst+2)
	}
	for index, event := range events {
		wantSequence := uint64(index + 2)
		if event.Identity.Sequence != wantSequence {
			t.Fatalf("event %d sequence=%d, want %d", index, event.Identity.Sequence, wantSequence)
		}
		if index < burst && event.Kind != providercontract.EventUsageUpdated {
			t.Fatalf("event %d kind=%q, want usage", index, event.Kind)
		}
	}
	if events[burst].Kind != providercontract.EventTurnTerminal {
		t.Fatalf("event after burst kind=%q, want terminal", events[burst].Kind)
	}
	if events[len(events)-1].Kind != providercontract.EventLaneDetached {
		t.Fatalf("last event kind=%q, want detach", events[len(events)-1].Kind)
	}
}

func TestPhaseCManagerPublicationWaitsForDurableActorState(t *testing.T) {
	t.Parallel()
	const (
		chatID    = "phase-c-publication-chat"
		sessionID = "phase-c-publication-session"
	)
	store := &phaseCBlockingStateStore{
		base:      chat.FileStore{Path: filepath.Join(t.TempDir(), "actor.json")},
		laneID:    phaseCACPIdentity(chatID).ID,
		blockUsed: 42,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	engine, err := chat.NewDurableEngine(chatID, store)
	if err != nil {
		t.Fatal(err)
	}
	identity := phaseCACPIdentity(chatID)
	if err := engine.Apply(chat.SelectLane{Identity: identity}); err != nil {
		t.Fatal(err)
	}
	created, ok, err := engine.ClaimNext()
	if err != nil || !ok {
		t.Fatalf("claim lane create: ok=%v err=%v", ok, err)
	}
	if _, ok := created.(chat.CreateLaneEffect); !ok {
		t.Fatalf("claimed lane effect=%T, want create", created)
	}
	thread := providercontract.ThreadRef{ProviderID: identity.Realm.ProviderID, RootID: sessionID, HeadID: sessionID, Lineage: 1}
	if err := engine.Apply(chat.LaneOpened{
		LaneID: identity.ID, Identity: identity, Thread: thread, ConnectionGeneration: 1,
		Context: providercontract.ContextCapabilities{ExactResume: true, ImportMode: providercontract.ContextImportUnsupported},
	}); err != nil {
		t.Fatal(err)
	}

	published := make(chan chat.State, 1)
	manager := NewManager(Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, _ any) {
			if channel != "job:event" {
				return
			}
			state, ok, loadErr := store.Load(chatID)
			if loadErr != nil {
				t.Errorf("published event could not read actor state: %v", loadErr)
				return
			}
			if !ok {
				t.Errorf("published event had no durable actor state")
				return
			}
			published <- state
		},
	})
	t.Cleanup(func() { manager.Reset() })
	lane := newManagerLane(manager, identity, providercontract.AttachmentOwner{TabID: "phase-c-tab"}, SessionInfo{SessionID: sessionID, ProviderID: "custom"}, thread)
	lane.RequireDurableEventCommits()

	forwarderDone := make(chan struct{})
	go func() {
		defer close(forwarderDone)
		for event := range lane.Events() {
			if event.Kind == providercontract.EventLaneDetached {
				lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
				return
			}
			applyErr := engine.Apply(chat.ProviderEventReceived{ConnectionGeneration: 1, Event: event})
			lane.AcknowledgeDurableEvent(event.Identity.Sequence, applyErr)
			if applyErr != nil {
				return
			}
		}
	}()

	emitted := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "usage", "sessionId": sessionID, "used": 42, "size": 100,
		})
		close(emitted)
	}()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("actor store was not reached for normalized provider event")
	}
	select {
	case <-published:
		t.Fatal("provider event was published before durable actor Save returned")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-emitted:
		t.Fatal("provider callback returned while durable actor commit was blocked")
	default:
	}

	close(store.release)
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("provider callback did not resume after durable actor commit")
	}
	select {
	case state := <-published:
		usage, ok := state.Usage[identity.ID]
		if !ok || usage.Used != 42 {
			t.Fatalf("published actor state usage=%#v, want committed usage 42", usage)
		}
	case <-time.After(time.Second):
		t.Fatal("normalized provider event was not published after durable actor commit")
	}

	lane.attachmentClosed()
	select {
	case <-forwarderDone:
	case <-time.After(time.Second):
		t.Fatal("provider event forwarder did not close")
	}
}

type phaseCBlockingStateStore struct {
	base      chat.FileStore
	laneID    providercontract.LaneID
	blockUsed int
	entered   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *phaseCBlockingStateStore) Load(chatID string) (chat.State, bool, error) {
	return s.base.Load(chatID)
}

func (s *phaseCBlockingStateStore) Save(state chat.State) error {
	if usage, ok := state.Usage[s.laneID]; ok && usage.Used == s.blockUsed {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.base.Save(state)
}

func phaseCACPIdentity(chatID string) providercontract.LaneIdentity {
	return providercontract.LaneIdentity{
		ChatID: chatID,
		Realm: providercontract.Realm{
			ProviderID: "custom", MachineID: "machine", AccountScope: "account", InstallScope: "install",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
}

var _ chat.StateStore = (*phaseCBlockingStateStore)(nil)
