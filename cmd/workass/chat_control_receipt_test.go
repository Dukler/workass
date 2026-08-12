package main

import (
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
