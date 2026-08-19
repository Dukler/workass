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

func TestSessionRefreshCoordinatorFocusIsOneShotBeforeGenericRefresh(t *testing.T) {
	emissions := make(chan refreshEmission, 4)
	coordinator := newSessionRefreshCoordinator(func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	})
	t.Cleanup(coordinator.stop)

	coordinator.RequestFocus("tab-focused", "chat-focused")
	focused := waitRefreshEmission(t, emissions, time.Second)
	if focused.channel != "agent:apply" || fieldString(focused.payload, "action") != "session-refresh" ||
		fieldString(focused.payload, "tabId") != "tab-focused" || fieldString(focused.payload, "chatId") != "chat-focused" ||
		focused.payload["focus"] != true || len(focused.payload) != 4 {
		t.Fatalf("focused wire event = %#v", focused)
	}
	generic := waitRefreshEmission(t, emissions, time.Second)
	if generic.channel != "agent:apply" || fieldString(generic.payload, "action") != "session-refresh" || len(generic.payload) != 1 {
		t.Fatalf("trailing cache-clearing refresh = %#v", generic)
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
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	const parentTabID, parentChatID = "refresh-parent-tab", "refresh-parent-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-refresh-parent",
		"tabId":       parentTabID, "chatId": parentChatID,
		"title": "Refresh parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask", "focus": true,
	}); err != nil {
		t.Fatalf("create actor-native refresh parent: %v", err)
	}
	coordinator := newChatControlCoordinator(manager, func(channel string, payload any) {
		emissions <- refreshEmission{at: time.Now(), channel: channel, payload: mapFromAnyMain(payload)}
	}, runtime)
	t.Cleanup(coordinator.refreshes.stop)
	assertImmediate := func(name string, invoke func() error) refreshEmission {
		t.Helper()
		if err := invoke(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case emission := <-emissions:
			if emission.channel != "agent:apply" || fieldString(emission.payload, "action") != "session-refresh" {
				t.Fatalf("%s refresh = %#v", name, emission)
			}
			return emission
		default:
			t.Fatalf("%s returned before its immediate refresh", name)
			return refreshEmission{}
		}
	}

	var created map[string]any
	assertImmediate("create", func() error {
		var createErr error
		created, createErr = coordinator.create(
			context.Background(), parentTabID, parentChatID,
			map[string]any{"operation_id": "test:refresh-create", "title": "Refresh child", "cwd": root, "provider_id": "mock", "model_id": "mock-deterministic"},
		)
		return createErr
	})
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	assertImmediate("rename", func() error {
		_, err := coordinator.rename(map[string]any{"operation_id": "test:refresh-rename", "tab_id": tabID, "chat_id": chatID, "title": "Renamed child"})
		return err
	})
	assertImmediate("configure", func() error {
		_, err := coordinator.configure(context.Background(), map[string]any{
			"operation_id": "test:refresh-configure",
			"tab_id":       tabID, "chat_id": chatID, "cwd": root,
			"provider_id": "mock", "model_id": "mock-deterministic", "mode_id": "ask",
		})
		return err
	})
	focused := assertImmediate("focus", func() error {
		_, err := coordinator.focus(map[string]any{"operation_id": "test:refresh-focus", "tab_id": tabID, "chat_id": chatID})
		return err
	})
	if focused.payload["focus"] != true || fieldString(focused.payload, "tabId") != tabID || fieldString(focused.payload, "chatId") != chatID {
		t.Fatalf("focus intent = %#v", focused)
	}
	trailing := waitRefreshEmission(t, emissions, time.Second)
	if len(trailing.payload) != 1 || fieldString(trailing.payload, "action") != "session-refresh" {
		t.Fatalf("focus trailing refresh = %#v", trailing)
	}
	assertImmediate("send receipt", func() error {
		_, err := coordinator.send(map[string]any{
			"operation_id": "test:refresh-send",
			"tab_id":       tabID, "chat_id": chatID, "message": "queued visibly", "delivery": "queue",
		})
		return err
	})
	assertImmediate("delete", func() error {
		_, err := coordinator.delete(map[string]any{"operation_id": "test:refresh-delete", "tab_id": tabID, "chat_id": chatID})
		return err
	})
}

func TestChatControlRefreshUsesExactActorRevisionWithoutSessionMirror(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	const tabID, chatID = "refresh-revision-tab", "refresh-revision-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-refresh-revision",
		"tabId":       tabID, "chatId": chatID,
		"title": "Refresh revision", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native refresh chat: %v", err)
	}

	// The chat-control coordinator intentionally receives no sessionStore here.
	// Refresh generations must come from the exact actor, never from the retired
	// renderer mirror's global generation.
	coordinator := newChatControlCoordinator(manager, func(string, any) {}, runtime)
	t.Cleanup(coordinator.refreshes.stop)
	batches := make(chan map[sessionRefreshTarget]uint64, 2)
	coordinator.refreshes.flushObserver = func(batch map[sessionRefreshTarget]uint64) { batches <- batch }
	expectActorRevision := func(name string) {
		t.Helper()
		state, ok := runtime.Snapshot(chatID)
		if !ok {
			t.Fatalf("%s actor disappeared", name)
		}
		coordinator.refresh(tabID, chatID, false)
		select {
		case batch := <-batches:
			got, exists := batch[sessionRefreshTarget{tabID: tabID, chatID: chatID}]
			if !exists || got != state.Revision {
				t.Fatalf("%s refresh generation = %d, want exact actor revision %d; batch=%#v", name, got, state.Revision, batch)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s refresh did not flush", name)
		}
	}

	expectActorRevision("create")
	if err := runtime.RenameChat(tabID, chatID, "Refresh revision renamed", "test:rename-refresh-revision"); err != nil {
		t.Fatalf("rename actor chat: %v", err)
	}
	expectActorRevision("rename")
}

func TestAdapterSessionRefreshCannotOverwriteActorFromStaleAttachment(t *testing.T) {
	root := repoRoot(t)
	stateDir := t.TempDir()
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(stateDir, "acp"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	const tabID, chatID = "refresh-fence-tab", "refresh-fence-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-refresh-fence", "tabId": tabID, "chatId": chatID,
		"title": "Refresh fence", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native refresh chat: %v", err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{
		TabID: tabID, ChatID: chatID, CWD: root, ProviderID: "mock",
		ModelID: "mock-deterministic", ModeID: "ask",
	})
	if err != nil {
		t.Fatalf("attach provider lane: %v", err)
	}
	stale := map[string]any{
		"action": "session-refresh", "tabId": tabID, "chatId": chatID,
		"sessionId": "stale-provider-connection", "providerId": "mock", "modelId": "stale-model",
	}
	if err := runtime.ApplySessionRefresh(stale); err == nil {
		t.Fatal("stale provider attachment refresh unexpectedly mutated the actor")
	}
	state, ok := runtime.Snapshot(chatID)
	if !ok || state.Presentation.CurrentModelID != "mock-deterministic" {
		t.Fatalf("stale refresh changed actor controls: %#v", state.Presentation)
	}
	current := map[string]any{
		"action": "session-refresh", "tabId": tabID, "chatId": chatID,
		"sessionId": info.SessionID, "providerId": "mock", "modelId": "adapter-selected-model",
	}
	if err := runtime.ApplySessionRefresh(current); err != nil {
		t.Fatalf("current provider attachment refresh: %v", err)
	}
	state, _ = runtime.Snapshot(chatID)
	if state.Presentation.CurrentModelID != "adapter-selected-model" {
		t.Fatalf("current refresh model = %q, want adapter-selected-model", state.Presentation.CurrentModelID)
	}
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
