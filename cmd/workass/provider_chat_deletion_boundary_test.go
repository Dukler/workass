package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
)

func TestDeletedActorDoesNotBlockStartupReconciliation(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	chatID := "deleted-startup-chat"
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatalf("create deleted actor: %v", err)
	}
	if err := engine.Apply(chat.MigrateLegacyChat{
		Version: 2, Digest: "deleted-startup-digest",
		Presentation: chat.PresentationState{TabID: "deleted-startup-tab", Title: "Deleted"},
	}); err != nil {
		t.Fatalf("initialize deleted actor: %v", err)
	}
	if err := engine.Apply(chat.MigrateLegacyObligation{Obligation: &chat.ObligationState{
		State: "needs_input", Source: "test", OpenedAt: "2026-08-11T12:00:00Z", UpdatedAt: "2026-08-11T12:00:00Z",
	}}); err != nil {
		t.Fatalf("add deleted actor obligation: %v", err)
	}
	if err := engine.Apply(chat.DeleteChat{OperationID: "delete-startup", Force: true}); err != nil {
		t.Fatalf("tombstone deleted actor: %v", err)
	}

	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newProviderChatRuntimeBeforeProviderStartup(manager, store, stateDir)
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("deleted actor blocked startup reconciliation: %v", err)
	}
	if err := runtime.ResumeActors(); err != nil {
		t.Fatalf("resume deleted actor cleanup: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := runtime.Snapshot(chatID)
		if ok && state.Deleted && len(state.Outbox) == 1 && state.Outbox[0].Status == chat.OutboxCompleted {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, _ := runtime.Snapshot(chatID)
	t.Fatalf("deleted actor cleanup did not complete after startup: %#v", state)
}

func TestDeletedActorRejectsOriginalCreateReplay(t *testing.T) {
	stateDir := t.TempDir()
	store := sharedSessionStore(stateDir)
	chatID := "deleted-create-replay"
	presentation := chat.PresentationState{TabID: "deleted-create-tab", Title: "Deleted native", TitleLocked: true}
	digest, err := chatCreationDigest(presentation, false)
	if err != nil {
		t.Fatalf("create digest: %v", err)
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatalf("create actor: %v", err)
	}
	if err := engine.Apply(chat.InitializeChat{Presentation: presentation, OperationID: "create-replay", Digest: digest}); err != nil {
		t.Fatalf("initialize actor: %v", err)
	}
	if err := engine.Apply(chat.DeleteChat{OperationID: "delete-replay", Force: true}); err != nil {
		t.Fatalf("delete actor: %v", err)
	}

	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
	runtime := newProviderChatRuntimeBeforeProviderStartup(manager, store, stateDir)
	t.Cleanup(func() {
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("deleted actor blocked startup: %v", err)
	}
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": presentation.TabID, "chatId": chatID, "operationId": "create-replay",
		"title": presentation.Title, "titleLocked": true,
	}); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("original create replay was accepted: %v", err)
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || !state.Deleted {
		t.Fatalf("create replay changed tombstone: ok=%v state=%#v", ok, state)
	}
}
