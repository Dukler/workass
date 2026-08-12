package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestVerifiedV5CutoverCleansStaleArtifactsBeforeStartupReconciliation(t *testing.T) {
	const chatID = "migration-current-chat"
	const oldTabID = "migration-old-tab"
	const currentTabID = "migration-current-tab"
	stateDir := t.TempDir()

	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatalf("create actor fixture: %v", err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: oldTabID, Title: "Current actor", ProviderID: "mock"},
		OperationID:  "migration-create",
		Digest:       "migration-create-digest",
	}); err != nil {
		t.Fatalf("initialize actor fixture: %v", err)
	}
	if err := writeLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename), legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ChatIDs: []string{chatID},
	}); err != nil {
		t.Fatalf("write verified v5 receipt: %v", err)
	}
	if err := engine.Apply(chat.AttachTab{TabID: currentTabID}); err != nil {
		t.Fatalf("move actor to its current tab: %v", err)
	}
	for _, stale := range []struct {
		version uint32
		path    string
	}{
		{version: 3, path: filepath.Join(stateDir, olderLegacyChatCutoverReceiptFilename)},
		{version: 4, path: filepath.Join(stateDir, previousLegacyChatCutoverReceiptFilename)},
	} {
		path := stale.path
		if err := writeLegacyChatCutoverReceipt(path, legacyChatCutoverReceipt{
			Version: stale.version, Complete: true, CompletedAt: "2026-08-11T10:00:00Z", ChatIDs: []string{chatID},
		}); err != nil {
			t.Fatalf("write stale v%d receipt: %v", stale.version, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "obligations"), 0o700); err != nil {
		t.Fatalf("create stale obligation store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "obligations", oldTabID+".json"), []byte(`{"open":[]}`), 0o600); err != nil {
		t.Fatalf("write stale obligation: %v", err)
	}
	backgroundPath := filepath.Join(stateDir, "spawned-work", oldTabID+".json")
	if err := os.MkdirAll(filepath.Dir(backgroundPath), 0o700); err != nil {
		t.Fatalf("create stale background store: %v", err)
	}
	background := []map[string]any{{
		"id": "stale-background", "taskId": "stale-background", "tabId": oldTabID, "chatId": chatID,
		"providerId": "mock", "kind": "background", "status": "running",
		"startedAt": "2026-08-11T10:00:00Z", "updatedAt": "2026-08-11T10:00:00Z",
	}}
	backgroundRaw, err := json.Marshal(background)
	if err != nil {
		t.Fatalf("marshal stale background: %v", err)
	}
	if err := os.WriteFile(backgroundPath, backgroundRaw, 0o600); err != nil {
		t.Fatalf("write stale background: %v", err)
	}
	receiptsPath := filepath.Join(stateDir, "spawned-work-receipts", oldTabID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(receiptsPath), 0o700); err != nil {
		t.Fatalf("create stale background receipts: %v", err)
	}
	if err := os.WriteFile(receiptsPath, []byte(`{"receiptId":"stale-background","taskId":"stale-background","tabId":"`+oldTabID+`","chatId":"`+chatID+`"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write stale background receipt: %v", err)
	}
	currentExecutorPath := filepath.Join(stateDir, "spawned-work", currentTabID+".json")
	if err := os.WriteFile(currentExecutorPath, []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write current executor cache: %v", err)
	}
	currentReceiptPath := filepath.Join(stateDir, "spawned-work-receipts", currentTabID+".jsonl")
	if err := os.WriteFile(currentReceiptPath, []byte(`{"receiptId":"current-background","taskId":"current-background","tabId":"`+currentTabID+`","chatId":"`+chatID+`"}`+"\n"), 0o600); err != nil {
		t.Fatalf("write current executor receipt: %v", err)
	}
	cutoverPath := filepath.Join(stateDir, legacyChatCutoverReceiptFilename)
	cutoverBefore, err := os.ReadFile(cutoverPath)
	if err != nil {
		t.Fatalf("read immutable v5 receipt before cleanup: %v", err)
	}

	boot := func() (*providerChatRuntime, *acp.Manager, map[string]any) {
		manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "dev"})
		store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
		runtime := newProviderChatRuntime(manager, store, stateDir)
		if err := runtime.StartupError(); err != nil {
			manager.Reset()
			t.Fatalf("startup with verified v5 receipt: %v", err)
		}
		projection, err := runtime.ProjectSession()
		if err != nil {
			_ = runtime.Close(context.Background())
			manager.Reset()
			t.Fatalf("project actor-only session: %v", err)
		}
		return runtime, manager, projection
	}

	first, firstManager, firstProjection := boot()
	if _, err := os.Stat(providerChatStatePath(stateDir, chatID)); err != nil {
		t.Fatalf("cleanup removed current actor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, previousLegacyChatCutoverReceiptFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale v4 receipt remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, olderLegacyChatCutoverReceiptFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale v3 receipt remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "obligations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale obligation store remains: %v", err)
	}
	if _, err := os.Stat(backgroundPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale old-tab executor cache remains: %v", err)
	}
	if _, err := os.Stat(receiptsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale old-tab executor receipt remains: %v", err)
	}
	if _, err := os.Stat(currentExecutorPath); err != nil {
		t.Fatalf("current executor cache was deleted by legacy cleanup: %v", err)
	}
	if _, err := os.Stat(currentReceiptPath); err != nil {
		t.Fatalf("current executor receipt was deleted by legacy cleanup: %v", err)
	}
	if items := firstManager.ListSpawnedWork(currentTabID, chatID); len(items) != 0 {
		t.Fatalf("stale background work remained on the current actor attachment: %#v", items)
	}
	cutoverAfter, err := os.ReadFile(cutoverPath)
	if err != nil || !reflect.DeepEqual(cutoverBefore, cutoverAfter) {
		t.Fatalf("immutable v5 cutover receipt changed during cleanup: before=%s after=%s err=%v", cutoverBefore, cutoverAfter, err)
	}
	cleanupReceipt, ok, err := readLegacyChatCleanupReceipt(filepath.Join(stateDir, legacyChatCleanupReceiptFilename))
	if err != nil || !ok || cleanupReceipt.Version != legacyChatCleanupVersion || !cleanupReceipt.Complete || cleanupReceipt.CutoverDigest != legacyChatCutoverDigest(legacyChatCutoverReceipt{Version: legacyChatCutoverVersion, Complete: true, ChatIDs: []string{chatID}}) {
		t.Fatalf("separate legacy cleanup receipt was not durably committed: receipt=%#v ok=%v err=%v", cleanupReceipt, ok, err)
	}
	cleanupBefore, err := os.ReadFile(filepath.Join(stateDir, legacyChatCleanupReceiptFilename))
	if err != nil {
		t.Fatalf("read cleanup receipt before restart: %v", err)
	}
	if err := first.Close(context.Background()); err != nil {
		t.Fatalf("close first runtime: %v", err)
	}
	firstManager.Reset()

	// A matching cleanup receipt is the durable one-time boundary. Recreate
	// every class of stale legacy artifact, including an unreadable obligation
	// payload, before the second boot. Startup must leave these files alone and
	// continue to use the actor projection as its semantic authority.
	for _, stale := range []struct {
		version uint32
		path    string
	}{
		{version: 3, path: filepath.Join(stateDir, olderLegacyChatCutoverReceiptFilename)},
		{version: 4, path: filepath.Join(stateDir, previousLegacyChatCutoverReceiptFilename)},
	} {
		if err := writeLegacyChatCutoverReceipt(stale.path, legacyChatCutoverReceipt{
			Version: stale.version, Complete: true, CompletedAt: "2026-08-11T10:01:00Z", ChatIDs: []string{chatID},
		}); err != nil {
			t.Fatalf("recreate stale v%d receipt before second boot: %v", stale.version, err)
		}
	}
	if err := os.WriteFile(backgroundPath, backgroundRaw, 0o600); err != nil {
		t.Fatalf("recreate stale background before second boot: %v", err)
	}
	if err := os.WriteFile(receiptsPath, []byte(`{"receiptId":"recreated-stale-background","taskId":"recreated-stale-background","tabId":"`+oldTabID+`","chatId":"`+chatID+`"}`+"\n"), 0o600); err != nil {
		t.Fatalf("recreate stale background receipt before second boot: %v", err)
	}
	obligationPath := filepath.Join(stateDir, "obligations", oldTabID+".json")
	if err := os.MkdirAll(filepath.Dir(obligationPath), 0o700); err != nil {
		t.Fatalf("recreate stale obligation store before second boot: %v", err)
	}
	if err := os.WriteFile(obligationPath, []byte(`{"not":"a legacy obligation"`), 0o600); err != nil {
		t.Fatalf("recreate unreadable obligation before second boot: %v", err)
	}

	second, secondManager, secondProjection := boot()
	t.Cleanup(func() {
		_ = second.Close(context.Background())
		secondManager.Reset()
	})
	firstChat := chatFromSnapshot(firstProjection, currentTabID)
	secondChat := chatFromSnapshot(secondProjection, currentTabID)
	if firstChat == nil || secondChat == nil {
		t.Fatalf("actor-only chat disappeared across boots: first=%#v second=%#v", firstProjection, secondProjection)
	}
	firstChat = mapFromAnyMain(cloneJSON(firstChat))
	secondChat = mapFromAnyMain(cloneJSON(secondChat))
	// actorRevision is monotonic durable bookkeeping advanced by the existing
	// startup reconciliation commands; it is not semantic chat projection.
	delete(firstChat, "actorRevision")
	delete(secondChat, "actorRevision")
	if !reflect.DeepEqual(firstChat, secondChat) {
		t.Fatalf("actor-only semantic projection changed across boots: first=%#v second=%#v", firstChat, secondChat)
	}
	if _, err := os.Stat(currentExecutorPath); err != nil {
		t.Fatalf("current executor cache was deleted on the second boot: %v", err)
	}
	if _, err := os.Stat(currentReceiptPath); err != nil {
		t.Fatalf("current executor receipt was deleted on the second boot: %v", err)
	}
	for _, path := range []string{backgroundPath, receiptsPath,
		filepath.Join(stateDir, olderLegacyChatCutoverReceiptFilename),
		filepath.Join(stateDir, previousLegacyChatCutoverReceiptFilename), obligationPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("recreated legacy artifact was consulted or deleted on the second boot (%s): %v", path, err)
		}
	}
	cleanupAfter, err := os.ReadFile(filepath.Join(stateDir, legacyChatCleanupReceiptFilename))
	if err != nil || !reflect.DeepEqual(cleanupBefore, cleanupAfter) {
		t.Fatalf("legacy cleanup receipt was not idempotent across restart: before=%s after=%s err=%v", cleanupBefore, cleanupAfter, err)
	}
}

func TestLegacySteerWithoutExplicitTurnOwnerQuarantinesOnlyThatChat(t *testing.T) {
	stateDir := t.TempDir()
	legacy := map[string]any{"activeId": "good-tab", "chats": []any{
		map[string]any{"id": "bad-tab", "chatId": "bad-chat", "title": "Unowned steer", "messages": []any{
			map[string]any{"id": "user-1", "role": "user", "content": "first", "status": "done"},
			map[string]any{"id": "assistant-1", "role": "assistant", "content": "working", "status": "done"},
			map[string]any{"id": "steer-1", "role": "user", "content": "redirect", "status": "done", "steerState": "applied"},
		}, "queue": []any{}},
		map[string]any{"id": "good-tab", "chatId": "good-chat", "title": "Unaffected", "messages": []any{}, "queue": []any{}},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, sessionStateFilename), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "test", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("unowned legacy steer blocked daemon startup: %v", err)
	}

	badEngine, err := chat.NewDurableEngine("bad-chat", chat.FileStore{Path: providerChatStatePath(stateDir, "bad-chat")})
	if err != nil {
		t.Fatal(err)
	}
	bad := badEngine.Snapshot()
	if bad.Migration.BlockedError != providercontract.ErrorNativeIdentityConflict || len(bad.Ledger) != 3 {
		t.Fatalf("quarantined steer migration = blocked=%q ledger=%d", bad.Migration.BlockedError, len(bad.Ledger))
	}
	if bad.Ledger[2].MessageID != "steer-1" || bad.Ledger[2].Text != "redirect" || bad.Ledger[2].SteerState != "" || bad.Ledger[2].TurnRootID != "" {
		t.Fatalf("quarantined steer row did not preserve visible semantics safely: %#v", bad.Ledger[2])
	}
	goodEngine, err := chat.NewDurableEngine("good-chat", chat.FileStore{Path: providerChatStatePath(stateDir, "good-chat")})
	if err != nil {
		t.Fatal(err)
	}
	if good := goodEngine.Snapshot(); good.Migration.BlockedError != "" || !good.Migration.LegacyBackgroundMigrated {
		t.Fatalf("unrelated chat was not migrated normally: %#v", good.Migration)
	}
}

func TestInterruptedV5ReceiptRepairsUninitializedOrphanFromExactNativeInventory(t *testing.T) {
	stateDir := t.TempDir()
	const chatID, tabID = "receipt-orphan-chat", "receipt-orphan-tab"
	if _, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)}); err != nil {
		t.Fatal(err)
	}
	if err := writeLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename), legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: []string{chatID},
	}); err != nil {
		t.Fatal(err)
	}
	writeJSONTestFile(t, filepath.Join(stateDir, "native-sessions.json"), map[string]any{
		"v": 2, "bindings": []any{map[string]any{
			"tabId": tabID, "chatId": chatID, "providerId": "mock", "sessionId": "orphan-native-thread",
			"cwd": t.TempDir(), "modelId": "mock-deterministic", "modeId": "ask", "generation": 1, "resumeSafe": true,
		}},
	})
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "test", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("interrupted receipt repair blocked daemon startup: %v", err)
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	if !state.Initialized || state.Presentation.TabID != tabID || !state.Migration.LegacyBackgroundMigrated || state.Migration.BlockedError != providercontract.ErrorNativeIdentityConflict {
		t.Fatalf("repaired interrupted receipt actor = %#v", state.Migration)
	}
	if cleanup, ok, err := readLegacyChatCleanupReceipt(filepath.Join(stateDir, legacyChatCleanupReceiptFilename)); err != nil || !ok || !cleanup.Complete {
		t.Fatalf("receipt repair did not complete legacy cleanup: cleanup=%#v ok=%v err=%v", cleanup, ok, err)
	}
}

func TestInterruptedV5ReceiptQuarantinesGhostActorWithoutGuessingOwner(t *testing.T) {
	stateDir := t.TempDir()
	const chatID = "receipt-ghost-chat"
	if _, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)}); err != nil {
		t.Fatal(err)
	}
	if err := writeLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename), legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: []string{chatID},
	}); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "test", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("ghost receipt actor blocked daemon startup: %v", err)
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatal(err)
	}
	state := engine.Snapshot()
	if !state.Initialized || !strings.HasPrefix(state.Presentation.TabID, "quarantined-native-") || state.Presentation.ProviderID != "" || len(state.Ledger) != 0 || len(state.Lanes) != 0 || state.Migration.BlockedError != providercontract.ErrorNativeIdentityConflict {
		t.Fatalf("ghost actor repair guessed authority or lost quarantine: %#v", state)
	}
}

func TestInterruptedV5ReceiptPreservesPrecutoverActorLedgerWhileQuarantining(t *testing.T) {
	stateDir := t.TempDir()
	const chatID, tabID = "receipt-precutover-chat", "receipt-precutover-tab"
	state, err := chat.NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Presentation = chat.PresentationState{TabID: tabID, Title: "Pre-cutover actor"}
	state.Ledger = []chat.LedgerEvent{
		{EventID: "legacy:user", MessageID: "legacy-user", Sequence: 1, Role: "user", Text: "keep this", Status: "done", OperationID: "legacy-op", Legacy: true},
		{EventID: "legacy:assistant", MessageID: "legacy-assistant", Sequence: 2, Role: "assistant", Text: "and this", Result: "and this", Status: "done", TerminalState: "done", OperationID: "legacy-op", Legacy: true},
	}
	state.Operations["legacy-op"] = struct{}{}
	if err := (chat.FileStore{Path: providerChatStatePath(stateDir, chatID)}).Save(state); err != nil {
		t.Fatal(err)
	}
	if err := writeLegacyChatCutoverReceipt(filepath.Join(stateDir, legacyChatCutoverReceiptFilename), legacyChatCutoverReceipt{
		Version: legacyChatCutoverVersion, Complete: true, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), ChatIDs: []string{chatID},
	}); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{StateDir: stateDir, RuntimeProfile: "test", SpawnedWorkReconcileInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntime(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("pre-cutover receipt actor blocked daemon startup: %v", err)
	}
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatal(err)
	}
	repaired := engine.Snapshot()
	if !repaired.Initialized || repaired.Migration.BlockedError != providercontract.ErrorNativeIdentityConflict || len(repaired.Ledger) != 2 || repaired.Ledger[0].Text != "keep this" || repaired.Ledger[1].Result != "and this" {
		t.Fatalf("pre-cutover repair lost semantic ledger: %#v", repaired)
	}
}
