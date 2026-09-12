package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

func TestCodexServiceTierSurvivesExactResumeAndClearsExplicitly(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true, Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args)}}})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "speed-tab", "chatId": "speed-chat", "operationId": "speed-create", "cwd": root, "providerId": "codex"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "speed-tab", ChatID: "speed-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	save := func(tier, operation string) {
		t.Helper()
		state, _ := runtime.Snapshot("speed-chat")
		if _, err := runtime.SaveRuntimeControls("speed-tab", "speed-chat", providercontract.OperationID(operation), state.Presentation.RuntimeControlRevision, map[string]any{
			"providerId": "codex", "currentModelId": "gpt-fixture[high]", "currentModeId": "agent", "modelControls": map[string]any{"codex": map[string]any{"gpt-fixture": map[string]any{"serviceTier": tier, "effort": "high"}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	run := func(tier, operation string) {
		t.Helper()
		if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "speed-tab", "chatId": "speed-chat", "sessionId": info.SessionID, "operationId": operation, "userMessageId": operation + "-user", "assistantMessageId": operation + "-assistant", "prompt": "[fixture:speed:" + tier + "]"}, "human"); err != nil {
			t.Fatal(err)
		}
		waitProviderChatIdle(t, runtime, "speed-chat", 5*time.Second)
		state, _ := runtime.Snapshot("speed-chat")
		last := state.Ledger[len(state.Ledger)-1]
		if last.OperationID != providercontract.OperationID(operation) || last.Status != "done" {
			t.Fatalf("native tier/effort assertion failed: %#v", last)
		}
	}
	save("fast", "speed-fast")
	run("fast", "speed-first")
	info, err = runtime.Select(ctx, acp.SessionOptions{TabID: "speed-tab", ChatID: "speed-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Models) == 0 || !slices.Contains(info.Models[0].ServiceTiers, "fast") {
		t.Fatalf("catalog lost speed choices: %#v", info.Models)
	}
	before, _ := runtime.Snapshot("speed-chat")
	thread := before.Lanes[before.ActiveLaneID].Thread
	if !runtime.CloseSession(ctx, info.SessionID) {
		t.Fatal("could not detach exact session")
	}
	info, err = runtime.Select(ctx, acp.SessionOptions{TabID: "speed-tab", ChatID: "speed-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	run("fast", "speed-resumed")
	after, _ := runtime.Snapshot("speed-chat")
	if after.Lanes[after.ActiveLaneID].Thread != thread {
		t.Fatal("speed selection replaced native thread")
	}
	save("default", "speed-standard")
	run("default", "speed-last")
	save("fast", "speed-queue-fast")
	start := func(operation, prompt string) {
		t.Helper()
		if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "speed-tab", "chatId": "speed-chat", "sessionId": info.SessionID, "operationId": operation, "userMessageId": operation + "-user", "assistantMessageId": operation + "-assistant", "prompt": prompt, "busyMode": "queue-v1"}, "human"); err != nil {
			t.Fatal(err)
		}
	}
	start("speed-held", "[fixture:speed:fast] keep running")
	// Wait for native activity before testing queue behavior; pre-prompt Stop is
	// a different race and does not prove the selected speed reached Codex.
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		diagnostics := manager.TurnDiagnostics("speed-tab", "speed-chat", 1)
		turns, _ := diagnostics["turns"].([]any)
		if len(turns) > 0 && mapFromAnyMain(turns[0])["active"] == true && mapFromAnyMain(turns[0])["firstUpdateMs"] != nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("held native turn did not emit activity")
	}

	start("speed-queued", "[fixture:speed:fast]")
	save("default", "speed-queue-standard")
	queued, _ := runtime.Snapshot("speed-chat")
	if len(queued.Queue) != 1 || queued.Queue[0].ServiceTier != "fast" {
		t.Fatalf("control edit changed queued speed: %#v", queued.Queue)
	}
	if queued.Foreground == nil {
		t.Fatal("missing held foreground")
	}
	if _, handled, err := runtime.Cancel(ctx, queued.Foreground.Turn.NativeID); err != nil || !handled {
		t.Fatalf("cancel held fixture: %v handled=%v", err, handled)
	}
	waitProviderChatIdle(t, runtime, "speed-chat", 5*time.Second)
	completed, _ := runtime.Snapshot("speed-chat")
	last := completed.Ledger[len(completed.Ledger)-1]
	if last.OperationID != "speed-queued" || last.Status != "done" {
		t.Fatalf("queued turn lost its frozen tier: %#v", last)
	}

}
