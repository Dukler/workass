package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendChatArchiveCachesDedupeIndexAndRefreshesAfterExternalChanges(t *testing.T) {
	stateDir := t.TempDir()
	tabID := "cached-archive-tab"
	path := chatArchivePath(stateDir, tabID)
	message := func(id, content string) map[string]any {
		return map[string]any{
			"id": id, "role": "user", "content": content,
			"status": "done", "at": "2026-07-25T00:00:00Z",
		}
	}

	if err := appendChatArchive(stateDir, tabID, []any{message("id-1", "one")}); err != nil {
		t.Fatal(err)
	}
	index := archiveIndexForPath(path)
	index.mu.Lock()
	firstRebuilds := index.rebuilds
	index.mu.Unlock()
	if firstRebuilds != 1 {
		t.Fatalf("first append rebuilt archive index %d times, want 1", firstRebuilds)
	}
	if err := appendChatArchive(stateDir, tabID, []any{
		message("id-1", "duplicate id"), message("id-2", "two"),
	}); err != nil {
		t.Fatal(err)
	}
	index.mu.Lock()
	secondRebuilds := index.rebuilds
	index.mu.Unlock()
	if secondRebuilds != firstRebuilds {
		t.Fatalf("warm append reparsed the complete archive: rebuilds %d -> %d", firstRebuilds, secondRebuilds)
	}
	if records := loadChatArchiveRecords(stateDir, tabID); len(records) != 2 {
		t.Fatalf("cached append changed id dedupe semantics: %#v", records)
	}

	// An external append changes the file fingerprint. The next Workass append
	// must rebuild once and dedupe against the externally added id.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(message("id-3", "external")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := appendChatArchive(stateDir, tabID, []any{message("id-3", "must dedupe")}); err != nil {
		t.Fatal(err)
	}
	index.mu.Lock()
	externalRebuilds := index.rebuilds
	index.mu.Unlock()
	if externalRebuilds != secondRebuilds+1 {
		t.Fatalf("external append did not invalidate cached index: rebuilds=%d", externalRebuilds)
	}
	if records := loadChatArchiveRecords(stateDir, tabID); len(records) != 3 {
		t.Fatalf("external id was duplicated: %#v", records)
	}

	// Truncation must discard stale ids from the cache, allowing an id that no
	// longer exists on disk to be appended again.
	truncated, err := json.Marshal(message("id-4", "replacement"))
	if err != nil {
		t.Fatal(err)
	}
	truncated = append(truncated, '\n')
	if err := os.WriteFile(path, truncated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendChatArchive(stateDir, tabID, []any{message("id-1", "one after truncate")}); err != nil {
		t.Fatal(err)
	}
	records := loadChatArchiveRecords(stateDir, tabID)
	if len(records) != 2 || fieldString(records[0], "id") != "id-4" || fieldString(records[1], "id") != "id-1" {
		t.Fatalf("truncated archive reused stale dedupe state: %#v", records)
	}
}

func TestArchiveAppendCostProfile(t *testing.T) {
	stateDir := os.Getenv("WORKASS_ARCHIVE_COST_STATE_DIR")
	if stateDir == "" {
		t.Skip("set WORKASS_ARCHIVE_COST_STATE_DIR to a copied state directory")
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "chat-archive"))
	if err != nil {
		t.Fatal(err)
	}
	var largest os.DirEntry
	var largestSize int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > largestSize {
			largest, largestSize = entry, info.Size()
		}
	}
	if largest == nil {
		t.Skip("copied state has no chat archive")
	}
	tabID := strings.TrimSuffix(largest.Name(), ".jsonl")
	appendOne := func(suffix string) time.Duration {
		start := time.Now()
		err := appendChatArchive(stateDir, tabID, []any{map[string]any{
			"id":   fmt.Sprintf("archive-cost-%d-%s", time.Now().UnixNano(), suffix),
			"role": "assistant", "content": "archive cache cost probe",
			"status": "done", "at": time.Now().UTC().Format(time.RFC3339Nano),
		}})
		if err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	cold := appendOne("cold")
	warm := appendOne("warm")
	index := archiveIndexForPath(chatArchivePath(stateDir, tabID))
	index.mu.Lock()
	rebuilds := index.rebuilds
	index.mu.Unlock()
	t.Logf("largest archive bytes=%d coldAppend=%s warmAppend=%s indexRebuilds=%d",
		largestSize, cold.Round(time.Millisecond), warm.Round(time.Microsecond), rebuilds)
	if rebuilds != 1 {
		t.Fatalf("warm append rebuilt archive index: rebuilds=%d", rebuilds)
	}
	if warm >= 50*time.Millisecond {
		t.Fatalf("warm archive append=%s, want <50ms", warm)
	}
}
