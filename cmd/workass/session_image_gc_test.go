package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionStoreBootSweepsOnlyOldUnclaimedImageFiles(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("gc-live-tab", "gc-live-chat", "live image")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "gc-live-tab"))[0])
	liveData := base64.StdEncoding.EncodeToString([]byte("live screenshot"))
	user["images"] = []any{refNativeImage(liveData)}
	// The snapshot is migration input. Boot import externalizes the referenced
	// image before the orphan sweep; runtime session Save is retired after the
	// actor cutover and must not be used to seed this fixture.
	writeLegacySnapshotForImageTest(t, statePath, snapshot)
	store := newSessionStore(statePath)
	if err := store.LoadError(); err != nil {
		t.Fatalf("import legacy image snapshot: %v", err)
	}
	imageDir := filepath.Join(stateDir, sessionImageDirname)
	liveName := sessionImageName(liveData)

	writeImageFile := func(name string, age time.Duration) string {
		t.Helper()
		path := filepath.Join(imageDir, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	oldOrphanName := strings.Repeat("a", 64)
	recentOrphanName := strings.Repeat("b", 64)
	tombstoneClaimName := strings.Repeat("c", 64)
	journalClaimName := strings.Repeat("d", 64)
	oldOrphan := writeImageFile(oldOrphanName, 48*time.Hour)
	recentOrphan := writeImageFile(recentOrphanName, time.Hour)
	tombstoneClaim := writeImageFile(tombstoneClaimName, 48*time.Hour)
	journalClaim := writeImageFile(journalClaimName, 48*time.Hour)
	livePath := filepath.Join(imageDir, liveName)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(livePath, old, old); err != nil {
		t.Fatal(err)
	}

	tombstoneDir := filepath.Join(stateDir, droppedChatDirname)
	if err := os.MkdirAll(tombstoneDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tombstone := map[string]any{"images": []any{map[string]any{
		sessionImageDataRefField: sessionImageDirname + "/" + tombstoneClaimName,
	}}}
	tombstoneData, err := json.Marshal(tombstone)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tombstoneDir, "deleted-tab.json"), tombstoneData, 0o600); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(stateDir, sessionJournalDirname, sessionJournalQuarantineDir)
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := []byte(`{"image":"images/` + journalClaimName + `"}` + "\n")
	if err := os.WriteFile(filepath.Join(journalDir, "broken.jsonl"), journal, 0o600); err != nil {
		t.Fatal(err)
	}

	// The archive format remains inline and therefore has no sidecar claim.
	if err := appendChatArchive(stateDir, "gc-archive-tab", []any{map[string]any{
		"id": "gc-archive-message", "role": "user", "content": "inline archive image",
		"status": "done", "images": []any{refNativeImage(liveData)},
	}}); err != nil {
		t.Fatal(err)
	}
	archiveRaw, err := os.ReadFile(chatArchivePath(stateDir, "gc-archive-tab"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(archiveRaw, []byte(sessionImageDataRefField)) {
		t.Fatalf("archive unexpectedly claimed state/images: %s", archiveRaw)
	}

	reloaded := newSessionStore(statePath)
	if got := reloaded.Get(); got == nil {
		t.Fatal("image sweep prevented mirror load")
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Fatalf("old unclaimed image survived boot sweep: %v", err)
	}
	for label, path := range map[string]string{
		"recent orphan": recentOrphan,
		"live mirror":   livePath,
		"tombstone":     tombstoneClaim,
		"journal":       journalClaim,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s image was swept: %v", label, err)
		}
	}
}
