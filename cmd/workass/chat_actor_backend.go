package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// parseSQLiteStores reads the per-store SQLite flag. The default is empty:
// every store stays on its JSON files unless explicitly enabled. The flag
// wins over the environment variable.
func parseSQLiteStores(flagValue string, flagSet bool, env func(string) string) (map[string]bool, error) {
	raw := flagValue
	if !flagSet {
		raw = env(sqliteStoresEnv)
	}
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
