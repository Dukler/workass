package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/wire"
)

// A client that attaches after the fact — a reloaded renderer, a phone joining
// the LAN — learns the chat's background state by INVOKING spawned-work:list,
// not by waiting for spawned-work:changed. For a chat whose background state is
// quiet that event never fires again, so an obligation carried only by the
// event would be invisible to every client that was not already listening.
func TestSpawnedWorkListCarriesTheObligation(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	store := sharedSessionStore(stateDir)
	runtime := newProviderChatRuntime(manager, store, stateDir)
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("start actor runtime: %v", err)
	}
	t.Cleanup(func() { _ = runtime.Close(t.Context()) })
	for _, fixture := range []struct {
		tabID, chatID, title string
	}{
		{tabID: "tab-1", chatID: "chat-1", title: "Needs input"},
		{tabID: "tab-2", chatID: "chat-2", title: "Quiet"},
	} {
		if _, err := runtime.CreateRendererChat(map[string]any{
			"tabId": fixture.tabID, "chatId": fixture.chatID, "operationId": "obligation-create-" + fixture.chatID,
			"title": fixture.title, "cwd": root, "providerId": "mock",
		}); err != nil {
			t.Fatalf("create obligation actor %s: %v", fixture.chatID, err)
		}
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: "tab-1", ChatID: "chat-1", ProviderID: "mock", CWD: root,
	})
	if err != nil {
		t.Fatalf("attach obligation actor: %v", err)
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": "tab-1", "chatId": "chat-1", "sessionId": info.SessionID,
		"providerId": "mock", "cwd": root, "operationId": "obligation-failed-turn",
		"userMessageId": "obligation-user", "assistantMessageId": "obligation-assistant", "prompt": "[mock:error]",
	}, "human"); err != nil {
		t.Fatalf("start obligation turn: %v", err)
	}
	waitProviderChatIdle(t, runtime, "chat-1", 5*time.Second)
	hub := wire.NewHub()
	registerAcpHandlers(hub, manager, stateDir, store, nil, runtime)

	reply := invokeSpawnedWorkList(t, hub, "tab-1", "chat-1")
	obligation, ok := reply["obligation"].(map[string]any)
	if !ok {
		t.Fatalf("spawned-work:list reply = %#v, want an obligation", reply)
	}
	if obligation["state"] != "needs_input" || obligation["source"] == "" {
		t.Fatalf("obligation = %#v, want actor-owned needs_input state", obligation)
	}

	// A chat that owes nothing must produce exactly the pre-obligation reply
	// shape: the field is additive, so its absence is what an older daemon
	// sends and what a client falling back to its previous behaviour reads.
	quiet := invokeSpawnedWorkList(t, hub, "tab-2", "chat-2")
	if _, present := quiet["obligation"]; present {
		t.Fatalf("quiet chat reply = %#v, want no obligation key", quiet)
	}
}

func invokeSpawnedWorkList(t *testing.T, hub *wire.Hub, tabID, chatID string) map[string]any {
	t.Helper()
	raw, err := hub.Invoke("spawned-work:list", []any{map[string]any{"tabId": tabID, "chatId": chatID}})
	if err != nil {
		t.Fatalf("spawned-work:list invoke: %v", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	var reply map[string]any
	if err := json.Unmarshal(data, &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}
