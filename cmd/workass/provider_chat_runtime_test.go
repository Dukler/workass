package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestProjectSessionPreservesManualChatOrder(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	for _, item := range []struct {
		tabID  string
		chatID string
	}{
		{tabID: "manual-tab-a", chatID: "manual-chat-a"},
		{tabID: "manual-tab-b", chatID: "manual-chat-b"},
		{tabID: "manual-tab-c", chatID: "manual-chat-c"},
	} {
		if _, err := runtime.CreateRendererChat(map[string]any{
			"tabId": item.tabID, "chatId": item.chatID, "operationId": "create-" + item.chatID,
			"title": item.tabID, "cwd": stateDir, "providerId": "codex",
		}); err != nil {
			t.Fatalf("create %s: %v", item.chatID, err)
		}
	}

	global := store.GlobalSnapshot()
	global["chatOrder"] = []any{"manual-tab-c", "unknown-tab", "manual-tab-a", "manual-tab-c"}
	global[globalPresentationOperationField] = "save-manual-chat-order"
	if _, err := store.SaveActorGlobalSnapshot(global); err != nil {
		t.Fatalf("save manual chat order: %v", err)
	}
	projection, err := runtime.ProjectSession()
	if err != nil {
		t.Fatal(err)
	}
	rows := anySlice(projection["chats"])
	got := make([]string, 0, len(rows))
	for _, raw := range rows {
		got = append(got, fieldString(mapFromAnyMain(raw), "id"))
	}
	want := []string{"manual-tab-c", "manual-tab-a", "manual-tab-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projected chat order = %v, want %v", got, want)
	}
	if gotOrder := stringSliceField(store.GlobalSnapshot()["chatOrder"]); !reflect.DeepEqual(gotOrder, []string{"manual-tab-c", "unknown-tab", "manual-tab-a"}) {
		t.Fatalf("persisted normalized chat order = %v", gotOrder)
	}
}

func TestActorGlobalChatOrderIsBoundedAndSanitized(t *testing.T) {
	values := []any{" first ", "", "first", 42, strings.Repeat("x", actorGlobalTabIDLimit+1)}
	for index := 0; index < actorGlobalChatOrderLimit+20; index++ {
		values = append(values, fmt.Sprintf("tab-%04d", index))
	}
	root := actorGlobalSessionSnapshot(map[string]any{"chatOrder": values})
	order := stringSliceField(root["chatOrder"])
	if len(order) != actorGlobalChatOrderLimit {
		t.Fatalf("normalized chat order length = %d, want %d", len(order), actorGlobalChatOrderLimit)
	}
	if order[0] != "first" || order[1] != "tab-0000" {
		t.Fatalf("normalized chat order prefix = %v", order[:2])
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

func TestProviderLaneSelectionRetryCreatesAfterOldZeroThreadFailure(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	nativeStore := filepath.Join(stateDir, "mock-native.json")
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS":      "0",
				"WORKASS_MOCK_ACP_SESSION_STORE": nativeStore,
			},
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	const tabID, chatID = "zero-thread-tab", "zero-thread-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-zero-thread-chat",
		"title": "Zero thread retry", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	opts := acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
		ModelID: "mock-deterministic", ModeID: "ask", OperationID: "select-zero-thread",
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	err = func() error {
		actor.mu.Lock()
		defer actor.mu.Unlock()
		selection, err := runtime.resolveSelectionLocked(context.Background(), actor, opts)
		if err != nil {
			return err
		}
		if err := runtime.commitLaneSelectionLocked(actor, selection, opts); err != nil {
			return err
		}
		effect, ok, err := actor.engine.ClaimNext()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("initial create effect was not durable")
		}
		create, ok := effect.(chat.CreateLaneEffect)
		if !ok {
			return fmt.Errorf("initial lane effect is %T, want CreateLaneEffect", effect)
		}
		return actor.engine.Apply(chat.LaneOpenFailed{
			LaneID: create.Identity.ID, Kind: providercontract.ErrorAcceptanceAmbiguous, Ambiguous: true,
		})
	}()
	if err != nil {
		t.Fatalf("construct old zero-thread failure: %v", err)
	}
	failed, _ := runtime.Snapshot(chatID)
	failedReceipt := failed.LaneSelectionMutationReceipts[opts.OperationID]
	failedLane := failed.Lanes[failedReceipt.LaneID]
	if failedLane.Phase != chat.LaneAbsent || !failedLane.Thread.IsZero() || !failedLane.CreationFailedBeforeEstablishment() {
		t.Fatalf("old zero-thread failure = %#v", failedLane)
	}

	first, err := runtime.Select(context.Background(), opts)
	if err != nil {
		t.Fatalf("retry committed zero-thread selection: %v", err)
	}
	if first.SessionID == "" {
		t.Fatal("zero-thread selection retry did not return a native session")
	}
	established, _ := runtime.Snapshot(chatID)
	receipt := established.LaneSelectionMutationReceipts[opts.OperationID]
	lane := established.Lanes[receipt.LaneID]
	if lane.Phase != chat.LaneReady || lane.Thread.IsZero() || lane.CreateGeneration != 2 {
		t.Fatalf("zero-thread retry did not establish a fresh generation: %#v", lane)
	}
	var ambiguousCreates, completedCreates int
	for _, entry := range established.Outbox {
		if entry.Kind != chat.EffectCreateLane {
			continue
		}
		switch entry.Status {
		case chat.OutboxAmbiguous:
			ambiguousCreates++
		case chat.OutboxCompleted:
			completedCreates++
		}
	}
	if ambiguousCreates != 1 || completedCreates != 1 {
		t.Fatalf("create generations lost their separate receipts: %#v", established.Outbox)
	}
	second, err := runtime.Select(context.Background(), opts)
	if err != nil {
		t.Fatalf("read back established selection: %v", err)
	}
	afterReadback, _ := runtime.Snapshot(chatID)
	afterReceipt := afterReadback.LaneSelectionMutationReceipts[opts.OperationID]
	afterLane := afterReadback.Lanes[afterReceipt.LaneID]
	createEntries := 0
	for _, entry := range afterReadback.Outbox {
		if entry.Kind == chat.EffectCreateLane {
			createEntries++
		}
	}
	if second.SessionID != first.SessionID || afterLane.CreateGeneration != 2 || createEntries != 2 {
		t.Fatalf("established readback recreated the provider lane: first=%#v second=%#v lane=%#v createEntries=%d", first, second, afterLane, createEntries)
	}

	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Zero thread retry", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "sessionId": first.SessionID, "cwd": root, "prompt": "talk after retry",
		"operationId": "zero-thread-turn", "userMessageId": "zero-thread-user", "assistantMessageId": "zero-thread-assistant",
	}, "human"); err != nil {
		t.Fatalf("send after zero-thread creation: %v", err)
	}
	waitProviderChatLedger(t, runtime, chatID, 2, 5*time.Second)
	raw, err := os.ReadFile(nativeStore)
	if err != nil {
		t.Fatalf("read mock native sessions: %v", err)
	}
	var persisted struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode mock native sessions: %v", err)
	}
	if len(persisted.Sessions) != 1 {
		t.Fatalf("zero-thread retry created %d native sessions, want exactly one", len(persisted.Sessions))
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

func TestProviderChatRuntimeLoadsExactLaneAcrossActorRestart(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	tracePath := filepath.Join(stateDir, "provider-load-trace.log")
	providerState := filepath.Join(stateDir, "provider-load-native.json")
	const tabID, chatID = "load-actor-tab", "load-actor-chat"
	store := sharedSessionStore(stateDir)
	newManager := func() *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{
				ID: "mock", Name: "Load-only ACP", Command: "node",
				Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root,
				Env: map[string]string{
					"WORKASS_MOCK_ACP_DELAY_MS":           "0",
					"WORKASS_MOCK_ACP_SESSION_STORE":      providerState,
					"WORKASS_MOCK_ACP_TRACE_FILE":         tracePath,
					"WORKASS_MOCK_ACP_SESSION_CAPABILITY": "load",
				},
				Enabled: true,
			},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
		})
	}

	firstManager := newManager()
	first := newProviderChatRuntime(firstManager, store, stateDir)
	if _, err := first.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-load-actor-chat",
		"title": "Load actor lane", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	initial, err := first.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	if initial.SessionID != "" {
		firstManager.Reset()
		t.Fatalf("load-only session/new became durable before first ACP activity: %#v", initial)
	}
	if _, err := first.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Load actor lane", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "cwd": root, "prompt": "first load-only turn",
		"userMessageId": "load-user-1", "assistantMessageId": "load-assistant-1",
	}, "human"); err != nil {
		firstManager.Reset()
		t.Fatal(err)
	}
	waitProviderChatIdle(t, first, chatID, 5*time.Second)
	firstState, ok := first.Snapshot(chatID)
	if !ok || firstState.ActiveLaneID == "" {
		firstManager.Reset()
		t.Fatalf("load-only actor lane did not become active: %#v", firstState)
	}
	threadID := firstState.Lanes[firstState.ActiveLaneID].Thread.HeadID
	if threadID == "" {
		firstManager.Reset()
		t.Fatalf("first standard ACP activity did not commit the exact thread: %#v", firstState.Lanes[firstState.ActiveLaneID])
	}
	if err := first.Close(context.Background()); err != nil && !strings.Contains(err.Error(), "already unavailable") {
		firstManager.Reset()
		t.Fatal(err)
	}
	firstManager.Reset()

	secondManager := newManager()
	t.Cleanup(func() { secondManager.Reset() })
	second := newProviderChatRuntime(secondManager, store, stateDir)
	t.Cleanup(func() { _ = second.Close(context.Background()) })
	loaded, err := second.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionID != threadID {
		t.Fatalf("same-id load changed provider thread: first=%q loaded=%q", threadID, loaded.SessionID)
	}
	if _, err := second.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Load actor lane", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "sessionId": loaded.SessionID, "cwd": root, "prompt": "second load-only turn",
		"userMessageId": "load-user-2", "assistantMessageId": "load-assistant-2",
	}, "human"); err != nil {
		t.Fatal(err)
	}
	waitProviderChatLedger(t, second, chatID, 4, 5*time.Second)
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(raw)
	if strings.Count(trace, "session/load") != 1 || strings.Contains(trace, "session/resume") ||
		strings.Contains(trace, "[mock:loaded-history]") || strings.Contains(trace, "Previous conversation") {
		t.Fatalf("actor restart did not use one exact load without replay: %s", trace)
	}
}

func TestActorNativeChatProjectsAfterRestartFromCanonicalStorage(t *testing.T) {
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
		t.Fatalf("actor projection wrote chat rows into the global session store: %#v", cached)
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
	if bindings, err := firstManager.StoredProviderLaneSelections("delete-chat"); err != nil || len(bindings) != 1 {
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
		bindings, bindingErr := secondManager.StoredProviderLaneSelections("delete-chat")
		if bindingErr == nil && len(bindings) == 0 && ok && len(state.Outbox) == 1 && state.Outbox[0].Status == chat.OutboxCompleted {
			if !state.Deleted {
				t.Fatal("cleanup receipt cleared the actor tombstone")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := second.Snapshot("delete-chat")
	bindings, bindingErr := secondManager.StoredProviderLaneSelections("delete-chat")
	t.Fatalf("recovered tombstone did not finish cleanup: state=%#v bindings=%#v err=%v", state, bindings, bindingErr)
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
	// Queue promotion moves ownership into the transcript atomically. A fast
	// provider may finish before this race oracle snapshots the actor, so count
	// the committed turn as one logical owner only after every transient owner
	// has disappeared (the user and assistant ledger rows are one turn).
	if count == 0 {
		for _, event := range state.Ledger {
			if string(event.OperationID) == operationID {
				count = 1
				break
			}
		}
	}
	return count
}
