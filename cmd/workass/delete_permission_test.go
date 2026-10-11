package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/httpserve"
	"workass/internal/wire"
)

// 2026-10-11 QA chat-core B8: force-deleting a chat with a pending permission
// card emptied the pending list but never sent chat:permission-resolved, so a
// connected client could keep the dead card; its closed engine also stayed
// in the process list as an "active / failed" ghost row.
func TestForceDeleteResolvesPendingPermissionCard(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock", Broadcast: hub.Broadcast, RSSSampleInterval: time.Hour,
	})
	providerChats := newProviderChatRuntime(manager, sharedSessionStore(stateDir), stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	const tabID, chatID = "delete-perm-tab", "delete-perm-chat"
	createWireActorChat(t, client, 100, tabID, chatID, "mock")
	client.invoke(t, 1, "job:start", map[string]any{
		"kind": "app-chat", "chatId": chatID, "tabId": tabID, "providerId": "mock", "prompt": "[mock:permission] approve this",
		"operationId": "delete-perm-turn", "userMessageId": "delete-perm-user", "assistantMessageId": "delete-perm-assistant",
	})
	if reply := client.waitReply(t, 1, 5*time.Second); reply.Error != nil {
		t.Fatalf("job:start: %s", *reply.Error)
	}
	requestID := ""
	for requestID == "" {
		msg := client.waitEvent(t, 5*time.Second)
		if msg.Channel == "chat:permission-request" {
			requestID = fieldString(mapFromAnyMain(msg.Payload), "id")
		}
	}
	client.invoke(t, 2, "chat:delete", map[string]any{"tabId": tabID, "chatId": chatID, "operationId": "delete-perm-delete", "force": true})
	if reply := client.waitReply(t, 2, 5*time.Second); reply.Error != nil {
		t.Fatalf("chat:delete: %s", *reply.Error)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msg := client.waitEvent(t, time.Until(deadline))
		if msg.Channel == "chat:permission-resolved" && fieldString(mapFromAnyMain(msg.Payload), "id") == requestID {
			for _, proc := range manager.Processes() {
				if fieldString(proc, "chatId") == chatID {
					t.Fatalf("deleted chat left an engine row: %v", proc)
				}
			}
			return
		}
	}
	t.Fatalf("no chat:permission-resolved for %s after force delete", requestID)
}
