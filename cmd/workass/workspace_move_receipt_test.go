package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
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
}
