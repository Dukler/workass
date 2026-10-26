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
	chats := map[string]int{"screen-a": 9, "screen-b": 14, "screen-empty": 0, "screen-huge": 7, "screen-big": 12}
	for chatID, rows := range chats {
		var mutate func(*chat.State)
		switch chatID {
		case "screen-huge":
			// A last exchange far larger than an entry may be.
			mutate = func(state *chat.State) { state.Ledger[6].Text = strings.Repeat("h", 9<<20) }
		case "screen-big":
			// A tail too large to be the first screen whole: its first screen
			// is the last exchange, and the rest of the slice a 3 MiB page.
			mutate = func(state *chat.State) { state.Ledger[8].Text = strings.Repeat("b", 3<<20) }
		}
		lazyTestChat(t, stateDir, backend, chatID, rows, mutate)
	}
	ids := sortedKeys(chats)

	// What every chat's first screen is, read from open actors.
	t.Setenv(eagerActorsEnv, "1")
	eager := startLazyTestRuntime(t, stateDir, backend, "tab-none")
	want := map[string][]byte{}
	wantSlices := map[string][][]byte{}
	for _, chatID := range ids {
		wantSlices[chatID] = rendererSlice(t, eager, "tab-"+chatID, chats[chatID])
		raw, found, err := eager.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmSummaryOptions)
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
	if kept := lazy.warmFirstScreens(context.Background()); kept != 3 {
		t.Fatalf("warm kept %d first screens, want 3", kept)
	}
	if open, _ := lazy.lazyActorCounts(); open != 0 {
		t.Fatalf("the warm opened %d actors", open)
	}
	for _, chatID := range ids {
		summary, _ := lazy.closedSummary(chatID)
		cached, ok := lazy.cachedFirstScreen(summary, firstScreenWarmLimit, firstScreenWarmSummaryOptions)
		if ok != (chatID == "screen-a" || chatID == "screen-b" || chatID == "screen-big") {
			t.Fatalf("%s kept=%v", chatID, ok)
		}
		raw, found, err := lazy.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmSummaryOptions)
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
		again, _, _ := lazy.ProjectLastExchangeByTabRaw("tab-"+chatID, firstScreenWarmLimit, firstScreenWarmSummaryOptions)
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

	// The pages a renderer reads after a large tail's first screen are kept,
	// and every read of the first slice is the bytes an open actor answers.
	for _, chatID := range ids {
		lazy.mu.Lock()
		_, sliced := lazy.firstScreens.slices[chatID]
		lazy.mu.Unlock()
		if sliced != (chatID == "screen-big" || chatID == "screen-huge") {
			t.Fatalf("%s slice kept=%v", chatID, sliced)
		}
		got := rendererSlice(t, lazy, "tab-"+chatID, chats[chatID])
		if len(got) != len(wantSlices[chatID]) {
			t.Fatalf("%s: %d slice reads, an open actor's took %d", chatID, len(got), len(wantSlices[chatID]))
		}
		for index := range got {
			if !bytes.Equal(got[index], wantSlices[chatID][index]) {
				t.Fatalf("%s: slice read %d differs from an open actor's", chatID, index)
			}
		}
	}
	if open, _ := lazy.lazyActorCounts(); open != 0 {
		t.Fatalf("slice reads opened %d actors", open)
	}

	// Opening a chat drops what was read ahead of it; what it then commits is
	// read, not the kept bytes.
	actor, err := lazy.actor("screen-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lazy.actor("screen-big"); err != nil {
		t.Fatal(err)
	}
	lazy.mu.Lock()
	_, stillKept := lazy.firstScreens.entries["screen-a"]
	_, sliceKept := lazy.firstScreens.slices["screen-big"]
	_, sizeKept := lazy.firstScreens.tails["screen-big"]
	sliceBytes := lazy.firstScreens.sliceBytes
	slices := 0
	for _, entry := range lazy.firstScreens.slices {
		slices += entry.bytes
	}
	lazy.mu.Unlock()
	if stillKept || sliceKept || sizeKept {
		t.Fatal("an opened chat kept what was read ahead of it")
	}
	if slices != sliceBytes || slices > slicePageCacheBytes {
		t.Fatalf("slices account %d bytes, hold %d", sliceBytes, slices)
	}
	next := actor.engine.Snapshot().Presentation.Clone()
	next.Title = "renamed while open"
	if err := actor.engine.Apply(chat.SavePresentation{OperationID: "screen-rename", Digest: "screen-rename", Presentation: next}); err != nil {
		t.Fatal(err)
	}
	if raw, _, err := lazy.ProjectLastExchangeByTabRaw("tab-screen-a", firstScreenWarmLimit, firstScreenWarmSummaryOptions); err != nil || !bytes.Equal(raw, want["screen-a"]) {
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

// rendererSlice reads a chat's first slice as the renderer opening it does:
// its first screen, then pages before the oldest row it has for the rows it
// still lacks. It returns every reply.
func rendererSlice(t *testing.T, runtime *providerChatRuntime, tabID string, count int) [][]byte {
	t.Helper()
	first, found, err := runtime.ProjectLastExchangeByTabRaw(tabID, firstScreenWarmLimit, firstScreenWarmSummaryOptions)
	if err != nil || !found {
		t.Fatalf("%s first screen: %v %v", tabID, found, err)
	}
	replies := [][]byte{append([]byte(nil), first...)}
	ids, _ := replyMessageIDs(first)
	want := min(firstScreenWarmLimit, count)
	for len(ids) < want && len(ids) > 0 {
		raw, found, err := runtime.ProjectArchivePageBeforeByTabRaw(tabID, ids[0], want-len(ids), firstScreenWarmOptions)
		if err != nil || !found {
			t.Fatalf("%s page: %v %v", tabID, found, err)
		}
		replies = append(replies, append([]byte(nil), raw...))
		page, _ := replyMessageIDs(raw)
		if len(page) == 0 {
			break
		}
		ids = append(page[max(0, len(page)-(want-len(ids))):], ids...)
	}
	return replies
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw := snapshotParityJSONBytes(t, value)
	return string(raw)
}
