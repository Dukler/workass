package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestT4ModelControlKeysMigrateToBaseAndCompositeCreateValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	snapshot := sessionMirrorFixture("polluted-tab", "polluted-chat", "polluted controls")
	legacyChat := chatFromSnapshot(snapshot, "polluted-tab")
	legacyChat["modelControls"] = map[string]any{"mock": map[string]any{
		"mock-deterministic":       map[string]any{"effort": "low", "modeId": "ask"},
		"mock-deterministic[high]": map[string]any{"effort": "high", "modeId": "bypass"},
		"literal[1m]":              map[string]any{"modeId": "literal"},
	}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal polluted snapshot: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write polluted snapshot: %v", err)
	}
	var logLines []string
	prevLog := sessionStoreLogLine
	sessionStoreLogLine = func(line string) { logLines = append(logLines, line) }
	t.Cleanup(func() { sessionStoreLogLine = prevLog })

	reloaded := newSessionStore(path)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload polluted controls: %v", err)
	}
	gotChat := chatFromSnapshot(reloaded.Get().(map[string]any), "polluted-tab")
	memory := mapFromAnyMain(mapFromAnyMain(gotChat["modelControls"])["mock"])
	if _, exists := memory["mock-deterministic[high]"]; exists {
		t.Fatalf("composite modelControls key survived migration: %#v", memory)
	}
	base := mapFromAnyMain(memory["mock-deterministic"])
	if fieldString(base, "effort") != "low" || fieldString(base, "modeId") != "ask" {
		t.Fatalf("base controls did not win conflict: %#v", base)
	}
	if _, exists := memory["literal[1m]"]; !exists {
		t.Fatalf("literal bracketed adapter id was migrated away: %#v", memory)
	}
	if len(logLines) != 1 || !strings.Contains(logLines[0], "from=mock-deterministic[high]") || !strings.Contains(logLines[0], "to=mock-deterministic") {
		t.Fatalf("migration logs = %#v", logLines)
	}

	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	const parentTabID, parentChatID = "composite-parent-tab", "composite-parent-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-composite-parent",
		"tabId":       parentTabID, "chatId": parentChatID,
		"title": "Parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native parent chat: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, func(string, any) {}, runtime)
	created, err := coordinator.create(context.Background(), parentTabID, parentChatID, map[string]any{
		"operation_id": "test:create-composite-child",
		"title":        "Composite child", "cwd": stateDir, "provider_id": "mock", "model_id": "mock-deterministic[high]",
	})
	if err != nil {
		t.Fatalf("create chat with composite model: %v", err)
	}
	if created["modelId"] != "mock-deterministic" || created["effort"] != "high" || created["resolvedModelId"] != "mock-deterministic[high]" {
		t.Fatalf("created composite controls = %#v", created)
	}
}
