package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func newTestProviderChatRuntime(t *testing.T, manager *acp.Manager, store *sessionStore, stateDir string) *providerChatRuntime {
	t.Helper()
	runtime := newProviderChatRuntime(manager, store, stateDir)
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("start authoritative provider chat runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	return runtime
}

func TestRendererChatCreationIsDurableIdempotentAndIndependentFromProviderAttachment(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	raw := map[string]any{
		"tabId": "create-tab", "chatId": "create-chat", "operationId": "create-op", "focus": true,
		"title": "Empty durable chat", "titleLocked": false, "group": "Project", "cwd": "/workspace",
		"providerId": "codex", "currentModelId": "gpt", "currentModeId": "default",
		"modelControls": map[string]any{"codex": map[string]any{"gpt": map[string]any{"effort": "high"}}},
	}
	first, err := runtime.CreateRendererChat(raw)
	if err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	second, err := runtime.CreateRendererChat(raw)
	if err != nil {
		t.Fatalf("repeat exact create: %v", err)
	}
	if fieldString(first, "operationId") != "create-op" || intValue(first["actorRevision"]) != intValue(second["actorRevision"]) {
		t.Fatalf("stable create receipts differ: first=%#v second=%#v", first, second)
	}
	if active := fieldString(store.GlobalSnapshot(), "activeId"); active != "create-tab" {
		t.Fatalf("focused create did not commit global active tab: %q", active)
	}
	focusConflict := cloneJSON(raw).(map[string]any)
	focusConflict["focus"] = false
	if _, err := runtime.CreateRendererChat(focusConflict); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("changed focus reused a committed create operation: %v", err)
	}
	if active := fieldString(store.GlobalSnapshot(), "activeId"); active != "create-tab" {
		t.Fatalf("conflicting create changed global focus: %q", active)
	}
	projection, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	created := chatFromSnapshot(projection, "create-tab")
	if fieldString(created, "chatId") != "create-chat" || fieldString(created, "title") != "Empty durable chat" || fieldString(created, "draft") != "" {
		t.Fatalf("empty actor chat projection = %#v", created)
	}
	if _, exists := created["liveSession"]; exists {
		t.Fatalf("chat creation spawned or invented a provider attachment: %#v", created["liveSession"])
	}
	conflict := cloneJSON(raw).(map[string]any)
	conflict["title"] = "different create payload"
	if _, err := runtime.CreateRendererChat(conflict); err == nil {
		t.Fatal("changed payload reused a committed create operation")
	}
	if _, err := runtime.SelectNewChat(context.Background(), map[string]any{
		"tabId": "missing-tab", "chatId": "missing-chat", "providerId": "codex",
	}); err == nil || (!strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), "does not exist")) {
		t.Fatalf("provider attachment manufactured a missing chat: %v", err)
	}
}

func TestForkRetryAfterChildActorCommitAttachesExactlyOnce(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "fork-source-tab", "chatId": "fork-source-chat", "operationId": "create-fork-source",
		"title": "Fork source", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"tabId": "fork-source-tab", "chatId": "fork-source-chat", "newTabId": "fork-child-tab",
		"newChatId": "fork-child-chat", "operationId": "fork-child-once", "cwd": root,
	}
	digest, err := forkInitializationDigest(request, "fork-source-tab", "fork-source-chat", "fork-child-tab", "fork-child-chat")
	if err != nil {
		t.Fatal(err)
	}
	childCWD := root
	child, err := runtime.actorForFork("fork-child-chat", chat.InitializeFork{
		Presentation: chat.PresentationState{
			TabID: "fork-child-tab", Title: "Fork source", CWD: &childCWD,
			ProviderID: "mock", CurrentModelID: "mock-deterministic",
		},
		SourceChatID: "fork-source-chat", OperationID: "fork-child-once", Digest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if state := child.engine.Snapshot(); len(state.Lanes) != 0 || state.CreationOperationID != "fork-child-once" {
		t.Fatalf("pre-attachment child actor = %#v", state)
	}
	result, err := runtime.Fork(context.Background(), request)
	if err != nil {
		t.Fatalf("retry after child actor commit: %v", err)
	}
	if fieldString(result, "sessionId") == "" || fieldString(result, "providerId") != "mock" {
		t.Fatalf("fork retry receipt = %#v", result)
	}
	state := child.engine.Snapshot()
	if len(state.Lanes) != 1 || len(state.LaneSelectionMutationReceipts) != 1 {
		t.Fatalf("fork retry did not attach exactly one lane: lanes=%#v receipts=%#v", state.Lanes, state.LaneSelectionMutationReceipts)
	}
}

func TestForkRetryAfterChildCommitDoesNotReadSourceOrRecreateLane(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "fork-source-delete-tab", "chatId": "fork-source-delete-chat", "operationId": "create-fork-source-delete",
		"title": "Fork source", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"tabId": "fork-source-delete-tab", "chatId": "fork-source-delete-chat", "newTabId": "fork-child-delete-tab",
		"newChatId": "fork-child-delete-chat", "operationId": "fork-child-delete-once", "cwd": root,
	}
	digest, err := forkInitializationDigest(request, "fork-source-delete-tab", "fork-source-delete-chat", "fork-child-delete-tab", "fork-child-delete-chat")
	if err != nil {
		t.Fatal(err)
	}
	childCWD := root
	if _, err := runtime.actorForFork("fork-child-delete-chat", chat.InitializeFork{
		Presentation: chat.PresentationState{
			TabID: "fork-child-delete-tab", Title: "Fork source", CWD: &childCWD,
			ProviderID: "mock", CurrentModelID: "mock-deterministic",
		},
		SourceChatID: "fork-source-delete-chat", OperationID: "fork-child-delete-once", Digest: digest,
	}); err != nil {
		t.Fatal(err)
	}

	if err := runtime.DeleteChat(context.Background(), "fork-source-delete-tab", "fork-source-delete-chat", "delete-fork-source", true); err != nil {
		t.Fatalf("delete fork source after child commit: %v", err)
	}
	if err := os.Remove(providerChatStatePath(stateDir, "fork-source-delete-chat")); err != nil {
		t.Fatalf("remove deleted source actor state: %v", err)
	}
	runtime.mu.Lock()
	delete(runtime.actors, "fork-source-delete-chat")
	delete(runtime.known, "fork-source-delete-chat")
	runtime.mu.Unlock()

	first, err := runtime.Fork(context.Background(), request)
	if err != nil {
		t.Fatalf("exact fork retry required source actor: %v", err)
	}
	second, err := runtime.Fork(context.Background(), request)
	if err != nil {
		t.Fatalf("second exact fork retry: %v", err)
	}
	if fieldString(first, "sessionId") == "" || fieldString(first, "sessionId") != fieldString(second, "sessionId") {
		t.Fatalf("fork retries created different native sessions: first=%#v second=%#v", first, second)
	}
	childState, ok := runtime.Snapshot("fork-child-delete-chat")
	if !ok || len(childState.Lanes) != 1 || len(childState.LaneSelectionMutationReceipts) != 1 {
		t.Fatalf("fork retry did not preserve one durable child lane: %#v", childState)
	}

	changed := cloneJSON(request).(map[string]any)
	changed["cwd"] = filepath.Join(root, "changed")
	if _, err := runtime.Fork(context.Background(), changed); err == nil {
		t.Fatal("changed fork request reused the child creation operation")
	}
	unchanged, _ := runtime.Snapshot("fork-child-delete-chat")
	if len(unchanged.Lanes) != 1 || len(unchanged.LaneSelectionMutationReceipts) != 1 {
		t.Fatalf("changed fork retry mutated child attachment: %#v", unchanged)
	}
}

func TestForkProviderFailureCommitsChildBeforeSelectionAndRetryIsDurable(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "fork-fail-source-tab", "chatId": "fork-fail-source-chat", "operationId": "create-fork-fail-source",
		"title": "Fork source", "cwd": root, "providerId": "provider-that-is-not-configured",
	}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"tabId": "fork-fail-source-tab", "chatId": "fork-fail-source-chat", "newTabId": "fork-fail-child-tab",
		"newChatId": "fork-fail-child-chat", "operationId": "fork-fail-child-once", "cwd": root,
	}
	if _, err := runtime.Fork(context.Background(), request); err == nil {
		t.Fatal("fork unexpectedly selected an unavailable provider")
	}
	firstState, ok := runtime.Snapshot("fork-fail-child-chat")
	if !ok || !firstState.Initialized {
		t.Fatalf("provider failure did not leave a durable initialized child receipt: ok=%v state=%#v", ok, firstState)
	}
	if firstState.CreationOperationID != "fork-fail-child-once" || firstState.CreationDigest == "" {
		t.Fatalf("child creation receipt = operation=%q digest=%q", firstState.CreationOperationID, firstState.CreationDigest)
	}
	if len(firstState.Lanes) != 0 || len(firstState.LaneSelectionMutationReceipts) != 0 {
		t.Fatalf("provider failure attached a child lane before a successful retry: lanes=%#v receipts=%#v", firstState.Lanes, firstState.LaneSelectionMutationReceipts)
	}
	childActor, err := runtime.actor("fork-fail-child-chat")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Fork(context.Background(), request); err == nil {
		t.Fatal("lost fork retry unexpectedly selected an unavailable provider")
	}
	secondState, ok := runtime.Snapshot("fork-fail-child-chat")
	if !ok || secondState.CreationOperationID != firstState.CreationOperationID || secondState.CreationDigest != firstState.CreationDigest {
		t.Fatalf("lost retry changed the durable child receipt: first=%#v second=%#v", firstState, secondState)
	}
	secondActor, err := runtime.actor("fork-fail-child-chat")
	if err != nil {
		t.Fatal(err)
	}
	if childActor != secondActor {
		t.Fatal("lost fork retry recreated the child actor")
	}
	if len(secondState.Lanes) != 0 || len(secondState.LaneSelectionMutationReceipts) != 0 {
		t.Fatalf("lost retry changed child attachment state: lanes=%#v receipts=%#v", secondState.Lanes, secondState.LaneSelectionMutationReceipts)
	}
	changed := cloneJSON(request).(map[string]any)
	changed["cwd"] = filepath.Join(root, "changed-fork-request")
	if _, err := runtime.Fork(context.Background(), changed); err == nil {
		t.Fatal("changed fork retry reused the child creation operation")
	}
	unchanged, ok := runtime.Snapshot("fork-fail-child-chat")
	if !ok || unchanged.CreationDigest != firstState.CreationDigest || len(unchanged.Lanes) != 0 {
		t.Fatalf("changed retry mutated child receipt or attachment: %#v", unchanged)
	}
}

func TestRuntimeControlsCommitToActorBeforeProviderAndApplyOnlyAtTurnBoundary(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0"},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "controls-tab", "chatId": "controls-chat", "operationId": "controls-create", "focus": false,
		"title": "Actor controls", "titleLocked": false, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "controls-tab", ChatID: "controls-chat", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("attach provider lane: %v", err)
	}
	before, ok := manager.LiveSession(info.SessionID)
	if !ok {
		t.Fatal("attached provider session is not live")
	}
	state, ok := runtime.Snapshot("controls-chat")
	if !ok {
		t.Fatal("actor snapshot is missing")
	}
	raw := map[string]any{
		"tabId": "controls-tab", "chatId": "controls-chat", "operationId": "controls-save",
		"expectedRevision": state.Presentation.RuntimeControlRevision, "providerId": "mock",
		"currentModelId": "mock-deterministic[high]", "currentModeId": "bypass",
		"modelControls": map[string]any{"mock": map[string]any{"mock-deterministic": map[string]any{"effort": "high", "modeId": "bypass"}}},
	}
	first, err := runtime.SaveRuntimeControls("controls-tab", "controls-chat", "controls-save", state.Presentation.RuntimeControlRevision, raw)
	if err != nil {
		t.Fatalf("save actor controls: %v", err)
	}
	second, err := runtime.SaveRuntimeControls("controls-tab", "controls-chat", "controls-save", state.Presentation.RuntimeControlRevision, raw)
	if err != nil {
		t.Fatalf("repeat actor controls: %v", err)
	}
	if intValue(first["runtimeControlRevision"]) != intValue(second["runtimeControlRevision"]) {
		t.Fatalf("idempotent control receipts differ: first=%#v second=%#v", first, second)
	}
	stillLive, ok := manager.LiveSession(info.SessionID)
	if !ok {
		t.Fatal("provider session disappeared after actor-only control save")
	}
	if stringPointerValue(stillLive.Info.CurrentModelID) != stringPointerValue(before.Info.CurrentModelID) || stringPointerValue(stillLive.Info.CurrentModeID) != stringPointerValue(before.Info.CurrentModeID) {
		t.Fatalf("control save mutated provider before a journaled turn: before=%#v after=%#v", before.Info, stillLive.Info)
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": "controls-tab", "chatId": "controls-chat", "sessionId": info.SessionID,
		"operationId": "controls-turn", "userMessageId": "controls-user", "assistantMessageId": "controls-assistant",
		"prompt": "apply actor controls at the turn boundary",
	}, "human"); err != nil {
		t.Fatalf("start actor-controlled turn: %v", err)
	}
	waitProviderChatIdle(t, runtime, "controls-chat", 5*time.Second)
	applied, ok := manager.LiveSession(info.SessionID)
	if !ok {
		t.Fatal("provider session disappeared after controlled turn")
	}
	if stringPointerValue(applied.Info.CurrentModelID) != "mock-deterministic[high]" || stringPointerValue(applied.Info.CurrentModeID) != "bypass" {
		t.Fatalf("turn boundary did not apply actor controls: %#v", applied.Info)
	}

	state, _ = runtime.Snapshot("controls-chat")
	clearRaw := map[string]any{
		"tabId": "controls-tab", "chatId": "controls-chat", "operationId": "controls-clear-mode",
		"expectedRevision": state.Presentation.RuntimeControlRevision, "providerId": "mock",
		"currentModelId": "mock-deterministic[high]", "currentModeId": nil, "modelControls": map[string]any{},
	}
	if _, err := runtime.SaveRuntimeControls("controls-tab", "controls-chat", "controls-clear-mode", state.Presentation.RuntimeControlRevision, clearRaw); err != nil {
		t.Fatalf("clear nullable actor mode: %v", err)
	}
	state, _ = runtime.Snapshot("controls-chat")
	if state.Presentation.CurrentModeID != "" {
		t.Fatalf("nullable mode retained stale provider value %q", state.Presentation.CurrentModeID)
	}
}

func TestProviderLaneSelectionIsReadOnlyUntilAtomicReceiptCommit(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "selection-tab", "chatId": "selection-chat", "operationId": "selection-create",
		"title": "Selection", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	before, ok := runtime.Snapshot("selection-chat")
	if !ok {
		t.Fatal("selection actor missing")
	}
	_, err := runtime.SelectNewChat(context.Background(), map[string]any{
		"tabId": "selection-tab", "chatId": "selection-chat", "operationId": "selection-invalid",
		"providerId": "provider-that-does-not-exist", "currentModelId": "invalid-model", "currentModeId": "invalid-mode",
	})
	if err == nil {
		t.Fatal("invalid provider selection unexpectedly succeeded")
	}
	afterInvalid, _ := runtime.Snapshot("selection-chat")
	if before.Revision != afterInvalid.Revision || !reflect.DeepEqual(before.Presentation, afterInvalid.Presentation) ||
		!reflect.DeepEqual(before.Lanes, afterInvalid.Lanes) || len(afterInvalid.LaneSelectionMutationReceipts) != 0 {
		t.Fatalf("invalid provider selection mutated actor state: before=%#v after=%#v", before, afterInvalid)
	}

	selection := map[string]any{
		"tabId": "selection-tab", "chatId": "selection-chat", "operationId": "selection-commit",
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}
	first, err := runtime.SelectNewChat(context.Background(), selection)
	if err != nil {
		t.Fatalf("first lane selection: %v", err)
	}
	second, err := runtime.SelectNewChat(context.Background(), selection)
	if err != nil {
		t.Fatalf("idempotent lane selection retry: %v", err)
	}
	if first.SessionID != second.SessionID {
		t.Fatalf("lane retry changed native attachment: first=%#v second=%#v", first, second)
	}
	committed, _ := runtime.Snapshot("selection-chat")
	receipt, ok := committed.LaneSelectionMutationReceipts["selection-commit"]
	if !ok || receipt.Digest == "" || receipt.LaneID == "" {
		t.Fatalf("missing durable lane-selection receipt: %#v", committed.LaneSelectionMutationReceipts)
	}
	revision, providerID, modelID, modeID := committed.Presentation.RuntimeControlRevision, committed.Presentation.ProviderID, committed.Presentation.CurrentModelID, committed.Presentation.CurrentModeID
	conflict := cloneJSON(selection).(map[string]any)
	conflict["currentModelId"] = "different-model"
	if _, err := runtime.SelectNewChat(context.Background(), conflict); err == nil {
		t.Fatal("conflicting lane-selection operation id was accepted")
	}
	afterConflict, _ := runtime.Snapshot("selection-chat")
	if afterConflict.Presentation.RuntimeControlRevision != revision || afterConflict.Presentation.ProviderID != providerID ||
		afterConflict.Presentation.CurrentModelID != modelID || afterConflict.Presentation.CurrentModeID != modeID {
		t.Fatalf("conflicting lane selection changed durable controls: before=%#v after=%#v", committed.Presentation, afterConflict.Presentation)
	}
}

func TestProjectLedgerMessageUsesOnlyRichActorState(t *testing.T) {
	terminal := true
	message, err := projectLedgerMessage(chat.LedgerEvent{
		MessageID: "assistant", Role: "assistant", Text: "A😀", Result: "answer", Status: "done",
		At: "2026-08-11T12:00:01Z", NativeTurnID: "native-turn", TurnRootID: "root", TurnTerminal: &terminal,
		Interrupted: true, RetryPrompt: "retry me",
		Attachments: []providercontract.Attachment{{ID: "assistant-image", MIMEType: "image/png", Ref: "workass-session-image:assistant-image"}},
		Timeline: []chat.TimelineEntry{
			{Key: "thinking", At: 3, Kind: providercontract.EventThinkingUpdate, Thinking: &providercontract.ThinkingEvent{Text: "reason"}},
			{Key: "tool", At: 3, Kind: providercontract.EventToolUpdate, Tool: &providercontract.ToolEvent{
				ToolCallID: "tool-1", ToolKind: "terminal", Title: "Run", Status: "completed", Command: "go test", Output: "ok",
				SubagentID: "sub", SubagentLabel: "review", SubagentProvider: "codex", SubagentModel: "gpt", SubagentHeader: true,
				StartedAtUnixMS: 100, EndedAtUnixMS: 200,
				Attachments: []providercontract.Attachment{{ID: "tool-image", MIMEType: "image/png", Ref: "workass-session-image:tool-image"}},
			}},
			{Key: "plan", At: 3, Kind: providercontract.EventPlanUpdate, Plan: &providercontract.PlanEvent{Entries: []providercontract.PlanEntry{{Text: "Verify", Status: "completed"}}}},
			{Key: "compaction", At: 3, Kind: providercontract.EventCompactionCheckpoint, Compaction: &providercontract.CompactionEvent{CheckpointID: "checkpoint", Digest: "digest"}},
		},
		Permission: &providercontract.PermissionEvent{
			RequestID: "permission", Title: "Choose", Kind: "question", Status: "pending",
			OptionDetails: []providercontract.PermissionOption{{ID: "yes", Name: "Yes", Kind: "allow"}},
			Question:      &providercontract.PermissionQuestion{Question: "Proceed?", Header: "Decision", Options: []providercontract.PermissionQuestionOption{{Label: "Yes", Description: "Continue"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fieldString(message, "content") != "A😀" || fieldString(message, "result") != "answer" || fieldString(message, "turnRootId") != "root" || message["turnTerminal"] != true {
		t.Fatalf("assistant projection lost semantic fields: %#v", message)
	}
	if message["interrupted"] != true || fieldString(message, "retryPrompt") != "retry me" {
		t.Fatalf("interruption projection = %#v", message)
	}
	events := anySlice(message["events"])
	if len(events) != 4 || intValue(mapFromAnyMain(events[0])["at"]) != 3 || fieldString(mapFromAnyMain(events[0]), "text") != "reason" {
		t.Fatalf("timeline projection = %#v", events)
	}
	tool := mapFromAnyMain(events[1])
	if fieldString(tool, "output") != "ok" || intValue(tool["startedAt"]) != 100 || intValue(tool["endedAt"]) != 200 || tool["subagentHeader"] != true || len(anySlice(tool["images"])) != 1 {
		t.Fatalf("tool projection lost lifecycle fields: %#v", tool)
	}
	permission := mapFromAnyMain(message["permission"])
	if fieldString(permission, "id") != "permission" || len(anySlice(permission["options"])) != 1 || fieldString(mapFromAnyMain(permission["question"]), "question") != "Proceed?" {
		t.Fatalf("permission projection = %#v", permission)
	}
	if len(anySlice(message["images"])) != 1 {
		t.Fatalf("assistant media projection = %#v", message["images"])
	}
}

func TestProjectActorChatRendersAmbiguousAdmissionAsBlockedInsteadOfRunning(t *testing.T) {
	state, err := chat.NewState("ambiguous-chat")
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: "ambiguous-chat",
		Realm: providercontract.Realm{
			ProviderID: "mock", MachineID: "machine", AccountScope: "default", InstallScope: "official",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
	apply := func(command chat.Command) {
		t.Helper()
		state, _, err = chat.Reduce(state, command)
		if err != nil {
			t.Fatalf("reduce %T: %v", command, err)
		}
	}
	apply(chat.InitializeChat{Presentation: chat.PresentationState{TabID: "tab"}, OperationID: "create:runtime", Digest: "create-runtime"})
	apply(chat.SelectLane{Identity: identity})
	apply(chat.LaneOpened{
		LaneID:               identity.ID,
		Thread:               providercontract.ThreadRef{ProviderID: "mock", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              providercontract.ContextCapabilities{ExactResume: true, ImportMode: providercontract.ContextImportUnsupported},
	})
	apply(chat.Submit{
		OperationID: "ambiguous", Text: "send once",
		Presentation: providercontract.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", StartedAt: "2026-08-11T12:00:00Z"},
	})
	apply(chat.TurnAdmitted{OperationID: "ambiguous", Ambiguous: true})

	projection := map[string]any{"chatId": "ambiguous-chat", "messages": []any{}}
	if err := projectActorChat(projection, state); err != nil {
		t.Fatal(err)
	}
	messages := anySlice(projection["messages"])
	if len(messages) != 2 {
		t.Fatalf("ambiguous projection messages = %#v", messages)
	}
	user := mapFromAnyMain(messages[0])
	assistant := mapFromAnyMain(messages[1])
	if fieldString(user, "status") != "done" || fieldString(assistant, "status") != "failed" || assistant["interrupted"] != true {
		t.Fatalf("ambiguous projection remained optimistic/running: user=%#v assistant=%#v", user, assistant)
	}
	if _, retryable := assistant["retryPrompt"]; retryable {
		t.Fatalf("ambiguous delivery exposed an unsafe resend: %#v", assistant)
	}
	if strings.TrimSpace(fieldString(assistant, "content")) == "" {
		t.Fatalf("ambiguous delivery has no visible blocked explanation: %#v", assistant)
	}
}

func TestProviderChatRuntimeMigratesLegacyTranscriptBeforeOpeningLane(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	const tabID, chatID = "legacy-migration-tab", "legacy-migration-chat"
	snapshot := sessionMirrorFixture(tabID, chatID, "old question")
	chat := chatFromSnapshot(snapshot, tabID)
	chat["title"] = "Legacy actor migration"
	chat["cwd"] = root
	chat["currentModelId"] = "mock-deterministic"
	chat["currentModeId"] = "ask"
	chat["draft"] = "unsent legacy draft"
	chat["messages"] = []any{
		map[string]any{"id": "legacy-user", "role": "user", "content": "old question", "status": "done", "at": "2026-08-11T00:00:00Z", "events": []any{}},
		map[string]any{"id": "legacy-assistant", "role": "assistant", "content": "old commentary", "result": "old answer", "status": "done", "at": "2026-08-11T00:00:01Z", "events": []any{
			map[string]any{"key": "tool-1", "at": 3, "kind": "tool", "id": "tool", "title": "Read", "status": "done", "output": "ok"},
		}},
	}
	writeLegacySessionSnapshot(t, stateDir, snapshot)
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0"},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, store, stateDir)
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	if !state.Migration.Complete || state.LedgerHead() != 2 || state.Presentation.Draft != "unsent legacy draft" || len(state.Lanes) != 0 {
		t.Fatalf("legacy chat did not migrate before provider selection: %#v", state)
	}
	if !state.Ledger[0].Legacy || state.Ledger[0].LaneID != "" || state.Ledger[1].ProviderID != "" {
		t.Fatalf("migration guessed historical provider ownership: %#v", state.Ledger)
	}
	if state.Ledger[1].Result != "old answer" || len(state.Ledger[1].Timeline) != 1 || state.Ledger[1].Timeline[0].Tool == nil || state.Ledger[1].Timeline[0].Tool.Output != "ok" {
		t.Fatalf("rich legacy row was not normalized into actor state: %#v", state.Ledger[1])
	}
	rootProjection, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	var projectedChat map[string]any
	for _, raw := range anySlice(rootProjection["chats"]) {
		candidate := mapFromAnyMain(raw)
		if fieldString(candidate, "chatId") == chatID {
			projectedChat = candidate
			break
		}
	}
	projectedMessages := messageSlice(projectedChat)
	if fieldString(projectedChat, "draft") != "unsent legacy draft" || len(projectedMessages) != 2 {
		t.Fatalf("actor snapshot parity failed: %#v", projectedChat)
	}
	projectedAssistant := mapFromAnyMain(projectedMessages[1])
	if fieldString(projectedAssistant, "result") != "old answer" || len(anySlice(projectedAssistant["events"])) != 1 {
		t.Fatalf("actor projector lost rich assistant state: %#v", projectedAssistant)
	}
	projectedChat["title"] = "Renamed by renderer"
	projectedChat["draft"] = "new draft"
	projectedChat["messages"] = []any{
		map[string]any{"id": "forged", "role": "user", "content": "overwrite actor history", "status": "done", "events": []any{}},
	}
	projectedChat["queue"] = []any{map[string]any{"id": "queued-1", "text": "follow up", "delivery": "queue"}}
	rootProjection[globalPresentationOperationField] = "global-test-save"
	if saved, err := runtime.ApplyRendererSnapshot(rootProjection); err != nil || !saved {
		t.Fatalf("apply renderer global snapshot: saved=%v err=%v", saved, err)
	}
	if _, err := runtime.SavePresentation(tabID, chatID, "presentation-test", 0, map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "presentation-test", "expectedRevision": 0,
		"title": "Renamed by renderer", "titleLocked": true, "group": nil, "draft": "new draft",
		"unread": false, "settled": "", "pane": nil,
	}); err != nil {
		t.Fatalf("apply actor presentation command: %v", err)
	}
	afterSave := actor.engine.Snapshot()
	if afterSave.Presentation.Title != "Renamed by renderer" || afterSave.Presentation.Draft != "new draft" || len(afterSave.StagedQueue) != 0 {
		t.Fatalf("renderer presentation command was not committed: %#v", afterSave)
	}
	if afterSave.LedgerHead() != 2 || afterSave.Ledger[0].MessageID != "legacy-user" || afterSave.Presentation.AgentQueueRevision != 0 {
		t.Fatalf("renderer save overwrote semantic state or missed queue CAS: %#v", afterSave)
	}
	authoritative, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range anySlice(authoritative["chats"]) {
		candidate := mapFromAnyMain(raw)
		if fieldString(candidate, "chatId") != chatID {
			continue
		}
		if got := messageSlice(candidate); len(got) != 2 || fieldString(mapFromAnyMain(got[0]), "id") != "legacy-user" {
			t.Fatalf("forged renderer transcript escaped projection boundary: %#v", candidate)
		}
		if queue := anySlice(candidate["queue"]); len(queue) != 0 {
			t.Fatalf("forged renderer queue escaped the presentation-only save boundary: %#v", candidate)
		}
	}
	if _, err := runtime.Select(context.Background(), acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("nonempty migrated chat joined provider without verified import: %v", err)
	}
}

func TestProviderChatRuntimeMigratesExactLegacyBindingAndContinuesSameThread(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	const tabID, chatID = "legacy-exact-tab", "legacy-exact-chat"
	const nativeSessionID = "mock-session-before-actor-cutover"
	legacyMessages := []map[string]any{
		{"id": "legacy-exact-user", "role": "user", "content": "question already seen by native thread", "status": "done", "at": "2026-08-11T00:00:00Z", "events": []any{}},
		{"id": "legacy-exact-assistant", "role": "assistant", "content": "answer already produced by native thread", "result": "answer already produced by native thread", "status": "done", "at": "2026-08-11T00:00:01Z", "events": []any{}},
	}
	snapshot := sessionMirrorFixture(tabID, chatID, "question already seen by native thread")
	legacyChat := chatFromSnapshot(snapshot, tabID)
	legacyChat["title"] = "Legacy exact lane"
	legacyChat["messages"] = []any{legacyMessages[0], legacyMessages[1]}
	legacyChat["sessionId"] = nativeSessionID
	legacyChat["sessionProviderId"] = "mock"
	legacyChat["providerId"] = "mock"
	legacyChat["cwd"] = root
	legacyChat["currentModelId"] = "mock-deterministic"
	legacyChat["currentModeId"] = "ask"
	writeLegacySessionSnapshot(t, stateDir, snapshot)
	store := sharedSessionStore(stateDir)
	writeJSONTestFile(t, filepath.Join(stateDir, "native-sessions.json"), map[string]any{
		"v": 2,
		"bindings": []any{map[string]any{
			"tabId": tabID, "chatId": chatID, "providerId": "mock", "sessionId": nativeSessionID,
			"cwd": root, "modelId": "mock-deterministic", "modeId": "ask",
			// A selected exact binding from the oldest ledger schema may have no
			// cursor hash. The cutover must still continue that exact thread; it
			// must not reinterpret the same-chat history as a cross-provider import.
			"syncedMessages": 0,
			"generation":     1, "resumeSafe": true,
		}},
	})
	providerState := filepath.Join(stateDir, "mock-native-sessions.json")
	writeJSONTestFile(t, providerState, map[string]any{
		"v": 1,
		"sessions": []any{map[string]any{
			"id": nativeSessionID, "cwd": root, "model": "mock-deterministic", "mode": "ask", "turn": 1,
			"operations": map[string]any{}, "contextImports": map[string]any{},
		}},
	})
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": providerState,
			},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.LedgerHead() != 2 || len(state.Lanes) != 1 {
		t.Fatalf("legacy exact lane was not migrated: ok=%v state=%#v", ok, state)
	}
	lane := state.Lanes[state.ActiveLaneID]
	if lane.Thread.RootID != nativeSessionID || lane.CoveredThrough != state.LedgerHead() || lane.Phase != chat.LaneDetached {
		t.Fatalf("legacy exact lane lost native coverage: %#v", lane)
	}
	resumed, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("resume exact migrated lane: %v", err)
	}
	if resumed.SessionID != nativeSessionID {
		t.Fatalf("migration replaced native thread: got %q want %q", resumed.SessionID, nativeSessionID)
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Legacy exact lane", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "sessionId": nativeSessionID, "cwd": root, "prompt": "continue after actor cutover",
		"userMessageId": "post-cutover-user", "assistantMessageId": "post-cutover-assistant",
	}, "human"); err != nil {
		t.Fatalf("continue exact migrated lane: %v", err)
	}
	waitProviderChatLedger(t, runtime, chatID, 4, 5*time.Second)
	var providerDisk struct {
		Sessions []struct {
			ID   string `json:"id"`
			Turn int    `json:"turn"`
		} `json:"sessions"`
	}
	readJSONTestFile(t, providerState, &providerDisk)
	if len(providerDisk.Sessions) != 1 || providerDisk.Sessions[0].ID != nativeSessionID || providerDisk.Sessions[0].Turn != 2 {
		t.Fatalf("cutover created or changed provider thread: %#v", providerDisk.Sessions)
	}
}

func TestLegacyPendingPermissionRehydratesAcrossCutoverAndRestart(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	const tabID, chatID = "legacy-permission-tab", "legacy-permission-chat"
	const nativeSessionID = "mock-session-with-legacy-permission"
	const permissionID = "legacy-permission-request"
	const turnID = "legacy-permission-turn"

	snapshot := sessionMirrorFixture(tabID, chatID, "continue the existing thread")
	legacyChat := chatFromSnapshot(snapshot, tabID)
	legacyChat["title"] = "Legacy pending permission"
	legacyChat["sessionId"] = nativeSessionID
	legacyChat["sessionProviderId"] = "mock"
	legacyChat["providerId"] = "mock"
	legacyChat["cwd"] = root
	legacyChat["currentModelId"] = "mock-deterministic"
	legacyChat["currentModeId"] = "ask"
	legacyChat["queue"] = []any{}
	legacyChat["messages"] = []any{
		map[string]any{
			"id": "legacy-permission-user", "role": "user", "content": "continue the existing thread",
			"status": "done", "at": "2026-08-11T00:00:00Z", "events": []any{},
		},
		map[string]any{
			"id": "legacy-permission-assistant", "role": "assistant", "content": "waiting for approval",
			"result": "waiting for approval", "status": "running", "jobId": turnID,
			"at": "2026-08-11T00:00:01Z", "events": []any{},
			"permission": map[string]any{
				"id": permissionID, "title": "Choose an action", "kind": "question",
				"options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow"}},
				"question": map[string]any{
					"question": "Continue?", "header": "Approval", "multiSelect": false,
					"options": []any{map[string]any{"label": "Allow", "description": "Continue the native turn"}},
				},
			},
		},
	}
	writeLegacySessionSnapshot(t, stateDir, snapshot)
	writeJSONTestFile(t, filepath.Join(stateDir, "native-sessions.json"), map[string]any{
		"v": 2,
		"bindings": []any{map[string]any{
			"tabId": tabID, "chatId": chatID, "providerId": "mock", "sessionId": nativeSessionID,
			"cwd": root, "modelId": "mock-deterministic", "modeId": "ask",
			"syncedMessages": 0, "generation": 1, "resumeSafe": true,
		}},
	})
	providerState := filepath.Join(stateDir, "mock-native-sessions.json")
	writeJSONTestFile(t, providerState, map[string]any{
		"v": 1,
		"sessions": []any{map[string]any{
			"id": nativeSessionID, "cwd": root, "model": "mock-deterministic", "mode": "ask", "turn": 1,
			"operations": map[string]any{}, "contextImports": map[string]any{},
		}},
	})

	newManager := func() *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{
				ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
				CWD: root, Enabled: true, Env: map[string]string{
					"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": providerState,
				},
			},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		})
	}
	boot := func() (*providerChatRuntime, *acp.Manager) {
		manager := newManager()
		runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
		if err := runtime.StartupError(); err != nil {
			manager.Reset()
			t.Fatalf("legacy permission cutover boot: %v", err)
		}
		return runtime, manager
	}
	assertPending := func(runtime *providerChatRuntime) {
		t.Helper()
		pending, err := runtime.PendingPermissions()
		if err != nil {
			t.Fatalf("read pending permission: %v", err)
		}
		if len(pending) != 1 {
			t.Fatalf("pending permissions after actor boot = %#v", pending)
		}
		projected := mapFromAnyMain(pending[0])
		if fieldString(projected, "id") != permissionID || fieldString(projected, "jobId") != turnID ||
			fieldString(projected, "tabId") != tabID || fieldString(projected, "chatId") != chatID ||
			fieldString(projected, "sessionId") != nativeSessionID || fieldString(projected, "title") != "Choose an action" ||
			fieldString(projected, "kind") != "question" || len(anySlice(projected["options"])) != 1 {
			t.Fatalf("rehydrated permission projection = %#v", projected)
		}
		question := mapFromAnyMain(projected["question"])
		if fieldString(question, "question") != "Continue?" || fieldString(question, "header") != "Approval" {
			t.Fatalf("rehydrated permission question = %#v", question)
		}
		state, ok := runtime.Snapshot(chatID)
		if !ok {
			t.Fatal("migrated permission actor is missing")
		}
		permission, ok := state.Permissions[permissionID]
		if !ok || permission.Owner.OperationID != "legacy-permission-user" || permission.Owner.TurnID != turnID {
			t.Fatalf("durable permission owner = %#v state=%#v", permission, state)
		}
	}

	first, firstManager := boot()
	assertPending(first)
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstManager.Reset()

	second, secondManager := boot()
	t.Cleanup(func() {
		_ = second.Close(context.Background())
		secondManager.Reset()
	})
	assertPending(second)
}

func writeJSONTestFile(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONTestFile(t *testing.T, path string, target any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}

func TestProviderChatRuntimeResumesExactLaneAcrossActorAndTabRestart(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	tracePath := filepath.Join(stateDir, "provider-trace.log")
	providerState := filepath.Join(stateDir, "provider-native.json")
	const oldTabID, chatID = "exact-actor-tab", "exact-actor-chat"
	store := sharedSessionStore(stateDir)
	newManager := func() *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{
				ID: "mock", Name: "Mock Provider", Command: "node",
				Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root,
				Env: map[string]string{
					"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": providerState,
					"WORKASS_MOCK_ACP_TRACE_FILE": tracePath,
				},
				Enabled: true,
			},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		})
	}

	firstManager := newManager()
	firstRuntime := newProviderChatRuntime(firstManager, store, stateDir)
	if _, err := firstRuntime.CreateRendererChat(map[string]any{
		"tabId": oldTabID, "chatId": chatID, "operationId": "create-exact-actor-chat",
		"title": "Exact actor lane", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		firstManager.Reset()
		t.Fatalf("create actor-native exact lane: %v", err)
	}
	first, err := firstRuntime.Select(context.Background(), acp.SessionOptions{
		TabID: oldTabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		firstManager.Reset()
		t.Fatalf("create first exact lane: %v", err)
	}
	if _, err := firstRuntime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Exact actor lane", "tabId": oldTabID, "chatId": chatID,
		"providerId": "mock", "sessionId": first.SessionID, "cwd": root, "prompt": "first durable turn",
		"userMessageId": "actor-user-1", "assistantMessageId": "actor-assistant-1",
	}, "human"); err != nil {
		firstManager.Reset()
		t.Fatalf("start first actor turn: %v", err)
	}
	waitProviderChatIdle(t, firstRuntime, chatID, 5*time.Second)
	if _, handled, err := firstRuntime.Steer(context.Background(), map[string]any{
		"sessionId": first.SessionID, "tabId": "attacker-tab", "chatId": chatID,
		"prompt": "must not steer through a stale tab", "clientUserMessageId": "stale-steer-user",
		"continuationAssistantMessageId": "stale-steer-assistant",
	}); !handled || err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale app-chat steer was not rejected: handled=%v err=%v", handled, err)
	}
	if _, handled, err := firstRuntime.SteerQueued(context.Background(), "attacker-tab", chatID, "stale-queue", "must not steal actor tab"); !handled || err == nil || !strings.Contains(err.Error(), "own") {
		t.Fatalf("stale queued steer was not rejected: handled=%v err=%v", handled, err)
	}
	if err := firstRuntime.Close(context.Background()); err != nil && !strings.Contains(err.Error(), "already unavailable") {
		firstManager.Reset()
		t.Fatalf("detach first runtime: %v", err)
	}
	firstManager.Reset()

	// A renderer may recreate/rekey its tab after restart, but ordinary provider
	// selection is not a tab-migration authority. Knowing the immutable ChatID
	// must not let a stale caller seize the actor or resume its native lane.
	const newTabID = "replacement-renderer-tab"
	secondManager := newManager()
	t.Cleanup(func() { secondManager.Reset() })
	secondRuntime := newProviderChatRuntime(secondManager, store, stateDir)
	t.Cleanup(func() { _ = secondRuntime.Close(context.Background()) })
	if _, err := secondRuntime.Select(context.Background(), acp.SessionOptions{
		TabID: newTabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale select was allowed to seize actor tab: %v", err)
	}
	if _, err := secondRuntime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": newTabID, "chatId": chatID, "providerId": "mock",
		"sessionId": first.SessionID, "cwd": root, "prompt": "must not start from stale tab",
		"operationId": "stale-start", "userMessageId": "stale-start-user", "assistantMessageId": "stale-start-assistant",
	}, "human"); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale start was allowed to seize actor tab: %v", err)
	}
	actor, err := secondRuntime.actor(chatID)
	if err != nil {
		t.Fatalf("open actor for explicit tab migration: %v", err)
	}
	actor.mu.Lock()
	err = actor.engine.Apply(chat.AttachTab{TabID: newTabID})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("explicit actor tab migration: %v", err)
	}
	resumed, err := secondRuntime.Select(context.Background(), acp.SessionOptions{
		TabID: newTabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("resume actor lane after explicit tab migration: %v", err)
	}
	if resumed.SessionID != first.SessionID {
		t.Fatalf("provider-native thread changed across actor restart: first=%s resumed=%s", first.SessionID, resumed.SessionID)
	}
	state, ok := secondRuntime.Snapshot(chatID)
	if !ok || state.ActiveLaneID == "" || state.Lanes[state.ActiveLaneID].Owner.TabID != newTabID {
		t.Fatalf("replacement attachment was not adopted: %#v", state)
	}

	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(raw)
	if strings.Count(trace, "session/resume") != 1 || strings.Contains(trace, "session/load") || strings.Contains(trace, "Previous conversation") {
		t.Fatalf("actor restart did not use exact resume without replay: %s", trace)
	}
}

func TestActorNativeChatProjectsAfterRestartWithoutLegacyMirror(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	providerState := filepath.Join(stateDir, "provider-native.json")
	newManager := func() *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{
				ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
				CWD: root, Enabled: true, Env: map[string]string{
					"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": providerState,
				},
			},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		})
	}
	store := sharedSessionStore(stateDir)
	firstManager := newManager()
	first := newProviderChatRuntime(firstManager, store, stateDir)
	if _, err := first.CreateRendererChat(map[string]any{
		"tabId": "native-tab", "chatId": "native-chat", "operationId": "create-native-chat",
		"title": "Native chat", "titleLocked": true, "cwd": root, "providerId": "mock",
	}); err != nil {
		firstManager.Reset()
		t.Fatalf("create actor-native chat: %v", err)
	}
	info, err := first.SelectNewChat(context.Background(), map[string]any{
		"tabId": "native-tab", "chatId": "native-chat", "operationId": "select-native-chat", "providerId": "mock", "cwd": root,
	})
	if err != nil || info.SessionID == "" {
		t.Fatalf("initialize actor-native chat: info=%#v err=%v", info, err)
	}
	if cached := store.GlobalSnapshot(); len(anySlice(cached["chats"])) != 0 {
		t.Fatalf("actor-native creation wrote chat semantics into the global presentation store: %#v", cached)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstManager.Reset()

	secondManager := newManager()
	t.Cleanup(func() { secondManager.Reset() })
	second := newProviderChatRuntime(secondManager, store, stateDir)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	projected, err := second.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	chats := anySlice(projected["chats"])
	if len(chats) != 1 {
		t.Fatalf("actor discovery projected chats = %#v", chats)
	}
	chatRow := mapFromAnyMain(chats[0])
	if fieldString(chatRow, "id") != "native-tab" || fieldString(chatRow, "chatId") != "native-chat" || fieldString(chatRow, "providerId") != "mock" {
		t.Fatalf("actor-native projection = %#v", chatRow)
	}
	if cached := store.GlobalSnapshot(); len(anySlice(cached["chats"])) != 0 {
		t.Fatalf("read-only actor projection recreated legacy chat rows: %#v", cached)
	}
}

func TestActorDeleteCrashRecoveryCompletesNativeCleanupFromTombstone(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	providerState := filepath.Join(stateDir, "provider-native.json")
	newManager := func() *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{
				ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
				CWD: root, Enabled: true, Env: map[string]string{
					"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_SESSION_STORE": providerState,
				},
			},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		})
	}
	store := sharedSessionStore(stateDir)
	firstManager := newManager()
	first := newProviderChatRuntime(firstManager, store, stateDir)
	if _, err := first.CreateRendererChat(map[string]any{
		"tabId": "delete-tab", "chatId": "delete-chat", "operationId": "create-delete-chat",
		"title": "Delete chat", "titleLocked": true, "cwd": root, "providerId": "mock",
	}); err != nil {
		firstManager.Reset()
		t.Fatalf("create actor-native delete chat: %v", err)
	}
	info, err := first.SelectNewChat(context.Background(), map[string]any{
		"tabId": "delete-tab", "chatId": "delete-chat", "operationId": "select-delete-chat", "providerId": "mock", "cwd": root,
	})
	if err != nil || info.SessionID == "" {
		firstManager.Reset()
		t.Fatalf("create exact lane: info=%#v err=%v", info, err)
	}
	if bindings, err := firstManager.LegacyProviderLaneMigrations("delete-chat", nil); err != nil || len(bindings) != 1 {
		firstManager.Reset()
		t.Fatalf("native binding before tombstone = %#v err=%v", bindings, err)
	}
	actor, err := first.actor("delete-chat")
	if err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	// Commit the tombstone but simulate a daemon crash before its cleanup effect
	// can be claimed. No direct manager deletion is allowed here.
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: "delete-test", Force: true}); err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	if err := actor.coordinator.Close(context.Background()); err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	firstManager.Reset()

	secondManager := newManager()
	t.Cleanup(func() { secondManager.Reset() })
	second := newProviderChatRuntime(secondManager, store, stateDir)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := second.Snapshot("delete-chat")
		bindings, bindingErr := secondManager.LegacyProviderLaneMigrations("delete-chat", nil)
		if bindingErr == nil && len(bindings) == 0 && ok && len(state.Outbox) == 1 && state.Outbox[0].Status == chat.OutboxCompleted {
			if !state.Deleted {
				t.Fatal("cleanup receipt cleared the actor tombstone")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := second.Snapshot("delete-chat")
	bindings, bindingErr := secondManager.LegacyProviderLaneMigrations("delete-chat", nil)
	t.Fatalf("recovered tombstone did not finish cleanup: state=%#v bindings=%#v err=%v", state, bindings, bindingErr)
}

func TestActorCutoverRebuildsFromActorsWhenLegacyCacheIsMissingCorruptOrExtra(t *testing.T) {
	for _, scenario := range []string{"missing", "corrupt", "extra"} {
		t.Run(scenario, func(t *testing.T) {
			root := repoRoot(t)
			stateDir := t.TempDir()
			newManager := func() *acp.Manager {
				return acp.NewManager(acp.Options{
					RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
					Provider: acp.ProviderConfig{
						ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
						CWD: root, Enabled: true,
					},
					DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
				})
			}
			firstStore := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
			firstManager := newManager()
			first := newProviderChatRuntime(firstManager, firstStore, stateDir)
			if _, err := first.actorForNewChat("durable-actor-chat", chat.PresentationState{
				TabID: "durable-actor-tab", Title: "Actor authority", ProviderID: "mock",
			}); err != nil {
				t.Fatal(err)
			}
			if err := first.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			firstManager.Reset()

			cachePath := filepath.Join(stateDir, sessionStateFilename)
			switch scenario {
			case "missing":
				if err := os.Remove(cachePath); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(cachePath, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				writeJSONTestFile(t, cachePath, map[string]any{
					"v": 1, "activeId": "stale-tab", "seq": 99,
					"chats": []any{map[string]any{
						"id": "stale-tab", "chatId": "stale-chat", "title": "must not resurrect",
						"messages": []any{map[string]any{"id": "forged", "role": "user", "content": "legacy cache"}},
					}},
				})
			}

			secondStore := newSessionStore(cachePath)
			secondManager := newManager()
			t.Cleanup(func() { secondManager.Reset() })
			second := newProviderChatRuntime(secondManager, secondStore, stateDir)
			t.Cleanup(func() { _ = second.Close(context.Background()) })
			projected, err := second.ProjectSession()
			if err != nil {
				t.Fatalf("project actor state with %s legacy cache: %v", scenario, err)
			}
			rows := anySlice(projected["chats"])
			if len(rows) != 1 || fieldString(mapFromAnyMain(rows[0]), "chatId") != "durable-actor-chat" {
				t.Fatalf("%s cache influenced actor projection: %#v", scenario, rows)
			}
			if cached := secondStore.GlobalSnapshot(); len(anySlice(cached["chats"])) != 0 {
				t.Fatalf("%s cache survived actor cutover: %#v", scenario, cached)
			}
			if err := secondStore.LoadError(); err != nil {
				t.Fatalf("%s obsolete cache still blocks actor runtime: %v", scenario, err)
			}
		})
	}
}

func TestActorCutoverReceiptFailsClosedWhenReferencedActorIsMissing(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	if err := writeLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename), legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ChatIDs: []string{"missing-actor-chat"},
	}); err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider:          acp.ProviderConfig{ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, store, stateDir)
	if _, err := runtime.ProjectSession(); err == nil || !strings.Contains(err.Error(), "missing actor") {
		t.Fatalf("missing receipt actor did not fail closed: %v", err)
	}
}

func TestInterruptedV2ActorMigrationUpgradesObligationExactlyOnceAcrossBoots(t *testing.T) {
	stateDir := t.TempDir()
	legacyChat := map[string]any{
		"id": "v2-tab", "chatId": "v2-chat", "title": "Interrupted v2",
		"messages": []any{map[string]any{"id": "v2-user", "role": "user", "content": "keep me", "status": "done"}},
		"queue":    []any{},
	}
	legacyRoot := map[string]any{"activeId": "v2-tab", "chats": []any{legacyChat}}
	raw, err := json.Marshal(legacyRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "obligations"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyObligation := `{"open":[{"tabId":"v2-tab","chatId":"v2-chat","state":"needs_input","source":"declared","openedAt":"2026-08-11T10:00:00Z","updatedAt":"2026-08-11T10:00:00Z"}]}`
	if err := os.WriteFile(filepath.Join(stateDir, "obligations", "v2-tab.json"), []byte(legacyObligation), 0o600); err != nil {
		t.Fatal(err)
	}
	preCutoverStore := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	command, err := buildLegacyChatMigration(legacyChat, stateDir, preCutoverStore)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := chat.NewDurableEngine("v2-chat", chat.FileStore{Path: providerChatStatePath(stateDir, "v2-chat")})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(command); err != nil {
		t.Fatal(err)
	}
	if engine.Snapshot().Migration.LegacyObligationMigrated {
		t.Fatal("fixture did not stop at the intended pre-obligation crash boundary")
	}

	boot := func() (*providerChatRuntime, *acp.Manager) {
		manager := acp.NewManager(acp.Options{StateDir: stateDir})
		store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
		runtime := newProviderChatRuntime(manager, store, stateDir)
		if err := runtime.StartupError(); err != nil {
			manager.Reset()
			t.Fatalf("actor cutover boot: %v", err)
		}
		return runtime, manager
	}
	first, firstManager := boot()
	state, err := first.ReadChat("v2-tab", "v2-chat", 20, true)
	if err != nil {
		t.Fatal(err)
	}
	if fieldString(mapFromAnyMain(state["obligation"]), "state") != "needs_input" {
		obligation, obligationErr := first.Obligation("v2-tab", "v2-chat")
		if obligationErr != nil || obligation == nil || obligation.State != "needs_input" {
			t.Fatalf("migrated obligation = %#v err=%v", obligation, obligationErr)
		}
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	firstManager.Reset()
	second, secondManager := boot()
	t.Cleanup(func() {
		_ = second.Close(context.Background())
		secondManager.Reset()
	})
	obligation, err := second.Obligation("v2-tab", "v2-chat")
	if err != nil || obligation == nil || obligation.State != "needs_input" {
		t.Fatalf("second boot obligation = %#v err=%v", obligation, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "obligations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy obligation store survived v5 receipt: %v", err)
	}
}

func TestProviderChatRuntimeSwitchesAndReturnsThroughVerifiedContextImport(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	const tabID, chatID = "multi-provider-tab", "multi-provider-chat"
	providerConfig := func(id string) acp.ProviderConfig {
		return acp.ProviderConfig{
			ID: id, Name: id, Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS":       "0",
				"WORKASS_MOCK_ACP_SESSION_STORE":  filepath.Join(stateDir, id+"-native.json"),
				"WORKASS_MOCK_ACP_CONTEXT_IMPORT": "1",
			},
		}
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Providers:         []acp.ProviderConfig{providerConfig("mock"), providerConfig("second-provider")},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-multi-provider-chat",
		"title": "Multi-provider lanes", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}

	firstSelection := acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, OperationID: "multi-select-first",
	}
	first, err := runtime.Select(context.Background(), firstSelection)
	if err != nil {
		t.Fatal(err)
	}
	firstTurnRequest := map[string]any{
		"kind": "app-chat", "title": "Multi-provider lanes", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "sessionId": first.SessionID, "cwd": root, "prompt": "turn on provider one",
		"userMessageId": "multi-user-1", "assistantMessageId": "multi-assistant-1",
	}
	if _, err := runtime.Start(context.Background(), firstTurnRequest, "human"); err != nil {
		t.Fatal(err)
	}
	waitProviderChatLedger(t, runtime, chatID, 2, 5*time.Second)

	second, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "second-provider", CWD: root, OperationID: "multi-select-second",
	})
	if err != nil {
		t.Fatalf("switch to import-capable provider: %v", err)
	}
	if second.ProviderID != "second-provider" || second.SessionID == "" || second.SessionID == first.SessionID {
		t.Fatalf("second exact lane = %#v", second)
	}
	state, _ := runtime.Snapshot(chatID)
	secondLane := state.Lanes[state.ActiveLaneID]
	if secondLane.Identity.Realm.ProviderID != "second-provider" || secondLane.CoveredThrough != state.LedgerHead() {
		t.Fatalf("second lane did not import exact visible coverage: %#v", secondLane)
	}
	// A delayed retry of the first selection receipt must reconstruct the first
	// reply without following the chat's newer DesiredLaneID back to provider
	// two or changing that newer selection.
	retriedFirst, err := runtime.Select(context.Background(), firstSelection)
	if err != nil {
		t.Fatalf("retry first selection after selecting second provider: %v", err)
	}
	if retriedFirst.SessionID != first.SessionID || retriedFirst.ProviderID != "mock" {
		t.Fatalf("old selection retry followed current desired lane: first=%#v retry=%#v", first, retriedFirst)
	}
	afterRetry, _ := runtime.Snapshot(chatID)
	if afterRetry.DesiredLaneID != state.DesiredLaneID || afterRetry.ActiveLaneID != state.ActiveLaneID || afterRetry.Revision != state.Revision {
		t.Fatalf("old selection retry mutated newer selection: before=%#v after=%#v", state, afterRetry)
	}
	if _, err := runtime.Start(context.Background(), firstTurnRequest, "human"); err != nil {
		t.Fatalf("retry first turn after selecting second provider: %v", err)
	}
	afterTurnRetry, _ := runtime.Snapshot(chatID)
	if afterTurnRetry.DesiredLaneID != state.DesiredLaneID || afterTurnRetry.ActiveLaneID != state.ActiveLaneID || afterTurnRetry.Revision != state.Revision {
		t.Fatalf("old turn retry mutated newer provider selection: before=%#v after=%#v", state, afterTurnRetry)
	}
	changedFirstTurn := cloneJSON(firstTurnRequest).(map[string]any)
	changedFirstTurn["prompt"] = "different content under old operation"
	if _, err := runtime.Start(context.Background(), changedFirstTurn, "human"); err == nil {
		t.Fatal("old turn operation accepted different retry content")
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Multi-provider lanes", "tabId": tabID, "chatId": chatID,
		"providerId": "second-provider", "sessionId": second.SessionID, "cwd": root, "prompt": "turn on provider two",
		"userMessageId": "multi-user-2", "assistantMessageId": "multi-assistant-2",
	}, "human"); err != nil {
		t.Fatal(err)
	}
	waitProviderChatLedger(t, runtime, chatID, 4, 5*time.Second)

	returned, err := runtime.Select(context.Background(), acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root})
	if err != nil {
		state, _ := runtime.Snapshot(chatID)
		t.Fatalf("switch back to first provider: %v; state=%#v", err, state)
	}
	if returned.SessionID != first.SessionID {
		t.Fatalf("switch-back replaced first provider thread: first=%s returned=%s", first.SessionID, returned.SessionID)
	}
	state, _ = runtime.Snapshot(chatID)
	firstLane := state.Lanes[state.ActiveLaneID]
	if firstLane.Identity.Realm.ProviderID != "mock" || firstLane.CoveredThrough != state.LedgerHead() || len(state.Lanes) != 2 {
		t.Fatalf("switch-back lane state = %#v", state)
	}
}

func waitProviderChatIdle(t *testing.T, runtime *providerChatRuntime, chatID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, ok := runtime.Snapshot(chatID)
		if ok && state.Foreground == nil && state.LedgerHead() >= 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := runtime.Snapshot(chatID)
	t.Fatalf("provider chat did not reach an idle terminal boundary: %#v", state)
}

func waitProviderChatLedger(t *testing.T, runtime *providerChatRuntime, chatID string, want uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, ok := runtime.Snapshot(chatID)
		if ok && state.Foreground == nil && state.LedgerHead() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := runtime.Snapshot(chatID)
	t.Fatalf("provider chat ledger did not reach %d events: %#v", want, state)
}

func newSteerRegressionFixture(t *testing.T) (*providerChatRuntime, *acp.Manager, string, string, acp.SessionInfo) {
	t.Helper()
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node",
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, CWD: root,
			Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "1"}, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "steer-regression-tab", "chatId": "steer-regression-chat", "operationId": "steer-regression-create",
		"title": "Steer regression", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create steer regression chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "steer-regression-tab", ChatID: "steer-regression-chat", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("attach steer regression lane: %v", err)
	}
	return runtime, manager, root, stateDir, info
}

func steerRegressionImage(data, name string) map[string]any {
	return map[string]any{"mimeType": "image/png", "name": name, "data": data}
}

func sessionImageFileCount(t *testing.T, stateDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read session image sidecars: %v", err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}
	return count
}

func startSteerRegressionTurn(t *testing.T, runtime *providerChatRuntime, info acp.SessionInfo) {
	t.Helper()
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Steer regression", "tabId": "steer-regression-tab", "chatId": "steer-regression-chat",
		"providerId": "mock", "sessionId": info.SessionID, "prompt": "[mock:hold-until-steer] [mock:steer] base turn",
		"userMessageId": "steer-regression-base-user", "assistantMessageId": "steer-regression-base-assistant",
	}, "human"); err != nil {
		t.Fatalf("start steer regression turn: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := runtime.Snapshot("steer-regression-chat")
		if ok && state.Foreground != nil && state.Foreground.Status == chat.ForegroundRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := runtime.Snapshot("steer-regression-chat")
	t.Fatalf("steer regression turn did not reach running state: %#v", state)
}

func TestProviderChatSteerRejectsStaleDurableAttachmentBeforeManagerOrSidecars(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	actor, err := runtime.actor("steer-regression-chat")
	if err != nil {
		t.Fatal(err)
	}
	state, ok := runtime.Snapshot("steer-regression-chat")
	if !ok || state.ActiveLaneID == "" {
		t.Fatalf("missing steer regression lane: %#v", state)
	}
	lane := state.Lanes[state.ActiveLaneID]
	// Retire the actor's exact disposable attachment without touching the
	// manager's old live row. The old implementation treated that manager row
	// as enough authority and queued a steer after writing its image sidecar.
	actor.mu.Lock()
	err = actor.engine.Apply(chat.HostLost{LaneID: lane.Identity.ID, ConnectionGeneration: lane.ConnectionGeneration})
	actor.mu.Unlock()
	if err != nil {
		t.Fatalf("retire durable steer attachment: %v", err)
	}
	_, handled, err := runtime.Steer(context.Background(), map[string]any{
		"sessionId": info.SessionID, "tabId": "steer-regression-tab", "chatId": "steer-regression-chat",
		"prompt": "must reject the stale session", "clientUserMessageId": "stale-steer-operation",
		"continuationAssistantMessageId": "stale-steer-assistant",
		"images":                         []any{steerRegressionImage("stale-sidecar-must-not-exist", "stale.png")},
	})
	if !handled || err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale durable attachment was accepted: handled=%v err=%v", handled, err)
	}
	if got := sessionImageFileCount(t, stateDir); got != 0 {
		t.Fatalf("stale steer wrote %d attachment sidecars before admission", got)
	}
	after, _ := runtime.Snapshot("steer-regression-chat")
	if _, exists := after.Operations["stale-steer-operation"]; exists || len(after.Queue) != 0 {
		t.Fatalf("stale steer changed actor ownership: %#v", after)
	}
}

func TestProviderChatSteerDuplicateAndChangedRetryAreDurableReadbackOnly(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	startSteerRegressionTurn(t, runtime, info)
	image := steerRegressionImage("same-steer-image", "same.png")
	request := map[string]any{
		"sessionId": info.SessionID, "prompt": "steer exactly once", "clientUserMessageId": "steer-once-operation",
		"continuationAssistantMessageId": "steer-once-assistant", "images": []any{image},
	}
	if _, handled, err := runtime.Steer(context.Background(), request); !handled || err != nil {
		t.Fatalf("initial steer admission failed: handled=%v err=%v", handled, err)
	}
	firstCount := sessionImageFileCount(t, stateDir)
	if firstCount != 1 {
		t.Fatalf("initial steer sidecar count=%d, want 1", firstCount)
	}
	if _, handled, err := runtime.Steer(context.Background(), cloneJSON(request).(map[string]any)); !handled || err != nil {
		t.Fatalf("identical steer retry was not durable readback: handled=%v err=%v", handled, err)
	}
	if got := sessionImageFileCount(t, stateDir); got != firstCount {
		t.Fatalf("identical steer retry wrote sidecars: before=%d after=%d", firstCount, got)
	}
	changedPrompt := cloneJSON(request).(map[string]any)
	changedPrompt["prompt"] = "changed steer content"
	if _, handled, err := runtime.Steer(context.Background(), changedPrompt); !handled || err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed steer prompt was accepted: handled=%v err=%v", handled, err)
	}
	changedImage := cloneJSON(request).(map[string]any)
	changedImage["images"] = []any{steerRegressionImage("different-steer-image", "same.png")}
	if _, handled, err := runtime.Steer(context.Background(), changedImage); !handled || err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed steer attachment was accepted: handled=%v err=%v", handled, err)
	}
	if got := sessionImageFileCount(t, stateDir); got != firstCount {
		t.Fatalf("changed steer retry wrote sidecars: before=%d after=%d", firstCount, got)
	}
}

func TestProviderChatStartChangedImageRetryDoesNotPersistSidecar(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	request := map[string]any{
		"kind": "app-chat", "title": "Steer regression", "tabId": "steer-regression-tab", "chatId": "steer-regression-chat",
		"providerId": "mock", "sessionId": info.SessionID, "prompt": "turn with an image",
		"operationId": "turn-image-once", "userMessageId": "turn-image-once-user", "assistantMessageId": "turn-image-once-assistant",
		"images": []any{steerRegressionImage("turn-image-original", "turn.png")},
	}
	if _, err := runtime.Start(context.Background(), request, "human"); err != nil {
		t.Fatalf("initial turn with image failed: %v", err)
	}
	firstCount := sessionImageFileCount(t, stateDir)
	if firstCount != 1 {
		t.Fatalf("initial turn sidecar count=%d, want 1", firstCount)
	}
	changed := cloneJSON(request).(map[string]any)
	changed["images"] = []any{steerRegressionImage("turn-image-changed", "turn.png")}
	if _, err := runtime.Start(context.Background(), changed, "human"); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed same-operation image was accepted: %v", err)
	}
	if got := sessionImageFileCount(t, stateDir); got != firstCount {
		t.Fatalf("changed same-operation retry wrote sidecars: before=%d after=%d", firstCount, got)
	}
}

func TestReplaceStagedQueueStaleRevisionDoesNotPersistAttachmentSidecar(t *testing.T) {
	runtime, _, _, stateDir, _ := newSteerRegressionFixture(t)
	_, err := runtime.ReplaceStagedQueue(
		"steer-regression-tab", "steer-regression-chat", "stale-queue-replacement", 1,
		[]any{map[string]any{
			"id": "stale-queue-row", "text": "must not persist", "source": "human", "delivery": "queue",
			"images": []any{steerRegressionImage("stale-queue-sidecar", "stale-queue.png")},
		}},
	)
	if err == nil || !strings.Contains(err.Error(), "revision is stale") {
		t.Fatalf("stale queue revision was accepted: %v", err)
	}
	if got := sessionImageFileCount(t, stateDir); got != 0 {
		t.Fatalf("stale queue replacement wrote %d attachment sidecars", got)
	}
	state, _ := runtime.Snapshot("steer-regression-chat")
	if _, exists := state.QueueMutationReceipts["stale-queue-replacement"]; exists {
		t.Fatal("stale queue replacement entered the actor receipt ledger")
	}
}

func TestSteerAttachmentInputIdentityMatchesPersistedAttachment(t *testing.T) {
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	data := "pure-attachment-identity"
	inline := []any{steerRegressionImage(data, "identity.png")}
	pure, err := steerAttachmentInputIdentity(inline)
	if err != nil {
		t.Fatalf("compute pure inline attachment identity: %v", err)
	}
	if got := sessionImageFileCount(t, stateDir); got != 0 {
		t.Fatalf("pure identity computation wrote %d sidecars", got)
	}
	persisted, err := sessions.PersistProviderAttachments(inline)
	if err != nil {
		t.Fatalf("persist inline attachment: %v", err)
	}
	if !reflect.DeepEqual(pure, persisted) {
		t.Fatalf("pure inline identity differs from persisted attachment: pure=%#v persisted=%#v", pure, persisted)
	}

	refImage := map[string]any{
		"mimeType": pure[0].MIMEType, "name": pure[0].Name,
		sessionImageDataRefField: sessionImageDirname + "/" + pure[0].Digest,
		"digest":                 pure[0].Digest, "size": pure[0].Size,
	}
	pureRef, err := steerAttachmentInputIdentity([]any{refImage})
	if err != nil {
		t.Fatalf("compute pure ref attachment identity: %v", err)
	}
	persistedRef, err := sessions.PersistProviderAttachments([]any{refImage})
	if err != nil {
		t.Fatalf("persist ref attachment: %v", err)
	}
	if !reflect.DeepEqual(pureRef, persistedRef) {
		t.Fatalf("pure ref identity differs from persisted attachment: pure=%#v persisted=%#v", pureRef, persistedRef)
	}
}

func TestProviderChatSteerRejectedInputDoesNotPersistAttachmentSidecar(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	_, handled, err := runtime.Steer(context.Background(), map[string]any{
		"sessionId": info.SessionID, "prompt": "invalid attachment must fail before admission", "clientUserMessageId": "invalid-steer-operation",
		"continuationAssistantMessageId": "invalid-steer-assistant",
		"images":                         []any{map[string]any{"mimeType": "text/plain", "name": "not-an-image", "data": "must-not-be-written"}},
	})
	if !handled || err == nil || !strings.Contains(err.Error(), "MIME") {
		t.Fatalf("invalid steer attachment was not rejected before admission: handled=%v err=%v", handled, err)
	}
	if got := sessionImageFileCount(t, stateDir); got != 0 {
		t.Fatalf("rejected steer wrote %d attachment sidecars", got)
	}
	state, _ := runtime.Snapshot("steer-regression-chat")
	if _, exists := state.Operations["invalid-steer-operation"]; exists {
		t.Fatal("rejected steer entered the actor mailbox")
	}
}

func TestProviderChatSteerForegroundEndUsesDurableQueueExactlyOnce(t *testing.T) {
	runtime, _, _, stateDir, info := newSteerRegressionFixture(t)
	request := map[string]any{
		"sessionId": info.SessionID, "prompt": "queue after foreground end", "clientUserMessageId": "race-steer-operation",
		"continuationAssistantMessageId": "race-steer-assistant", "images": []any{steerRegressionImage("race-sidecar", "race.png")},
	}
	result, handled, err := runtime.Steer(context.Background(), request)
	if !handled || err != nil || result["daemonQueued"] != true || result["queued"] != true {
		t.Fatalf("foreground-end steer did not use durable queue: handled=%v err=%v result=%#v", handled, err, result)
	}
	state, _ := runtime.Snapshot("steer-regression-chat")
	if _, exists := state.Operations["race-steer-operation"]; !exists {
		t.Fatal("queued foreground-end steer is missing its actor operation")
	}
	if owners := countSteerOwners(state, "race-steer-operation"); owners != 1 {
		t.Fatalf("foreground-end steer has %d durable owners, want exactly one", owners)
	}
	count := sessionImageFileCount(t, stateDir)
	retry, handled, err := runtime.Steer(context.Background(), cloneJSON(request).(map[string]any))
	if !handled || err != nil || retry["operationId"] != "race-steer-operation" {
		t.Fatalf("queued foreground-end retry was not durable readback: handled=%v err=%v result=%#v", handled, err, retry)
	}
	if got := sessionImageFileCount(t, stateDir); got != count {
		t.Fatalf("queued foreground-end retry wrote sidecars: before=%d after=%d", count, got)
	}
	state, _ = runtime.Snapshot("steer-regression-chat")
	if owners := countSteerOwners(state, "race-steer-operation"); owners != 1 {
		t.Fatalf("foreground-end retry duplicated durable ownership %d times", owners)
	}
}

func TestProviderChatSteerForegroundEndRaceUsesDurableQueueExactlyOnce(t *testing.T) {
	runtime, _, _, _, info := newSteerRegressionFixture(t)
	actor, err := runtime.actor("steer-regression-chat")
	if err != nil {
		t.Fatal(err)
	}
	baseState, ok := runtime.Snapshot("steer-regression-chat")
	if !ok || baseState.ActiveLaneID == "" {
		t.Fatalf("missing synthetic race lane: %#v", baseState)
	}
	request := map[string]any{
		"sessionId": info.SessionID, "tabId": "steer-regression-tab", "chatId": "steer-regression-chat",
		"prompt": "queue the concurrent foreground end", "clientUserMessageId": "race-steer-concurrent-operation",
		"continuationAssistantMessageId": "race-steer-concurrent-assistant",
	}
	type steerResponse struct {
		result  map[string]any
		handled bool
		err     error
	}
	responses := make(chan steerResponse, 1)
	// Hold the actor-facing mutex while the request resolves its exact durable
	// attachment. The provider cancellation and terminal commit below can still
	// advance the engine, forcing the request to recheck the foreground boundary
	// before it can admit the steer. Build the synthetic foreground directly in
	// the actor to avoid an unrelated provider job from owning this race test.
	actor.mu.Lock()
	if err := actor.engine.Apply(chat.Submit{
		OperationID: "race-steer-base-operation", LaneID: baseState.ActiveLaneID, Text: "synthetic foreground",
		Presentation: providercontract.TurnPresentation{
			UserMessageID: "race-steer-base-user", AssistantMessageID: "race-steer-base-assistant", Origin: "human",
		},
	}); err != nil {
		actor.mu.Unlock()
		t.Fatalf("create synthetic foreground: %v", err)
	}
	foregroundState := actor.engine.Snapshot()
	if foregroundState.Foreground == nil {
		actor.mu.Unlock()
		t.Fatalf("synthetic submit did not create foreground: %#v", foregroundState)
	}
	foregroundOperationID := foregroundState.Foreground.OperationID
	if err := actor.engine.Apply(chat.TurnAdmitted{
		OperationID: foregroundOperationID,
		Turn:        providercontract.TurnRef{OperationID: foregroundOperationID, NativeID: "synthetic-native-turn"},
		Accepted:    true,
	}); err != nil {
		actor.mu.Unlock()
		t.Fatalf("admit synthetic foreground: %v", err)
	}
	go func() {
		result, handled, err := runtime.Steer(context.Background(), request)
		responses <- steerResponse{result: result, handled: handled, err: err}
	}()
	time.Sleep(10 * time.Millisecond)
	state := actor.engine.Snapshot()
	if state.Foreground == nil {
		actor.mu.Unlock()
		t.Fatal("steer regression foreground ended before the concurrent boundary")
	}
	terminalErr := actor.engine.Apply(chat.TurnTerminated{OperationID: state.Foreground.OperationID, Status: "completed"})
	actor.mu.Unlock()
	if terminalErr != nil {
		t.Fatalf("commit concurrent foreground terminal: %v", terminalErr)
	}
	response := <-responses
	if !response.handled || response.err != nil || response.result["daemonQueued"] != true || response.result["queued"] != true {
		t.Fatalf("concurrent foreground-end steer did not queue exactly once: handled=%v err=%v result=%#v", response.handled, response.err, response.result)
	}
	after, _ := runtime.Snapshot("steer-regression-chat")
	if _, exists := after.Operations["race-steer-concurrent-operation"]; !exists {
		t.Fatal("concurrent foreground-end steer lost its actor operation")
	}
	if owners := countSteerOwners(after, "race-steer-concurrent-operation"); owners != 1 {
		t.Fatalf("concurrent foreground-end steer has %d durable owners, want exactly one", owners)
	}
}

func TestProviderChatRuntimeCheckpointExecutorCarriesOperationToReceipt(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	chatID := "runtime-checkpoint-operation-chat"
	tabID := "runtime-checkpoint-operation-tab"
	const operationID = providercontract.OperationID("runtime-checkpoint-operation")
	var published any
	publishedBeforeReceipt := false
	var runtime *providerChatRuntime
	runtime = newProviderChatRuntime(manager, store, stateDir, func(channel string, payload any) {
		if channel != "chat:checkpoint-restored" {
			return
		}
		if state, ok := runtime.Snapshot(chatID); ok {
			for _, entry := range state.Outbox {
				if entry.Kind == chat.EffectCheckpointRestore && entry.OperationID == operationID && entry.Status != chat.OutboxCompleted {
					publishedBeforeReceipt = true
				}
			}
		}
		published = payload
	})
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("start authoritative provider chat runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })

	checkpointPath := filepath.Join(stateDir, "checkpoints", chatID+".json")
	if err := os.MkdirAll(filepath.Dir(checkpointPath), 0o755); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := json.Marshal(map[string]any{
		"version": 1, "chatId": chatID, "checkpoints": []any{
			map[string]any{"turnSeq": 1, "repos": []any{}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointPath, checkpoint, 0o644); err != nil {
		t.Fatal(err)
	}

	actor, err := runtime.actorForFork(chatID, chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: tabID}, SourceChatID: "runtime-checkpoint-source",
		OperationID: "runtime-checkpoint-create", Digest: "runtime-checkpoint-create-digest",
		Messages: []chat.LedgerEvent{{
			EventID: "runtime-checkpoint-assistant-event", MessageID: "runtime-checkpoint-assistant",
			Role: "assistant", Text: "completed", Status: "done", OperationID: "runtime-checkpoint-turn",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	checkpointList := json.RawMessage(`[{"turnSeq":1,"repos":[]}]`)
	actor.mu.Lock()
	if err := actor.engine.Apply(chat.UpdateEnvironment{
		ExpectedTabID: tabID, Payload: json.RawMessage(`{"chatId":"runtime-checkpoint-operation-chat"}`),
		Checkpoints: checkpointList, Reference: json.RawMessage(`null`),
	}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	checkpointTarget, checkpointDigest, err := actor.engine.Snapshot().CheckpointRestoreTarget(1)
	if err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	if err := actor.engine.Apply(chat.RestoreCheckpoint{
		OperationID: operationID, TurnSequence: 1, ObservedAtUnixMS: 1000,
		Checkpoint: checkpointTarget, CheckpointDigest: checkpointDigest,
	}); err != nil {
		actor.mu.Unlock()
		t.Fatal(err)
	}
	actor.mu.Unlock()
	// The manager checkpoint file is mutable executor state. The pending actor
	// effect must continue using checkpointTarget even when that file changes
	// before dispatch.
	if err := os.WriteFile(checkpointPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := actor.coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publishedBeforeReceipt {
		t.Fatal("runtime published checkpoint before actor receipt commit")
	}
	payload, ok := published.(map[string]any)
	if !ok || payload["operationId"] != string(operationID) {
		t.Fatalf("checkpoint publication lost operation identity: %#v", published)
	}
	state := actor.engine.Snapshot()
	if len(state.Outbox) != 1 || state.Outbox[0].Status != chat.OutboxCompleted || string(state.Outbox[0].Checkpoint) != string(checkpointTarget) || state.Outbox[0].CheckpointDigest != checkpointDigest {
		t.Fatalf("checkpoint actor receipt = %#v", state.Outbox)
	}
	if err := actor.engine.Apply(chat.RestoreCheckpoint{OperationID: operationID, TurnSequence: 2, ObservedAtUnixMS: 2000, Checkpoint: checkpointTarget, CheckpointDigest: checkpointDigest}); err == nil {
		t.Fatal("changed checkpoint request reused the committed operation")
	}
}

func countSteerOwners(state chat.State, operationID string) int {
	count := 0
	for _, entry := range state.Queue {
		if string(entry.OperationID) == operationID {
			count++
		}
	}
	if state.Foreground != nil && string(state.Foreground.OperationID) == operationID {
		count++
	}
	if state.PendingSteer != nil && string(state.PendingSteer.OperationID) == operationID {
		count++
	}
	return count
}
