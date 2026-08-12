package main

import (
	"context"
	"encoding/json"
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestActorEnvironmentProjectionSurvivesRuntimeRestartAndRejectsWrongTab(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.actorForNewChat("env-actor-chat", newChatPresentation(map[string]any{
		"tabId": "env-tab", "title": "Entorno", "cwd": "/workspace",
	}, acp.ProviderLaneSelection{})); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	payload := acp.ChatEnvPayload{
		ChatID: "env-actor-chat", TabID: "env-tab", CWD: "/workspace",
		Repos:     []acp.ChatEnvRepo{{Name: "repo", Branch: "main", Files: []acp.ChatEnvFile{{Path: "work.txt", Adds: 2}}}},
		Unchanged: []string{}, RepoLimit: 12, FileLimit: 200,
		Approximation: "test observation",
	}
	if err := runtime.observeChatEnv(payload); err != nil {
		t.Fatalf("commit environment observation: %v", err)
	}
	got, err := runtime.ChatEnvGet("env-tab", "env-actor-chat")
	if err != nil || len(got.Repos) != 1 || got.Repos[0].Files[0].Adds != 2 {
		t.Fatalf("actor environment = %#v, err=%v", got, err)
	}
	if _, err := runtime.ChatEnvGet("wrong-tab", "env-actor-chat"); err == nil {
		t.Fatal("wrong tab read bypassed actor ownership")
	}
	if _, err := runtime.ChatCheckpoints("wrong-tab", "env-actor-chat"); err == nil {
		t.Fatal("wrong tab checkpoint read bypassed actor ownership")
	}
	if _, err := runtime.ChatDiff(context.Background(), "wrong-tab", "env-actor-chat", "repo", "work.txt"); err == nil {
		t.Fatal("wrong tab diff read bypassed actor ownership")
	}

	_ = runtime.Close(context.Background())
	_ = manager.Reset()
	secondManager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	secondStore := sharedSessionStore(stateDir)
	second := newTestProviderChatRuntime(t, secondManager, secondStore, stateDir)
	restarted, err := second.ChatEnvGet("env-tab", "env-actor-chat")
	if err != nil || len(restarted.Repos) != 1 || restarted.Repos[0].Files[0].Path != "work.txt" {
		t.Fatalf("restarted actor environment = %#v, err=%v", restarted, err)
	}
	if cps, err := second.ChatCheckpoints("env-tab", "env-actor-chat"); err != nil || cps == nil {
		t.Fatalf("restarted actor checkpoints = %#v, err=%v", cps, err)
	}
}

func TestCheckpointRestorePublishesOnlyAfterEnvironmentActorCommit(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.actorForNewChat("restore-env-chat", newChatPresentation(map[string]any{
		"tabId": "restore-env-tab", "title": "Restore", "cwd": "/workspace",
	}, acp.ProviderLaneSelection{})); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor("restore-env-chat")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a manager observation belonging to a stale tab. The lifecycle receipt
	// must not publish checkpoint-restored when that observation cannot pass the
	// actor's exact attachment fence.
	stalePayload := acp.ChatEnvPayload{ChatID: "restore-env-chat", TabID: "stale-tab", CWD: "/workspace", Repos: []acp.ChatEnvRepo{}, Unchanged: []string{}}
	if err := manager.RestoreChatEnvReference(rawChatEnvReference(t, stalePayload)); err != nil {
		t.Fatal(err)
	}
	published := make([]string, 0)
	runtime.publish = func(channel string, _ any) { published = append(published, channel) }
	runtime.publishLifecycleReceipt(chat.LifecycleReceipt{
		Kind: chat.LifecycleCheckpointRestored, ChatID: "restore-env-chat", OperationID: providercontract.OperationID("restore-op"),
		TurnSequence: 1, Result: json.RawMessage(`{"ok":true}`),
	})
	if len(published) != 0 {
		t.Fatalf("checkpoint restore published before environment commit: %v", published)
	}
	if actor.engine.Snapshot().Environment.Revision != 0 {
		t.Fatal("stale environment unexpectedly committed")
	}
}

func rawChatEnvReference(t *testing.T, payload acp.ChatEnvPayload) []byte {
	t.Helper()
	// RestoreChatEnvReference accepts the opaque versioned manager reference;
	// keeping it here avoids giving the test direct access to manager caches.
	raw, err := json.Marshal(map[string]any{
		"version": 1, "chatId": payload.ChatID, "tabId": payload.TabID, "cwd": payload.CWD,
		"payload": payload, "reposTruncated": false, "turnSeq": 0, "repos": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
