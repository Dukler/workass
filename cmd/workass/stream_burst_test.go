package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
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
		Broadcast:              daemonEventBroadcaster(nil, hub.Broadcast),
	})
	providerChats := newProviderChatRuntime(manager, sessionState, stateDir, hub.Broadcast)
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	createWireActorChat(t, client, 100, "burst-wire-tab", "burst-wire-chat", "mock")
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
		"operationId":   "burst-turn",
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
	snapshot, err := providerChats.ProjectSession()
	if err != nil {
		t.Fatalf("project authoritative burst actor: %v", err)
	}
	chats := anySlice(snapshot["chats"])
	if len(chats) != 1 {
		t.Fatalf("persisted chats = %d, want 1", len(chats))
	}
	messages := anySlice(mapFromAnyMain(chats[0])["messages"])
	if len(messages) == 0 {
		t.Fatal("authoritative burst actor projected no transcript")
	}
	gotPersisted := strings.TrimSpace(stringValue(mapFromAnyMain(messages[len(messages)-1])["content"]))
	if gotPersisted != wantPersisted {
		t.Fatalf("authoritative streamed content did not catch up after visible end delivery: gotBytes=%d wantBytes=%d", len(gotPersisted), len(wantPersisted))
	}
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatalf("marshal authoritative burst snapshot: %v", err)
	}
}

func TestDaemonEventBroadcasterPreservesPublicationOrder(t *testing.T) {
	visible := make(chan string, 2)
	broadcast := daemonEventBroadcaster(nil, func(channel string, raw any) {
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
		t.Fatal("event broadcaster did not drain")
	}
}
