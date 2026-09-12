package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestCodexRuntimeDiagnosticsThroughActorAndRestart(t *testing.T) {
	root, stateDir := repoRoot(t), t.TempDir()
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	opts := acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true, Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args)}}}
	m := acp.NewManager(opts)
	t.Cleanup(func() { m.Reset() })
	runtime := newTestProviderChatRuntime(t, m, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "diag-tab", "chatId": "diag-chat", "operationId": "diag-create", "cwd": root, "providerId": "codex"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "diag-tab", ChatID: "diag-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "diag-tab", "chatId": "diag-chat", "sessionId": info.SessionID, "operationId": "diag-operation", "userMessageId": "diag-user", "assistantMessageId": "diag-assistant", "prompt": "[fixture:diagnostic] PRIVATE_CURRENT_PROMPT"}, "human"); err != nil {
		t.Fatal(err)
	}
	waitProviderChatIdle(t, runtime, "diag-chat", 5*time.Second)
	actorState, _ := runtime.Snapshot("diag-chat")
	nativeThread := actorState.Lanes[actorState.ActiveLaneID].Thread.HeadID
	if nativeThread == "" {
		t.Fatal("fixture never established a native thread")
	}
	read := func(d map[string]any) map[string]any {
		t.Helper()
		data, _ := json.Marshal(d)
		for _, secret := range []string{nativeThread, "PRIVATE_CURRENT_PROMPT", "fixture-key", "fixture-credential", "fixture-warning", "private.invalid", "additionalDetails"} {
			if secret != "" && strings.Contains(string(data), secret) {
				t.Fatalf("diagnostics leaked %s", secret)
			}
		}
		var result map[string]any
		if err := json.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		turns := result["turns"].([]any)
		if len(turns) != 1 {
			t.Fatal(result)
		}
		return turns[0].(map[string]any)
	}
	d, err := runtime.TurnDiagnostics(map[string]any{"tab_id": "diag-tab", "chat_id": "diag-chat", "limit": 1})
	if err != nil {
		t.Fatal(err)
	}
	turn := read(d)
	observations := turn["runtime"].(map[string]any)
	if observations["retries"] != float64(1) || observations["compactions"] != float64(1) || observations["fallbacks"] != float64(1) {
		t.Fatal(observations)
	}
	input := observations["input"].(map[string]any)
	if input["resumeReplyBytes"].(float64) <= 0 || input["inputBytes"].(float64) <= 0 || input["resumed"] != false {
		t.Fatal(input)
	}
	if observations["usage"].(map[string]any)["used"] != float64(123) {
		t.Fatal(observations)
	}
	state, _ := runtime.Snapshot("diag-chat")
	ledger, _ := json.Marshal(state.Ledger)
	if strings.Contains(string(ledger), "_workass_diagnostic") || strings.Contains(string(ledger), input["hostInstanceId"].(string)) {
		t.Fatal("private diagnostic became transcript content")
	}
	m.Reset() // Includes a joined final diagnostic flush.
	restarted := acp.NewManager(opts)
	t.Cleanup(func() { restarted.Reset() })
	turn = read(restarted.TurnDiagnostics("diag-tab", "diag-chat", 1))
	if turn["active"] != false || turn["historical"] != true || turn["outcome"] != "completed" {
		t.Fatal(turn)
	}
	if turn["runtime"].(map[string]any)["retries"] != float64(1) {
		t.Fatal("restart lost failure evidence")
	}
}
