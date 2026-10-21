package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

func sqliteStoresTestEnv(value string, set bool) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if key == sqliteStoresEnv && set {
			return value, true
		}
		return "", false
	}
}

func TestParseSQLiteStoresDefaultsByProfileAndRejectsUnknownStores(t *testing.T) {
	unset := sqliteStoresTestEnv("", false)
	for _, tc := range []struct {
		name      string
		flag      string
		flagSet   bool
		env       func(string) (string, bool)
		profile   string
		goos      string
		wantChats bool
		source    string
	}{
		{name: "dev default", env: unset, profile: "dev", wantChats: true, source: "dev profile default"},
		{name: "prod default on macOS", env: unset, profile: "prod", goos: "darwin", wantChats: true, source: "prod profile default"},
		{name: "prod default on Windows", env: unset, profile: "prod", goos: "windows", source: "prod profile default"},
		{name: "explicit empty env in macOS prod", env: sqliteStoresTestEnv("", true), profile: "prod", goos: "darwin", source: sqliteStoresEnv},
		{name: "test default", env: unset, profile: "test", source: "test profile default"},
		{name: "env on in prod", env: sqliteStoresTestEnv(" Chats ", true), profile: "prod", wantChats: true, source: sqliteStoresEnv},
		{name: "explicit empty env in dev", env: sqliteStoresTestEnv("", true), profile: "dev", source: sqliteStoresEnv},
		{name: "explicit empty flag in dev", flagSet: true, env: unset, profile: "dev", source: "--sqlite-stores"},
		{name: "explicit empty flag over env", flagSet: true, env: sqliteStoresTestEnv("chats", true), profile: "prod", source: "--sqlite-stores"},
		{name: "flag on in prod", flag: "chats", flagSet: true, env: sqliteStoresTestEnv("", true), profile: "prod", wantChats: true, source: "--sqlite-stores"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			goos := tc.goos
			if goos == "" {
				goos = "windows"
			}
			setSQLiteStoresGOOS(t, goos)
			stores, source, err := parseSQLiteStores(tc.flag, tc.flagSet, tc.env, tc.profile)
			if err != nil {
				t.Fatal(err)
			}
			if stores[sqliteStoreChats] != tc.wantChats || len(stores) > 1 {
				t.Fatalf("stores = %v, want chats=%v", stores, tc.wantChats)
			}
			if source != tc.source {
				t.Fatalf("source = %q, want %q", source, tc.source)
			}
		})
	}
	if _, _, err := parseSQLiteStores("chats,lanes", true, unset, "dev"); err == nil || !strings.Contains(err.Error(), "lanes") {
		t.Fatalf("unknown store accepted: %v", err)
	}
	if _, _, err := parseSQLiteStores("", false, sqliteStoresTestEnv("lanes", true), "dev"); err == nil || !strings.Contains(err.Error(), "lanes") {
		t.Fatalf("unknown store from the env accepted: %v", err)
	}
}

// The --sqlite-stores help states each profile default the parser applies.
func TestSQLiteStoresHelpStatesTheProfileDefaults(t *testing.T) {
	for _, tc := range []struct {
		profile, goos, phrase string
		on                    bool
	}{
		{"dev", "windows", "chats in the dev profile", true},
		{"prod", "darwin", "in prod on macOS", true},
		{"prod", "windows", "none in Windows prod", false},
		{"test", "darwin", "or the test profile", false},
	} {
		setSQLiteStoresGOOS(t, tc.goos)
		if on := sqliteStoresProfileDefault(tc.profile) == sqliteStoreChats; on != tc.on {
			t.Fatalf("%s on %s: default on=%v, want %v", tc.profile, tc.goos, on, tc.on)
		}
		if !strings.Contains(sqliteStoresUsage, tc.phrase) {
			t.Fatalf("help %q does not say %q", sqliteStoresUsage, tc.phrase)
		}
	}
}

// main passes workassRuntimeProfile(): WORKASS_PROFILE=dev turns the default
// on, and an unset or unknown profile is prod, which is on only on macOS.
func TestSQLiteStoresFollowTheRuntimeProfile(t *testing.T) {
	setSQLiteStoresGOOS(t, "darwin")
	for profile, want := range map[string]bool{" Dev ": true, "test": false, "prod": true, "": true, "unexpected": true} {
		t.Setenv("WORKASS_PROFILE", profile)
		stores, _, err := parseSQLiteStores("", false, sqliteStoresTestEnv("", false), workassRuntimeProfile())
		if err != nil || stores[sqliteStoreChats] != want {
			t.Fatalf("WORKASS_PROFILE=%q: stores = %v %v, want chats=%v", profile, stores, err, want)
		}
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

func setSQLiteStoresGOOS(t *testing.T, goos string) {
	t.Helper()
	previous := sqliteStoresGOOS
	sqliteStoresGOOS = goos
	t.Cleanup(func() { sqliteStoresGOOS = previous })
}

// A rollback or backup restore puts back a copy of a SQLite-live state
// directory. The copy has the same bytes and new file times: the macOS
// updater's snapshot (fs.cpSync with preserveTimestamps keeps microseconds),
// a plain `cp -R`, or any tool that rounds times. Each must boot, and a real
// JSON write after the cutover must still be refused.
func TestOpenChatActorBackendBootsFromARestoredCopy(t *testing.T) {
	copies := map[string]func(t *testing.T, from, to string){
		"updater snapshot": func(t *testing.T, from, to string) {
			node, err := exec.LookPath("node")
			if err != nil {
				t.Skip("node is not on PATH")
			}
			// desktop/shell/update-worker.js, the mutable-state snapshot.
			script := `const fs=require("fs");fs.cpSync(process.argv[1],process.argv[2],{recursive:true,errorOnExist:true,force:false,dereference:false,preserveTimestamps:true,verbatimSymlinks:true,mode:fs.constants.COPYFILE_FICLONE});`
			if out, err := exec.Command(node, "-e", script, from, to).CombinedOutput(); err != nil {
				t.Fatalf("cpSync: %v %s", err, out)
			}
		},
		"cp -R": func(t *testing.T, from, to string) {
			if runtime.GOOS == "windows" {
				t.Skip("no cp on Windows")
			}
			if out, err := exec.Command("cp", "-R", from, to).CombinedOutput(); err != nil {
				t.Fatalf("cp -R: %v %s", err, out)
			}
		},
		"microsecond times": func(t *testing.T, from, to string) {
			copyTreeWithTimes(t, from, to, func(at time.Time) time.Time { return at.Truncate(time.Microsecond) })
		},
	}
	for name, copyTree := range copies {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			stateDir := filepath.Join(root, "state")
			engine := writeBackendTestActor(t, stateDir, "restored-chat")
			// The JSON times must carry nanoseconds for a microsecond copy to move them.
			jsonPath := providerChatStatePath(stateDir, "restored-chat")
			withNanos := time.Unix(1_700_000_000, 123_456_789)
			if err := os.Chtimes(jsonPath, withNanos, withNanos); err != nil {
				t.Fatal(err)
			}
			backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
			if err != nil {
				t.Fatal(err)
			}
			if err := backend.close(); err != nil {
				t.Fatal(err)
			}
			snapshot := filepath.Join(root, "snapshot")
			copyTree(t, stateDir, snapshot)
			before, _ := os.Stat(jsonPath)
			// The restore: the live directory moves aside, the copy takes its place.
			if err := os.Rename(stateDir, filepath.Join(root, "failed")); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(snapshot, stateDir); err != nil {
				t.Fatal(err)
			}
			if after, _ := os.Stat(jsonPath); after.ModTime().Equal(before.ModTime()) {
				t.Logf("this copy kept the exact file time")
			}
			backend, err = openChatActorBackend(stateDir, true, "test", t.Logf)
			if err != nil {
				t.Fatalf("restored copy refused: %v", err)
			}
			heads, err := backend.discover()
			if err != nil || len(heads) != 1 || heads[0].State.ChatID != "restored-chat" {
				t.Fatalf("restored discovery = %#v %v", heads, err)
			}
			if err := backend.close(); err != nil {
				t.Fatal(err)
			}

			// A JSON-only build writes the restored JSON: refused.
			restored, err := chat.NewDurableEngine("restored-chat", chat.FileStore{Path: jsonPath})
			if err != nil {
				t.Fatal(err)
			}
			next := engine.Snapshot().Presentation.Clone()
			next.Draft = "written after the restore"
			if err := restored.Apply(chat.SavePresentation{OperationID: "late-save", Digest: "late", Presentation: next}); err != nil {
				t.Fatal(err)
			}
			if _, err := openChatActorBackend(stateDir, true, "test", t.Logf); !errors.Is(err, chat.ErrFileActorsChangedAfterCutover) {
				t.Fatalf("JSON written after the restore: %v", err)
			}
		})
	}
}

// copyTreeWithTimes copies a directory tree, giving every file the time
// adjust returns for its own.
func copyTreeWithTimes(t *testing.T, from, to string, adjust func(time.Time) time.Time) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, raw, info.Mode().Perm()); err != nil {
			return err
		}
		at := adjust(info.ModTime())
		return os.Chtimes(target, at, at)
	})
	if err != nil {
		t.Fatal(err)
	}
}
