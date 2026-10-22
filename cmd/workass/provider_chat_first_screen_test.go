package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"workass/internal/chat"
)

func TestClosedFirstScreensAreKeptExactlyAndDroppedOnOpen(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	chats := map[string]int{"screen-a": 9, "screen-b": 14, "screen-empty": 0, "screen-huge": 7}
	for chatID, rows := range chats {
		var mutate func(*chat.State)
		if chatID == "screen-huge" {
			// A last exchange far larger than an entry may be.
			mutate = func(state *chat.State) { state.Ledger[6].Text = strings.Repeat("h", 9<<20) }
		}
		lazyTestChat(t, stateDir, backend, chatID, rows, mutate)
	}
	ids := sortedKeys(chats)

	// What every chat's first screen is, read from open actors.
	t.Setenv(eagerActorsEnv, "1")
	eager := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	want := map[string][]byte{}
	for _, chatID := range ids {
		raw, found, err := eager.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmOptions)
		if err != nil || !found {
			t.Fatalf("%s: %v %v", chatID, found, err)
		}
		want[chatID] = append([]byte(nil), raw...)
	}
	if err := eager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Setenv(eagerActorsEnv, "")
	lazy := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	defer lazy.Close(context.Background())
	// The background warm keeps the first screen of every closed chat with
	// rows but the one too large to keep, without opening any.
	if kept := lazy.warmFirstScreens(context.Background()); kept != 2 {
		t.Fatalf("warm kept %d first screens, want 2", kept)
	}
	if open, _ := lazy.lazyActorCounts(); open != 0 {
		t.Fatalf("the warm opened %d actors", open)
	}
	for _, chatID := range ids {
		summary, _ := lazy.closedSummary(chatID)
		cached, ok := lazy.cachedFirstScreen(summary, firstScreenWarmLimit, firstScreenWarmOptions)
		if ok != (chatID == "screen-a" || chatID == "screen-b") {
			t.Fatalf("%s kept=%v", chatID, ok)
		}
		raw, found, err := lazy.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmOptions)
		if err != nil || !found || !bytes.Equal(raw, want[chatID]) {
			t.Fatalf("%s: first screen differs from an open actor's (%v %v)", chatID, found, err)
		}
		if ok && !bytes.Equal(cached, want[chatID]) {
			t.Fatalf("%s: kept first screen differs from an open actor's", chatID)
		}
		// A reply is framed inside its own buffer: the kept bytes are a copy.
		for index := range raw {
			raw[index] = 'x'
		}
		again, _, _ := lazy.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmOptions)
		if !bytes.Equal(again, want[chatID]) {
			t.Fatalf("%s: a framed reply changed the kept first screen", chatID)
		}
		// Another read of the chat is answered, not taken from the cache.
		other, _, err := lazy.ProjectLastExchangeByTabRaw("tab-"+chatID, 3, firstScreenWarmOptions)
		fresh, _, _ := lazy.ProjectLastExchangeByTab("tab-"+chatID, 3, firstScreenWarmOptions)
		if encoded := mustJSON(t, fresh); err != nil || string(other) != encoded {
			t.Fatalf("%s: a different tail was answered from the cache", chatID)
		}
	}

	// Opening a chat drops its first screen; what it then commits is read,
	// not the kept bytes.
	actor, err := lazy.actor("screen-a")
	if err != nil {
		t.Fatal(err)
	}
	lazy.mu.Lock()
	_, stillKept := lazy.firstScreens.entries["screen-a"]
	lazy.mu.Unlock()
	if stillKept {
		t.Fatal("an opened chat kept its first screen")
	}
	next := actor.engine.Snapshot().Presentation.Clone()
	next.Title = "renamed while open"
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: "screen-rename", Digest: "screen-rename", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := lazy.ProjectLastExchangeByTabRaw("tab-screen-a", firstScreenWarmLimit, firstScreenWarmOptions); err != nil || !bytes.Equal(raw, want["screen-a"]) {
		t.Fatalf("open chat first screen: %v", err)
	}
	lazy.mu.Lock()
	total := lazy.firstScreens.bytes
	counted := 0
	for _, entry := range lazy.firstScreens.entries {
		counted += len(entry.raw)
	}
	lazy.mu.Unlock()
	if total != counted || total > firstScreenCacheBytes {
		t.Fatalf("cache accounts %d bytes, holds %d", total, counted)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw := snapshotParityJSONBytes(t, value)
	return string(raw)
}
