package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/wire"
)

// 2026-10-11 QA lanes B-11: detect-acp ignored command/args and probed every
// provider; an unknown or custom id answered ok with nothing.
func TestDetectChannelsRefuseCommandsAndReportUnknownProviders(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RSSSampleInterval: time.Hour,
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock",
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})
	if _, err := hub.Invoke("app-chat:detect-acp", []any{map[string]any{"command": "/bin/echo", "args": []any{"hi"}}}); err == nil || !strings.Contains(err.Error(), "configured providers only") {
		t.Fatalf("detect-acp with a raw command: err=%v", err)
	}
	result, err := hub.Invoke("providers:detect", []any{map[string]any{"provider": "no-such-provider"}})
	if err != nil {
		t.Fatal(err)
	}
	if reply := result.(map[string]any); reply["ok"] != false || !strings.Contains(fieldString(reply, "error"), "not configured") {
		t.Fatalf("unknown provider detection = %#v", reply)
	}
}
