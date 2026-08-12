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
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "title": "Multi-provider lanes", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "sessionId": first.SessionID, "cwd": root, "prompt": "turn on provider one",
		"userMessageId": "multi-user-1", "assistantMessageId": "multi-assistant-1",
	}, "human"); err != nil {
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
