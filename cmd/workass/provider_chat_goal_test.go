package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestCodexNativeGoalThroughActorAndExactResume(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	activity := make(chan struct{}, 1)
	signalActivity := func(string, any) {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour, Broadcast: signalActivity,
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true, Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args), "WORKASS_CODEX_FIXTURE_GOAL_STORE": filepath.Join(stateDir, "native-goal-fixture.json")}}})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir, signalActivity)
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
			state, _ := runtime.Snapshot("goal-chat")
			foreground := ""
			if state.Foreground != nil {
				foreground = string(state.Foreground.OperationID)
			}
			t.Fatalf("start %s: %v (foreground=%s)", operation, err, foreground)
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
	before, _ := runtime.Snapshot("goal-chat")
	attachment := before.Lanes[before.ActiveLaneID].Attachment
	if attachment == nil || !attachment.CommandCatalogSupported || attachment.CommandCatalog == nil || len(attachment.CommandCatalog.Commands) != 1 || attachment.CommandCatalog.Commands[0].Name != "goal" {
		t.Fatalf("native slash catalog lost from actor: %#v", attachment)
	}
	thread := before.Lanes[before.ActiveLaneID].Thread
	if !runtime.CloseSession(ctx, attachment.ConnectionID) {
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
	for {
		diagnostics := manager.TurnDiagnostics("goal-tab", "goal-chat", 1)
		turns, _ := diagnostics["turns"].([]any)
		state, _ := runtime.Snapshot("goal-chat")
		// Transport diagnostics may arrive before the actor has admitted this
		// turn. Live control needs the actor's exact running owner as well.
		if state.Foreground != nil && state.Foreground.OperationID == "goal-held" && state.Foreground.Status == chat.ForegroundRunning && state.Foreground.Turn.NativeID != "" && len(turns) > 0 && mapFromAnyMain(turns[0])["active"] == true && mapFromAnyMain(turns[0])["firstUpdateMs"] != nil {
			break
		}
		select {
		case <-activity:
		case <-ctx.Done():
			t.Fatal("goal did not reach the actor's running native turn:", ctx.Err())
		}
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
