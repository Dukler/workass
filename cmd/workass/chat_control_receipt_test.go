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
