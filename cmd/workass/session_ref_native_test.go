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

func refNativeImage(data string) map[string]any {
	return map[string]any{
		"mimeType": "image/png",
		"name":     "fixture.png",
		"data":     data,
	}
}

func decodeSessionSnapshotForTest(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return out
}

func TestSessionStoreRefNativeMirrorMatchesLegacyWireHydration(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("ref-tab", "ref-chat", "show image")
	assistant := sessionAssistant(t, snapshot, "ref-tab")
	assistant["status"] = "done"
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("wire-image", 4096)))
	image := refNativeImage(data)
	assistant["images"] = []any{cloneJSON(image)}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "tool-image",
		"images": []any{cloneJSON(image)},
	}}
	if !store.Save(snapshot) {
		t.Fatal("save ref-native snapshot")
	}

	store.mu.Lock()
	internal, err := json.Marshal(store.snapshot)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(internal, []byte(data)) || !bytes.Contains(internal, []byte(sessionImageDataRefField)) {
		t.Fatalf("live mirror is not ref-native: %s", internal)
	}

	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	legacy := decodeSessionSnapshotForTest(t, persisted)
	if err := rehydrateExternalSessionImages(legacy, stateDir); err != nil {
		t.Fatalf("legacy external hydration: %v", err)
	}
	if err := rehydrateSessionEventImageRefs(legacy); err != nil {
		t.Fatalf("legacy event hydration: %v", err)
	}
	got := store.Get()
	wantJSON, _ := json.Marshal(legacy)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Fatalf("session:get differs from the pre-change hydration path\nwant=%s\ngot=%s", wantJSON, gotJSON)
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloaded.mu.Lock()
	reloadedInternal, err := json.Marshal(reloaded.snapshot)
	reloaded.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(reloadedInternal, []byte(data)) {
		t.Fatal("load rehydrated image bytes back into the live mirror")
	}
	reloadedAssistant := sessionAssistant(t, reloaded.Get().(map[string]any), "ref-tab")
	if fieldString(mapFromAnyMain(anySlice(reloadedAssistant["images"])[0]), "data") != data {
		t.Fatal("reload/session:get lost inline wire image data")
	}
}

func TestSessionStoreDegradesDanglingImageRefWithoutBlankingMirror(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("dangling-tab", "dangling-chat", "image")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "dangling-tab"))[0])
	data := base64.StdEncoding.EncodeToString([]byte("must remain durable"))
	user["images"] = []any{refNativeImage(data)}
	if !store.Save(snapshot) {
		t.Fatal("save")
	}
	files, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil || len(files) != 1 {
		t.Fatalf("image files=%d err=%v", len(files), err)
	}
	if err := os.Remove(filepath.Join(stateDir, sessionImageDirname, files[0].Name())); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		got, ok := store.Get().(map[string]any)
		if !ok {
			t.Fatalf("session:get attempt %d blanked the complete mirror", attempt)
		}
		gotUser := mapFromAnyMain(messageSlice(chatFromSnapshot(got, "dangling-tab"))[0])
		gotImage := mapFromAnyMain(anySlice(gotUser["images"])[0])
		if fieldString(gotImage, "data") != "" || fieldString(gotImage, sessionImageDataRefField) != "" {
			t.Fatalf("broken image remained usable after degradation: %#v", gotImage)
		}
		if err := validateSessionEventImageRefs(got); err != nil {
			t.Fatalf("degraded wire mirror failed downstream validation: %v", err)
		}
	}
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "session image ref") {
		t.Fatalf("runtime dangling image warning = %v", err)
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err == nil || !strings.Contains(err.Error(), "session image ref") {
		t.Fatalf("boot dangling image warning = %v", err)
	}
	if got, ok := reloaded.Get().(map[string]any); !ok || chatFromSnapshot(got, "dangling-tab") == nil {
		t.Fatal("missing image sidecar prevented the daemon mirror from loading")
	}
}

func TestSessionArchiveKeepsTurnTextWhenOneImageRefIsUnreadable(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("archive-broken-ref-tab", "archive-broken-ref-chat", "text survives")
	chat := chatFromSnapshot(snapshot, "archive-broken-ref-tab")
	user := mapFromAnyMain(messageSlice(chat)[0])
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	user["images"] = []any{refNativeImage(base64.StdEncoding.EncodeToString([]byte("lost thumbnail")))}
	assistant["content"] = "assistant text survives"
	assistant["status"] = "done"
	if !store.Save(snapshot) {
		t.Fatal("save")
	}
	files, err := os.ReadDir(filepath.Join(stateDir, sessionImageDirname))
	if err != nil || len(files) != 1 {
		t.Fatalf("image files=%d err=%v", len(files), err)
	}
	if err := os.Remove(filepath.Join(stateDir, sessionImageDirname, files[0].Name())); err != nil {
		t.Fatal(err)
	}

	job := &sessionJob{
		TabID: "archive-broken-ref-tab", ChatID: "archive-broken-ref-chat",
		UserID: fieldString(user, "id"), AssistantID: fieldString(assistant, "id"), Finished: true,
	}
	store.mu.Lock()
	archiveErr := store.archiveJobLocked(job)
	store.mu.Unlock()
	if archiveErr != nil {
		t.Fatalf("one unreadable thumbnail aborted the turn archive: %v", archiveErr)
	}
	raw, err := os.ReadFile(chatArchivePath(stateDir, "archive-broken-ref-tab"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("text survives")) || !bytes.Contains(raw, []byte("assistant text survives")) {
		t.Fatalf("archive dropped turn text after image loss: %s", raw)
	}
	if bytes.Contains(raw, []byte(sessionImageDataRefField)) {
		t.Fatalf("archive retained an unreadable image ref: %s", raw)
	}
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "session image ref") {
		t.Fatalf("archive image warning = %v", err)
	}
}

func TestSessionStoreDegradesOutOfRangeEventImageRef(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("range-ref-tab", "range-ref-chat", "text survives invalid event image")
	assistant := sessionAssistant(t, snapshot, "range-ref-tab")
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "range-tool",
		"images": []any{map[string]any{sessionMessageImageRefField: 7}},
	}}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := newSessionStore(statePath)
	if err := store.LoadError(); err == nil || !strings.Contains(err.Error(), "outside its owning message") {
		t.Fatalf("out-of-range image warning = %v", err)
	}
	got, ok := store.Get().(map[string]any)
	if !ok {
		t.Fatal("one out-of-range event image blanked session:get")
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

func TestSessionStoreRejectsLeanMergeWithMismatchedEventImageRefs(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("mismatched-ref-tab", "mismatched-ref-chat", "two images")
	assistant := sessionAssistant(t, snapshot, "mismatched-ref-tab")
	first := refNativeImage(base64.StdEncoding.EncodeToString([]byte("first image")))
	second := refNativeImage(base64.StdEncoding.EncodeToString([]byte("second image")))
	assistant["images"] = []any{first, second}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "mismatched-tool",
		"images": []any{map[string]any{sessionMessageImageRefField: 1}},
	}}
	if !store.Save(snapshot) {
		t.Fatal("seed valid ref-native snapshot")
	}

	lean := store.Get().(map[string]any)
	lean["_workassSave"] = "lean-payload-v2"
	lean["_workassDeletedChatIds"] = []any{}
	leanAssistant := sessionAssistant(t, lean, "mismatched-ref-tab")
	leanAssistant["images"] = anySlice(leanAssistant["images"])[:1]
	leanAssistant["events"] = []any{map[string]any{"kind": "tool", "id": "mismatched-tool"}}
	if store.Save(lean) {
		t.Fatal("lean save accepted event refs against a different owning image array")
	}

	got, ok := store.Get().(map[string]any)
	if !ok {
		t.Fatal("rejected mismatched merge bricked session:get")
	}
	gotAssistant := sessionAssistant(t, got, "mismatched-ref-tab")
	if len(anySlice(gotAssistant["images"])) != 2 {
		t.Fatal("rejected mismatched merge silently lost a user image")
	}
	if err := validateSessionEventImageRefs(got); err != nil {
		t.Fatalf("published mirror is not decodable after rejection: %v", err)
	}
}

func TestSessionStoreLoadsLegacyInlineSnapshotIntoRefNativeMirror(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	snapshot := sessionMirrorFixture("legacy-inline-tab", "legacy-inline-chat", "legacy image")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "legacy-inline-tab"))[0])
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("legacy-inline", 1024)))
	user["images"] = []any{refNativeImage(data)}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	store := newSessionStore(statePath)
	if err := store.LoadError(); err != nil {
		t.Fatalf("load legacy inline snapshot: %v", err)
	}
	store.mu.Lock()
	internal, err := json.Marshal(store.snapshot)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(internal, []byte(data)) || !bytes.Contains(internal, []byte(sessionImageDataRefField)) {
		t.Fatalf("legacy snapshot was not normalized to refs: %s", internal)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte(data)) || !bytes.Contains(persisted, []byte(sessionImageDataRefField)) {
		t.Fatalf("legacy canonical snapshot was not rewritten compatibly: %s", persisted)
	}
	gotUser := mapFromAnyMain(messageSlice(chatFromSnapshot(store.Get().(map[string]any), "legacy-inline-tab"))[0])
	if fieldString(mapFromAnyMain(anySlice(gotUser["images"])[0]), "data") != data {
		t.Fatal("legacy inline image was lost during ref-native normalization")
	}
}

func TestQueuedTurnAfterRestartMaterializesACPImages(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("queue-ref-tab", "queue-ref-chat", "seed")
	chat := chatFromSnapshot(snapshot, "queue-ref-tab")
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("queued-image", 1024)))
	chat["queue"] = []any{map[string]any{
		"id": "queue-image", "text": "continue with image", "source": "agent",
		"delivery": "queue", "images": []any{refNativeImage(data)},
	}}
	if !store.Save(snapshot) {
		t.Fatal("save queued turn")
	}

	reloaded := newSessionStore(statePath)
	if err := reloaded.LoadError(); err != nil {
		t.Fatal(err)
	}
	opts, err := reloaded.AgentPrepareQueuedTurn("queue-ref-tab", "queue-ref-chat", "queue-image", "native-session")
	if err != nil {
		t.Fatalf("prepare queued turn: %v", err)
	}
	image := mapFromAnyMain(anySlice(opts["images"])[0])
	if fieldString(image, "data") != data || fieldString(image, sessionImageDataRefField) != "" {
		t.Fatalf("ACP queued image contract = %#v", image)
	}
	reloaded.mu.Lock()
	internalUser := mapFromAnyMain(messageSlice(reloaded.chatLocked("queue-ref-tab"))[2])
	internalImage := mapFromAnyMain(anySlice(internalUser["images"])[0])
	reloaded.mu.Unlock()
	if fieldString(internalImage, "data") != "" || fieldString(internalImage, sessionImageDataRefField) == "" {
		t.Fatalf("prepared mirror user image is not ref-native: %#v", internalImage)
	}
}

func TestProviderMediaJournalAndMirrorStayRefNative(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	if !store.Save(sessionMirrorFixture("media-ref-tab", "media-ref-chat", "seed")) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "media-ref-tab", "chatId": "media-ref-chat", "prompt": "media",
		"userMessageId": "media-ref-user", "assistantMessageId": "media-ref-assistant",
	}) {
		t.Fatal("prepare")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "media-ref-job", "tabId": "media-ref-tab", "chatId": "media-ref-chat",
		},
	})
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("provider-media", 1024)))
	store.RecordJobEvent("job:event", map[string]any{
		"type": "assistant-media", "id": "media-ref-job",
		"images": []any{refNativeImage(data)},
	})

	store.mu.Lock()
	internal, err := json.Marshal(store.snapshot)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(internal, []byte(data)) || !bytes.Contains(internal, []byte(sessionImageDataRefField)) {
		t.Fatalf("provider media entered the live mirror inline: %s", internal)
	}
	journal, err := os.ReadFile(store.journalPath("media-ref-assistant"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journal, []byte(data)) || !bytes.Contains(journal, []byte(sessionImageDataRefField)) {
		t.Fatalf("provider media entered the journal inline: %s", journal)
	}
	gotAssistant := sessionAssistant(t, store.Get().(map[string]any), "media-ref-tab")
	if fieldString(mapFromAnyMain(anySlice(gotAssistant["images"])[0]), "data") != data {
		t.Fatal("provider media was not restored on the wire")
	}
}

func TestSessionArchiveRemainsInlineWithRefNativeMirror(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("archive-ref-tab", "archive-ref-chat", "archive image")
	chat := chatFromSnapshot(snapshot, "archive-ref-tab")
	user := mapFromAnyMain(messageSlice(chat)[0])
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("archive-image", 1024)))
	user["images"] = []any{refNativeImage(data)}
	assistant["status"] = "done"
	assistant["content"] = "done"
	if !store.Save(snapshot) {
		t.Fatal("save")
	}
	job := &sessionJob{
		TabID: "archive-ref-tab", ChatID: "archive-ref-chat",
		UserID: fieldString(user, "id"), AssistantID: fieldString(assistant, "id"),
		Finished: true,
	}
	store.mu.Lock()
	err := store.archiveJobLocked(job)
	store.mu.Unlock()
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	raw, err := os.ReadFile(chatArchivePath(stateDir, "archive-ref-tab"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(data)) || bytes.Contains(raw, []byte(sessionImageDataRefField)) {
		t.Fatalf("archive format is not inline-compatible: %s", raw)
	}
}

func TestSessionGetRehydratesAfterReleasingStoreMutex(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("get-lock-tab", "get-lock-chat", "large image")
	user := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "get-lock-tab"))[0])
	data := base64.StdEncoding.EncodeToString(make([]byte, 8<<20))
	user["images"] = []any{refNativeImage(data)}
	if !store.Save(snapshot) {
		t.Fatal("save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "get-lock-tab", "chatId": "get-lock-chat", "prompt": "stream",
		"userMessageId": "get-lock-user", "assistantMessageId": "get-lock-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "get-lock-job", "tabId": "get-lock-tab", "chatId": "get-lock-chat",
		},
	})

	rehydrateStarted := make(chan struct{})
	releaseRehydrate := make(chan struct{})
	store.beforeGetRehydrate = func() {
		close(rehydrateStarted)
		<-releaseRehydrate
	}
	got := make(chan any, 1)
	go func() { got <- store.Get() }()
	select {
	case <-rehydrateStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("session:get never reached wire rehydration")
	}

	streamed := make(chan struct{})
	go func() {
		store.RecordJobEvent("job:event", map[string]any{
			"type": "data", "id": "get-lock-job", "stream": "stdout", "chunk": "x",
		})
		close(streamed)
	}()
	select {
	case <-streamed:
	case <-time.After(250 * time.Millisecond):
		close(releaseRehydrate)
		t.Fatal("provider chunk could not acquire the store mutex during session:get image hydration")
	}
	close(releaseRehydrate)
	select {
	case result := <-got:
		gotUser := mapFromAnyMain(messageSlice(chatFromSnapshot(result.(map[string]any), "get-lock-tab"))[0])
		if fieldString(mapFromAnyMain(anySlice(gotUser["images"])[0]), "data") != data {
			t.Fatal("wire hydration lost the large image")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session:get did not finish")
	}
}
