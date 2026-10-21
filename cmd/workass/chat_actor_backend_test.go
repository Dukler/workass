package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/store"
)

func newTestManagerForBackend(t *testing.T, stateDir string) *acp.Manager {
	t.Helper()
	manager := acp.NewManager(acp.Options{RootDir: t.TempDir(), StateDir: stateDir, RuntimeProfile: "test"})
	t.Cleanup(func() { manager.Reset() })
	return manager
}

func TestParseSQLiteStoresDefaultsOffAndRejectsUnknownStores(t *testing.T) {
	noEnv := func(string) string { return "" }
	env := func(key string) string {
		if key == sqliteStoresEnv {
			return " Chats "
		}
		return ""
	}
	if stores, err := parseSQLiteStores("", false, noEnv); err != nil || len(stores) != 0 {
		t.Fatalf("default = %v %v, want none", stores, err)
	}
	if stores, err := parseSQLiteStores("", false, env); err != nil || !stores[sqliteStoreChats] {
		t.Fatalf("env = %v %v", stores, err)
	}
	if stores, err := parseSQLiteStores("", true, env); err != nil || len(stores) != 0 {
		t.Fatalf("an explicit empty flag must win over the env: %v %v", stores, err)
	}
	if _, err := parseSQLiteStores("chats,lanes", true, noEnv); err == nil || !strings.Contains(err.Error(), "lanes") {
		t.Fatalf("unknown store accepted: %v", err)
	}
}

func writeBackendTestActor(t *testing.T, stateDir, chatID string) *chat.Engine {
	t.Helper()
	engine, err := chat.NewDurableEngine(chatID, chat.FileStore{Path: providerChatStatePath(stateDir, chatID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(chat.InitializeChat{
		Presentation: chat.PresentationState{TabID: "tab-" + chatID, Title: "backend " + chatID},
		OperationID:  providercontract.OperationID("chat-create:" + chatID), Digest: "digest-" + chatID,
	}); err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestOpenChatActorBackendSelectsImportsAndFailsClosed(t *testing.T) {
	stateDir := t.TempDir()
	engine := writeBackendTestActor(t, stateDir, "backend-chat")

	// Disabled with no database: the JSON store, and no database file appears.
	backend, err := openChatActorBackend(stateDir, false, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.(fileActorBackend); !ok {
		t.Fatalf("disabled backend = %T", backend)
	}
	if _, err := os.Stat(filepath.Join(stateDir, store.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the disabled flag created a database: %v", err)
	}

	// Enabled: one-shot import; the JSON stays byte-identical.
	before, err := os.ReadFile(providerChatStatePath(stateDir, "backend-chat"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err = openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	sqlBackend, ok := backend.(sqlActorBackend)
	if !ok {
		t.Fatalf("enabled backend = %T", backend)
	}
	heads, err := sqlBackend.discover()
	if err != nil || len(heads) != 1 || heads[0].State.ChatID != "backend-chat" {
		t.Fatalf("SQLite discovery = %#v %v", heads, err)
	}
	loaded, found, err := sqlBackend.store("backend-chat").Load("backend-chat")
	if err != nil || !found || loaded.Presentation.Title != engine.Snapshot().Presentation.Title {
		t.Fatalf("SQLite actor = found:%v err:%v", found, err)
	}

	// A second daemon on the same state directory fails fast.
	started := time.Now()
	if _, err := openChatActorBackend(stateDir, true, "test", t.Logf); !errors.Is(err, store.ErrLocked) {
		t.Fatalf("second open while held: %v", err)
	}
	if time.Since(started) > 30*time.Second {
		t.Fatal("locked database did not fail fast")
	}
	if err := backend.close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(providerChatStatePath(stateDir, "backend-chat"))
	if err != nil || string(before) != string(after) {
		t.Fatal("the import rewrote the JSON backup")
	}

	// Disabled after the cutover: the JSON is stale, refuse to start from it.
	if _, err := openChatActorBackend(stateDir, false, "test", t.Logf); err == nil || !strings.Contains(err.Error(), "--sqlite-stores=chats") {
		t.Fatalf("disabled flag after cutover: %v", err)
	}

	// Enabled, but an older build wrote the JSON after the cutover.
	next := engine.Snapshot().Presentation.Clone()
	next.Draft = "written by a JSON-only build"
	if err := engine.Apply(chat.SavePresentation{OperationID: "late-save", Digest: "late", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := openChatActorBackend(stateDir, true, "test", t.Logf); !errors.Is(err, chat.ErrFileActorsChangedAfterCutover) {
		t.Fatalf("JSON written after the cutover: %v", err)
	}
}

func TestProviderChatRuntimeRunsOnTheSQLiteBackend(t *testing.T) {
	stateDir := t.TempDir()
	writeBackendTestActor(t, stateDir, "runtime-sql-chat")
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	manager := newTestManagerForBackend(t, stateDir)
	runtime := newProviderChatRuntimeBeforeProviderStartupWithBackend(manager, newSessionStore(filepath.Join(stateDir, sessionStateFilename)), stateDir, backend)
	defer runtime.Close(context.Background())
	if err := runtime.StartupError(); err != nil {
		t.Fatal(err)
	}
	actor, err := runtime.actor("runtime-sql-chat")
	if err != nil {
		t.Fatal(err)
	}
	next := actor.engine.Snapshot().Presentation.Clone()
	next.Draft = "saved through SQLite"
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: "sql-save", Digest: "sql-save", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	// A new actor never touches provider-chats.
	if _, err := runtime.actorForNewChat("runtime-sql-new", chat.PresentationState{TabID: "tab-new", Title: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(providerChatStatePath(stateDir, "runtime-sql-new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a SQLite-backed actor wrote JSON: %v", err)
	}
	reloaded, found, err := backend.store("runtime-sql-chat").Load("runtime-sql-chat")
	if err != nil || !found || reloaded.Presentation.Draft != "saved through SQLite" {
		t.Fatalf("SQLite readback: found=%v err=%v draft=%q", found, err, reloaded.Presentation.Draft)
	}
}
