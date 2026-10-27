package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The raw-store code is portable, so its tests run it on every platform: the
// paths a platform turns on later are proven there already. Production uses
// sessionImageRawStoreSupported (macOS only); the tests below prove what a
// platform with the store off does, by turning it off themselves.
func init() { sessionImageRawStore = true }

func withRawStore(t testing.TB, on bool) {
	t.Helper()
	previous := sessionImageRawStore
	sessionImageRawStore = on
	t.Cleanup(func() { sessionImageRawStore = previous })
}

// With the raw store off (Windows and Linux today) a new image is written
// exactly as 0.1.229 writes it, and the maintenance pass touches nothing:
// no migration, no reclaim, no deleted-chat collection.
func TestSessionImageRawStoreOffKeepsTheLegacyStore(t *testing.T) {
	withRawStore(t, false)
	stateDir := t.TempDir()
	sessions := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	raw := testImageBytes(t, 40)
	text := base64.StdEncoding.EncodeToString(raw)
	source := filepath.Join(t.TempDir(), "viewed.png")
	if err := os.WriteFile(source, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	attachments, err := sessions.PersistProviderAttachments([]any{map[string]any{
		"mimeType": "image/png", "data": text, sessionImageSourceField: source,
	}})
	if err != nil {
		t.Fatal(err)
	}
	name := attachments[0].Digest
	entries, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Fatalf("the store must hold exactly the legacy file, got %v", entries)
	}
	stored, err := os.ReadFile(sessionImageLegacyPath(stateDir, name))
	if err != nil || string(stored) != text {
		t.Fatalf("legacy file is not the base64 text: %v", err)
	}
	if info, err := os.Stat(sessionImageLegacyPath(stateDir, name)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("legacy file mode %v %v", info.Mode(), err)
	}

	// An eligible deleted chat that alone references a second image: with
	// the store off the pass leaves both images, and the store, untouched.
	other := writeLegacyTestImage(t, stateDir, testImageBytes(t, 41))
	refs := &fakeImageRefs{rows: map[string][][]byte{"gone": {userRow(other)}}, list: []imageRefChat{
		{chatID: "gone", deleted: true, updatedAt: time.Now().Add(-30 * 24 * time.Hour)},
	}}
	m := newImageStoreMaintenance(stateDir, "test", refs, t.Logf)
	m.window, m.grace, m.startDelay = 0, 0, time.Hour
	m.applyMarker()
	if !sessionImageLegacyShadow.Load() {
		t.Fatal("with the store off the legacy file is the only form")
	}
	if report, err := m.runOnce(t.Context()); err != nil || report.Phase != "off" {
		t.Fatalf("pass ran: %+v %v", report, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	m.run(ctx) // returns at once instead of waiting out its start delay
	if time.Since(started) > time.Second {
		t.Fatal("the maintenance loop started with the store off")
	}
	entries, _ = os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if len(entries) != 2 {
		t.Fatalf("the pass changed the store: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(stateDir, imageStoreDirname)); !os.IsNotExist(err) {
		t.Fatalf("the pass wrote its state: %v", err)
	}
	if metrics := m.metrics(); metrics["rawStore"] != false {
		t.Fatalf("metrics %v", metrics)
	}
}
