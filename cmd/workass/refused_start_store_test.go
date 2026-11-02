package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/store"
)

// A start with SQLite off only asks state.db whether the chats moved there. Asking
// must change nothing: a build that opens it to migrate has a backup step first, and
// this path had none, so the next daemon inherited a schema no older build can read.
func TestAskingStateDBWhereTheChatsAreChangesNothing(t *testing.T) {
	migrations, err := store.EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	older := migrations[:len(migrations)-1]
	seed := func(t *testing.T, list []store.Migration, live bool) (string, *store.DB) {
		t.Helper()
		dir := t.TempDir()
		db, err := store.Open(store.Options{Path: filepath.Join(dir, store.FileName), BinaryVersion: "test", Migrations: list})
		if err != nil {
			t.Fatal(err)
		}
		if live {
			if err := db.Write(context.Background(), func(tx *store.Tx) error {
				if err := tx.BeginStoreImport(chat.ActorStoreName, "provider-chats", "test"); err != nil {
					return err
				}
				return tx.CompleteStoreImport(chat.ActorStoreName, 1)
			}); err != nil {
				t.Fatal(err)
			}
		}
		return dir, db
	}
	closed := func(t *testing.T, list []store.Migration, live bool) string {
		t.Helper()
		dir, db := seed(t, list, live)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("an older schema that says the chats moved", func(t *testing.T) {
		dir := closed(t, older, true)
		before := stateDirBytes(t, dir)
		_, err := openChatActorBackend(dir, false, "test", t.Logf)
		if err == nil || !strings.Contains(err.Error(), "chat actors moved to") {
			t.Fatalf("the start was not refused: %v", err)
		}
		assertStateDirUnchanged(t, dir, before)
	})

	t.Run("an older schema that says nothing moved", func(t *testing.T) {
		dir := closed(t, older, false)
		before := stateDirBytes(t, dir)
		if _, err := openChatActorBackend(dir, false, "test", t.Logf); err != nil {
			t.Fatal(err)
		}
		assertStateDirUnchanged(t, dir, before)
	})

	t.Run("a crash left the answer in the WAL", func(t *testing.T) {
		source, db := seed(t, migrations, true)
		defer db.Close()
		dir := t.TempDir()
		for _, name := range []string{store.FileName, store.FileName + "-wal"} {
			data, err := os.ReadFile(filepath.Join(source, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		before := stateDirBytes(t, dir)
		_, err := openChatActorBackend(dir, false, "test", t.Logf)
		if err == nil || !strings.Contains(err.Error(), "chat actors moved to") {
			t.Fatalf("the marker in the WAL was not read: %v", err)
		}
		assertStateDirUnchanged(t, dir, before)
	})

	t.Run("a live daemon holds the file", func(t *testing.T) {
		dir, db := seed(t, migrations, true)
		defer db.Close()
		done := make(chan error, 1)
		go func() {
			_, err := openChatActorBackend(dir, false, "test", t.Logf)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a second daemon started over a live one")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the second daemon is waiting on the first")
		}
	})

	t.Run("a schema from a newer build", func(t *testing.T) {
		dir := closed(t, migrations, false)
		db, err := store.Open(store.Options{Path: filepath.Join(dir, store.FileName), BinaryVersion: "test", Migrations: append(append([]store.Migration(nil), migrations...), store.Migration{
			Version: len(migrations) + 1, Name: "future", SQL: "CREATE TABLE future(x)",
		})})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := stateDirBytes(t, dir)
		if _, err := openChatActorBackend(dir, false, "test", t.Logf); !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("a newer schema was not refused: %v", err)
		}
		assertStateDirUnchanged(t, dir, before)
	})
}

func stateDirBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertStateDirUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := stateDirBytes(t, dir)
	for path, data := range after {
		previous, existed := before[path]
		if !existed {
			t.Errorf("created %s", path)
		} else if previous != data {
			t.Errorf("changed %s", path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			t.Errorf("removed %s", path)
		}
	}
	if t.Failed() {
		t.Fatal(fmt.Sprintf("state dir %s was changed by a start that only asked", dir))
	}
}
