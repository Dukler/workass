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
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

// A send that arrives while the FIFO head is still attaching its provider runs
// behind that head, so it must be receipted as queued (queue-v1) or refused as
// busy, never answered as a running job.
func TestStartDuringProviderAttachWindowIsQueuedNotRunning(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	hub := wire.NewHub()
	store := sharedSessionStore(stateDir)
	const tabID, chatID = "attach-window-tab", "attach-window-chat"
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_NEW_SESSION_DELAY_MS": "1500",
			}, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(store, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	if _, err := providerChats.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-attach-window-chat",
		"focus": true, "title": "Attach window", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	client.waitChannelEvent(t, "agent:apply", 2*time.Second)

	start := func(id int, op, prompt string, extra map[string]any) map[string]any {
		arg := map[string]any{
			"operationId": op, "kind": "app-chat", "title": "Attach window", "tabId": tabID, "chatId": chatID,
			"providerId": "mock", "prompt": prompt, "userMessageId": op + "-user", "assistantMessageId": op + "-assistant",
		}
		for k, v := range extra {
			arg[k] = v
		}
		client.invoke(t, id, "job:start", arg)
		reply := client.waitReply(t, id, 5*time.Second)
		if reply.Error != nil {
			t.Fatalf("%s error: %s", op, *reply.Error)
		}
		return mapFromAnyMain(reply.Result)
	}
	head := start(1, "attach-head", "head turn", nil)
	if fieldString(head, "id") == "" {
		t.Fatalf("head receipt = %#v", head)
	}
	follow := start(2, "attach-follow", "follow during attach", map[string]any{"busyMode": "queue-v1"})
	if follow["queued"] != true || fieldString(follow, "status") == "running" {
		t.Fatalf("send during attach window must be queued, got %#v", follow)
	}

	client.invoke(t, 3, "job:start", map[string]any{
		"operationId": "attach-plain", "kind": "app-chat", "title": "Attach window", "tabId": tabID, "chatId": chatID,
		"providerId": "mock", "prompt": "plain during attach", "userMessageId": "plain-user", "assistantMessageId": "plain-assistant",
	})
	if reply := client.waitReply(t, 3, 5*time.Second); reply.Error == nil {
		t.Fatalf("non-queue send during attach window must be busy, got %#v", reply.Result)
	}
}

// 2026-10-11 QA chat-core B1b: cancelling the follow-up sent during the
// attach window failed the head ("transient_transport") and listed the
// cancelled follow-up before it.
func TestCancellingTheAttachWindowFollowUpLeavesTheHeadRunning(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><head></head><body></body>"), 0o644); err != nil {
		t.Fatal(err)
	}
	hub := wire.NewHub()
	store := sharedSessionStore(stateDir)
	const tabID, chatID = "attach-cancel-tab", "attach-cancel-chat"
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Env: map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "0", "WORKASS_MOCK_ACP_NEW_SESSION_DELAY_MS": "300"},
			Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", Broadcast: daemonEventBroadcaster(store, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, store, stateDir)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	if _, err := providerChats.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "create-attach-cancel-chat",
		"focus": true, "title": "Attach cancel", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})
	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()
	client.waitChannelEvent(t, "agent:apply", 2*time.Second)

	start := func(id int, op, prompt string, extra map[string]any) map[string]any {
		arg := map[string]any{
			"operationId": op, "kind": "app-chat", "title": "Attach cancel", "tabId": tabID, "chatId": chatID,
			"providerId": "mock", "prompt": prompt, "userMessageId": op + "-user", "assistantMessageId": op + "-assistant",
		}
		for k, v := range extra {
			arg[k] = v
		}
		client.invoke(t, id, "job:start", arg)
		reply := client.waitReply(t, id, 5*time.Second)
		if reply.Error != nil {
			t.Fatalf("%s: %s", op, *reply.Error)
		}
		return mapFromAnyMain(reply.Result)
	}
	head := start(1, "cancel-head", "[mock:slow] head", nil)
	follow := start(2, "cancel-follow", "follow-up", map[string]any{"busyMode": "queue-v1", "queueId": "q-cancel-follow"})
	if follow["queued"] != true {
		t.Fatalf("follow-up during attach was not queued: %#v", follow)
	}
	client.invoke(t, 3, "job:cancel", providercontract.DeriveJobID(chatID, "cancel-follow"))
	client.waitReply(t, 3, 5*time.Second)
	end := client.waitJobEvent(t, fieldString(head, "id"), "end", 15*time.Second)
	job := mapFromAnyMain(end["job"])
	if fieldString(job, "status") != "done" {
		t.Fatalf("head did not survive cancelling the follow-up: %#v", job)
	}
	state, _ := providerChats.Snapshot(chatID)
	if len(state.Ledger) < 2 || state.Ledger[0].OperationID != "cancel-head" {
		ops := make([]string, 0, len(state.Ledger))
		for _, row := range state.Ledger {
			ops = append(ops, string(row.OperationID)+"/"+row.Role)
		}
		t.Fatalf("transcript order inverted: %v", ops)
	}
}
