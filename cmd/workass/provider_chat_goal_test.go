package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
)

func TestCodexNativeGoalThroughActorAndExactResume(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true, Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args), "WORKASS_CODEX_FIXTURE_GOAL_STORE": filepath.Join(stateDir, "native-goal-fixture.json")}}})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": "goal-tab", "chatId": "goal-chat", "operationId": "goal-create", "cwd": root, "providerId": "codex"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: "goal-tab", ChatID: "goal-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	run := func(operation, prompt string) {
		t.Helper()
		if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "goal-tab", "chatId": "goal-chat", "sessionId": info.SessionID, "operationId": operation, "userMessageId": operation + "-user", "assistantMessageId": operation + "-assistant", "prompt": prompt}, "human"); err != nil {
			t.Fatal(err)
		}
		waitProviderChatIdle(t, runtime, "goal-chat", 5*time.Second)
		state, _ := runtime.Snapshot("goal-chat")
		last := state.Ledger[len(state.Ledger)-1]
		if last.OperationID != providercontract.OperationID(operation) || last.Status != "done" {
			t.Fatalf("native goal command failed: %#v", last)
		}
	}
	// The fixture rejects this marker on turn/start: success proves the actor's
	// wrapped context retained explicit command intent all the way to goal/set.
	run("goal-start", "/goal [fixture:goal-complete] finish the migration")
	info, err = runtime.Select(ctx, acp.SessionOptions{TabID: "goal-tab", ChatID: "goal-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := runtime.Snapshot("goal-chat")
	attachment := before.Lanes[before.ActiveLaneID].Attachment
	if attachment == nil || !attachment.CommandCatalogSupported || attachment.CommandCatalog == nil || len(attachment.CommandCatalog.Commands) != 1 || attachment.CommandCatalog.Commands[0].Name != "goal" {
		t.Fatalf("native slash catalog lost from actor: %#v", attachment)
	}
	thread := before.Lanes[before.ActiveLaneID].Thread
	if !runtime.CloseSession(ctx, info.SessionID) {
		t.Fatal("cannot detach goal session")
	}
	info, err = runtime.Select(ctx, acp.SessionOptions{TabID: "goal-tab", ChatID: "goal-chat", ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	run("goal-inspect", "/goal")
	after, _ := runtime.Snapshot("goal-chat")
	if after.Lanes[after.ActiveLaneID].Thread != thread {
		t.Fatal("native goal resume changed exact thread")
	}
	run("goal-clear", "/goal clear")
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": "goal-tab", "chatId": "goal-chat", "sessionId": info.SessionID, "operationId": "goal-held", "userMessageId": "goal-held-user", "assistantMessageId": "goal-held-assistant", "prompt": "/goal [fixture:goal-hold] keep checking"}, "human"); err != nil {
		t.Fatal(err)
	}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		diagnostics := manager.TurnDiagnostics("goal-tab", "goal-chat", 1)
		turns, _ := diagnostics["turns"].([]any)
		if len(turns) > 0 && mapFromAnyMain(turns[0])["active"] == true && mapFromAnyMain(turns[0])["firstUpdateMs"] != nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("goal did not reach native host")
	}
	result, handled, err := runtime.Steer(ctx, map[string]any{"tabId": "goal-tab", "chatId": "goal-chat", "sessionId": info.SessionID, "clientUserMessageId": "goal-live-status", "prompt": "/goal", "continuationAssistantMessageId": "goal-status-assistant"})
	if err != nil || !handled || result["ok"] != true {
		t.Fatalf("native goal control did not reach actor: result=%#v handled=%v err=%v", result, handled, err)
	}
	state, _ := runtime.Snapshot("goal-chat")
	if state.Foreground == nil {
		t.Fatal("goal inspection ended native continuation")
	}
	if _, _, err := runtime.Cancel(ctx, state.Foreground.Turn.NativeID); err != nil {
		t.Fatal(err)
	}
	waitProviderChatIdle(t, runtime, "goal-chat", 5*time.Second)
	run("goal-paused-inspect", "/goal")

}
