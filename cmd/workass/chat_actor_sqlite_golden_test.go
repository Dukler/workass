package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"workass/internal/chat"
	"workass/internal/store"
)

// TestRealCopiedActorsProjectIdenticallyFromSQLite is the opt-in golden gate
// over real actor shapes. Point WORKASS_SQLITE_GOLDEN_DIR at a disposable,
// read-only copy of provider-chats files (never the live profile). The copy
// is only read; the database lives in a test temp dir. For every actor the
// wire projection (full history, as session:get/archive build it) from the
// file store and from SQLite must be byte-identical.
func TestRealCopiedActorsProjectIdenticallyFromSQLite(t *testing.T) {
	actorDir := strings.TrimSpace(os.Getenv("WORKASS_SQLITE_GOLDEN_DIR"))
	if actorDir == "" {
		t.Skip("set WORKASS_SQLITE_GOLDEN_DIR to a disposable copy of provider-chats files")
	}
	actorDir, err := filepath.Abs(actorDir)
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Workass")
	if actorDir == live || strings.HasPrefix(actorDir, live+string(filepath.Separator)) {
		t.Fatalf("golden gate refuses the live profile path %q", actorDir)
	}
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), store.FileName), BinaryVersion: "golden"})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	peak := uint64(0)
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var stats runtime.MemStats
				runtime.ReadMemStats(&stats)
				if stats.HeapInuse > peak {
					peak = stats.HeapInuse
				}
			}
		}
	}()
	started := time.Now()
	result, err := chat.ImportFileActors(context.Background(), db, chat.ImportOptions{StateDir: filepath.Dir(actorDir), ActorDir: actorDir})
	elapsed := time.Since(started)
	close(stop)
	<-sampled
	if err != nil {
		t.Fatalf("import copied actors: %v", err)
	}
	var largest int64
	entries, _ := os.ReadDir(actorDir)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && filepath.Ext(entry.Name()) == ".json" && info.Size() > largest {
			largest = info.Size()
		}
	}
	t.Logf("import receipt: %d imported, %d skipped in %s; largest source %d MiB; peak heap in use %d MiB over a %d MiB baseline",
		result.Imported, result.Skipped, elapsed.Round(time.Millisecond), largest>>20, peak>>20, before.HeapInuse>>20)

	actors := chat.NewSQLActors(db)
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(actorDir, entry.Name())
		var header struct {
			State struct{ ChatID string } `json:"state"`
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		raw = nil
		chatID := header.State.ChatID
		fileState, found, err := (chat.FileStore{Path: path}).Load(chatID)
		if err != nil || !found {
			t.Fatalf("%s: file load: found=%v err=%v", entry.Name(), found, err)
		}
		sqlState, found, err := actors.Store(chatID).Load(chatID)
		if err != nil || !found {
			t.Fatalf("%s: SQLite load: found=%v err=%v", entry.Name(), found, err)
		}
		fileProjection, sqlProjection := map[string]any{}, map[string]any{}
		fileErr := projectActorChat(fileProjection, fileState)
		sqlErr := projectActorChat(sqlProjection, sqlState)
		if (fileErr == nil) != (sqlErr == nil) || (fileErr != nil && fileErr.Error() != sqlErr.Error()) {
			t.Fatalf("%s: projection errors differ: file=%v sqlite=%v", entry.Name(), fileErr, sqlErr)
		}
		fileBytes, err := json.Marshal(fileProjection)
		if err != nil {
			t.Fatal(err)
		}
		sqlBytes, err := json.Marshal(sqlProjection)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(fileBytes, sqlBytes) {
			t.Fatalf("%s (%s): wire projection differs between stores", entry.Name(), chatID)
		}
		checked++
	}
	t.Logf("golden receipt: %d actors project byte-identically from SQLite", checked)

	// For scale: the peak of opening the largest actor from each store.
	var largestName, largestChat string
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Size() == largest {
			largestName = entry.Name()
		}
	}
	if heads, err := actors.Discover(); err == nil {
		for _, head := range heads {
			if providerChatStatePathBase(head.State.ChatID) == largestName {
				largestChat = head.State.ChatID
			}
		}
	}
	if largestChat == "" {
		return
	}
	measure := func(label string, load func() error) {
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		peak := uint64(0)
		done := make(chan struct{})
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					var stats runtime.MemStats
					runtime.ReadMemStats(&stats)
					if stats.HeapInuse > peak {
						peak = stats.HeapInuse
					}
				}
			}
		}()
		started := time.Now()
		err := load()
		elapsed := time.Since(started)
		close(done)
		<-finished
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		t.Logf("open receipt: %s of the largest actor took %s, peak heap in use %d MiB", label, elapsed.Round(time.Millisecond), peak>>20)
	}
	measure("FileStore.Load", func() error {
		_, _, err := (chat.FileStore{Path: filepath.Join(actorDir, largestName)}).Load(largestChat)
		return err
	})
	measure("SQLStore.Load", func() error {
		_, _, err := actors.Store(largestChat).Load(largestChat)
		return err
	})

	// A message-sized commit on the largest actor (the shape of a terminal
	// receipt or any ordinary mutation): one appended row.
	sqlStore := actors.Store(largestChat)
	state, _, err := sqlStore.Load(largestChat)
	if err != nil {
		t.Fatal(err)
	}
	next := state
	next.Revision++
	row := state.Ledger[len(state.Ledger)-1]
	row.EventID, row.MessageID = row.EventID+":golden-append", row.MessageID+":golden-append"
	row.Sequence = uint64(len(state.Ledger) + 1)
	row.Text, row.Result, row.Timeline, row.Attachments = "appended", "", nil, nil
	next.Ledger = append(state.Ledger[:len(state.Ledger):len(state.Ledger)], row)
	if err := next.Validate(); err != nil {
		t.Logf("append receipt skipped: synthetic row is not a valid actor transition: %v", err)
		return
	}
	measure("SQLStore.Save (append one row)", func() error { return sqlStore.Save(next) })
	measure("FileStore.Save (same state)", func() error {
		return (chat.FileStore{Path: filepath.Join(t.TempDir(), largestName)}).Save(next)
	})
}

func providerChatStatePathBase(chatID string) string {
	return filepath.Base(providerChatStatePath("", chatID))
}
