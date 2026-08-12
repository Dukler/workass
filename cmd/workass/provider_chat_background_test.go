package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestAmbiguousLegacyBackgroundQuarantinesOneChatWithoutBlockingCutover(t *testing.T) {
	stateDir := t.TempDir()
	legacy := map[string]any{"activeId": "good-tab", "chats": []any{
		map[string]any{"id": "bad-tab", "chatId": "bad-chat", "title": "Ambiguous background", "messages": []any{}, "queue": []any{}},
		map[string]any{"id": "good-tab", "chatId": "good-chat", "title": "Unaffected", "messages": []any{}, "queue": []any{}},
	}}
	legacyData, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	spawnedDir := filepath.Join(stateDir, "spawned-work")
	if err := os.MkdirAll(spawnedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	spawnedData, err := json.Marshal([]acp.SpawnedWorkItem{{
		ID: "legacy-ownerless", TaskID: "legacy-ownerless", TabID: "bad-tab", ChatID: "bad-chat",
		ProviderID: "codex", Kind: "subagent", Label: "Pre-actor work", Status: "exited",
		StartedAt: now, UpdatedAt: now, FinishedAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spawnedDir, safeArchiveNameMain("bad-tab")+".json"), spawnedData, 0o600); err != nil {
		t.Fatal(err)
	}

	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "test", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("ambiguous background blocked daemon startup: %v", err)
	}

	badEngine, err := chat.NewDurableEngine("bad-chat", chat.FileStore{Path: providerChatStatePath(stateDir, "bad-chat")})
	if err != nil {
		t.Fatal(err)
	}
	bad := badEngine.Snapshot()
	if bad.Migration.BlockedError != providercontract.ErrorNativeIdentityConflict || !bad.Migration.LegacyBackgroundMigrated || len(bad.Background) != 0 {
		t.Fatalf("ambiguous chat migration = blocked=%q backgroundMigrated=%v background=%#v", bad.Migration.BlockedError, bad.Migration.LegacyBackgroundMigrated, bad.Background)
	}
	goodEngine, err := chat.NewDurableEngine("good-chat", chat.FileStore{Path: providerChatStatePath(stateDir, "good-chat")})
	if err != nil {
		t.Fatal(err)
	}
	good := goodEngine.Snapshot()
	if good.Migration.BlockedError != "" || !good.Migration.LegacyBackgroundMigrated {
		t.Fatalf("unrelated chat was not migrated normally: %#v", good.Migration)
	}
	if receipt, ok, err := readLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename)); err != nil || !ok || !receipt.Complete || len(receipt.ChatIDs) != 2 {
		t.Fatalf("global cutover did not commit after per-chat quarantine: receipt=%#v ok=%v err=%v", receipt, ok, err)
	}
}

func TestRuntimeBackgroundOwnerRequiresExactOrigin(t *testing.T) {
	state, err := chat.NewState("chat")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(command chat.Command) {
		t.Helper()
		next, _, reduceErr := chat.Reduce(state, command)
		if reduceErr != nil {
			t.Fatalf("apply %T: %v", command, reduceErr)
		}
		state = next
	}
	realm := providercontract.Realm{
		ProviderID: "codex", MachineID: "machine", AccountScope: "account", InstallScope: "install", Verified: true,
	}
	identity := providercontract.LaneIdentity{ChatID: "chat", WorkspaceEpoch: "workspace-1", Realm: realm}.Normalize()
	apply(chat.InitializeChat{Presentation: chat.PresentationState{TabID: "tab"}, OperationID: "create:background", Digest: "create-background"})
	apply(chat.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "tab"}})
	apply(chat.LaneOpened{
		LaneID: identity.ID, Thread: providercontract.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: providercontract.ContextCapabilities{ExactResume: true},
	})
	apply(chat.Submit{OperationID: "turn-op", Text: "run it"})
	apply(chat.TurnAdmitted{OperationID: "turn-op", Accepted: true, Turn: providercontract.TurnRef{OperationID: "turn-op", NativeID: "native-turn"}})

	if owner, ok := exactBackgroundOwner(state, acp.SpawnedWorkItem{ID: "work", ProviderID: "codex"}); ok {
		t.Fatalf("ownerless live item guessed historical owner %#v", owner)
	}
	item := acp.SpawnedWorkItem{ID: "work", ProviderID: "codex", OriginOperationID: "turn-op"}
	owner, ok := exactBackgroundOwner(state, item)
	if !ok || owner.LaneID != identity.ID || owner.OperationID != "turn-op" || owner.TurnID != "native-turn" {
		t.Fatalf("exact operation owner = %#v, %v", owner, ok)
	}
	item.OriginTurnID = "another-turn"
	if owner, ok := exactBackgroundOwner(state, item); ok {
		t.Fatalf("mismatched operation/turn origin was accepted: %#v", owner)
	}

	// Migration may reuse an exact persisted origin, but it cannot synthesize an
	// operation from an otherwise ownerless work id.
	owner, ok = legacyBackgroundOwner(state, acp.SpawnedWorkItem{
		ID: "legacy", ProviderID: "codex", OriginOperationID: "turn-op", OriginTurnID: "native-turn",
	}, "legacy")
	if !ok || owner.OperationID != "turn-op" || owner.TurnID != "native-turn" {
		t.Fatalf("exact legacy owner = %#v, %v", owner, ok)
	}
	if owner, ok = legacyBackgroundOwner(state, acp.SpawnedWorkItem{ID: "legacy", ProviderID: "codex"}, "legacy"); ok {
		t.Fatalf("ownerless legacy work was synthesized: %#v", owner)
	}
}
