package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func TestConfigureChatReplaysWorkspaceAndControlsReceiptsAsOneRecoverableAction(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	oldCWD, targetCWD := t.TempDir(), t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	const tabID, chatID = "configure-receipt-tab", "configure-receipt-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "configure-receipt-create",
		"title": "Configure receipt", "cwd": oldCWD, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	controls := resolvedChatControls{
		ProviderID: "mock", ModelID: "mock-deterministic", BaseModel: "mock-deterministic", ModeID: "ask",
	}
	if err := runtime.ConfigureChat(context.Background(), tabID, chatID, targetCWD, controls, "configure-receipt-op"); err != nil {
		t.Fatalf("first configure action: %v", err)
	}
	first, ok := runtime.Snapshot(chatID)
	if !ok || first.Presentation.CWD == nil || *first.Presentation.CWD != targetCWD {
		t.Fatalf("first configure workspace = %#v", first.Presentation)
	}
	if _, ok := first.WorkspaceMutationReceipts["configure-receipt-op"]; !ok {
		t.Fatalf("configure did not persist workspace receipt: %#v", first.WorkspaceMutationReceipts)
	}
	if _, ok := first.RuntimeControlMutationReceipts["configure-receipt-op"]; !ok {
		t.Fatalf("configure did not persist controls receipt: %#v", first.RuntimeControlMutationReceipts)
	}

	// A lost ConfigureChat reply must replay both actor receipts. The workspace
	// half returns before manager invalidation, and the controls half returns
	// before applying a second runtime mutation.
	if err := runtime.ConfigureChat(context.Background(), tabID, chatID, targetCWD, controls, "configure-receipt-op"); err != nil {
		t.Fatalf("same configure action retry: %v", err)
	}
	second, _ := runtime.Snapshot(chatID)
	if second.Presentation.WorkspaceRevision != first.Presentation.WorkspaceRevision || second.Presentation.RuntimeControlRevision != first.Presentation.RuntimeControlRevision {
		t.Fatalf("configure retry changed actor revisions: first=%#v second=%#v", first.Presentation, second.Presentation)
	}
}

func TestChatWorkspaceForExactPairRejectsStaleTab(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	workspace := t.TempDir()
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev"})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "workspace-ownership-tab", "workspace-ownership-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "workspace-ownership-create",
		"title": "Workspace ownership", "cwd": workspace, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	if cwd, revision, ok, err := runtime.ChatWorkspaceForExactPair(tabID, chatID); err != nil || !ok || cwd != workspace || revision != 0 {
		t.Fatalf("exact workspace read = cwd %q revision %d ok %v err %v", cwd, revision, ok, err)
	}
	if _, _, ok, err := runtime.ChatWorkspaceForExactPair("stale-workspace-tab", chatID); err == nil || ok || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale workspace read was accepted: ok=%v err=%v", ok, err)
	}
}

func TestStaleSelectionRejectsBeforeEnvironmentRestore(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	workspace := t.TempDir()
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev"})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "selection-ownership-tab", "selection-ownership-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "selection-ownership-create",
		"title": "Selection ownership", "cwd": workspace, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatalf("open actor: %v", err)
	}
	actor.mu.Lock()
	err = actor.engine.Apply(chat.UpdateEnvironment{
		ExpectedTabID: tabID, CWD: workspace, Payload: []byte(`{}`), Checkpoints: []byte(`[]`),
		Reference: []byte(`{"version":0}`),
	})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("seed environment reference: %v", err)
	}
	if _, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "stale-selection-tab", ChatID: chatID, ProviderID: "mock", CWD: workspace,
	}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale selection did not fail before manager environment restore: %v", err)
	}
}

func TestWorkspaceHandlersRequireExactPairBeforeCWDOrProviderWork(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	workspace := t.TempDir()
	attackerCWD := t.TempDir()
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev"})
	t.Cleanup(func() { manager.Reset() })
	store := sharedSessionStore(stateDir)
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	const ownerTabID, chatID = "handler-owner-tab", "handler-workspace-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": ownerTabID, "chatId": chatID, "operationId": "handler-workspace-create",
		"title": "Handler workspace", "cwd": workspace, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	hub := wire.NewHub()
	registerAcpHandlers(hub, manager, stateDir, store, nil, runtime)

	newSessionResult, err := hub.Invoke("app-chat:new-session", []any{map[string]any{
		"tabId": "stale-handler-tab", "chatId": chatID, "operationId": "stale-handler-new-session",
		"providerId": "mock", "cwd": attackerCWD,
	}})
	if err != nil {
		t.Fatalf("stale new-session handler transport error: %v", err)
	}
	newSession := mapFromAnyMain(newSessionResult)
	if !strings.Contains(fieldString(newSession, "error"), "chat surface tab attachment is stale") ||
		fieldString(newSession, "cwd") != "" || fieldString(newSession, "sessionId") != "" {
		t.Fatalf("stale new-session result exposed cwd or attached a provider: %#v", newSession)
	}

	if _, err := hub.Invoke("job:start", []any{map[string]any{
		"tabId": "stale-handler-tab", "chatId": chatID, "sessionId": "forged-session",
		"providerId": "mock", "cwd": attackerCWD, "prompt": "must not start",
		"operationId": "stale-handler-job", "userMessageId": "stale-handler-user",
		"assistantMessageId": "stale-handler-assistant",
	}}); err == nil || !strings.Contains(err.Error(), "chat surface tab attachment is stale") {
		t.Fatalf("stale job:start was not rejected by the exact workspace fence: %v", err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.Presentation.CWD == nil || *state.Presentation.CWD != workspace || len(state.Lanes) != 0 || state.Foreground != nil {
		t.Fatalf("stale handler request changed actor/provider state: ok=%v state=%#v", ok, state)
	}
}

func TestMoveWorkspaceReceiptRetryDoesNotCloseProviderHostAgain(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	oldCWD, targetCWD := t.TempDir(), t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	const tabID, chatID = "workspace-receipt-tab", "workspace-receipt-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "workspace-receipt-create",
		"title": "Workspace receipt", "cwd": oldCWD, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: oldCWD, OperationID: "workspace-receipt-select",
	})
	if err != nil {
		t.Fatalf("attach provider lane: %v", err)
	}
	if _, ok := manager.LiveSession(info.SessionID); !ok {
		t.Fatal("provider host did not attach before workspace move")
	}
	request := map[string]any{
		"tabId": tabID, "chatId": chatID, "cwd": targetCWD, "providerId": "mock",
		"operationId": "workspace-receipt-move", "workspaceRebind": true,
		"replaceSessionId": info.SessionID, "expectedWorkspaceRevision": 0,
	}
	first, err := runtime.MoveWorkspace(context.Background(), request)
	if err != nil {
		t.Fatalf("first workspace move: %v", err)
	}
	if first["workspaceCommitted"] != true || first["workspaceRebound"] != true || first["operationId"] != request["operationId"] {
		t.Fatalf("first move receipt = %#v", first)
	}
	firstSession := info.SessionID
	if _, ok := manager.LiveSession(firstSession); ok {
		t.Fatal("first workspace move left the old provider host live")
	}

	// The old reply is lost, but the renderer/controller retries the exact
	// operation. The actor receipt must answer without entering manager
	// invalidation again (which would otherwise reject or recreate a host).
	second, err := runtime.MoveWorkspace(context.Background(), request)
	if err != nil {
		t.Fatalf("same workspace move retry: %v", err)
	}
	for _, key := range []string{"operationId", "cwd", "sessionId", "workspaceCommitted", "workspaceRebound", "workspaceRevision"} {
		if second[key] != first[key] {
			t.Fatalf("retry changed %s: first=%#v second=%#v", key, first, second)
		}
	}
	if _, ok := manager.LiveSession(firstSession); ok {
		t.Fatal("workspace receipt retry touched the old provider host")
	}

	conflict := cloneJSON(request).(map[string]any)
	conflict["cwd"] = t.TempDir()
	if _, err := runtime.MoveWorkspace(context.Background(), conflict); err == nil {
		t.Fatal("workspace operation id reuse with changed request was accepted")
	}
	revisionConflict := cloneJSON(request).(map[string]any)
	revisionConflict["expectedWorkspaceRevision"] = 1
	if _, err := runtime.MoveWorkspace(context.Background(), revisionConflict); err == nil {
		t.Fatal("workspace operation id reuse with changed revision was accepted")
	}
}

func TestMoveWorkspaceReceiptReplayFinishesCrashWindowWithoutNewEpoch(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	oldCWD, targetCWD := t.TempDir(), t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	const tabID, chatID = "workspace-crash-tab", "workspace-crash-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "workspace-crash-create",
		"title": "Workspace crash window", "cwd": oldCWD, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: oldCWD, OperationID: "workspace-crash-select",
	})
	if err != nil {
		t.Fatalf("attach provider lane: %v", err)
	}
	request := map[string]any{
		"tabId": tabID, "chatId": chatID, "cwd": targetCWD, "providerId": "mock",
		"operationId": "workspace-crash-move", "workspaceRebind": true,
		"replaceSessionId": info.SessionID, "expectedWorkspaceRevision": 0,
	}
	actor, _, err := runtime.exactActor(tabID, chatID)
	if err != nil {
		t.Fatalf("find workspace actor: %v", err)
	}
	beforeCommit, beforeCommitOK := runtime.Snapshot(chatID)
	if !beforeCommitOK {
		t.Fatal("crash-window fixture actor disappeared before durable commit")
	}
	var target chat.DetachTarget
	for laneID, lane := range beforeCommit.Lanes {
		if lane.Attachment == nil {
			continue
		}
		target = chat.DetachTarget{
			OperationID:          chat.DetachOperationID(chatID, laneID, lane.Attachment.ConnectionID, lane.ConnectionGeneration),
			LaneID:               laneID,
			Owner:                lane.Owner,
			ConnectionID:         lane.Attachment.ConnectionID,
			ConnectionGeneration: lane.ConnectionGeneration,
		}
		break
	}
	if target.OperationID == "" {
		t.Fatal("crash-window fixture has no exact durable detach target")
	}
	digest, err := workspaceMoveRequestDigest(request, tabID, chatID, targetCWD, 0, []providercontract.OperationID{target.OperationID})
	if err != nil {
		t.Fatalf("workspace move digest: %v", err)
	}
	// Simulate a daemon crash after the durable actor commit and before the
	// manager's attachment-close half of the transaction.
	actor.mu.Lock()
	err = actor.engine.Apply(chat.ChangeWorkspace{
		OperationID: "workspace-crash-move", Digest: digest, CWD: targetCWD, ExpectedRevision: 0,
		DetachTargets: []chat.DetachTarget{target},
	})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("durable workspace commit: %v", err)
	}
	if _, live := manager.LiveSession(info.SessionID); !live {
		t.Fatal("crash-window fixture lost the old provider attachment before replay")
	}
	beforeReplay, ok := runtime.Snapshot(chatID)
	if !ok || len(beforeReplay.Lanes) != 1 {
		t.Fatalf("crash-window actor snapshot = %#v", beforeReplay)
	}
	var oldLane chat.LaneState
	for _, candidate := range beforeReplay.Lanes {
		oldLane = candidate
		break
	}
	if oldLane.Identity.ID == "" {
		t.Fatalf("crash-window actor lost its historical lane identity: %#v", beforeReplay.Lanes)
	}
	replayed, err := runtime.MoveWorkspace(context.Background(), request)
	if err != nil {
		t.Fatalf("replay committed workspace receipt: %v", err)
	}
	replayedRevision, revisionOK := replayed["workspaceRevision"].(uint64)
	if !revisionOK || replayedRevision != 1 || replayed["cwd"] != targetCWD || replayed["sessionId"] != "" {
		t.Fatalf("replayed workspace receipt = %#v", replayed)
	}
	if liveSession, live := manager.LiveSession(info.SessionID); live {
		state, _ := runtime.Snapshot(chatID)
		lane := state.Lanes[oldLane.Identity.ID]
		var detach chat.OutboxEntry
		for _, entry := range state.Outbox {
			if entry.OperationID == target.OperationID {
				detach = entry
				break
			}
		}
		t.Fatalf("receipt replay left the pre-move provider attachment live: provider=%q active=%q desired=%q lanePhase=%q laneGeneration=%d laneAttached=%t detachStatus=%q",
			liveSession.Info.ProviderID, state.ActiveLaneID, state.DesiredLaneID, lane.Phase, lane.ConnectionGeneration, lane.Attachment != nil, detach.Status)
	}
	// The old provider lane is still durable and remains tied to its original
	// workspace/thread; only the disposable attachment was reconciled.
	afterReplay, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("workspace actor disappeared after receipt replay")
	}
	if _, exists := afterReplay.Lanes[oldLane.Identity.ID]; !exists {
		t.Fatal("receipt replay deleted the old workspace lane epoch")
	}
	if afterReplay.Lanes[oldLane.Identity.ID].CWD != oldLane.CWD ||
		afterReplay.Lanes[oldLane.Identity.ID].Thread != oldLane.Thread {
		t.Fatalf("receipt replay changed the historical lane: before=%#v after=%#v", oldLane, afterReplay.Lanes[oldLane.Identity.ID])
	}
}
