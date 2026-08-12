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
)

func TestProviderChatAgentReadRejectsWrongPairBeforeManagerCapability(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "agent-read-tab", "chatId": "agent-read-chat", "operationId": "agent-read-create",
		"title": "Actor read gate", "cwd": t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.ListSpawnedWorkForOwner("missing-owner", "agent-read-chat", "stale-tab", "agent-read-chat", "agent-read-tab", 0); err == nil ||
		!strings.Contains(err.Error(), "tab id") {
		t.Fatalf("wrong parent pair was not rejected by actor before manager: %v", err)
	}
	if _, err := runtime.ListSpawnedWorkForOwner("missing-owner", "agent-read-chat", "agent-read-tab", "other-chat", "agent-read-tab", 0); err == nil ||
		!strings.Contains(err.Error(), "exactly match") {
		t.Fatalf("wrong requested pair was not rejected before manager owner lookup: %v", err)
	}
	if _, err := runtime.AgentOwnerCWD("missing-owner", "stale-tab", "agent-read-chat"); err == nil ||
		!strings.Contains(err.Error(), "tab id") {
		t.Fatalf("artifact CWD gate accepted stale pair: %v", err)
	}
}

func TestProviderChatAgentReadProjectsActorBackgroundState(t *testing.T) {
	root := repoRoot(t)
	workspace := t.TempDir()
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID, ownerKey = "agent-read-tab", "agent-read-chat", "agent-read-owner"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "agent-read-create",
		"title": "Actor read state", "cwd": workspace, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: workspace, AgentOwnerKey: ownerKey,
	}); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	lane := state.Lanes[state.ActiveLaneID]
	owner := chat.ProviderActivityOwner{
		LaneID: state.ActiveLaneID, OperationID: "agent-read-turn", TurnID: "agent-read-native-turn",
		ConnectionGeneration: lane.ConnectionGeneration,
	}
	finished := "2026-08-11T12:00:01Z"
	if err := actor.engine.Apply(chat.ReconcileBackgroundSnapshot{Items: []chat.BackgroundState{{
		Owner: owner,
		Event: providercontract.BackgroundEvent{
			WorkID: "child-1", TaskID: "child-1", Kind: "subagent", Title: "Review child",
			Status: "exited", StartedAt: "2026-08-11T12:00:00Z", UpdatedAt: finished, FinishedAt: finished,
			ModelLabel: "Mock-high", ResultExcerpt: "actor result",
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.ListSubagents(ownerKey, tabID, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "child-1" || runs[0].Status != "done" || runs[0].Result != "actor result" {
		t.Fatalf("actor subagent projection = %#v", runs)
	}
	receipts, err := runtime.ListSubagentReceipts(ownerKey, tabID, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 1 || receipts[0].SubagentID != "child-1" || receipts[0].Status != "done" {
		t.Fatalf("actor subagent receipts = %#v", receipts)
	}
	items, err := runtime.ListSpawnedWorkForOwner(ownerKey, chatID, tabID, chatID, tabID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || fieldString(items[0], "id") != "child-1" || fieldString(items[0], "status") != "exited" {
		t.Fatalf("actor spawned work projection = %#v", items)
	}
	workReceipts, err := runtime.ListSpawnedWorkReceipts(ownerKey, chatID, tabID, chatID, tabID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(workReceipts) != 1 || workReceipts[0].TaskID != "child-1" || workReceipts[0].Status != "exited" {
		t.Fatalf("actor spawned work receipts = %#v", workReceipts)
	}
	if cwd, err := runtime.AgentOwnerCWD(ownerKey, tabID, chatID); err != nil || cwd != workspace {
		t.Fatalf("actor workspace = %q, %v", cwd, err)
	}
}
