package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// TestSessionSaveCostProfile times each stage of the session:save critical
// section against a real snapshot. It is a diagnostic, not a gate: without
// WORKASS_COST_SNAPSHOT pointing at a session-state.json it skips.
//
//	WORKASS_COST_SNAPSHOT=~/Library/.../session-state.json \
//	  go test ./cmd/workass -run TestSessionSaveCostProfile -v
func TestSessionSaveCostProfile(t *testing.T) {
	src := os.Getenv("WORKASS_COST_SNAPSHOT")
	if src == "" {
		t.Skip("set WORKASS_COST_SNAPSHOT to a session-state.json to profile save cost")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	t.Logf("snapshot bytes = %d", len(data))

	stage := func(name string, fn func()) time.Duration {
		start := time.Now()
		fn()
		d := time.Since(start)
		t.Logf("%-34s %8.1f ms", name, float64(d.Microseconds())/1000)
		return d
	}

	dir := t.TempDir()
	statePath := filepath.Join(dir, sessionStateFilename)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	// Externalized image refs must resolve or the load aborts and every later
	// stage would silently measure an empty store.
	if srcImages := filepath.Join(filepath.Dir(src), sessionImageDirname); dirExists(srcImages) {
		if out, err := exec.Command("cp", "-R", srcImages, filepath.Join(dir, sessionImageDirname)).CombinedOutput(); err != nil {
			t.Fatalf("copy images: %v: %s", err, out)
		}
	}
	runtime.GC()
	rssBeforeLoad := currentRSSBytes()
	var store *sessionStore
	stage("newSessionStore (load from disk)", func() { store = newSessionStore(statePath) })
	if err := store.LoadError(); err != nil {
		t.Fatalf("store load error (measurements would be vacuous): %v", err)
	}
	store.mu.Lock()
	loadedChats := len(anySlice(store.snapshot["chats"]))
	store.mu.Unlock()
	if loadedChats == 0 {
		t.Fatalf("store loaded 0 chats; measurements would be vacuous")
	}
	t.Logf("loaded chats = %d", loadedChats)
	runtime.GC()
	rssAfterRefLoad := currentRSSBytes()
	t.Logf("rss before-load=%d after-ref-load=%d ref-load-delta=%d", rssBeforeLoad, rssAfterRefLoad, rssAfterRefLoad-rssBeforeLoad)

	var refNativeBytes []byte
	store.mu.Lock()
	refNativeBytes, err = json.Marshal(store.snapshot)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if inline := inlineImageBytesInSessionSnapshot(store.snapshot); inline != 0 {
		t.Fatalf("loaded mirror retained %d inline image bytes", inline)
	}

	runtime.GC()
	var rawBefore runtime.MemStats
	runtime.ReadMemStats(&rawBefore)
	var rawWire []byte
	rawTotal := stage("session:get raw handler total", func() {
		rawWire = store.GetRawWithLiveSessions(nil)
	})
	var rawAfter runtime.MemStats
	runtime.ReadMemStats(&rawAfter)
	rawAllocated := rawAfter.TotalAlloc - rawBefore.TotalAlloc
	rawRSS := currentRSSBytes()
	t.Logf("session:get raw bytes=%d allocatedBytes=%d rss=%d totalMs=%.3f",
		len(rawWire), rawAllocated, rawRSS, float64(rawTotal.Microseconds())/1000)
	if rawAllocated > 48_411_912 {
		t.Fatalf("session:get raw allocated %d bytes, target <= 48411912", rawAllocated)
	}
	if rssBeforeLoad >= 0 && rssBeforeLoad < 200<<20 && rawRSS > 256<<20 {
		t.Fatalf("session:get raw RSS %d bytes, fresh-process target <= 256 MiB", rawRSS)
	}

	var wire map[string]any
	runtime.GC()
	var legacyBefore runtime.MemStats
	runtime.ReadMemStats(&legacyBefore)
	getTotal := stage("session:get handler total", func() {
		wire, _ = store.Get().(map[string]any)
	})
	var legacyAfterGet runtime.MemStats
	runtime.ReadMemStats(&legacyAfterGet)
	wireBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var legacyAfter runtime.MemStats
	runtime.ReadMemStats(&legacyAfter)
	legacyGetAllocated := legacyAfterGet.TotalAlloc - legacyBefore.TotalAlloc
	legacyReplyAllocated := legacyAfter.TotalAlloc - legacyAfterGet.TotalAlloc
	legacyAllocated := legacyAfter.TotalAlloc - legacyBefore.TotalAlloc
	t.Logf("session:get legacy allocatedBytes=%d replyAllocatedBytes=%d combinedAllocatedBytes=%d",
		legacyGetAllocated, legacyReplyAllocated, legacyAllocated)
	if !bytes.Equal(rawWire, wireBytes) {
		t.Fatalf("raw session:get differs from legacy: raw=%d legacy=%d", len(rawWire), len(wireBytes))
	}
	t.Logf("ref-native bytes=%d hydrated-wire bytes=%d avoided-live-bytes=%d", len(refNativeBytes), len(wireBytes), len(wireBytes)-len(refNativeBytes))
	t.Logf("session:get lock=%#v totalMs=%.3f", store.getLock.snapshot(), float64(getTotal.Microseconds())/1000)
	t.Logf("rss with detached hydrated-wire=%d", currentRSSBytes())

	payload := leanRendererSaveForCost(wire)
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("realistic lean renderer save bytes=%d", len(payloadBytes))

	// Stage the pre-lock ingress against its own detached payload. This is not
	// part of saveLock, but is reported beside it so the handler total is honest.
	var detached map[string]any
	stage("outside: redact detached tree", func() {
		detached = mapFromAnyMain(redactSessionValue(payload))
	})
	stage("outside: migrations", func() {
		migrateLegacySteerChronology(detached)
		migrateModelControlKeys(detached)
	})
	stage("outside: image externalization", func() {
		if err := makeSessionSnapshotRefNative(detached, dir); err != nil {
			t.Fatalf("externalize detached payload: %v", err)
		}
	})

	var receipt sessionSaveStageReceipt
	store.saveStageObserver = func(value sessionSaveStageReceipt) { receipt = value }
	saveTotal := stage("session:save handler total", func() {
		if !store.Save(payload) {
			t.Fatalf("Save rejected the snapshot")
		}
	})
	names := make([]string, 0, len(receipt.Stages))
	var stageSum time.Duration
	for name := range receipt.Stages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		duration := receipt.Stages[name]
		stageSum += duration
		t.Logf("lock stage %-28s %8.3f ms", name, float64(duration.Microseconds())/1000)
	}
	t.Logf("lock stage sum=%0.3f ms measured held=%0.3f ms delta=%0.3f ms",
		float64(stageSum.Microseconds())/1000,
		float64(receipt.Held.Microseconds())/1000,
		float64((receipt.Held-stageSum).Microseconds())/1000,
	)
	t.Logf("session:save lock=%#v handlerTotalMs=%.3f", store.saveLock.snapshot(), float64(saveTotal.Microseconds())/1000)

	// Exercise the actual provider-chunk lock against repeated realistic saves,
	// rather than inferring stream wait from save held time.
	firstChat := mapFromAnyMain(anySlice(store.snapshot["chats"])[0])
	tabID, chatID := fieldString(firstChat, "id"), fieldString(firstChat, "chatId")
	if !store.PrepareTurn(map[string]any{
		"tabId": tabID, "chatId": chatID, "prompt": "cost-profile-stream",
		"userMessageId": "cost-profile-user", "assistantMessageId": "cost-profile-assistant",
	}) {
		t.Fatal("prepare cost-profile stream")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "cost-profile-job", "tabId": tabID, "chatId": chatID,
		},
	})
	concurrentPayload := leanRendererSaveForCost(store.Get().(map[string]any))
	store.streamLock.reset()
	store.saveLock.reset()
	store.saveStageObserver = nil
	startConcurrent := make(chan struct{})
	var concurrent sync.WaitGroup
	var concurrentSaveFailed bool
	concurrent.Add(2)
	go func() {
		defer concurrent.Done()
		<-startConcurrent
		for index := 0; index < 8; index++ {
			if !store.Save(concurrentPayload) {
				concurrentSaveFailed = true
				return
			}
		}
	}()
	go func() {
		defer concurrent.Done()
		<-startConcurrent
		for index := 0; index < 240; index++ {
			store.RecordJobEvent("job:event", map[string]any{
				"type": "data", "id": "cost-profile-job", "stream": "stdout", "chunk": "x",
			})
			time.Sleep(500 * time.Microsecond)
		}
	}()
	close(startConcurrent)
	concurrent.Wait()
	if concurrentSaveFailed {
		t.Fatal("concurrent realistic save was rejected")
	}
	t.Logf("concurrent realistic saves=%#v", store.saveLock.snapshot())
	t.Logf("concurrent provider stream=%#v", store.streamLock.snapshot())

	wire = nil
	payload = nil
	detached = nil
	concurrentPayload = nil
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("post-profile heapAlloc=%d heapSys=%d rss=%d", memory.HeapAlloc, memory.HeapSys, currentRSSBytes())
}

func TestSessionSaveDeletionCostProfile(t *testing.T) {
	src := os.Getenv("WORKASS_COST_SNAPSHOT")
	if src == "" {
		t.Skip("set WORKASS_COST_SNAPSHOT to a session-state.json to profile deletion save cost")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	dir := t.TempDir()
	statePath := filepath.Join(dir, sessionStateFilename)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	if srcImages := filepath.Join(filepath.Dir(src), sessionImageDirname); dirExists(srcImages) {
		if out, err := exec.Command("cp", "-R", srcImages, filepath.Join(dir, sessionImageDirname)).CombinedOutput(); err != nil {
			t.Fatalf("copy images: %v: %s", err, out)
		}
	}
	store := newSessionStore(statePath)
	if err := store.LoadError(); err != nil {
		t.Fatalf("store load error: %v", err)
	}
	firstChat := mapFromAnyMain(anySlice(store.publishedGeneration().root["chats"])[0])
	streamTabID, streamChatID := fieldString(firstChat, "id"), fieldString(firstChat, "chatId")
	if !store.PrepareTurn(map[string]any{
		"tabId": streamTabID, "chatId": streamChatID, "prompt": "deletion-cost-stream",
		"userMessageId": "deletion-cost-user", "assistantMessageId": "deletion-cost-assistant",
	}) {
		t.Fatal("prepare deletion-cost stream")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "deletion-cost-job", "tabId": streamTabID, "chatId": streamChatID,
		},
	})
	payload := leanRendererSaveForCost(store.Get().(map[string]any))
	chats := anySlice(payload["chats"])
	if len(chats) < 2 {
		t.Skip("real-state deletion profile needs at least two chats")
	}
	tabID := fieldString(mapFromAnyMain(chats[len(chats)-1]), "id")
	if tabID == "" {
		t.Fatal("last real-state chat has no tab id")
	}
	payload["_workassDeletedChatIds"] = []any{tabID}
	var firstCAS sync.Once
	store.beforeSaveCAS = func() {
		firstCAS.Do(func() {
			store.RecordJobEvent("job:event", map[string]any{
				"type": "data", "id": "deletion-cost-job", "stream": "stdout", "chunk": "forced-cas-retry",
			})
		})
	}
	stopStream := make(chan struct{})
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopStream:
				return
			case <-ticker.C:
				store.RecordJobEvent("job:event", map[string]any{
					"type": "data", "id": "deletion-cost-job", "stream": "stdout", "chunk": "x",
				})
			}
		}
	}()
	store.saveLock.reset()
	store.streamLock.reset()
	started := time.Now()
	saved := store.Save(payload)
	close(stopStream)
	<-streamDone
	if !saved {
		t.Fatal("real-state deletion save was rejected")
	}
	t.Logf("session:save deletion lock=%#v handlerTotalMs=%.3f",
		store.saveLock.snapshot(), float64(time.Since(started).Microseconds())/1000)
	t.Logf("session:save deletion concurrent stream=%#v", store.streamLock.snapshot())
	if chatFromSnapshot(store.Get().(map[string]any), tabID) != nil {
		t.Fatalf("profiled chat %q survived explicit deletion", tabID)
	}
}

func currentRSSBytes() int64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	return kib * 1024
}

func inlineImageBytesInSessionSnapshot(snapshot map[string]any) int {
	total := 0
	for _, rawChat := range anySlice(snapshot["chats"]) {
		for _, rawMessage := range messageSlice(mapFromAnyMain(rawChat)) {
			message := mapFromAnyMain(rawMessage)
			total += inlineImageBytes(message["images"])
			for _, rawEvent := range anySlice(message["events"]) {
				total += inlineImageBytes(mapFromAnyMain(rawEvent)["images"])
			}
		}
		for _, rawQueue := range anySlice(mapFromAnyMain(rawChat)["queue"]) {
			total += inlineImageBytes(mapFromAnyMain(rawQueue)["images"])
		}
	}
	return total
}

func leanRendererSaveForCost(snapshot map[string]any) map[string]any {
	out, _ := cloneSessionContainers(snapshot).(map[string]any)
	out["_workassSave"] = "lean-payload-v2"
	out["_workassDeletedChatIds"] = []any{}
	for _, rawChat := range anySlice(out["chats"]) {
		chat := mapFromAnyMain(rawChat)
		for _, rawMessage := range messageSlice(chat) {
			message := mapFromAnyMain(rawMessage)
			delete(message, "images")
			overlays := make([]any, 0)
			for _, rawEvent := range anySlice(message["events"]) {
				event := mapFromAnyMain(rawEvent)
				if fieldString(event, "kind") != "tool" {
					continue
				}
				overlay := map[string]any{"kind": "tool"}
				for _, key := range []string{"id", "terminalId", "key", "at", "startedAt", "endedAt", "subagentModel"} {
					if value, present := event[key]; present {
						overlay[key] = value
					}
				}
				overlays = append(overlays, overlay)
			}
			message["events"] = overlays
		}
		for _, rawQueue := range anySlice(chat["queue"]) {
			item := mapFromAnyMain(rawQueue)
			delete(item, "images")
			delete(item, "draftImages")
		}
	}
	return out
}
