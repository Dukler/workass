package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
)

// 2026-10-11 QA: switching back by sending a turn (the renderer's path:
// job:start with the new providerId and the session it last used) failed with
// unsupported_capability whenever the returned-to provider's engine was still
// warm — every first switch-back inside the hibernate TTL.
func TestTurnSwitchingBackToAWarmProviderRunsOnItsOwnEngine(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	const tabID, chatID = "switch-back-tab", "switch-back-chat"
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
		"tabId": tabID, "chatId": chatID, "operationId": "create-switch-back-chat",
		"title": "Switch back", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := runtime.Select(context.Background(), acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, OperationID: "select-first"})
	if err != nil {
		t.Fatal(err)
	}
	session := first.SessionID
	turn := func(n int, providerID string) {
		t.Helper()
		receipt, err := runtime.Start(context.Background(), map[string]any{
			"kind": "app-chat", "title": "Switch back", "tabId": tabID, "chatId": chatID,
			"providerId": providerID, "sessionId": session, "cwd": root, "prompt": "turn on " + providerID,
			"userMessageId": "sb-user-" + string(rune('0'+n)), "assistantMessageId": "sb-assistant-" + string(rune('0'+n)),
		}, "human")
		if err != nil {
			t.Fatalf("turn %d on %s: %v", n, providerID, err)
		}
		waitProviderChatLedger(t, runtime, chatID, uint64(2*n), 5*time.Second)
		state, _ := runtime.Snapshot(chatID)
		last := state.Ledger[len(state.Ledger)-1]
		if last.Status != "done" || string(last.ProviderID) != providerID {
			t.Fatalf("turn %d on %s did not complete on its provider: %#v (receipt %#v)", n, providerID, last, receipt)
		}
		if sid, ok := receipt["sessionId"].(string); ok && sid != "" {
			session = sid
		} else {
			session = state.Lanes[state.ActiveLaneID].Thread.HeadID
		}
	}
	turn(1, "mock")
	turn(2, "second-provider")
	turn(3, "mock")
	turn(4, "second-provider")
}
