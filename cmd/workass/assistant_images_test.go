package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const sessionAssistantImageFixture = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z5m8AAAAASUVORK5CYII="

func TestSessionStoreExternalizesAndRehydratesSnapshotImages(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("lean-image-tab", "lean-image-chat", "show images")
	chat := chatFromSnapshot(snapshot, "lean-image-tab")
	chat["_archivedCount"] = 7
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	assistant["status"] = "done"
	duplicate := map[string]any{
		"mimeType": "image/png", "data": sessionAssistantImageFixture,
		"name": "same", "source": "same.png",
	}
	uniqueData := base64.StdEncoding.EncodeToString([]byte("event-only-image"))
	unique := map[string]any{
		"mimeType": "image/png", "data": uniqueData,
		"name": "unique", "source": "unique.png",
	}
	assistant["images"] = []any{cloneJSON(duplicate)}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "image-tool",
		"images": []any{cloneJSON(duplicate), cloneJSON(unique)},
	}}
	if !store.Save(snapshot) {
		t.Fatal("save image snapshot")
	}

	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(sessionAssistantImageFixture)) || bytes.Contains(raw, []byte(uniqueData)) {
		t.Fatal("canonical snapshot retained inline image data")
	}
	var persisted map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	persistedAssistant := sessionAssistant(t, persisted, "lean-image-tab")
	persistedMessageImage := mapFromAnyMain(anySlice(persistedAssistant["images"])[0])
	persistedEventImages := anySlice(mapFromAnyMain(anySlice(persistedAssistant["events"])[0])["images"])
	if fieldString(persistedMessageImage, sessionImageDataRefField) == "" ||
		intValue(mapFromAnyMain(persistedEventImages[0])[sessionMessageImageRefField]) != 0 ||
		fieldString(mapFromAnyMain(persistedEventImages[1]), sessionImageDataRefField) == "" {
		t.Fatalf("persisted image refs = message=%#v event=%#v", persistedMessageImage, persistedEventImages)
	}
	files, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("content-addressed image files = %d, want 2", len(files))
	}

	immediate := sessionAssistant(t, store.Get().(map[string]any), "lean-image-tab")
	immediateEventImages := anySlice(mapFromAnyMain(anySlice(immediate["events"])[0])["images"])
	if fieldString(mapFromAnyMain(immediateEventImages[0]), "data") != sessionAssistantImageFixture {
		t.Fatalf("in-memory image contract changed = %#v", immediateEventImages)
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload externalized images: %v", err)
	}
	got := reloaded.Get().(map[string]any)
	gotChat := chatFromSnapshot(got, "lean-image-tab")
	if len(messageSlice(gotChat)) != 2 || intValue(gotChat["_archivedCount"]) != 7 {
		t.Fatalf("snapshot leaning removed rows or archive count: %#v", gotChat)
	}
	gotAssistant := sessionAssistant(t, got, "lean-image-tab")
	gotMessageImage := mapFromAnyMain(anySlice(gotAssistant["images"])[0])
	gotEventImages := anySlice(mapFromAnyMain(anySlice(gotAssistant["events"])[0])["images"])
	if fieldString(gotMessageImage, "data") != sessionAssistantImageFixture ||
		fieldString(mapFromAnyMain(gotEventImages[0]), "data") != sessionAssistantImageFixture ||
		fieldString(mapFromAnyMain(gotEventImages[1]), "data") != uniqueData {
		t.Fatalf("rehydrated session:get image contract = message=%#v event=%#v", gotMessageImage, gotEventImages)
	}
}

func TestSessionStoreMigratesNaturalAssistantImageMarkdown(t *testing.T) {
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

	read, err := store.AgentReadChat("image-tab", "image-chat", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	readAssistant := mapFromAnyMain(anySlice(read["messages"])[0])
	if _, exposed := readAssistant["images"]; exposed {
		t.Fatal("agent chat read exposed assistant image bytes")
	}
	if len(anySlice(readAssistant["attachments"])) != 1 {
		t.Fatalf("agent chat read attachment metadata = %#v", readAssistant)
	}
}

func TestSessionStorePersistsAssistantImagesFromTerminalJob(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("terminal-image-tab", "terminal-image-chat", "show it")) {
		t.Fatal("initial save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "terminal-image-tab", "chatId": "terminal-image-chat", "prompt": "show it"})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "terminal-image-job", "tabId": "terminal-image-tab", "chatId": "terminal-image-chat", "startedAt": "2026-07-20T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "terminal-image-job", "tabId": "terminal-image-tab", "chatId": "terminal-image-chat", "status": "done", "finishedAt": "2026-07-20T10:00:01Z",
		"result": "preview", "images": []any{map[string]any{"mimeType": "image/png", "data": sessionAssistantImageFixture, "name": "Preview", "source": "preview.png"}},
	}})
	assistant := sessionAssistant(t, store.Get().(map[string]any), "terminal-image-tab")
	if got := len(anySlice(assistant["images"])); got != 1 {
		t.Fatalf("terminal assistant images = %d, want 1", got)
	}
}

func TestSessionStorePersistsLiveAssistantMediaBeforeTerminalAndAcrossRecovery(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)
	if !store.Save(sessionMirrorFixture("live-image-tab", "live-image-chat", "show it while working")) {
		t.Fatal("initial save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "live-image-tab", "chatId": "live-image-chat", "prompt": "show it while working"})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "live-image-job", "tabId": "live-image-tab", "chatId": "live-image-chat", "startedAt": "2026-07-23T01:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{
		"type": "assistant-media", "id": "live-image-job",
		"images": []any{map[string]any{
			"mimeType": "image/png", "data": sessionAssistantImageFixture,
			"name": "Recording started", "source": "recording-started.png",
		}},
	})

	assistant := sessionAssistant(t, store.Get().(map[string]any), "live-image-tab")
	if fieldString(assistant, "status") != "running" || len(anySlice(assistant["images"])) != 1 {
		t.Fatalf("live assistant media was not visible before terminal: %#v", assistant)
	}
	store.flushScheduledWrite()

	recovered := newSessionStore(statePath)
	recoveredAssistant := sessionAssistant(t, recovered.Get().(map[string]any), "live-image-tab")
	if len(anySlice(recoveredAssistant["images"])) != 1 {
		t.Fatalf("live assistant media did not survive journal recovery: %#v", recoveredAssistant)
	}
}
