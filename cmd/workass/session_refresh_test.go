package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"workass/internal/acp"
)

type refreshEmission struct {
	at      time.Time
	channel string
	payload map[string]any
}

func TestSessionRefreshCoordinatorCoalescesTargetsAtHighestGeneration(t *testing.T) {
	emissions := make(chan refreshEmission, 4)
	batches := make(chan map[sessionRefreshTarget]uint64, 4)
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	coordinator.window = 20 * time.Millisecond
	coordinator.flushObserver = func(batch map[sessionRefreshTarget]uint64) { batches <- batch }
	t.Cleanup(coordinator.stop)

	coordinator.Request("tab-a", "chat-a", 2, refreshBackground)
	coordinator.Request("tab-a", "chat-a", 7, refreshBackground)
	coordinator.Request("tab-a", "chat-a", 4, refreshBackground)
	coordinator.Request("tab-b", "chat-b", 3, refreshBackground)

	emission := waitRefreshEmission(t, emissions, time.Second)
	if emission.channel != "agent:apply" || fieldString(emission.payload, "action") != "session-refresh" || len(emission.payload) != 1 {
		t.Fatalf("merged wire event = %#v", emission)
	}
	batch := <-batches
	if len(batch) != 2 ||
		batch[sessionRefreshTarget{tabID: "tab-a", chatID: "chat-a"}] != 7 ||
		batch[sessionRefreshTarget{tabID: "tab-b", chatID: "chat-b"}] != 3 {
		t.Fatalf("merged intent batch = %#v", batch)
	}
	select {
	case extra := <-emissions:
		t.Fatalf("coalesced requests emitted extra hydration: %#v", extra)
	case <-time.After(35 * time.Millisecond):
	}
}

func TestSessionRefreshCoordinatorDeadlineDoesNotReset(t *testing.T) {
	emissions := make(chan refreshEmission, 4)
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	coordinator.window = 80 * time.Millisecond
	t.Cleanup(coordinator.stop)

	started := time.Now()
	coordinator.Request("tab-first", "chat-first", 1, refreshBackground)
	for index := 0; index < 5; index++ {
		time.Sleep(15 * time.Millisecond)
		coordinator.Request("tab-later", "chat-later", uint64(index+2), refreshBackground)
	}
	emission := waitRefreshEmission(t, emissions, time.Second)
	elapsed := emission.at.Sub(started)
	if elapsed > 125*time.Millisecond {
		t.Fatalf("fixed refresh deadline moved with later intents: elapsed=%s", elapsed)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("background refresh fired before its window: elapsed=%s", elapsed)
	}
}

func TestSessionRefreshCoordinatorMutationDuringFlushUsesNextDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	emissions := make(chan refreshEmission, 4)
	var once sync.Once
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		once.Do(func() {
			close(entered)
			<-release
		})
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	coordinator.window = 25 * time.Millisecond
	t.Cleanup(coordinator.stop)

	firstDone := make(chan struct{})
	go func() {
		coordinator.Request("tab-first", "chat-first", 1, refreshImmediate)
		close(firstDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first immediate flush did not enter broadcaster")
	}
	coordinator.Request("tab-next", "chat-next", 2, refreshImmediate)
	select {
	case emission := <-emissions:
		t.Fatalf("mutation raced a second flush while the first was blocked: %#v", emission)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first flush did not finish")
	}
	waitRefreshEmission(t, emissions, time.Second)
	waitRefreshEmission(t, emissions, time.Second)
}

func TestSessionRefreshCoordinatorImmediateAndMergedEventsAreRendererEquivalent(t *testing.T) {
	emissions := make(chan refreshEmission, 4)
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	coordinator.window = 15 * time.Millisecond
	t.Cleanup(coordinator.stop)

	coordinator.Request("tab-immediate", "chat-immediate", 1, refreshImmediate)
	immediate := waitRefreshEmission(t, emissions, time.Second)
	coordinator.Request("tab-a", "chat-a", 2, refreshBackground)
	coordinator.Request("tab-b", "chat-b", 3, refreshBackground)
	merged := waitRefreshEmission(t, emissions, time.Second)

	immediateJSON, _ := json.Marshal(immediate.payload)
	mergedJSON, _ := json.Marshal(merged.payload)
	if string(immediateJSON) != `{"action":"session-refresh"}` || string(mergedJSON) != string(immediateJSON) {
		t.Fatalf("renderer invalidations differ: immediate=%s merged=%s", immediateJSON, mergedJSON)
	}
}

func TestSessionRefreshCoordinatorMeasuredBurst(t *testing.T) {
	emissions := make(chan refreshEmission, 64)
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	coordinator.window = 20 * time.Millisecond
	t.Cleanup(coordinator.stop)

	for index := 0; index < 20; index++ {
		coordinator.Request("tab-visible", "chat-visible", uint64(index+1), refreshImmediate)
	}
	for index := 0; index < 8; index++ {
		coordinator.Request("tab-background", "chat-background", uint64(index+21), refreshBackground)
	}
	count := 0
	for count < 21 {
		waitRefreshEmission(t, emissions, time.Second)
		count++
	}
	select {
	case extra := <-emissions:
		t.Fatalf("hydration burst emitted an unexpected 22nd refresh: %#v", extra)
	case <-time.After(35 * time.Millisecond):
	}
	t.Logf("hydration burst intents=28 emitted=%d merged=%d", count, 28-count)
	if count != 21 {
		t.Fatalf("hydration burst emitted %d refreshes, want 21", count)
	}
}

func TestChatControlVisibleMutationRefreshesAreImmediate(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	parent, err := store.AgentCreateChat("Refresh parent", root, "mock", "mock-deterministic", "ask", true)
	if err != nil {
		t.Fatal(err)
	}
	emissions := make(chan refreshEmission, 32)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	coordinator := newChatControlCoordinator(manager, store, func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	t.Cleanup(coordinator.refreshes.stop)
	assertImmediate := func(name string, invoke func() error) {
		t.Helper()
		if err := invoke(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case emission := <-emissions:
			if emission.channel != "agent:apply" || fieldString(emission.payload, "action") != "session-refresh" {
				t.Fatalf("%s refresh = %#v", name, emission)
			}
		default:
			t.Fatalf("%s returned before its immediate refresh", name)
		}
	}

	var created map[string]any
	assertImmediate("create", func() error {
		var createErr error
		created, createErr = coordinator.create(
			context.Background(), fieldString(parent, "tabId"), fieldString(parent, "chatId"),
			map[string]any{"title": "Refresh child", "cwd": root, "provider_id": "mock", "model_id": "mock-deterministic"},
		)
		return createErr
	})
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	assertImmediate("rename", func() error {
		_, err := coordinator.rename(map[string]any{"tab_id": tabID, "chat_id": chatID, "title": "Renamed child"})
		return err
	})
	assertImmediate("configure", func() error {
		_, err := coordinator.configure(context.Background(), map[string]any{
			"tab_id": tabID, "chat_id": chatID, "cwd": root,
			"provider_id": "mock", "model_id": "mock-deterministic", "mode_id": "ask",
		})
		return err
	})
	assertImmediate("focus", func() error {
		_, err := coordinator.focus(map[string]any{"tab_id": tabID, "chat_id": chatID})
		return err
	})
	coordinator.startQueuedTurnOverride = func(context.Context, string, string, map[string]any) error {
		return acp.ErrChatBusy
	}
	assertImmediate("send receipt", func() error {
		_, err := coordinator.send(map[string]any{
			"tab_id": tabID, "chat_id": chatID, "message": "queued visibly", "delivery": "queue",
		})
		return err
	})
	assertImmediate("delete", func() error {
		_, err := coordinator.delete(map[string]any{"tab_id": tabID, "chat_id": chatID})
		return err
	})
}

func waitRefreshEmission(t *testing.T, emissions <-chan refreshEmission, timeout time.Duration) refreshEmission {
	t.Helper()
	select {
	case emission := <-emissions:
		return emission
	case <-time.After(timeout):
		t.Fatal("timed out waiting for session refresh")
		return refreshEmission{}
	}
}
