package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"workass/internal/chat"
	"workass/internal/store"
)

// chatActorBackend is where durable chat actors live. The runtime touches
// actor storage only through it: boot discovery and one StateStore per actor.
type chatActorBackend interface {
	discover() ([]chat.ActorHead, error)
	store(chatID string) chat.StateStore
	close() error
	description() string
}

// fileActorBackend is the provider-chats/<sha256>.json store (the default).
type fileActorBackend struct{ stateDir string }

func (b fileActorBackend) discover() ([]chat.ActorHead, error) {
	states, err := chat.DiscoverFileStates(filepath.Join(b.stateDir, "provider-chats"))
	if err != nil {
		return nil, err
	}
	heads := make([]chat.ActorHead, 0, len(states))
	for _, state := range states {
		heads = append(heads, chat.ActorHead{State: state, LedgerLength: len(state.Ledger)})
	}
	return heads, nil
}

func (b fileActorBackend) store(chatID string) chat.StateStore {
	return chat.FileStore{Path: providerChatStatePath(b.stateDir, chatID)}
}

func (fileActorBackend) close() error        { return nil }
func (fileActorBackend) description() string { return "provider-chats JSON" }

// sqlActorBackend keeps actors in state/state.db.
type sqlActorBackend struct{ actors *chat.SQLActors }

func (b sqlActorBackend) discover() ([]chat.ActorHead, error) { return b.actors.Discover() }
func (b sqlActorBackend) store(chatID string) chat.StateStore {
	return b.actors.Store(chatID)
}
func (b sqlActorBackend) close() error { return b.actors.DB().Close() }
func (b sqlActorBackend) description() string {
	return "SQLite " + b.actors.DB().Path()
}

// sqliteStoreChats is the only store the --sqlite-stores flag knows today.
const sqliteStoreChats = "chats"

const sqliteStoresEnv = "WORKASS_SQLITE_STORES"

// sqliteStoresUsage is the --sqlite-stores help text; it states
// sqliteStoresProfileDefault.
const sqliteStoresUsage = "comma-separated stores kept in state/state.db instead of JSON files (known: " + sqliteStoreChats + "); also " + sqliteStoresEnv +
	"; when neither is given: " + sqliteStoreChats + " in the dev profile and in prod on macOS, none in Windows prod or the test profile; an explicit value, even an empty one, wins"

// sqliteStoresProfileDefault is what a profile runs with when neither the flag
// nor the environment names the SQLite stores: the dev profile keeps chats in
// SQLite everywhere, prod keeps them in SQLite on macOS only (Windows prod
// stays on JSON until it is measured there), and the test profile keeps every
// store on JSON.
//
// profile is workassRuntimeProfile(): WORKASS_PROFILE, set by
// config/environments/*.env, where anything but "dev" or "test" is "prod".
// --prod and WORKASS_PROD do not select it; they only change the Windows
// port and bind defaults.
func sqliteStoresProfileDefault(profile string) string {
	switch {
	case profile == "dev":
		return sqliteStoreChats
	case profile == "prod" && sqliteStoresGOOS == "darwin":
		return sqliteStoreChats
	}
	return ""
}

// sqliteStoresGOOS is runtime.GOOS; tests replace it to pin the platform rule.
var sqliteStoresGOOS = runtime.GOOS

// parseSQLiteStores resolves which stores live in state/state.db. The first
// source that is present wins, and an explicitly empty value is present (it
// turns SQLite off, even in the dev profile):
//
//  1. --sqlite-stores
//  2. WORKASS_SQLITE_STORES
//  3. the profile default (sqliteStoresProfileDefault)
//
// source names the winner, for the startup log.
func parseSQLiteStores(flagValue string, flagSet bool, lookupEnv func(string) (string, bool), profile string) (stores map[string]bool, source string, err error) {
	raw, source := flagValue, "--sqlite-stores"
	if !flagSet {
		if value, ok := lookupEnv(sqliteStoresEnv); ok {
			raw, source = value, sqliteStoresEnv
		} else {
			raw, source = sqliteStoresProfileDefault(profile), profile+" profile default"
		}
	}
	stores, err = parseSQLiteStoreList(raw)
	return stores, source, err
}

func parseSQLiteStoreList(raw string) (map[string]bool, error) {
	stores := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name == "" {
			continue
		}
		if name != sqliteStoreChats {
			return nil, fmt.Errorf("unknown SQLite store %q (known: %s)", name, sqliteStoreChats)
		}
		stores[name] = true
	}
	return stores, nil
}

// openChatActorBackend selects where chat actors live for this daemon run.
//
// Enabled: open state/state.db, import provider-chats once (resumable,
// receipted, JSON left in place), then verify no JSON was written after the
// cutover. Disabled: the JSON files are authoritative, unless a previous run
// already moved the actors into SQLite. Then the JSON is a stale backup and
// starting from it would silently lose everything written since, so startup
// fails closed instead.
func openChatActorBackend(stateDir string, enabled bool, binaryVersion string, logf func(string, ...any)) (chatActorBackend, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dbPath := filepath.Join(stateDir, store.FileName)
	if !enabled {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			return fileActorBackend{stateDir: stateDir}, nil
		} else if err != nil {
			return nil, err
		}
		db, err := store.Open(store.Options{Path: dbPath, BinaryVersion: binaryVersion})
		if err != nil {
			return nil, fmt.Errorf("inspect %s for migrated chat actors: %w", dbPath, err)
		}
		status, live, err := chatStoreStatus(db)
		closeErr := db.Close()
		if err != nil {
			return nil, errors.Join(err, closeErr)
		}
		if live {
			return nil, fmt.Errorf("chat actors moved to %s at %s; the provider-chats JSON is a stale backup. Start with --sqlite-stores=%s (or %s=%s), or restore a pre-migration state directory",
				dbPath, status.CompletedAt, sqliteStoreChats, sqliteStoresEnv, sqliteStoreChats)
		}
		return fileActorBackend{stateDir: stateDir}, closeErr
	}

	db, err := store.Open(store.Options{
		Path: dbPath, BinaryVersion: binaryVersion, BackupDir: filepath.Join(stateDir, "backups"),
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	options := chat.ImportOptions{
		StateDir: stateDir,
		ExpectedFileName: func(chatID string) string {
			return filepath.Base(providerChatStatePath(stateDir, chatID))
		},
		Logf: logf,
	}
	started := time.Now()
	result, err := chat.ImportFileActors(context.Background(), db, options)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("import chat actors into %s: %w", dbPath, err), db.Close())
	}
	if result.Imported+result.Skipped+result.Removed > 0 {
		logf("[store] chat actors live in SQLite: %d imported, %d unchanged, %d skipped, %d removed in %s",
			result.Imported, result.Unchanged, result.Skipped, result.Removed, time.Since(started).Round(time.Millisecond))
	}
	if err := chat.VerifyLiveFileActors(context.Background(), db, options); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return sqlActorBackend{actors: chat.NewSQLActors(db)}, nil
}

func chatStoreStatus(db *store.DB) (store.MigratedStore, bool, error) {
	var status store.MigratedStore
	var ok bool
	err := db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		status, ok, err = tx.MigratedStoreStatus(chat.ActorStoreName)
		return err
	})
	return status, ok && status.Status == store.StoreLive, err
}

// sortedSQLiteStores renders the enabled set for logs.
func sortedSQLiteStores(stores map[string]bool) string {
	names := make([]string, 0, len(stores))
	for name := range stores {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
