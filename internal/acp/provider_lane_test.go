package acp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestManagerLaneBackpressuresInsteadOfDroppingNormalizedEvents(t *testing.T) {
	manager := NewManager(Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	identity := providercontract.LaneIdentity{
		ChatID: "lossless-chat",
		Realm: providercontract.Realm{
			ProviderID: "custom", MachineID: "machine", AccountScope: "account", InstallScope: "install",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
	thread := providercontract.ThreadRef{ProviderID: "custom", RootID: "thread", HeadID: "thread", Lineage: 1}
	lane := newManagerLane(manager, identity, providercontract.AttachmentOwner{TabID: "tab"}, SessionInfo{SessionID: "thread", ProviderID: "custom"}, thread)
	if attached := <-lane.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("first event = %q, want lane attached", attached.Kind)
	}

	const total = 512
	var (
		mu        sync.Mutex
		sequences []uint64
		kinds     []providercontract.EventKind
	)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range lane.Events() {
			// Make the actor side intentionally slower than the producer. A
			// bounded nonblocking channel loses events under this exact shape.
			time.Sleep(100 * time.Microsecond)
			mu.Lock()
			sequences = append(sequences, event.Identity.Sequence)
			kinds = append(kinds, event.Kind)
			mu.Unlock()
		}
	}()
	for index := 0; index < total; index++ {
		lane.emit(providercontract.Event{
			Kind:  providercontract.EventUsageUpdated,
			Usage: &providercontract.UsageEvent{Used: index + 1, Size: total},
		})
	}
	lane.attachmentClosed()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("provider event stream did not close after detachment")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sequences) != total+1 { // usage events plus the terminal detach event
		t.Fatalf("normalized events delivered = %d, want %d", len(sequences), total+1)
	}
	for index, sequence := range sequences {
		want := uint64(index + 2) // sequence 1 was the consumed attach event
		if sequence != want {
			t.Fatalf("event %d sequence = %d, want %d", index, sequence, want)
		}
	}
	if kinds[len(kinds)-1] != providercontract.EventLaneDetached {
		t.Fatalf("last event = %q, want lane detached", kinds[len(kinds)-1])
	}
}

func TestFrozenWirePublicationWaitsForDurableActorCommit(t *testing.T) {
	published := make(chan struct{}, 1)
	manager := NewManager(Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			if channel == "job:event" {
				published <- struct{}{}
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	identity := providercontract.LaneIdentity{
		ChatID:         "durable-chat",
		Realm:          providercontract.Realm{ProviderID: "custom", MachineID: "machine", AccountScope: "account", InstallScope: "install"},
		WorkspaceEpoch: "workspace",
	}.Normalize()
	thread := providercontract.ThreadRef{ProviderID: "custom", RootID: "thread", HeadID: "thread", Lineage: 1}
	lane := newManagerLane(manager, identity, providercontract.AttachmentOwner{TabID: "tab"}, SessionInfo{SessionID: "thread", ProviderID: "custom"}, thread)
	<-lane.Events() // pre-coordinator attach event
	lane.RequireDurableEventCommits()

	emitted := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "usage", "sessionId": "thread", "used": 1, "size": 10,
		})
		close(emitted)
	}()
	event := <-lane.Events()
	select {
	case <-published:
		t.Fatal("frozen wire published before actor commit acknowledgement")
	case <-time.After(30 * time.Millisecond):
	}
	lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("provider emission did not resume after actor commit")
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("frozen wire was not published after actor commit")
	}

	detached := make(chan struct{})
	go func() {
		event := <-lane.Events()
		lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		close(detached)
	}()
	lane.attachmentClosed()
	<-detached
}

func TestProviderAdmissionReceiptIsOwnedByChatNotDisposableTab(t *testing.T) {
	manager := NewManager(Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	manager.recordProviderLaneAdmission("old-tab", "immutable-chat", "operation-1", map[string]any{"id": "job-1"})
	receipt, ok := manager.ProviderLaneAdmission("replacement-tab", "immutable-chat", "operation-1")
	if !ok || asString(receipt["id"]) != "job-1" {
		t.Fatalf("replacement tab lost immutable chat admission: %#v ok=%v", receipt, ok)
	}
}

func TestDescriptorOnlyACPProviderInheritsCompleteGenericLaneContract(t *testing.T) {
	root, stateDir := repoRoot(t), t.TempDir()
	const providerID = "descriptor-only-dummy"
	if _, registered := providerRegistrationForID(providerID); registered {
		t.Fatal("descriptor-only fixture unexpectedly has provider-specific source registration")
	}
	manager := NewManager(Options{
		RootDir: root, StateDir: stateDir, MachineID: "descriptor-machine",
		Providers: []ProviderConfig{{
			ID: providerID, Name: "Descriptor only", Command: "node",
			Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "provider.json")},
		}},
		DefaultProviderID: providerID, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	definition, err := manager.ProviderDefinition(providerID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Runtime == nil || definition.Realm == nil || definition.Metadata == nil || definition.Update == nil {
		t.Fatalf("descriptor-only definition required missing provider code: %#v", definition)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{
		ProviderID: providerID, MachineID: "descriptor-machine", ChatID: "descriptor-chat", TabID: "descriptor-tab",
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "descriptor-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(root),
	}.Normalize()
	owner := providercontract.AttachmentOwner{TabID: "descriptor-tab"}
	lane, thread, err := definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{
		Identity: identity, Owner: owner, CWD: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity = lane.Identity()
	if err := lane.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	resumed, err := definition.Runtime.Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: identity, Thread: thread, Owner: owner, CWD: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Thread().Equal(thread) || resumed.Identity() != identity {
		t.Fatalf("descriptor-only exact resume changed lane: identity=%#v thread=%#v", resumed.Identity(), resumed.Thread())
	}
	if err := resumed.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConfiguredProviderUsesUnifiedRegistryAndExactLaneFactory(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-cold-effort-resume", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatalf("provider definition: %v", err)
	}
	if definition.Identity.ID != "custom" || definition.Runtime == nil || definition.Realm == nil || definition.Metadata == nil {
		t.Fatalf("incomplete unified provider definition: %#v", definition)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{
		ProviderID: "custom", MachineID: manager.nativeSessions.machineID,
	})
	if err != nil {
		t.Fatalf("resolve realm: %v", err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "registry-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir),
	}.Normalize()
	request := providercontract.CreateLaneRequest{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "registry-tab"}, CWD: manager.opts.RootDir,
	}
	lane, thread, err := definition.Runtime.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("create lane: %v", err)
	}
	canonical := lane.Identity()
	if err := validateCanonicalCreatedLane(identity, canonical); err != nil || !lane.Thread().Equal(thread) || thread.RootID == "" || thread.HeadID != thread.RootID {
		t.Fatalf("created lane identity: lane=%#v thread=%#v", lane.Identity(), thread)
	}
	if canonical.Realm.InstallScope != "registered-custom" {
		t.Fatalf("provider create did not canonicalize its attested install realm: %#v", canonical.Realm)
	}
	attached := <-lane.Events()
	if attached.Kind != providercontract.EventLaneAttached || attached.Thread == nil || !attached.Thread.Equal(thread) {
		t.Fatalf("normalized attach event: %#v", attached)
	}
	if _, _, err := definition.Runtime.Create(context.Background(), request); !providercontract.ErrorIs(err, providercontract.ErrorNativeIdentityConflict) {
		t.Fatalf("established lane create error = %v, want identity conflict", err)
	}
	if err := lane.Detach(context.Background()); err != nil {
		t.Fatalf("detach lane: %v", err)
	}
	reconciled, reconciledThread, err := definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{
		Identity: identity, Owner: request.Owner, CWD: request.CWD, Reconcile: true,
	})
	if err != nil {
		t.Fatalf("reconcile committed create: %v", err)
	}
	if reconciled.Identity() != canonical || !reconciledThread.Equal(thread) {
		t.Fatalf("create reconciliation changed canonical lane: lane=%#v thread=%#v", reconciled.Identity(), reconciledThread)
	}
	if err := reconciled.Detach(context.Background()); err != nil {
		t.Fatalf("detach reconciled lane: %v", err)
	}
	resume, err := definition.Runtime.Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: canonical, Thread: thread, Owner: request.Owner, CWD: request.CWD,
	})
	if err != nil {
		t.Fatalf("resume exact lane: %v", err)
	}
	if !resume.Thread().Equal(thread) {
		t.Fatalf("resume replaced native thread: got %#v want %#v", resume.Thread(), thread)
	}
}

func TestProviderLaneSelectionIsReadOnlyAndReturnsExactStoredBinding(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-cold-effort-resume", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	opts := SessionOptions{TabID: "selection-tab", ChatID: "selection-chat", ProviderID: "custom", CWD: manager.opts.RootDir}
	proposal, err := manager.ResolveProviderLaneSelection(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Established || !proposal.Thread.IsZero() || proposal.Identity.Realm.ProviderID != "custom" {
		t.Fatalf("new lane proposal = %#v", proposal)
	}
	if _, exists := manager.nativeSessions.get(opts.TabID, opts.ChatID, opts.ProviderID); exists {
		t.Fatal("read-only lane selection created a provider binding")
	}
	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	lane, thread, err := definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{
		Identity: proposal.Identity, Owner: proposal.Owner, CWD: proposal.CWD,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lane.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := manager.ResolveProviderLaneSelection(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Established || stored.Identity != lane.Identity() || !stored.Thread.Equal(thread) {
		t.Fatalf("stored selection changed ownership: got=%#v lane=%#v thread=%#v", stored, lane.Identity(), thread)
	}
}

func TestUnifiedLaneCreateNeverRetriesAnAmbiguousProviderCreate(t *testing.T) {
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	manager, _ := newFakeManager(t, "crash-session-new-resume", Options{
		RSSSampleInterval: time.Hour,
		Provider: ProviderConfig{Env: map[string]string{
			"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
		}},
	})
	t.Cleanup(func() { manager.Reset() })
	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "ambiguous-create-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir),
	}.Normalize()
	_, _, err = definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "ambiguous-create-tab"}, CWD: manager.opts.RootDir,
	})
	if !providercontract.ErrorIs(err, providercontract.ErrorAcceptanceAmbiguous) {
		t.Fatalf("ambiguous provider create error = %v", err)
	}
	if methods := readMethodLog(t, methodLog); countMethod(methods, "session/new") != 1 {
		t.Fatalf("provider create methods = %v, want exactly one session/new", methods)
	}
	if _, exists := manager.nativeSessions.get("ambiguous-create-tab", "ambiguous-create-chat", "custom"); exists {
		t.Fatal("ambiguous create invented a durable native binding")
	}
}

func TestUnifiedLaneFactoryRejectsReplacementThreadBeforeProviderCall(t *testing.T) {
	manager, _ := newFakeManager(t, "claude-cold-effort-resume", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{ChatID: "replacement-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir)}.Normalize()
	owner := providercontract.AttachmentOwner{TabID: "replacement-tab"}
	lane, thread, err := definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{Identity: identity, Owner: owner, CWD: manager.opts.RootDir})
	if err != nil {
		t.Fatal(err)
	}
	identity = lane.Identity()
	thread.RootID = "replacement"
	thread.HeadID = "replacement"
	if _, err := definition.Runtime.Resume(context.Background(), providercontract.ResumeLaneRequest{
		Identity: identity, Thread: thread, Owner: owner, CWD: manager.opts.RootDir,
	}); !providercontract.ErrorIs(err, providercontract.ErrorNativeIdentityConflict) {
		t.Fatalf("replacement resume error = %v, want identity conflict", err)
	}
}

func TestCrossProviderSelectionFailsBeforeDetachingActiveLane(t *testing.T) {
	manager, _ := newFakeManager(t, "echo-prompt", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	const tabID = "lane-switch-tab"
	const chatID = "chat-lane-switch-tab"
	session := newFakeSession(t, manager, tabID)

	_, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: tabID, ChatID: chatID,
		ProviderID: "claude", Prompt: "must not reach either provider",
	})
	if !providercontract.ErrorIs(err, providercontract.ErrorUnsupportedCapability) {
		t.Fatalf("cross-provider selection error = %v, want capability-gated lane switch", err)
	}
	bridge := manager.bridgeForSession(session.SessionID, SessionOptions{
		SessionID: session.SessionID, TabID: tabID, ChatID: chatID, ProviderID: session.ProviderID,
	})
	if bridge == nil || !bridge.hasLiveSession(session.SessionID) {
		t.Fatal("blocked provider switch detached the active lane")
	}
	binding, ok := manager.nativeSessions.get(tabID, chatID, session.ProviderID)
	if !ok || binding.SessionID != session.SessionID {
		t.Fatalf("blocked provider switch rewrote the active binding: %#v ok=%v", binding, ok)
	}
	manager.mu.Lock()
	boundProvider := manager.boundProviderForChatLocked(SessionOptions{TabID: tabID, ChatID: chatID})
	jobCount := len(manager.jobs)
	manager.mu.Unlock()
	if boundProvider != session.ProviderID || jobCount != 0 {
		t.Fatalf("blocked switch mutated chat state: provider=%q jobs=%d", boundProvider, jobCount)
	}
}
