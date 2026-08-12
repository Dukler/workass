package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const sessionAssistantImageFixture = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z5m8AAAAASUVORK5CYII="

// TestLegacyImporterMigratesNaturalAssistantImageMarkdown exercises the only
// remaining session-store image responsibility: converting pre-cutover
// assistant Markdown into typed image metadata before actor migration. It
// writes the old snapshot directly because runtime Save/PrepareTurn/event
// mutation is retired after the actor cutover.
func TestLegacyImporterMigratesNaturalAssistantImageMarkdown(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(workspace, "preview.png")
	imageBytes, err := base64.StdEncoding.DecodeString(sessionAssistantImageFixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, imageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, sessionStateFilename)
	snapshot := map[string]any{
		"v": 1, "activeId": "image-tab", "seq": 1, "workspaces": []any{},
		"chats": []any{map[string]any{
			"id": "image-tab", "chatId": "image-chat", "cwd": workspace,
			"messages": []any{map[string]any{
				"id": "assistant-image", "role": "assistant", "content": "",
				"result": "[Open preview](" + imagePath + ")\n![Preview](" + imagePath + ")",
				"status": "done", "events": []any{},
			}},
		}},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := newSessionStore(statePath)
	assistant := sessionAssistant(t, store.Get().(map[string]any), "image-tab")
	images := anySlice(assistant["images"])
	if len(images) != 1 {
		t.Fatalf("migrated assistant images = %#v, want 1", images)
	}
	image := mapFromAnyMain(images[0])
	if image["mimeType"] != "image/png" || image["data"] != sessionAssistantImageFixture || image["source"] != imagePath {
		t.Fatalf("migrated assistant image = %#v", image)
	}

	// The old agent-read wire helper is not part of the post-cutover runtime.
	// Verify the importer’s durable output directly: it is ref-native on disk,
	// while the in-memory migration view still exposes the image for projection.
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) == "" || string(persisted) == string(raw) {
		t.Fatal("legacy image migration did not rewrite the canonical snapshot")
	}
	var normalized map[string]any
	if err := json.Unmarshal(persisted, &normalized); err != nil {
		t.Fatal(err)
	}
	normalizedAssistant := sessionAssistant(t, normalized, "image-tab")
	normalizedImages := anySlice(normalizedAssistant["images"])
	if len(normalizedImages) != 1 || fieldString(mapFromAnyMain(normalizedImages[0]), sessionImageDataRefField) == "" {
		t.Fatalf("legacy image was not persisted as a content reference: %#v", normalizedImages)
	}
}
