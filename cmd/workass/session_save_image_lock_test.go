package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionSavePersistsImagesAfterReleasingStoreMutex(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)
	imagePersistStarted := make(chan struct{})
	releaseImagePersist := make(chan struct{})
	store.beforePersistImages = func() {
		close(imagePersistStarted)
		<-releaseImagePersist
	}
	snapshot := sessionMirrorFixture("image-lock-tab", "image-lock-chat", "save image")
	chat := mapFromAnyMain(anySlice(snapshot["chats"])[0])
	user := mapFromAnyMain(messageSlice(chat)[0])
	user["images"] = []any{map[string]any{
		"mimeType": "image/png", "name": "screenshot.png",
		"data": base64.StdEncoding.EncodeToString(make([]byte, 8<<20)),
	}}

	saved := make(chan bool, 1)
	go func() {
		saved <- store.Save(snapshot)
	}()
	select {
	case <-imagePersistStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("session save never reached staged image persistence")
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		close(releaseImagePersist)
		t.Fatalf("snapshot became visible before its image was durable: %v", err)
	}

	mutexAcquired := make(chan struct{})
	go func() {
		store.mu.Lock()
		close(mutexAcquired)
		store.mu.Unlock()
	}()
	select {
	case <-mutexAcquired:
	case <-time.After(250 * time.Millisecond):
		close(releaseImagePersist)
		t.Fatal("image persistence still holds the session store mutex")
	}
	close(releaseImagePersist)
	select {
	case ok := <-saved:
		if !ok {
			t.Fatal("session save failed after staged image persistence")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session save did not finish")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("canonical snapshot missing after image persistence: %v", err)
	}
}

func TestSessionSaveMarshalsPublishedGenerationAfterReleasingStoreMutex(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("generation-lock-tab", "generation-lock-chat", "seed")
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "generation-lock-tab", "chatId": "generation-lock-chat", "prompt": "stream",
		"userMessageId": "generation-lock-user", "assistantMessageId": "generation-lock-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "generation-lock-job", "tabId": "generation-lock-tab", "chatId": "generation-lock-chat",
		},
	})
	payload := store.Get().(map[string]any)
	payload["_workassSave"] = "lean-payload-v2"
	payload["_workassDeletedChatIds"] = []any{}

	marshalStarted := make(chan struct{})
	releaseMarshal := make(chan struct{})
	store.beforeGenerationMarshal = func() {
		close(marshalStarted)
		<-releaseMarshal
	}
	saved := make(chan bool, 1)
	go func() { saved <- store.Save(payload) }()
	select {
	case <-marshalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("save never reached immutable-generation marshal")
	}

	streamed := make(chan struct{})
	go func() {
		store.RecordJobEvent("job:event", map[string]any{
			"type": "data", "id": "generation-lock-job", "stream": "stdout", "chunk": "x",
		})
		close(streamed)
	}()
	select {
	case <-streamed:
	case <-time.After(250 * time.Millisecond):
		close(releaseMarshal)
		t.Fatal("provider chunk could not acquire the store mutex during generation marshal")
	}
	close(releaseMarshal)
	select {
	case ok := <-saved:
		if !ok {
			t.Fatal("save failed after generation marshal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("save did not finish")
	}
}

func TestSessionWritersDoNotMutatePublishedSharedSubtrees(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("shared-writer-tab", "shared-writer-chat", "seed")) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "shared-writer-tab", "chatId": "shared-writer-chat", "prompt": "stream",
		"userMessageId": "shared-writer-user", "assistantMessageId": "shared-writer-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "shared-writer-job", "tabId": "shared-writer-tab", "chatId": "shared-writer-chat",
		},
	})
	firstImage := base64.StdEncoding.EncodeToString([]byte("first shared image"))
	store.RecordJobEvent("job:event", map[string]any{
		"type": "assistant-media", "id": "shared-writer-job",
		"images": []any{refNativeImage(firstImage)},
	})
	payload := store.Get().(map[string]any)
	payload["_workassSave"] = "lean-payload-v2"
	payload["_workassDeletedChatIds"] = []any{}

	published := make(chan struct{})
	release := make(chan struct{})
	var generation *sessionGeneration
	var before []byte
	store.beforeGenerationMarshal = func() {
		store.mu.Lock()
		generation = store.generation
		if generation != nil {
			before, _ = json.Marshal(generation.root)
		}
		store.mu.Unlock()
		close(published)
		<-release
	}
	saved := make(chan bool, 1)
	go func() { saved <- store.Save(payload) }()
	select {
	case <-published:
	case <-time.After(2 * time.Second):
		t.Fatal("save did not publish its immutable generation")
	}
	if generation == nil || len(before) == 0 {
		close(release)
		t.Fatal("published generation was unavailable to the outside-lock marshal seam")
	}

	secondImage := base64.StdEncoding.EncodeToString([]byte("second shared image"))
	store.RecordJobEvent("job:event", map[string]any{
		"type": "assistant-media", "id": "shared-writer-job",
		"images": []any{refNativeImage(secondImage)},
	})
	after, err := json.Marshal(generation.root)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		close(release)
		t.Fatal("a session writer mutated a shared subtree in the published generation")
	}
	close(release)
	select {
	case ok := <-saved:
		if !ok {
			t.Fatal("save failed after shared-subtree writer check")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("save did not finish")
	}
	gotAssistant := sessionAssistant(t, store.Get().(map[string]any), "shared-writer-tab")
	if len(anySlice(gotAssistant["images"])) != 2 {
		t.Fatal("copy-on-write media update was lost")
	}
}

func TestSessionSaveDeletionTombstoneCASRebuildsAfterStreamMutation(t *testing.T) {
	stateDir := t.TempDir()
	statePath := filepath.Join(stateDir, sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("cas-keep-tab", "cas-keep-chat", "keep")
	deleteSnapshot := sessionMirrorFixture("cas-delete-tab", "cas-delete-chat", "delete")
	snapshot["chats"] = append(anySlice(snapshot["chats"]), anySlice(deleteSnapshot["chats"])[0])
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "cas-keep-tab", "chatId": "cas-keep-chat", "prompt": "stream",
		"userMessageId": "cas-user", "assistantMessageId": "cas-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "cas-job", "tabId": "cas-keep-tab", "chatId": "cas-keep-chat",
		},
	})
	payload := store.Get().(map[string]any)
	payload["_workassSave"] = "lean-payload-v2"
	payload["_workassDeletedChatIds"] = []any{"cas-delete-tab"}

	tombstoneDurable := make(chan struct{})
	releaseCAS := make(chan struct{})
	var first sync.Once
	store.afterPersistTombstones = func() {
		first.Do(func() {
			close(tombstoneDurable)
			<-releaseCAS
		})
	}
	saved := make(chan bool, 1)
	go func() { saved <- store.Save(payload) }()
	select {
	case <-tombstoneDurable:
	case <-time.After(2 * time.Second):
		t.Fatal("delete save never made its tombstone durable")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "data", "id": "cas-job", "stream": "stdout", "chunk": "during-cas",
	})
	close(releaseCAS)
	select {
	case ok := <-saved:
		if !ok {
			t.Fatal("delete save failed after CAS rebuild")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete save did not finish")
	}
	got := store.Get().(map[string]any)
	if chatFromSnapshot(got, "cas-delete-tab") != nil {
		t.Fatal("explicitly deleted chat survived the CAS rebuild")
	}
	keep := chatFromSnapshot(got, "cas-keep-tab")
	foundChunk := false
	for _, raw := range messageSlice(keep) {
		if strings.Contains(fieldString(mapFromAnyMain(raw), "content"), "during-cas") {
			foundChunk = true
		}
	}
	if !foundChunk {
		t.Fatal("stream mutation was lost instead of rebuilding against the new generation")
	}
	if _, err := os.Stat(filepath.Join(stateDir, droppedChatDirname, "cas-delete-tab.json")); err != nil {
		t.Fatalf("durable tombstone missing: %v", err)
	}
}

func TestSessionSaveDeletionConvergesDuringOrdinaryStream(t *testing.T) {
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	snapshot := sessionMirrorFixture("delete-stream-keep-tab", "delete-stream-keep-chat", "keep")
	dropped := sessionMirrorFixture("delete-stream-drop-tab", "delete-stream-drop-chat", "drop")
	droppedChat := mapFromAnyMain(anySlice(dropped["chats"])[0])
	mapFromAnyMain(messageSlice(droppedChat)[1])["content"] = strings.Repeat("large dropped transcript", 100_000)
	snapshot["chats"] = append(anySlice(snapshot["chats"]), droppedChat)
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "delete-stream-keep-tab", "chatId": "delete-stream-keep-chat", "prompt": "stream",
		"userMessageId": "delete-stream-user", "assistantMessageId": "delete-stream-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "delete-stream-job", "tabId": "delete-stream-keep-tab", "chatId": "delete-stream-keep-chat",
		},
	})
	payload := store.Get().(map[string]any)
	payload["_workassSave"] = "lean-payload-v2"
	payload["_workassDeletedChatIds"] = []any{"delete-stream-drop-tab"}

	var tombstonePasses int
	store.afterPersistTombstones = func() {
		tombstonePasses++
		// Guarantee at least one ordinary 100 chunks/s event lands inside every
		// old unlock/fsync/CAS window.
		time.Sleep(20 * time.Millisecond)
	}
	streamStop := make(chan struct{})
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-streamStop:
				return
			case <-ticker.C:
				store.RecordJobEvent("job:event", map[string]any{
					"type": "data", "id": "delete-stream-job", "stream": "stdout", "chunk": "x",
				})
			}
		}
	}()

	started := time.Now()
	saved := make(chan bool, 1)
	go func() { saved <- store.Save(payload) }()
	var ok bool
	select {
	case ok = <-saved:
		close(streamStop)
		<-streamDone
	case <-time.After(750 * time.Millisecond):
		close(streamStop)
		<-streamDone
		select {
		case <-saved:
		case <-time.After(2 * time.Second):
			t.Fatal("delete save did not stop after the stream stopped")
		}
		t.Fatalf("delete save did not converge while a 100 chunks/s stream was active; tombstone passes=%d", tombstonePasses)
	}
	if !ok {
		t.Fatal("delete save rejected after a bounded CAS rebuild")
	}
	if tombstonePasses != 1 {
		t.Fatalf("tombstone fsync passes=%d, want exactly 1 across all CAS attempts", tombstonePasses)
	}
	if elapsed := time.Since(started); elapsed >= 750*time.Millisecond {
		t.Fatalf("delete save took %s, want bounded completion under 750ms", elapsed)
	}
	if chatFromSnapshot(store.Get().(map[string]any), "delete-stream-drop-tab") != nil {
		t.Fatal("deleted chat survived the bounded CAS rebuild")
	}
}

func TestSessionRefNativeRealisticLockBudgets(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("wall-clock mutex budgets are measured by the normal gate; race instrumentation changes timing")
	}
	statePath := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(statePath)
	snapshot := sessionMirrorFixture("budget-tab", "budget-chat", "large realistic mirror")
	chat := chatFromSnapshot(snapshot, "budget-tab")
	user := mapFromAnyMain(messageSlice(chat)[0])
	assistant := mapFromAnyMain(messageSlice(chat)[1])
	user["images"] = []any{map[string]any{
		"mimeType": "image/png", "name": "large.png",
		"data": base64.StdEncoding.EncodeToString(make([]byte, 27<<20)),
	}}
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "budget-tool", "at": 0,
		"status": "completed", "output": strings.Repeat("tool-result-payload", 320_000),
	}}
	if !store.Save(snapshot) {
		t.Fatal("seed realistic mirror")
	}

	store.getLock.reset()
	wire, ok := store.Get().(map[string]any)
	if !ok {
		t.Fatal("get realistic mirror")
	}
	getHeldMax := time.Duration(store.getLock.heldMax.Load())
	if getHeldMax >= 25*time.Millisecond {
		t.Fatalf("session:get held store mutex for %s, want <25ms", getHeldMax)
	}

	lean := leanRendererSaveForCost(wire)
	store.saveLock.reset()
	const saves = 5
	for index := 0; index < saves; index++ {
		if !store.Save(lean) {
			t.Fatalf("realistic lean save %d", index)
		}
	}
	count := store.saveLock.count.Load()
	saveHeldAvg := time.Duration(store.saveLock.heldNanos.Load() / max(count, 1))
	if saveHeldAvg >= 15*time.Millisecond {
		t.Fatalf("session:save held store mutex for %s on average, want <15ms", saveHeldAvg)
	}
	t.Logf("realistic ref-native lock budgets getHeldMax=%s saveHeldAvg=%s saveHeldMax=%s",
		getHeldMax.Round(time.Microsecond),
		saveHeldAvg.Round(time.Microsecond),
		time.Duration(store.saveLock.heldMax.Load()).Round(time.Microsecond),
	)
}

func TestPersistSessionImageRecreatesMissingVerifiedFile(t *testing.T) {
	stateDir := t.TempDir()
	data := base64.StdEncoding.EncodeToString([]byte("durable screenshot"))
	ref, err := persistSessionImageData(stateDir, data)
	if err != nil {
		t.Fatalf("initial image persist: %v", err)
	}
	name, ok := validSessionImageRef(ref)
	if !ok {
		t.Fatalf("invalid persisted ref %q", ref)
	}
	path := filepath.Join(stateDir, sessionImageDirname, name)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove verified image: %v", err)
	}
	if _, err := persistSessionImageData(stateDir, data); err != nil {
		t.Fatalf("recreate missing verified image: %v", err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != data {
		t.Fatalf("recreated image content = %q, err=%v", content, err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt verified image: %v", err)
	}
	if _, err := persistSessionImageData(stateDir, data); err != nil {
		t.Fatalf("repair corrupt verified image: %v", err)
	}
	if content, err := os.ReadFile(path); err != nil || string(content) != data {
		t.Fatalf("repaired image content = %q, err=%v", content, err)
	}
}
