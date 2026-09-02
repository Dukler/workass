package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/acp"
	"workass/internal/wire"
)

func TestGlobalSessionStoreRejectsChatRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	if err := os.WriteFile(path, []byte(`{"v":1,"chats":[{"id":"foreign-tab","chatId":"foreign-chat"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newSessionStore(path)
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "requires canonical actor storage") {
		t.Fatalf("chat-bearing global state load error = %v", err)
	}
	if got := store.Get(); got != nil {
		t.Fatalf("chat-bearing global state was published: %#v", got)
	}
}

func TestGlobalSessionStoreCompactsLegacyMutationReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	receipts := make(map[string]any, globalPresentationReceiptLimit+73)
	for index := 0; index < globalPresentationReceiptLimit+73; index++ {
		id := fmt.Sprintf("global-noop-m%08x-fixed", index)
		receipts[id] = map[string]any{
			"digest":   strings.Repeat("a", 64),
			"revision": index / 8,
		}
	}
	legacy := map[string]any{
		"v": 1, "chats": []any{}, globalPresentationReceiptsField: receipts,
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := newSessionStore(path)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load compacted global state: %v", err)
	}
	bounded := mapFromAnyMain(store.snapshot[globalPresentationReceiptsField])
	if len(bounded) != globalPresentationReceiptLimit {
		t.Fatalf("bounded receipts = %d, want %d", len(bounded), globalPresentationReceiptLimit)
	}
	oldest := "global-noop-m00000000-fixed"
	newest := fmt.Sprintf("global-noop-m%08x-fixed", globalPresentationReceiptLimit+72)
	if _, exists := bounded[oldest]; exists {
		t.Fatalf("oldest legacy receipt survived bounded compaction")
	}
	if _, exists := bounded[newest]; !exists {
		t.Fatalf("newest legacy receipt was lost during bounded compaction")
	}
	order := anySlice(store.snapshot[globalPresentationReceiptOrderField])
	if len(order) != globalPresentationReceiptLimit || stringValue(order[len(order)-1]) != newest {
		t.Fatalf("receipt order was not normalized: count=%d newest=%q", len(order), stringValue(order[len(order)-1]))
	}

	// Startup rewrites the rebuildable global cache atomically, so the next
	// launch does not pay to decode the discarded legacy ledger again.
	compactedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var compacted map[string]any
	if err := json.Unmarshal(compactedRaw, &compacted); err != nil {
		t.Fatal(err)
	}
	if got := len(mapFromAnyMain(compacted[globalPresentationReceiptsField])); got != globalPresentationReceiptLimit {
		t.Fatalf("persisted compacted receipts = %d, want %d", got, globalPresentationReceiptLimit)
	}
}

func TestGlobalSessionStoreNoopReceiptIsBoundedAndUnchanged(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	changed, err := store.SaveActorGlobalSnapshot(map[string]any{
		"v": 1, "chats": []any{}, "theme": "dark",
		globalPresentationRevisionField:  0,
		globalPresentationOperationField: "global-op-m00000001-first",
	})
	if err != nil || !changed.Changed || changed.Revision != 1 {
		t.Fatalf("changed global save = %#v, err=%v", changed, err)
	}
	var noopInput map[string]any
	var noop globalPresentationSaveResult
	for index := 0; index < globalPresentationReceiptLimit+8; index++ {
		noopInput = map[string]any{
			"v": 1, "chats": []any{}, "theme": "dark",
			globalPresentationRevisionField:  changed.Revision,
			globalPresentationOperationField: fmt.Sprintf("global-noop-m%08x-bounded", index+2),
		}
		noop, err = store.SaveActorGlobalSnapshot(noopInput)
		if err != nil || noop.Changed || noop.Revision != changed.Revision {
			t.Fatalf("no-op global save %d = %#v, err=%v", index, noop, err)
		}
	}
	if got := len(mapFromAnyMain(store.snapshot[globalPresentationReceiptsField])); got != globalPresentationReceiptLimit {
		t.Fatalf("runtime receipt window = %d, want %d", got, globalPresentationReceiptLimit)
	}
	beforeRetry := len(mapFromAnyMain(store.snapshot[globalPresentationReceiptsField]))
	retry, err := store.SaveActorGlobalSnapshot(noopInput)
	if err != nil || retry != noop {
		t.Fatalf("stable no-op retry = %#v, want %#v, err=%v", retry, noop, err)
	}
	if afterRetry := len(mapFromAnyMain(store.snapshot[globalPresentationReceiptsField])); afterRetry != beforeRetry {
		t.Fatalf("stable retry grew receipts: before=%d after=%d", beforeRetry, afterRetry)
	}
}

func TestWireSessionNoopSaveDoesNotBroadcastRefresh(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	hub := wire.NewHub()
	registerSessionHandlersWithActor(hub, store, manager, runtime)

	invoke := func(operationID string, revision int, theme string) map[string]any {
		t.Helper()
		result, err := hub.Invoke("session:save", []any{map[string]any{
			"v": 1, "chats": []any{}, "theme": theme,
			globalPresentationRevisionField:  revision,
			globalPresentationOperationField: operationID,
		}})
		if err != nil {
			t.Fatalf("session:save %s: %v", operationID, err)
		}
		return mapFromAnyMain(result)
	}
	first := invoke("global-op-m00000001-first", 0, "dark")
	if first["ok"] != true || intValue(first[globalPresentationRevisionField]) != 1 {
		t.Fatalf("changed save receipt = %#v", first)
	}
	if broadcasts := hub.Stats()["broadcasts"]; broadcasts != uint64(1) {
		t.Fatalf("changed save broadcasts = %v, want 1", broadcasts)
	}
	second := invoke("global-noop-m00000002-second", 1, "dark")
	if second["ok"] != true || intValue(second[globalPresentationRevisionField]) != 1 {
		t.Fatalf("no-op save receipt = %#v", second)
	}
	if broadcasts := hub.Stats()["broadcasts"]; broadcasts != uint64(1) {
		t.Fatalf("no-op save created a refresh broadcast: %v", broadcasts)
	}
}
