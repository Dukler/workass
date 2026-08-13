package acp

import (
	"context"
	"strings"
	"testing"
	"time"

	chatstate "workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestUnifiedCoordinatorRunsExistingACPManagerThroughTypedEvents(t *testing.T) {
	// A durable provider lane is not legal unless the provider proves exact
	// resume before its one session/new call. Use the resumable fixture so this
	// integration test exercises the same conformance boundary as production.
	startObserved := make(chan bool, 1)
	var engine *chatstate.Engine
	manager, _ := newFakeManager(t, "echo-prompt-resume", Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			if channel != "job:event" || engine == nil {
				return
			}
			event := mapFromAny(payload)
			if strings.TrimSpace(asString(event["type"])) != "start" {
				return
			}
			state := engine.Snapshot()
			owned := state.Foreground != nil && state.Foreground.Status == chatstate.ForegroundRunning &&
				state.Foreground.OperationID == "stable-operation" && strings.TrimSpace(state.Foreground.Turn.NativeID) != ""
			startObserved <- owned
		},
	})
	t.Cleanup(func() { manager.Reset() })
	var err error
	engine, err = chatstate.NewEngine("coordinator-live-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })

	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "coordinator-live-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir),
	}.Normalize()
	if err := engine.Apply(chatstate.SelectLane{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "coordinator-live-tab"}, CWD: manager.opts.RootDir,
	}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("execute create: executed=%v err=%v", executed, err)
	}
	if err := engine.Apply(chatstate.Submit{OperationID: "stable-operation", Text: "typed event turn"}); err != nil {
		t.Fatal(err)
	}
	if executed, err := coordinator.ExecuteNext(context.Background()); err != nil || !executed {
		t.Fatalf("execute turn: executed=%v err=%v", executed, err)
	}
	select {
	case owned := <-startObserved:
		if !owned {
			t.Fatal("frozen job:start published before durable actor admission")
		}
	case <-time.After(time.Second):
		t.Fatal("provider turn did not publish job:start")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		state := engine.Snapshot()
		if state.Foreground == nil && len(state.Ledger) == 2 {
			lane, ok := state.Lanes[state.ActiveLaneID]
			if !ok || lane.Phase != chatstate.LaneReady {
				t.Fatalf("terminal typed event left canonical lane in %q", lane.Phase)
			}
			if state.Ledger[0].OperationID != "stable-operation" || state.Ledger[1].OperationID != "stable-operation" {
				t.Fatalf("visible ledger lost stable operation ownership: %#v", state.Ledger)
			}
			if !strings.Contains(state.Ledger[1].Text, "typed event turn") {
				t.Fatalf("assistant stream was not normalized into the semantic ledger: %#v", state.Ledger[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("typed provider turn did not settle: %#v", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClaudeVerifiedLineageCommitsActorBeforeNativeMaterializationAndExactResume(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-cold-effort-resume", Options{
		RSSSampleInterval: time.Hour, Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatstate.NewEngine("claude-lineage-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := manager.ProviderDefinition("claude")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "claude-lineage-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir),
	}.Normalize()
	if err := engine.Apply(chatstate.SelectLane{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "claude-lineage-tab"}, CWD: manager.opts.RootDir,
	}); err != nil {
		t.Fatal(err)
	}
	if executed, executeErr := coordinator.ExecuteNext(context.Background()); executeErr != nil || !executed {
		t.Fatalf("create Claude lane: executed=%v err=%v", executed, executeErr)
	}
	createdState := engine.Snapshot()
	identity = createdState.Lanes[createdState.ActiveLaneID].Identity
	initial := createdState.Lanes[identity.ID].Thread
	if initial.Lineage != 1 {
		t.Fatalf("initial thread = %#v", initial)
	}
	if err := engine.Apply(chatstate.Submit{OperationID: "claude-lineage-turn", Text: "[fake:claude-updates] go"}); err != nil {
		t.Fatal(err)
	}
	if executed, executeErr := coordinator.ExecuteNext(context.Background()); executeErr != nil || !executed {
		t.Fatalf("start Claude lineage turn: executed=%v err=%v", executed, executeErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	var advanced providercontract.ThreadRef
	for time.Now().Before(deadline) {
		state := engine.Snapshot()
		advanced = state.Lanes[identity.ID].Thread
		if state.Foreground == nil && advanced.Lineage == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if advanced.Lineage != 2 || advanced.HeadID == initial.HeadID || advanced.Proof == "" {
		t.Fatalf("actor did not own verified Claude lineage: initial=%#v advanced=%#v", initial, advanced)
	}
	binding, ok := manager.nativeSessions.getForLane(identity)
	if !ok || !bindingThreadRef(binding).Equal(advanced) {
		t.Fatalf("native lookup was not derived from actor lineage: ok=%v binding=%#v actor=%#v", ok, binding, advanced)
	}
	// Simulate the only torn boundary: actor fsync succeeded, then the daemon
	// died before the derived native lookup was written. Exact resume may materialize
	// this one attested edge from actor state; it may not create or replay.
	manager.nativeSessions.mu.Lock()
	binding.ProviderSessionID = ""
	binding.ThreadLineage = initial.Lineage
	binding.LineageProof = initial.Proof
	manager.nativeSessions.bindings[nativeLaneStorageKey(string(identity.ID))] = binding
	if writeErr := manager.nativeSessions.writeLocked(); writeErr != nil {
		manager.nativeSessions.mu.Unlock()
		t.Fatal(writeErr)
	}
	manager.nativeSessions.mu.Unlock()
	if err := coordinator.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	afterClose := engine.Snapshot()
	if got := afterClose.Lanes[identity.ID].Phase; got != chatstate.LaneDetached {
		t.Fatalf("coordinator close lane = %#v, want detached", afterClose.Lanes[identity.ID])
	}
	coordinator, err = chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	if err := engine.Apply(chatstate.Submit{OperationID: "after-lineage-resume", Text: "resume exact fork head"}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	if got := state.Lanes[identity.ID].Thread; !got.Equal(advanced) {
		t.Fatalf("exact resume changed Claude lineage: got=%#v want=%#v", got, advanced)
	}
	if materialized, ok := manager.nativeSessions.getForLane(identity); !ok || !bindingThreadRef(materialized).Equal(advanced) {
		t.Fatalf("exact resume did not materialize actor-authoritative lineage: ok=%v binding=%#v", ok, materialized)
	}
}

func TestProviderLaneRejectsUnknownFrozenSemanticEvents(t *testing.T) {
	events := &eventCollector{ch: make(chan collectedEvent, 64)}
	manager, _ := newFakeManager(t, "echo-prompt-resume", Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			events.ch <- collectedEvent{channel: channel, payload: payload}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatstate.NewEngine("strict-provider-event-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{ChatID: "strict-provider-event-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir)}.Normalize()
	if err := engine.Apply(chatstate.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "strict-provider-event-tab"}, CWD: manager.opts.RootDir}); err != nil {
		t.Fatal(err)
	}
	if executed, executeErr := coordinator.ExecuteNext(context.Background()); executeErr != nil || !executed {
		t.Fatalf("create lane: executed=%v err=%v", executed, executeErr)
	}
	state := engine.Snapshot()
	laneState := state.Lanes[state.ActiveLaneID]
	lane := manager.providerLaneForSession(laneState.Thread.HeadID)
	if lane == nil {
		t.Fatal("created lane is not attached")
	}
	manager.bindProviderLaneJob(lane, "strict-provider-job", "strict-provider-operation")
	// LaneAttached is delivered asynchronously by the coordinator worker. Let
	// that valid event settle before characterizing the rejected event below.
	time.Sleep(50 * time.Millisecond)
	manager.emit("job:event", map[string]any{
		"type": "acp", "id": "strict-provider-job",
		"event": map[string]any{"kind": "future-semantic-kind", "text": "must not publish"},
	})
	events.expectNoChannel(t, "job:event", 100*time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state := engine.Snapshot()
		if state.Lanes[state.ActiveLaneID].Phase == chatstate.LaneBroken {
			if state.Lanes[state.ActiveLaneID].LastError != providercontract.ErrorProtocolViolation {
				t.Fatalf("protocol failure kind = %q", state.Lanes[state.ActiveLaneID].LastError)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("rejected semantic event did not break its actor lane: %#v", engine.Snapshot())
}

func TestProviderLaneCommandCatalogUpdateIsActorDurableBeforePublication(t *testing.T) {
	manager, events := newFakeManager(t, "claude-commands-resume", Options{
		RSSSampleInterval: time.Hour, Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatstate.NewEngine("actor-command-catalog-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	definition, err := manager.ProviderDefinition("claude")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{ChatID: "actor-command-catalog-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir)}.Normalize()
	if err := engine.Apply(chatstate.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "actor-command-catalog-tab"}, CWD: manager.opts.RootDir}); err != nil {
		t.Fatal(err)
	}
	if executed, executeErr := coordinator.ExecuteNext(context.Background()); executeErr != nil || !executed {
		t.Fatalf("create lane: executed=%v err=%v", executed, executeErr)
	}
	initial := engine.Snapshot().Lanes[engine.Snapshot().ActiveLaneID].Attachment
	if initial == nil || initial.CommandCatalog == nil || initial.CommandCatalog.AsOf != 1785000000000 {
		t.Fatalf("initial actor catalog = %#v", initial)
	}
	if err := engine.Apply(chatstate.Submit{OperationID: "catalog-update-operation", Text: "push commands please"}); err != nil {
		t.Fatal(err)
	}
	if executed, executeErr := coordinator.ExecuteNext(context.Background()); executeErr != nil || !executed {
		t.Fatalf("start catalog turn: executed=%v err=%v", executed, executeErr)
	}
	waitChatCommands(t, events, func(_ map[string]any, catalog *CommandCatalog) bool {
		return catalog != nil && catalog.AsOf == 1785000000001
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state := engine.Snapshot()
		attachment := state.Lanes[state.ActiveLaneID].Attachment
		if attachment != nil && attachment.CommandCatalog != nil && attachment.CommandCatalog.AsOf == 1785000000001 {
			if len(attachment.CommandCatalog.Commands) != 1 || attachment.CommandCatalog.Commands[0].Name != "changed-one" {
				t.Fatalf("actor catalog update = %#v", attachment.CommandCatalog)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("updated command catalog was published without durable actor state")
}
