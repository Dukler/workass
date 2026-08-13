package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
			lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
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

func TestProviderLaneArmsDurableCommitsBeforeLaneOpened(t *testing.T) {
	published := make(chan struct{}, 1)
	manager := NewManager(Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, _ any) {
			if channel == "job:event" {
				published <- struct{}{}
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	lane := newUnopenedManagerLaneForTest(t, manager, "startup-window-chat", "startup-window-thread")
	if attached := <-lane.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("first event = %q, want lane attached", attached.Kind)
	}

	emitted := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "usage", "sessionId": "startup-window-thread", "used": 1, "size": 10,
		})
		close(emitted)
	}()

	var event providercontract.Event
	select {
	case event = <-lane.Events():
	case <-time.After(time.Second):
		t.Fatal("provider callback did not enter the lane before LaneOpened")
	}
	select {
	case <-published:
		t.Fatal("provider callback was published before its durable commit")
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-emitted:
		t.Fatal("provider callback returned before its durable commit")
	case <-time.After(100 * time.Millisecond):
	}

	lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
	select {
	case <-emitted:
	case <-time.After(time.Second):
		t.Fatal("provider callback did not resume after its durable commit")
	}
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("provider callback was not published after its durable commit")
	}

	lane.RequireDurableEventCommits()
	detached := make(chan struct{})
	go func() {
		defer close(detached)
		for event := range lane.Events() {
			lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
	}()
	lane.attachmentClosed()
	select {
	case <-detached:
	case <-time.After(time.Second):
		t.Fatal("provider lane did not close after startup-window test")
	}
}

func TestManagedProviderJobRejectsLateEventsAfterTerminalCleanup(t *testing.T) {
	var (
		mu        sync.Mutex
		published []string
	)
	manager := NewManager(Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			if channel != "job:event" {
				return
			}
			mu.Lock()
			published = append(published, strings.TrimSpace(asString(mapFromAny(payload)["type"])))
			mu.Unlock()
		},
	})
	t.Cleanup(func() { manager.Reset() })
	lane := newUnopenedManagerLaneForTest(t, manager, "terminal-fence-chat", "terminal-fence-thread")
	<-lane.Events()
	lane.RequireDurableEventCommits()
	manager.bindProviderLaneJob(lane, "terminal-fence-job", "terminal-fence-operation")

	terminalDone := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "end",
			"job": map[string]any{
				"id": "terminal-fence-job", "sessionId": "terminal-fence-thread",
				"tabId": "terminal-fence-tab", "chatId": "terminal-fence-chat",
				"status": "done", "result": "terminal",
			},
		})
		close(terminalDone)
	}()
	terminal := <-lane.Events()
	if terminal.Kind != providercontract.EventTurnTerminal {
		t.Fatalf("terminal event = %q, want turn terminal", terminal.Kind)
	}
	lane.AcknowledgeDurableEvent(terminal.Identity.Sequence, nil)
	select {
	case <-terminalDone:
	case <-time.After(time.Second):
		t.Fatal("terminal provider event did not publish")
	}
	if manager.providerLaneForJob("terminal-fence-job") != nil || manager.providerLaneManagedJob("terminal-fence-job") {
		t.Fatal("terminal provider job remained in active lane maps")
	}
	if !manager.providerLaneClosedJob("terminal-fence-job") {
		t.Fatal("terminal provider job did not retain a closed-job fence")
	}

	lateEvents := []map[string]any{
		{"type": "data", "id": "terminal-fence-job", "stream": "stdout", "chunk": "late data"},
		{"type": "start", "job": map[string]any{
			"id": "terminal-fence-job", "sessionId": "terminal-fence-thread",
			"tabId": "terminal-fence-tab", "chatId": "terminal-fence-chat", "status": "running",
		}},
		{"type": "acp", "id": "terminal-fence-job", "event": map[string]any{
			"kind": "future-semantic-kind", "text": "late unknown semantic event",
		}},
	}
	for _, late := range lateEvents {
		manager.emit("job:event", late)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(published) != 1 || published[0] != "end" {
		t.Fatalf("late terminal callbacks escaped frozen publication: %#v", published)
	}
}

func TestManagerEmitSerializesDurableObserveAckAndPublication(t *testing.T) {
	firstBroadcastEntered := make(chan struct{})
	secondBroadcastEntered := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	visible := make(chan string, 2)
	manager := NewManager(Options{
		RSSSampleInterval: time.Hour,
		Broadcast: func(channel string, payload any) {
			if channel != "job:event" {
				return
			}
			chunk := asString(mapFromAny(payload)["chunk"])
			switch chunk {
			case "first":
				close(firstBroadcastEntered)
				<-releaseFirst
			case "second":
				secondBroadcastEntered <- struct{}{}
			}
			visible <- chunk
		},
	})
	t.Cleanup(func() { manager.Reset() })
	lane := newUnopenedManagerLaneForTest(t, manager, "publication-order-chat", "publication-order-thread")
	<-lane.Events()
	lane.RequireDurableEventCommits()
	manager.bindProviderLaneJob(lane, "publication-order-job", "publication-order-operation")

	acknowledge := make(chan struct{})
	go func() {
		defer close(acknowledge)
		for event := range lane.Events() {
			lane.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
		}
	}()

	firstDone := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "data", "id": "publication-order-job", "stream": "stdout", "chunk": "first",
		})
		close(firstDone)
	}()
	select {
	case <-firstBroadcastEntered:
	case <-time.After(time.Second):
		t.Fatal("first callback did not reach the frozen broadcaster")
	}

	secondDone := make(chan struct{})
	go func() {
		manager.emit("job:event", map[string]any{
			"type": "data", "id": "publication-order-job", "stream": "stdout", "chunk": "second",
		})
		close(secondDone)
	}()
	select {
	case <-secondBroadcastEntered:
		t.Fatal("second callback published while the first callback was blocked after observe")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first callback did not finish after release")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second callback did not finish after first publication")
	}
	if got := <-visible; got != "first" {
		t.Fatalf("first visible durable callback = %q, want first", got)
	}
	if got := <-visible; got != "second" {
		t.Fatalf("second visible durable callback = %q, want second", got)
	}

	lane.attachmentClosed()
	select {
	case <-acknowledge:
	case <-time.After(time.Second):
		t.Fatal("provider lane did not close after publication-order test")
	}
}

func newUnopenedManagerLaneForTest(t *testing.T, manager *Manager, chatID, sessionID string) *managerLane {
	t.Helper()
	identity := providercontract.LaneIdentity{
		ChatID: chatID,
		Realm: providercontract.Realm{
			ProviderID: "custom", MachineID: "machine", AccountScope: "account", InstallScope: "install",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
	return newManagerLane(manager, identity, providercontract.AttachmentOwner{TabID: "tab-" + chatID}, SessionInfo{
		SessionID: sessionID, ProviderID: "custom",
	}, providercontract.ThreadRef{ProviderID: "custom", RootID: sessionID, HeadID: sessionID, Lineage: 1})
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
			Env: map[string]string{
				"WORKASS_MOCK_ACP_SESSION_STORE":      filepath.Join(stateDir, "provider.json"),
				"WORKASS_MOCK_ACP_SESSION_CAPABILITY": "load",
			},
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
	creation, ok := lane.(providercontract.ThreadCreationReceipt)
	if !ok || creation.ThreadCreationCommitted() {
		t.Fatalf("descriptor-only ACP create did not preserve its first-input boundary: %#v", creation)
	}
	managed, ok := lane.(*managerLane)
	if !ok {
		t.Fatalf("descriptor-only ACP lane type = %T", lane)
	}
	if attached := <-lane.Events(); attached.Kind != providercontract.EventLaneAttached {
		t.Fatalf("descriptor-only first event = %q", attached.Kind)
	}
	turnDone := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		terminal := false
		for event := range lane.Events() {
			managed.AcknowledgeDurableEvent(event.Identity.Sequence, nil)
			if event.Kind == providercontract.EventTurnTerminal && !terminal {
				terminal = true
				close(turnDone)
			}
		}
	}()
	if _, err := lane.Delivery().StartTurn(context.Background(), providercontract.TurnInput{
		OperationID: "descriptor-operation", Text: "commit descriptor-only ACP thread",
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-turnDone:
	case <-time.After(5 * time.Second):
		t.Fatal("descriptor-only ACP turn did not reach its terminal receipt")
	}
	if !creation.ThreadCreationCommitted() {
		t.Fatal("standard ACP activity did not commit the descriptor-only thread")
	}
	if err := lane.Detach(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drainDone:
	case <-time.After(time.Second):
		t.Fatal("descriptor-only ACP lane did not detach")
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

func TestDescriptorOnlyACPProviderRejectsMissingExactAttachmentBeforeCreate(t *testing.T) {
	root, stateDir := repoRoot(t), t.TempDir()
	const providerID = "descriptor-without-attachment"
	providerStore := filepath.Join(stateDir, "provider.json")
	manager := NewManager(Options{
		RootDir: root, StateDir: stateDir, MachineID: "descriptor-machine",
		Providers: []ProviderConfig{{
			ID: providerID, Name: "Descriptor without attachment", Command: "node",
			Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{
				"WORKASS_MOCK_ACP_SESSION_STORE":      providerStore,
				"WORKASS_MOCK_ACP_SESSION_CAPABILITY": "none",
			},
		}},
		DefaultProviderID: providerID, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	selection, err := manager.ResolveProviderLaneSelection(context.Background(), SessionOptions{
		TabID: "descriptor-tab", ChatID: "descriptor-chat", ProviderID: providerID, CWD: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	definition, err := manager.ProviderDefinition(providerID)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = definition.Runtime.Create(context.Background(), providercontract.CreateLaneRequest{
		Identity: selection.Identity, Owner: selection.Owner, CWD: root,
	})
	if !providercontract.ErrorIs(err, providercontract.ErrorUnsupportedCapability) {
		t.Fatalf("missing exact attachment error = %v, want unsupported capability", err)
	}
	if _, exists := manager.nativeSessions.get("descriptor-tab", "descriptor-chat", providerID); exists {
		t.Fatal("unsupported ACP provider created a durable Workass binding")
	}
	if _, err := os.Stat(providerStore); !os.IsNotExist(err) {
		t.Fatalf("unsupported ACP provider reached session/new: stat error=%v", err)
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
