package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

// TestTrackedSubagentNoticeSurvivesWaitThatOutlivesItsTurn reproduces the
// 2026-10-16 "forking + subsessions" incident end to end: the parent turn
// spawns a child through the real spawn handler, parks a workass_wait_subagent
// (timeout_ms -1) that is still registered after the parent turn ends, and the
// child settles afterwards. The orphaned waiter must not swallow the settle;
// the parent chat gets exactly one agent-origin notice through its actor.
func TestTrackedSubagentNoticeSurvivesWaitThatOutlivesItsTurn(t *testing.T) {
	root := repoRoot(t)
	const tabID, chatID, ownerKey = "zombie-wait-tab", "zombie-wait-chat", "zombie-wait-owner"
	stateDir := filepath.Join(t.TempDir(), "state")
	holdFile := filepath.Join(t.TempDir(), "release")
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP", Enabled: true, Env: map[string]string{
			"WORKASS_MOCK_ACP_DELAY_MS": "1", "WORKASS_MOCK_ACP_HOLD_FILE": holdFile,
		}},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "zombie-wait-create", "focus": false,
		"title": "Zombie wait", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create parent chat: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, AgentOwnerKey: ownerKey})
	if err != nil {
		t.Fatalf("attach parent lane: %v", err)
	}
	parentEnded := make(chan struct{}, 4)
	manager.SetJobEndFunc(func(tab, chat string) {
		if tab == tabID && chat == chatID {
			parentEnded <- struct{}{}
		}
	})
	if _, err := runtime.Start(ctx, map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "providerId": "mock", "cwd": root,
		"operationId": "zombie-wait-parent", "userMessageId": "zombie-wait-parent-u", "assistantMessageId": "zombie-wait-parent-a",
		"prompt": "[mock:hold-until-steer] parent turn that spawns and waits",
	}, "human"); err != nil {
		t.Fatalf("start parent turn: %v", err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	spawnDeadline := time.Now().Add(10 * time.Second)
	var childID string
	for childID == "" {
		_, err := handler.call(request, agentControlRequest{Method: "agent.spawn", Params: map[string]any{
			"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "zombie-wait-spawn",
			"prompt": "[mock:hold-until-steer] [mock:slow] FINAL_ZOMBIE_MARKER", "label": "zombie child",
		}})
		if err == nil {
			for _, run := range manager.ListSubagents(ownerKey, chatID, tabID) {
				childID = run.ID
			}
		}
		if childID == "" {
			if time.Now().After(spawnDeadline) {
				t.Fatalf("spawn through the real handler never succeeded: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// The parent's background shell parks an event-only wait, then the turn ends.
	waitCtx, stopWait := context.WithCancel(ctx)
	defer stopWait()
	waitDone := make(chan error, 1)
	go func() {
		_, err := handler.call(request.WithContext(waitCtx), agentControlRequest{Method: "agent.wait", Params: map[string]any{
			"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "zombie-wait-shell",
			"id": childID, "timeout_ms": -1,
		}})
		waitDone <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _ := runtime.Snapshot(chatID)
		if len(state.AgentWaitObservationReceipts) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parked wait never registered with the actor")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(holdFile, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parentEnded:
	case <-time.After(10 * time.Second):
		t.Fatal("parent turn did not end")
	}
	stillRunning := false
	for _, run := range manager.ListSubagents(ownerKey, chatID, tabID) {
		stillRunning = run.ID == childID && run.Status == "running"
	}
	if !stillRunning {
		t.Fatal("fixture raced: the child settled before the parent turn ended, so the wait was not orphaned")
	}

	notice := subagentCompletionOperationID(tabID, chatID, childID)
	deadline = time.Now().Add(20 * time.Second)
	for {
		state, _ := runtime.Snapshot(chatID)
		delivered := 0
		for _, event := range state.Ledger {
			if event.Role == "assistant" && strings.Contains(event.Text, "FINAL_ZOMBIE_MARKER") {
				delivered++
			}
		}
		if _, ok := state.Operations[notice]; ok && state.Foreground == nil && delivered > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the parent chat never got the settle notice: operations=%#v foreground=%#v", state.Operations, state.Foreground)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("orphaned wait failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("orphaned wait did not return after the child settled")
	}

	state, _ := runtime.Snapshot(chatID)
	notices := 0
	for _, event := range state.Ledger {
		if event.Role == "user" && event.OperationID == notice {
			notices++
			if !strings.HasPrefix(event.Text, "<task-notification>") || !strings.Contains(event.Text, "<task-id>"+childID+"</task-id>") {
				t.Fatalf("notice does not name the finished subagent: %q", event.Text)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("expected exactly one settle notice, got %d: %#v", notices, state.Ledger)
	}
}
