package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/httpserve"
	"workass/internal/wire"
)

func TestWireMockBurstReachesClientAtDisplayCadence(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatalf("write renderer fixture: %v", err)
	}

	hub := wire.NewHub()
	sessionState := sharedSessionStore(stateDir)
	largeState := sessionMirrorFixture("burst-wire-tab", "burst-wire-chat", "[mock:burst]")
	// The user's production mirror was 8.6 MiB when this regression was found.
	// Keep the fixture at that scale: the old 2 MiB fixture serialized quickly
	// enough to hide the periodic feed stall on a development machine.
	largeState["streamFixturePadding"] = strings.Repeat("safe streaming state ", 400_000)
	if !sessionState.Save(largeState) {
		t.Fatal("save realistic large session fixture")
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root,
		Provider: acp.ProviderConfig{
			ID:      "mock",
			Command: "node",
			Args:    []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD:     root,
			Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS":          "0",
				"WORKASS_MOCK_ACP_BURST_CHUNKS":      "2048",
				"WORKASS_MOCK_ACP_BURST_CHUNK_BYTES": "128",
			},
			Enabled: true,
			Label:   "Workass Mock ACP",
		},
		DefaultProviderID:      "mock",
		StdoutFlushInterval:    16 * time.Millisecond,
		RSSSampleInterval:      time.Hour,
		LifecycleCheckInterval: time.Hour,
		Broadcast:              daemonEventBroadcaster(sessionState, hub.Broadcast),
	})
	t.Cleanup(func() { manager.Reset() })
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	client.invoke(t, 1, "app-chat:new-session", map[string]any{"tabId": "burst-wire-tab", "chatId": "burst-wire-chat", "providerId": "mock"})
	sessionReply := client.waitReply(t, 1, 5*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("new-session error: %s", *sessionReply.Error)
	}
	sessionID := fmt.Sprint(mapFromAnyMain(sessionReply.Result)["sessionId"])

	started := time.Now()
	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "chatId": "burst-wire-chat", "tabId": "burst-wire-tab",
		"sessionId": sessionID, "providerId": "mock", "prompt": "[mock:burst]",
		"userMessageId": "burst-user", "assistantMessageId": "burst-assistant",
	})
	startReply := client.waitReply(t, 2, 5*time.Second)
	if startReply.Error != nil {
		t.Fatalf("job:start error: %s", *startReply.Error)
	}
	jobID := fmt.Sprint(mapFromAnyMain(startReply.Result)["id"])

	type sample struct {
		at    time.Time
		chunk string
	}
	var samples []sample
	ended := false
	consume := func(msg wsMessage) {
		if msg.T != "event" || msg.Channel != "job:event" {
			return
		}
		payload := mapFromAnyMain(msg.Payload)
		switch fieldString(payload, "type") {
		case "data":
			if fieldString(payload, "id") == jobID && fieldString(payload, "stream") == "stdout" {
				chunk, _ := payload["chunk"].(string)
				samples = append(samples, sample{at: time.Now(), chunk: chunk})
			}
		case "end":
			if fieldString(mapFromAnyMain(payload["job"]), "id") == jobID {
				ended = true
			}
		}
	}
	for _, msg := range client.inbox {
		consume(msg)
	}
	client.inbox = nil
	for !ended {
		consume(client.readMessage(t, 8*time.Second))
	}

	var output strings.Builder
	for _, sample := range samples {
		output.WriteString(sample.chunk)
	}
	const wantBytes = 2048 * 128
	if output.Len() != wantBytes {
		t.Fatalf("wire bytes = %d, want %d", output.Len(), wantBytes)
	}
	if len(samples) < 20 || len(samples) >= 1024 {
		t.Fatalf("wire updates = %d, want sustained frame batches rather than per-token events", len(samples))
	}

	var sumGap, maxGap time.Duration
	for i := 1; i < len(samples); i++ {
		gap := samples[i].at.Sub(samples[i-1].at)
		sumGap += gap
		if gap > maxGap {
			maxGap = gap
		}
	}
	meanGap := sumGap / time.Duration(len(samples)-1)
	elapsed := time.Since(started)
	t.Logf("wire burst bytes=%d websocketUpdates=%d elapsed=%s throughput=%.1fKiB/s meanClientGap=%s maxClientGap=%s", output.Len(), len(samples), elapsed.Round(time.Millisecond), float64(output.Len())/elapsed.Seconds()/1024, meanGap.Round(time.Microsecond), maxGap.Round(time.Microsecond))
	if meanGap > 35*time.Millisecond || maxGap > 75*time.Millisecond {
		t.Fatalf("wire delivery cadence mean=%s max=%s", meanGap, maxGap)
	}

	wantPersisted := strings.TrimSpace(output.String())
	var snapshot map[string]any
	var gotPersisted string
	persistDeadline := time.Now().Add(2 * time.Second)
	for {
		snapshot = mapFromAnyMain(sessionState.Get())
		chats := anySlice(snapshot["chats"])
		if len(chats) != 1 {
			t.Fatalf("persisted chats = %d, want 1", len(chats))
		}
		messages := anySlice(mapFromAnyMain(chats[0])["messages"])
		gotPersisted = stringValue(mapFromAnyMain(messages[len(messages)-1])["content"])
		if gotPersisted == wantPersisted || time.Now().After(persistDeadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gotPersisted != wantPersisted {
		t.Fatalf("authoritative streamed content did not catch up after visible end delivery: gotBytes=%d wantBytes=%d", len(gotPersisted), len(wantPersisted))
	}
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatalf("marshal authoritative burst snapshot: %v", err)
	}
}

func TestSessionGetSeesChunkBeforeMatchingBroadcast(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("visible-tab", "visible-chat", "seed")) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "visible-tab", "chatId": "visible-chat", "prompt": "stream",
		"userMessageId": "visible-user", "assistantMessageId": "visible-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "visible-job", "tabId": "visible-tab", "chatId": "visible-chat",
		},
	})
	visible := make(chan struct{})
	broadcast := daemonEventBroadcaster(store, func(channel string, payload any) {
		if channel != "job:event" {
			t.Errorf("broadcast channel = %q", channel)
		}
		assistant := sessionAssistant(t, store.Get().(map[string]any), "visible-tab")
		if !strings.Contains(fieldString(assistant, "content"), "visible first") {
			t.Errorf("session:get did not contain the chunk before broadcast: %#v", assistant)
		}
		close(visible)
	})

	broadcast("job:event", map[string]any{
		"type": "data", "id": "visible-job", "stream": "stdout", "chunk": "visible first",
	})

	select {
	case <-visible:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("matching event was not broadcast")
	}
}

func TestDaemonEventBroadcasterDoesNotLetBookkeepingDelayNextVisibleFrame(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("ordered-tab", "ordered-chat", "seed")) {
		t.Fatal("seed save")
	}
	if !store.PrepareTurn(map[string]any{
		"tabId": "ordered-tab", "chatId": "ordered-chat", "prompt": "stream",
		"userMessageId": "ordered-user", "assistantMessageId": "ordered-assistant",
	}) {
		t.Fatal("prepare turn")
	}
	store.RecordJobEvent("job:event", map[string]any{
		"type": "start", "job": map[string]any{
			"id": "ordered-job", "tabId": "ordered-tab", "chatId": "ordered-chat",
		},
	})
	visible := make(chan string, 2)
	broadcast := daemonEventBroadcaster(store, func(channel string, raw any) {
		if channel != "job:event" {
			t.Errorf("broadcast channel = %q", channel)
			return
		}
		visible <- fieldString(mapFromAnyMain(raw), "chunk")
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, chunk := range []string{"first", "second"} {
			broadcast("job:event", map[string]any{
				"type": "data", "id": "ordered-job", "stream": "stdout", "chunk": chunk,
			})
		}
	}()

	select {
	case got := <-visible:
		if got != "first" {
			t.Fatalf("first visible chunk = %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("first visible frame did not arrive")
	}
	select {
	case got := <-visible:
		if got != "second" {
			t.Fatalf("second visible chunk = %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("next visible frame waited for prior-frame persistence bookkeeping")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("event broadcaster did not drain after bookkeeping resumed")
	}
}

func TestDaemonEventBroadcasterMakesTerminalStateDurableBeforeEndDelivery(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	visible := make(chan struct{}, 1)
	returned := make(chan struct{})
	broadcast := daemonEventBroadcaster(store, func(channel string, payload any) {
		if channel != "job:event" {
			t.Errorf("broadcast channel = %q", channel)
		}
		visible <- struct{}{}
	})

	store.mu.Lock()
	go func() {
		broadcast("job:event", map[string]any{
			"type": "end",
			"job":  map[string]any{"id": "terminal-job", "status": "done"},
		})
		close(returned)
	}()

	select {
	case <-visible:
		store.mu.Unlock()
		t.Fatal("terminal event became visible before ordered persistence completed")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-returned:
		store.mu.Unlock()
		t.Fatal("terminal broadcaster returned before ordered persistence completed")
	default:
	}
	store.mu.Unlock()

	select {
	case <-visible:
	case <-time.After(time.Second):
		t.Fatal("terminal event was not delivered after persistence resumed")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("terminal broadcaster did not return after persistence resumed")
	}
}

func TestDaemonEventBroadcasterTerminalPersistenceDoesNotPauseAnotherChat(t *testing.T) {
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	snapshot := sessionMirrorFixture("ending-tab", "ending-chat", "ending")
	other := sessionMirrorFixture("other-tab", "other-chat", "other")
	snapshot["chats"] = append(anySlice(snapshot["chats"]), anySlice(other["chats"])[0])
	if !store.Save(snapshot) {
		t.Fatal("seed save")
	}
	for _, ids := range [][3]string{
		{"ending-tab", "ending-chat", "ending-job"},
		{"other-tab", "other-chat", "other-live-job"},
	} {
		if !store.PrepareTurn(map[string]any{
			"tabId": ids[0], "chatId": ids[1], "prompt": "stream",
			"userMessageId": ids[2] + "-user", "assistantMessageId": ids[2] + "-assistant",
		}) {
			t.Fatalf("prepare %s", ids[2])
		}
		store.RecordJobEvent("job:event", map[string]any{
			"type": "start", "job": map[string]any{
				"id": ids[2], "tabId": ids[0], "chatId": ids[1],
			},
		})
	}
	visible := make(chan string, 2)
	broadcast := daemonEventBroadcaster(store, func(channel string, payload any) {
		if channel != "job:event" {
			t.Errorf("broadcast channel = %q", channel)
			return
		}
		visible <- fieldString(mapFromAnyMain(payload), "type")
	})

	persistStarted := make(chan struct{})
	releasePersist := make(chan struct{})
	var blockOnce sync.Once
	store.beforeGenerationMarshal = func() {
		blockOnce.Do(func() {
			close(persistStarted)
			<-releasePersist
		})
	}
	endReturned := make(chan struct{})
	go func() {
		broadcast("job:event", map[string]any{
			"type": "end", "job": map[string]any{
				"id": "ending-job", "status": "done", "finishedAt": time.Now().UTC().Format(time.RFC3339Nano),
			},
		})
		close(endReturned)
	}()
	select {
	case <-persistStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal event did not reach outside-mutex persistence")
	}
	dataReturned := make(chan struct{})
	go func() {
		broadcast("job:event", map[string]any{
			"type": "data", "id": "other-live-job", "stream": "stdout", "chunk": "still visible",
		})
		close(dataReturned)
	}()

	select {
	case got := <-visible:
		if got != "data" {
			t.Fatalf("visible event while terminal persistence blocked = %q", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("one chat's terminal persistence paused another chat's stream")
	}
	select {
	case <-dataReturned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("other chat broadcaster did not return")
	}
	close(releasePersist)

	select {
	case got := <-visible:
		if got != "end" {
			t.Fatalf("terminal event after persistence = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event never became visible")
	}
	select {
	case <-endReturned:
	case <-time.After(time.Second):
		t.Fatal("terminal broadcaster did not return")
	}
}
