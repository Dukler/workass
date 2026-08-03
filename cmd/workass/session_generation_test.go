package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSessionGetDoesNotAcquireStoreMutex(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("get-lock-tab", "get-lock-chat", "seed")) {
		t.Fatal("seed save")
	}

	store.mu.Lock()
	done := make(chan any, 1)
	go func() { done <- store.Get() }()
	select {
	case snapshot := <-done:
		if snapshot == nil {
			store.mu.Unlock()
			t.Fatal("session:get returned nil")
		}
	case <-time.After(100 * time.Millisecond):
		store.mu.Unlock()
		t.Fatal("session:get attempted to acquire the store mutex")
	}
	store.mu.Unlock()
}

func TestSessionGetLinearizableDuringConcurrentMutation(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("linear-tab", "linear-chat", "seed")) {
		t.Fatal("seed save")
	}

	getDetached := make(chan struct{})
	releaseGet := make(chan struct{})
	store.beforeGetRehydrate = func() {
		close(getDetached)
		<-releaseGet
	}
	first := make(chan map[string]any, 1)
	go func() {
		first <- store.Get().(map[string]any)
	}()
	select {
	case <-getDetached:
	case <-time.After(time.Second):
		t.Fatal("session:get did not detach its generation")
	}
	if !store.UpdateChatControls("linear-tab", "linear-chat", "mock", "new-model", "agent") {
		close(releaseGet)
		t.Fatal("concurrent control mutation")
	}
	close(releaseGet)
	oldSnapshot := <-first
	if got := fieldString(chatFromSnapshot(oldSnapshot, "linear-tab"), "currentModelId"); got == "new-model" {
		t.Fatalf("in-flight get observed a generation committed after its Load: %q", got)
	}
	store.beforeGetRehydrate = nil
	newSnapshot := store.Get().(map[string]any)
	if got := fieldString(chatFromSnapshot(newSnapshot, "linear-tab"), "currentModelId"); got != "new-model" {
		t.Fatalf("next get missed committed generation: %q", got)
	}
}

func TestEverySessionMutationAdvancesPublishedGeneration(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("advance-tab", "advance-chat", "seed")) {
		t.Fatal("seed save")
	}
	assertAdvanced := func(name string, mutate func()) {
		t.Helper()
		before := store.publishedGeneration()
		mutate()
		after := store.publishedGeneration()
		if before == nil || after == nil || after == before || after.number <= before.number ||
			after.stateRevision <= before.stateRevision {
			t.Fatalf("%s did not advance publication: before=%#v after=%#v", name, before, after)
		}
	}

	assertAdvanced("controls", func() {
		if !store.UpdateChatControls("advance-tab", "advance-chat", "mock", "model-2", "agent") {
			t.Fatal("control mutation")
		}
	})
	assertAdvanced("queue", func() {
		if _, err := store.AgentEnqueueChat("advance-tab", "advance-chat", "queued", "queue"); err != nil {
			t.Fatal(err)
		}
	})
	assertAdvanced("prepare", func() {
		if !store.PrepareTurn(map[string]any{
			"tabId": "advance-tab", "chatId": "advance-chat", "prompt": "stream",
			"userMessageId": "advance-user", "assistantMessageId": "advance-assistant",
		}) {
			t.Fatal("prepare turn")
		}
	})
	assertAdvanced("start", func() {
		store.RecordJobEvent("job:event", map[string]any{
			"type": "start", "job": map[string]any{
				"id": "advance-job", "tabId": "advance-tab", "chatId": "advance-chat",
			},
		})
	})
	assertAdvanced("chunk", func() {
		store.RecordJobEvent("job:event", map[string]any{
			"type": "data", "id": "advance-job", "stream": "stdout", "chunk": "visible",
		})
	})
}

func TestPublishedGenerationNeverAliasesWritableContainer(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("alias-tab", "alias-chat", "seed")
	assistant := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "alias-tab"))[1])
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "alias-tool", "status": "in_progress",
		"output": map[string]any{"text": "immutable payload"},
	}}
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	before := store.publishedGeneration()
	beforeBytes, err := json.Marshal(before.root)
	if err != nil {
		t.Fatal(err)
	}
	beforeChat := chatFromSnapshot(before.root, "alias-tab")
	beforeMessage := mapFromAnyMain(messageSlice(beforeChat)[1])
	beforeEvent := mapFromAnyMain(anySlice(beforeMessage["events"])[0])
	beforeOutput := mapFromAnyMain(beforeEvent["output"])

	store.mu.Lock()
	tx := store.beginSessionMutationLocked()
	writable := tx.messageForWrite("alias-tab", fieldString(beforeMessage, "id"))
	mapFromAnyMain(anySlice(writable["events"])[0])["status"] = "completed"
	after := store.commitSessionMutationLocked(tx)
	store.mu.Unlock()

	afterBytes, err := json.Marshal(before.root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatal("committing a path-copy mutated the prior published generation")
	}
	afterChat := chatFromSnapshot(after.root, "alias-tab")
	afterMessage := mapFromAnyMain(messageSlice(afterChat)[1])
	afterEvent := mapFromAnyMain(anySlice(afterMessage["events"])[0])
	if reflect.ValueOf(before.root).Pointer() == reflect.ValueOf(after.root).Pointer() ||
		reflect.ValueOf(beforeChat).Pointer() == reflect.ValueOf(afterChat).Pointer() ||
		reflect.ValueOf(beforeMessage).Pointer() == reflect.ValueOf(afterMessage).Pointer() ||
		reflect.ValueOf(beforeEvent).Pointer() == reflect.ValueOf(afterEvent).Pointer() {
		t.Fatal("published mutation path aliases a writable container")
	}
	if reflect.ValueOf(beforeOutput).Pointer() != reflect.ValueOf(mapFromAnyMain(afterEvent["output"])).Pointer() {
		t.Fatal("unchanged immutable tool payload was copied")
	}
}

func TestSessionGetWaitsForCapturedArchiveOutsideMutex(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("archive-fence-tab", "archive-fence-chat", "seed")) {
		t.Fatal("seed save")
	}
	archiveDone := make(chan struct{})
	store.mu.Lock()
	store.archivePending = map[string]chan struct{}{"archive-job": archiveDone}
	store.archivePendingTabs = map[string]string{"archive-job": "archive-fence-tab"}
	store.republishArchiveFencesLocked()
	store.mu.Unlock()

	getDone := make(chan any, 1)
	go func() { getDone <- store.Get() }()
	time.Sleep(10 * time.Millisecond)

	mutexAcquired := make(chan struct{})
	go func() {
		store.mu.Lock()
		close(mutexAcquired)
		store.mu.Unlock()
	}()
	select {
	case <-mutexAcquired:
	case <-time.After(100 * time.Millisecond):
		close(archiveDone)
		t.Fatal("archive fence wait held the store mutex")
	}
	select {
	case <-getDone:
		close(archiveDone)
		t.Fatal("session:get returned before its captured archive completed")
	default:
	}
	close(archiveDone)
	select {
	case snapshot := <-getDone:
		if snapshot == nil {
			t.Fatal("session:get returned nil after archive completion")
		}
	case <-time.After(time.Second):
		t.Fatal("session:get did not resume after archive completion")
	}
}

func TestSaveReusesUnchangedPublishedChats(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("reuse-tab", "reuse-chat", "seed")
	chat := chatFromSnapshot(snapshot, "reuse-tab")
	chat[agentQueueRevisionField] = 0
	chat[runtimeControlRevisionField] = 0
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	if !store.Save(leanRendererSaveForCost(store.Get().(map[string]any))) {
		t.Fatal("establish renderer intent")
	}
	before := chatFromSnapshot(store.publishedGeneration().root, "reuse-tab")
	if !store.Save(leanRendererSaveForCost(store.Get().(map[string]any))) {
		t.Fatal("unchanged save")
	}
	after := chatFromSnapshot(store.publishedGeneration().root, "reuse-tab")
	if reflect.ValueOf(before).Pointer() != reflect.ValueOf(after).Pointer() {
		t.Fatal("unchanged renderer intent rebuilt the published chat")
	}
}

func TestSaveDirtyMessagePreservesDaemonPayloads(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("dirty-tab", "dirty-chat", "seed")
	assistant := mapFromAnyMain(messageSlice(chatFromSnapshot(snapshot, "dirty-tab"))[1])
	assistant["events"] = []any{map[string]any{
		"kind": "tool", "id": "dirty-tool", "status": "completed",
		"input":  map[string]any{"path": "input.txt"},
		"output": map[string]any{"text": "daemon-owned output"},
	}}
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	lean := leanRendererSaveForCost(store.Get().(map[string]any))
	if !store.Save(lean) {
		t.Fatal("establish lean renderer intent")
	}
	dirty := leanRendererSaveForCost(store.Get().(map[string]any))
	mapFromAnyMain(messageSlice(chatFromSnapshot(dirty, "dirty-tab"))[1])["content"] = "renderer edit"
	if !store.Save(dirty) {
		t.Fatal("dirty message save")
	}
	got := sessionAssistant(t, store.Get().(map[string]any), "dirty-tab")
	event := mapFromAnyMain(anySlice(got["events"])[0])
	if fieldString(mapFromAnyMain(event["output"]), "text") != "daemon-owned output" {
		t.Fatalf("dirty message lost daemon tool output: %#v", event)
	}
	if fieldString(got, "content") != "renderer edit" {
		t.Fatalf("dirty renderer field was not published: %#v", got)
	}
}

func TestSaveHashMatchRequiresExactBytes(t *testing.T) {
	left := rendererIntentFingerprintFor(map[string]any{"value": "left"})
	right := rendererIntentFingerprint{
		digest: left.digest, canonical: []byte(`{"value":"right"}`),
	}
	if rendererIntentEqual(left, right) {
		t.Fatal("matching SHA-256 digest bypassed the exact canonical-byte check")
	}
}

func TestSaveConcurrentChunkCASRetriesWithoutLoss(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("save-cas-tab", "save-cas-chat", "seed")) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "save-cas-tab", "chatId": "save-cas-chat", "prompt": "stream",
		"userMessageId": "save-cas-user", "assistantMessageId": "save-cas-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "save-cas-job", "tabId": "save-cas-tab", "chatId": "save-cas-chat",
		},
	})
	payload := leanRendererSaveForCost(store.Get().(map[string]any))
	var once sync.Once
	store.beforeSaveCAS = func() {
		once.Do(func() {
			store.RecordJobEvent("job:event", map[string]any{
				"type": "data", "id": "save-cas-job", "stream": "stdout", "chunk": "during-save-cas",
			})
		})
	}
	if !store.Save(payload) {
		t.Fatal("save did not converge after a concurrent streamed chunk")
	}
	got := sessionAssistant(t, store.Get().(map[string]any), "save-cas-tab")
	if !bytes.Contains([]byte(fieldString(got, "content")), []byte("during-save-cas")) {
		t.Fatalf("CAS retry lost streamed output: %#v", got)
	}
}

func TestSaveStaleRuntimeRevisionKeepsDaemonControls(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("control-cas-tab", "control-cas-chat", "seed")) {
		t.Fatal("seed save")
	}
	stale := leanRendererSaveForCost(store.Get().(map[string]any))
	if !store.UpdateChatControls(
		"control-cas-tab", "control-cas-chat", "codex", "gpt-5.6-sol[xhigh]", "agent-full-access",
	) {
		t.Fatal("daemon control update")
	}
	if !store.Save(stale) {
		t.Fatal("stale renderer save")
	}
	chat := chatFromSnapshot(store.Get().(map[string]any), "control-cas-tab")
	if fieldString(chat, "providerId") != "codex" ||
		fieldString(chat, "currentModelId") != "gpt-5.6-sol[xhigh]" ||
		fieldString(chat, "currentModeId") != "agent-full-access" {
		t.Fatalf("stale save replaced daemon controls: %#v", chat)
	}
}

func TestSaveStaleQueueRevisionKeepsDaemonQueue(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("queue-cas-tab", "queue-cas-chat", "seed")) {
		t.Fatal("seed save")
	}
	stale := leanRendererSaveForCost(store.Get().(map[string]any))
	receipt, err := store.AgentEnqueueChat("queue-cas-tab", "queue-cas-chat", "daemon queue", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !store.Save(stale) {
		t.Fatal("stale renderer save")
	}
	head, agentFirst, exists := store.AgentQueueHead("queue-cas-tab", "queue-cas-chat")
	if !exists || !agentFirst || fieldString(head, "id") != fieldString(receipt, "queueId") {
		t.Fatalf("stale save replaced daemon queue: %#v", head)
	}
}
