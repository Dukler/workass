package main

import (
	"maps"
	"strings"
	"testing"

	"workass/internal/acp"
)

func TestChatControlIdleCancelReceiptIsDurableBeforeRetry(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "idle-cancel-tab", "idle-cancel-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "idle-cancel-create",
		"title": "Idle cancel", "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, nil, runtime)
	params := map[string]any{
		"operation_id": "idle-cancel-once", "tab_id": tabID, "chat_id": chatID,
	}
	first, err := coordinator.cancel(params)
	if err != nil || fieldString(first, "reason") != "idle" || first["cancelled"] != false {
		t.Fatalf("first idle cancel = %#v, err=%v", first, err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok {
		t.Fatal("actor disappeared after idle cancel")
	}
	if _, ok := state.CancelMutationReceipts["idle-cancel-once"]; !ok {
		t.Fatalf("idle cancel was not durably recorded: %#v", state.CancelMutationReceipts)
	}
	revision := state.Revision

	retry, err := coordinator.cancel(params)
	if err != nil || fieldString(retry, "reason") != "idle" || retry["cancelled"] != false {
		t.Fatalf("retry idle cancel = %#v, err=%v", retry, err)
	}
	state, ok = runtime.Snapshot(chatID)
	if !ok || state.Revision != revision || state.Foreground != nil {
		t.Fatalf("idle cancel retry changed actor state: %#v", state)
	}
}

func TestChatControlRenameReceiptIsStableAcrossLostReply(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "rename-receipt-tab", "rename-receipt-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "rename-receipt-create",
		"title": "Before rename", "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	const operationID = "rename-receipt-once"
	if err := runtime.RenameChat(tabID, chatID, "After rename", operationID); err != nil {
		t.Fatalf("first rename: %v", err)
	}
	first, ok := runtime.Snapshot(chatID)
	if !ok || first.Presentation.Title != "After rename" {
		t.Fatalf("first rename state = %#v", first)
	}
	if err := runtime.RenameChat(tabID, chatID, "After rename", operationID); err != nil {
		t.Fatalf("lost-reply rename retry: %v", err)
	}
	retried, ok := runtime.Snapshot(chatID)
	if !ok || retried.Revision != first.Revision || retried.Presentation.PresentationRevision != first.Presentation.PresentationRevision {
		t.Fatalf("rename retry changed actor revision: first=%#v retry=%#v", first, retried)
	}
	if err := runtime.RenameChat(tabID, chatID, "Changed intent", operationID); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed rename reused operation id: %v", err)
	}
	unchanged, _ := runtime.Snapshot(chatID)
	if unchanged.Presentation.Title != "After rename" || unchanged.Revision != first.Revision {
		t.Fatalf("changed rename mutated actor: %#v", unchanged)
	}
}

func TestRendererPresentationReceiptSurvivesLostReplyAndRevisionHydration(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "presentation-receipt-tab", "presentation-receipt-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "presentation-receipt-create",
		"title": "Before presentation mutation", "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	if err := runtime.RenameChat(tabID, chatID, "Prepared presentation", "presentation-receipt-prepare"); err != nil {
		t.Fatalf("prepare presentation revision: %v", err)
	}
	prepared, ok := runtime.Snapshot(chatID)
	if !ok || prepared.Presentation.PresentationRevision != 1 {
		t.Fatalf("prepared presentation revision = %#v", prepared)
	}

	const operationID = "presentation-receipt-once"
	request := map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": operationID, "expectedRevision": 1,
		"title": "Prepared presentation", "titleLocked": true, "group": nil, "draft": "",
		"unread": false, "settled": "settled", "settledAt": 1_787_000_000_000, "pane": nil,
	}
	first, err := runtime.SavePresentation(tabID, chatID, operationID, 1, request)
	if err != nil {
		t.Fatalf("first presentation save: %v", err)
	}
	committed, ok := runtime.Snapshot(chatID)
	if !ok || committed.Presentation.PresentationRevision != 2 || committed.Presentation.Settled != "settled" {
		t.Fatalf("committed presentation = %#v", committed)
	}
	committedActorRevision := committed.Revision

	// Model a lost reply followed by session:get: the renderer has learned the
	// committed revision, but retries the same immutable operation and fields.
	retryRequest := maps.Clone(request)
	retryRequest["expectedRevision"] = 2
	retried, err := runtime.SavePresentation(tabID, chatID, operationID, 2, retryRequest)
	if err != nil {
		t.Fatalf("lost-reply presentation retry: %v", err)
	}
	afterRetry, ok := runtime.Snapshot(chatID)
	if !ok || afterRetry.Revision != committedActorRevision || afterRetry.Presentation.PresentationRevision != 2 {
		t.Fatalf("lost-reply retry changed actor state: committed=%#v retry=%#v", committed, afterRetry)
	}
	if first["presentationRevision"] != retried["presentationRevision"] || first["actorRevision"] != retried["actorRevision"] {
		t.Fatalf("lost-reply retry changed receipt: first=%#v retry=%#v", first, retried)
	}

	changed := maps.Clone(retryRequest)
	changed["title"] = "Changed presentation intent"
	if _, err := runtime.SavePresentation(tabID, chatID, operationID, 2, changed); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed presentation reused operation id: %v", err)
	}
	unchanged, _ := runtime.Snapshot(chatID)
	if unchanged.Revision != committedActorRevision || unchanged.Presentation.Title != "Prepared presentation" || unchanged.Presentation.Settled != "settled" {
		t.Fatalf("changed presentation mutated actor: %#v", unchanged)
	}
}

func TestPresentationWithoutDraftNeverMutatesLegacyDraft(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "local-draft-tab", "local-draft-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": tabID, "chatId": chatID, "operationId": "create-local-draft", "providerId": "mock", "currentModelId": "mock-deterministic"}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.SavePresentation(tabID, chatID, "legacy-draft", 0, map[string]any{"draft": "legacy input"}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"title": "renamed"}
	_, err := runtime.SavePresentation(tabID, chatID, "metadata-only", 1, request)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := runtime.Snapshot(chatID)
	if state.Presentation.Draft != "legacy input" {
		t.Fatal("metadata save cleared draft")
	}
	if _, err = runtime.SavePresentation(tabID, chatID, "another-legacy-edit", 2, map[string]any{"draft": "new legacy input"}); err != nil {
		t.Fatal(err)
	}
	beforeRetry, _ := runtime.Snapshot(chatID)
	_, err = runtime.SavePresentation(tabID, chatID, "metadata-only", 3, request)
	if err != nil {
		t.Fatal(err)
	}
	state, _ = runtime.Snapshot(chatID)
	if state.Presentation.Draft != "new legacy input" || state.Revision != beforeRetry.Revision {
		t.Fatal("metadata retry rewrote another edit")
	}
}

// workass_settle_chat is the agent twin of the sidebar Settle button: it files
// the chat, clears unread, keeps every other presentation field, replays
// exactly, and never restarts the archive clock of an already-settled chat.
func TestChatControlSettleMatchesSidebarAndReplaysExactly(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "settle-receipt-tab", "settle-receipt-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "settle-receipt-create",
		"title": "Fork source", "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatalf("create actor chat: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, nil, runtime)
	before, _ := runtime.Snapshot(chatID)
	settle := map[string]any{"operation_id": "settle-once", "tab_id": tabID, "chat_id": chatID, "settled": true}
	if _, err := coordinator.settle(settle); err != nil {
		t.Fatalf("settle: %v", err)
	}
	first, _ := runtime.Snapshot(chatID)
	if first.Presentation.Settled != "settled" || first.Presentation.SettledAt <= 0 || first.Presentation.Unread {
		t.Fatalf("settled presentation = %#v", first.Presentation)
	}
	if first.Presentation.Title != before.Presentation.Title || first.Presentation.TitleLocked != before.Presentation.TitleLocked ||
		first.Presentation.CurrentModelID != before.Presentation.CurrentModelID {
		t.Fatalf("settle changed other chat state: before=%#v after=%#v", before.Presentation, first.Presentation)
	}
	if _, err := coordinator.settle(settle); err != nil {
		t.Fatalf("lost-reply settle retry: %v", err)
	}
	retried, _ := runtime.Snapshot(chatID)
	if retried.Revision != first.Revision {
		t.Fatalf("settle retry changed actor revision: %d -> %d", first.Revision, retried.Revision)
	}
	changed := maps.Clone(settle)
	changed["settled"] = false
	if _, err := coordinator.settle(changed); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("changed settle reused operation id: %v", err)
	}
	again := maps.Clone(settle)
	again["operation_id"] = "settle-twice"
	if _, err := coordinator.settle(again); err != nil {
		t.Fatalf("second settle: %v", err)
	}
	if twice, _ := runtime.Snapshot(chatID); twice.Presentation.SettledAt != first.Presentation.SettledAt {
		t.Fatalf("repeated settle restarted the archive clock: %d -> %d", first.Presentation.SettledAt, twice.Presentation.SettledAt)
	}
	listed, err := runtime.ListChats()
	if err != nil || len(listed) != 1 || listed[0]["settled"] != "settled" || listed[0]["settledAt"] != int(first.Presentation.SettledAt) {
		t.Fatalf("list chats = %#v, err=%v", listed, err)
	}

	reactivate := map[string]any{"operation_id": "reactivate-once", "tab_id": tabID, "chat_id": chatID, "settled": false}
	if _, err := coordinator.settle(reactivate); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	active, _ := runtime.Snapshot(chatID)
	if active.Presentation.Settled != "active" || active.Presentation.SettledAt != 0 {
		t.Fatalf("reactivated presentation = %#v", active.Presentation)
	}
	if _, err := coordinator.settle(map[string]any{"operation_id": "settle-bad", "tab_id": tabID, "chat_id": chatID, "settled": "yes"}); err == nil {
		t.Fatal("non-boolean settled was accepted")
	}
}
