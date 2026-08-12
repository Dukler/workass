package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func refNativeImage(data string) map[string]any {
	return map[string]any{
		"mimeType": "image/png",
		"name":     "fixture.png",
		"data":     data,
	}
}

func writeLegacySnapshotForImageTest(t *testing.T, path string, snapshot map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestLegacyImporterExternalizesInlineImages verifies the one-time migration
// boundary without exercising the retired runtime session Save API. The old
// snapshot is input; the canonical store is rewritten to content references,
// and only the in-memory projection rehydrates the bytes.
func TestLegacyImporterExternalizesInlineImages(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("legacy-ref-tab", "legacy-ref-chat", "show image")
	assistant := sessionAssistant(t, snapshot, "legacy-ref-tab")
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("legacy-inline", 1024)))
	image := refNativeImage(data)
	assistant["images"] = []any{cloneJSON(image)}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "tool-image",
		"images": []any{cloneJSON(image)},
	}}
	original := writeLegacySnapshotForImageTest(t, statePath, snapshot)

	store := newSessionStore(statePath)
	if err := store.LoadError(); err != nil {
		t.Fatalf("legacy image import: %v", err)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(persisted)) {
		t.Fatal("legacy inline snapshot was not rewritten at the migration boundary")
	}
	if bytes.Contains(persisted, []byte(data)) || !bytes.Contains(persisted, []byte(sessionImageDataRefField)) {
		t.Fatalf("canonical migration snapshot is not ref-native: %s", persisted)
	}
	files, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("content-addressed image files = %d, want 1", len(files))
	}

	got := store.Get().(map[string]any)
	gotAssistant := sessionAssistant(t, got, "legacy-ref-tab")
	gotMessageImage := mapFromAnyMain(anySlice(gotAssistant["images"])[0])
	gotEventImages := anySlice(mapFromAnyMain(anySlice(gotAssistant["events"])[0])["images"])
	if fieldString(gotMessageImage, "data") != data || fieldString(mapFromAnyMain(gotEventImages[0]), "data") != data {
		t.Fatalf("legacy image projection lost inline data: message=%#v event=%#v", gotMessageImage, gotEventImages)
	}
}

// TestLegacyImporterDropsDanglingImageRefWithoutBlankingChat keeps the
// recoverable-damage rule at the importer boundary: a missing sidecar removes
// only the unusable attachment, not the rest of the legacy chat.
func TestLegacyImporterDropsDanglingImageRefWithoutBlankingChat(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("dangling-tab", "dangling-chat", "text survives")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "dangling-tab"))[0])
	user["images"] = []any{map[string]any{
		sessionImageDataRefField: sessionImageDirname + "/missing-sidecar",
		"mimeType":               "image/png",
	}}
	writeLegacySnapshotForImageTest(t, statePath, snapshot)

	store := newSessionStore(statePath)
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "session image ref") {
		t.Fatalf("runtime dangling image warning = %v", err)
	}
	got, ok := store.Get().(map[string]any)
	if !ok {
		t.Fatal("missing image sidecar blanked the complete legacy chat")
	}
	gotChat := chatFromSnapshot(got, "dangling-tab")
	if gotChat == nil || len(messageSlice(gotChat)) != 2 || fieldString(mapFromAnyMain(messageSlice(gotChat)[0]), "content") == "" {
		t.Fatalf("legacy chat content was lost during image degradation: %#v", gotChat)
	}
	gotUser := mapFromAnyMain(messageSlice(gotChat)[0])
	for _, raw := range anySlice(gotUser["images"]) {
		image := mapFromAnyMain(raw)
		if fieldString(image, "data") != "" || fieldString(image, sessionImageDataRefField) != "" {
			t.Fatalf("unusable image remained in importer projection: %#v", gotUser["images"])
		}
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err != nil {
		// The importer already rewrote the damaged legacy row without the
		// unusable ref, so a second boot must not keep reporting the same damage.
		t.Fatalf("rewritten legacy snapshot retained image warning: %v", err)
	}
	if got, ok := reloaded.Get().(map[string]any); !ok || chatFromSnapshot(got, "dangling-tab") == nil {
		t.Fatal("missing image sidecar prevented legacy chat import")
	}
}

// TestLegacyImporterDegradesOutOfRangeEventImageRef keeps malformed event
// references local to the owning event. This is migration input validation;
// runtime actor events use typed provider.Attachment values instead.
func TestLegacyImporterDegradesOutOfRangeEventImageRef(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("range-ref-tab", "range-ref-chat", "text survives invalid event image")
	assistant := sessionAssistant(t, snapshot, "range-ref-tab")
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "range-tool",
		"images": []any{map[string]any{sessionMessageImageRefField: 7}},
	}}
	writeLegacySnapshotForImageTest(t, statePath, snapshot)

	store := newSessionStore(statePath)
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "outside its owning message") {
		t.Fatalf("out-of-range image warning = %v", err)
	}
	got, ok := store.Get().(map[string]any)
	if !ok {
		t.Fatal("one out-of-range event image blanked the legacy chat")
	}
	gotEvent := mapFromAnyMain(anySlice(sessionAssistant(t, got, "range-ref-tab")["events"])[0])
	gotImage := mapFromAnyMain(anySlice(gotEvent["images"])[0])
	if fieldString(gotImage, sessionMessageImageRefField) != "" || fieldString(gotImage, "data") != "" {
		t.Fatalf("invalid event image remained live: %#v", gotImage)
	}
	if err := validateSessionEventImageRefs(got); err != nil {
		t.Fatalf("degraded event image failed downstream validation: %v", err)
	}
}
