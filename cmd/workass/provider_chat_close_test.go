package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func newCloseSessionTestRuntime(t *testing.T) (*providerChatRuntime, *acp.Manager, string) {
	t.Helper()
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node",
			Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:  root, Enabled: true,
			Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0"},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	store := sharedSessionStore(stateDir)
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	t.Cleanup(func() { manager.Reset() })
	return runtime, manager, root
}

func attachCloseSessionTestChat(t *testing.T, runtime *providerChatRuntime, root string) acp.SessionInfo {
	t.Helper()
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "close-tab", "chatId": "close-chat", "operationId": "close-create",
		"title": "Close session", "cwd": root, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create close-session actor: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "close-tab", ChatID: "close-chat", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("attach close-session lane: %v", err)
	}
	return info
}

func TestProviderChatCloseSessionRejectsStaleConnectionID(t *testing.T) {
	identity := providercontract.LaneIdentity{
		ChatID: "close-chat", WorkspaceEpoch: "workspace",
		Realm: providercontract.Realm{ProviderID: "mock", MachineID: "machine", AccountScope: "account", InstallScope: "install"},
	}.Normalize()
	state, err := chat.NewState("close-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "close-tab"
	state.ActiveLaneID = identity.ID
	state.Lanes[identity.ID] = chat.LaneState{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "close-tab"},
		Thread:               providercontract.ThreadRef{ProviderID: "mock", RootID: "native-thread", HeadID: "native-thread", Lineage: 1},
		ConnectionGeneration: 2,
		Attachment:           &providercontract.LaneAttachmentSnapshot{ConnectionID: "new-connection", ProviderID: "mock"},
	}
	live := acp.LiveSession{TabID: "close-tab", ChatID: "close-chat", Info: acp.SessionInfo{SessionID: "native-thread", ProviderID: "mock"}}
	if _, _, ok := currentActorAttachmentForClose(state, live, "native-thread"); ok {
		t.Fatal("stale native-thread close was accepted for a newer connection attachment")
	}
	if state.Lanes[identity.ID].Thread.HeadID != "native-thread" || state.Lanes[identity.ID].Attachment.ConnectionID != "new-connection" {
		t.Fatalf("stale validation mutated the actor lane: %#v", state.Lanes[identity.ID])
	}
}

func TestProviderChatCloseSessionDetachesCurrentAttachmentPreservingThread(t *testing.T) {
	runtime, manager, root := newCloseSessionTestRuntime(t)
	info := attachCloseSessionTestChat(t, runtime, root)
	before, ok := runtime.Snapshot("close-chat")
	if !ok || before.ActiveLaneID == "" {
		t.Fatalf("missing active close-session lane: %#v", before)
	}
	lane := before.Lanes[before.ActiveLaneID]
	if closed := runtime.CloseSession(context.Background(), info.SessionID); !closed {
		t.Fatal("current close did not close the manager attachment")
	}
	if _, ok := manager.LiveSession(info.SessionID); ok {
		t.Fatal("current close left the manager attachment live")
	}
	after, ok := runtime.Snapshot("close-chat")
	if !ok {
		t.Fatal("actor disappeared after current close")
	}
	closedLane := after.Lanes[after.ActiveLaneID]
	if closedLane.Thread != lane.Thread {
		t.Fatalf("current close changed immutable native thread: before=%#v after=%#v", lane.Thread, closedLane.Thread)
	}
	if closedLane.Attachment != nil || closedLane.Phase != chat.LaneDetached {
		t.Fatalf("current close did not durably detach actor attachment: %#v", closedLane)
	}
}

func TestProviderChatCloseSessionRetryCannotCloseExactResumedAttachment(t *testing.T) {
	runtime, manager, root := newCloseSessionTestRuntime(t)
	first := attachCloseSessionTestChat(t, runtime, root)
	before, ok := runtime.Snapshot("close-chat")
	if !ok {
		t.Fatal("missing close-session actor before retry")
	}
	oldLane := before.Lanes[before.ActiveLaneID]
	if !runtime.CloseSession(context.Background(), first.SessionID) {
		t.Fatal("initial close did not settle")
	}

	resumed, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "close-tab", ChatID: "close-chat", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("exact resume after close: %v", err)
	}
	if resumed.SessionID == "" {
		t.Fatal("exact resume returned an empty session id")
	}
	if got, ok := manager.LiveSession(resumed.SessionID); !ok || got.ChatID != "close-chat" {
		t.Fatalf("exact resumed attachment is not live: %#v %v", got, ok)
	}

	// The frozen handler can only receive the old session id on a lost-reply
	// retry. It must return the durable old receipt and leave the newer exact
	// resume live, even when the provider reused the same native id.
	if !runtime.CloseSession(context.Background(), first.SessionID) {
		t.Fatal("lost-reply retry did not replay the durable close receipt")
	}
	if _, ok := manager.LiveSession(resumed.SessionID); !ok {
		t.Fatalf("lost-reply retry closed the newer attachment: resumed=%#v", resumed)
	}
	after, ok := runtime.Snapshot("close-chat")
	if !ok {
		t.Fatal("actor disappeared after close retry")
	}
	newLane := after.Lanes[after.ActiveLaneID]
	if newLane.Thread != oldLane.Thread || newLane.Attachment == nil {
		t.Fatalf("retry changed or removed the exact resumed lane: old=%#v new=%#v", oldLane.Thread, newLane)
	}
}

func TestProviderChatCloseSessionChangedActorTargetFailsClosed(t *testing.T) {
	runtime, manager, root := newCloseSessionTestRuntime(t)
	info := attachCloseSessionTestChat(t, runtime, root)
	actor, err := runtime.actor("close-chat")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := runtime.Snapshot("close-chat")
	if !ok {
		t.Fatal("missing actor before target change")
	}
	lane := state.Lanes[state.ActiveLaneID]
	actor.mu.Lock()
	err = actor.engine.Apply(chat.HostLost{LaneID: lane.Identity.ID, ConnectionGeneration: lane.ConnectionGeneration})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("change actor attachment target: %v", err)
	}

	if runtime.CloseSession(context.Background(), info.SessionID) {
		t.Fatal("close accepted a manager target that no longer matched actor state")
	}
	if _, ok := manager.LiveSession(info.SessionID); !ok {
		t.Fatal("changed-target close touched the manager attachment")
	}
}
