package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// closedControlChat gives a stored actor an attached lane whose snapshot
// carries a command catalog, as a chat that last ran before a restart has.
func closedControlChat(t *testing.T, backend chatActorBackend, chatID string) {
	t.Helper()
	engine, err := chat.NewDurableEngine(chatID, backend.store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: chatID,
		Realm: providercontract.Realm{
			ProviderID: "mock", MachineID: "closed-machine", AccountScope: "closed-account",
			InstallScope: "closed-install", Verified: true,
		},
		WorkspaceEpoch: "closed-workspace-epoch",
	}.Normalize()
	apply := func(command chat.Command) {
		t.Helper()
		if err := engine.Apply(command); err != nil {
			t.Fatalf("apply %T: %v", command, err)
		}
	}
	apply(chat.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "tab-" + chatID}, CWD: t.TempDir(), ModelID: "mock-model"})
	apply(chat.LaneOpened{
		LaneID: identity.ID, Identity: identity, ConnectionGeneration: 1,
		Thread: providercontract.ThreadRef{ProviderID: "mock", RootID: "closed-root", HeadID: "closed-head", Lineage: 1, Proof: "closed-proof"},
		Context: providercontract.ContextCapabilities{
			ExactResume: true, ImportMode: providercontract.ContextImportUnsupported, NativeCompaction: true, VerifiedLineage: true,
		},
		Attachment: &providercontract.LaneAttachmentSnapshot{
			ConnectionID: "closed-connection", ProviderID: "mock", ProviderName: "Mock ACP",
			CommandCatalogSupported: true, CommandCatalog: &providercontract.RuntimeCommandCatalog{
				Commands: []providercontract.RuntimeCommand{{Name: "/review", Description: "Review", ArgumentHint: "[path]", Aliases: []string{"r"}}},
				Agents:   []providercontract.RuntimeAgent{{Name: "reviewer", Description: "Reviews", Model: "mock-model"}},
				AsOf:     1786440010000,
			},
		},
	})
	if engine.Snapshot().Lanes[identity.ID].Attachment == nil {
		t.Fatal("the lane did not attach")
	}
}

// controlReads is every read-only control reply a renderer asks for when it
// shows a chat, for each tab/chat pairing (the right one, a foreign tab, an
// unknown chat).
func controlReads(t *testing.T, runtime *providerChatRuntime, chatIDs []string) []string {
	t.Helper()
	var out []string
	encode := func(value any, err error) string {
		raw, _ := json.Marshal(value)
		return fmt.Sprintf("%s|%v", raw, err)
	}
	for _, chatID := range chatIDs {
		for _, tabID := range []string{"tab-" + chatID, "tab-someone-else", ""} {
			out = append(out, encode(runtime.ChatCommands(tabID, chatID)))
			out = append(out, encode(runtime.ChatGoal(tabID, chatID)))
		}
	}
	out = append(out, encode(runtime.ChatCommands("tab-nobody", "nobody")))
	out = append(out, encode(runtime.ChatGoal("tab-nobody", "nobody")))
	return out
}

func TestClosedChatsAnswerControlReadsWithoutOpening(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	chats := map[string]int{"control-attached": 7, "control-plain": 4}
	for chatID, rows := range chats {
		lazyTestChat(t, stateDir, backend, chatID, rows, nil)
	}
	closedControlChat(t, backend, "control-attached")
	ids := sortedKeys(chats)

	t.Setenv(eagerActorsEnv, "1")
	eager := startLazyTestRuntime(t, stateDir, backend, "tab-control-plain")
	want := controlReads(t, eager, ids)
	if err := eager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Setenv(eagerActorsEnv, "")
	lazy := startLazyTestRuntime(t, stateDir, backend, "tab-control-plain")
	defer lazy.Close(context.Background())
	if _, closed := lazy.closedSummary("control-attached"); !closed {
		t.Fatal("the attached chat was opened at boot; this test needs it closed")
	}
	got := controlReads(t, lazy, ids)
	if open, _ := lazy.lazyActorCounts(); open != 0 {
		t.Fatalf("control reads opened %d actors", open)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("control read %d differs between a closed and an open actor:\nclosed %s\nopen   %s", index, got[index], want[index])
		}
	}
	if want[0] == `{"commandCatalog":null,"live":false,"supported":false}|<nil>` {
		t.Fatalf("the attached chat reported no catalog: %s", want[0])
	}
}

// waitOpen waits until the chat's actor is open.
func waitOpen(t *testing.T, runtime *providerChatRuntime, chatID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, closed := runtime.closedSummary(chatID); !closed {
			runtime.mu.Lock()
			_, open := runtime.actors[chatID]
			runtime.mu.Unlock()
			if open {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not opened in the background", chatID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestViewedClosedChatsOpenInTheBackgroundAndStillRetire(t *testing.T) {
	stateDir := t.TempDir()
	backend, err := openChatActorBackend(stateDir, true, "test", t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.close()
	chats := map[string]int{"warm-a": 9, "warm-b": 5, "warm-c": 3, "warm-selected": 2}
	for chatID, rows := range chats {
		lazyTestChat(t, stateDir, backend, chatID, rows, nil)
	}
	t.Setenv(eagerActorsEnv, "")
	runtime := startLazyTestRuntime(t, stateDir, backend, "tab-warm-selected")
	before := readEverything(t, runtime, chats)
	if open, _ := runtime.lazyActorCounts(); open != 0 {
		t.Fatalf("%d actors open before any view", open)
	}

	runtime.PrewarmChatByTab("tab-warm-a")
	waitOpen(t, runtime, "warm-a")
	// Viewing an open chat, an unknown tab or a chat twice changes nothing.
	runtime.PrewarmChatByTab("tab-warm-a")
	runtime.PrewarmChatByTab("tab-nobody")
	runtime.PrewarmChat("nobody")
	// Views arriving while an open runs wait as one: the newest is opened
	// and nothing is left pending.
	runtime.PrewarmChat("warm-b")
	runtime.PrewarmChat("warm-c")
	waitOpen(t, runtime, "warm-c")
	runtime.prewarm.wg.Wait()
	runtime.mu.Lock()
	running, next := runtime.prewarm.running, runtime.prewarm.next
	runtime.mu.Unlock()
	if running || next != "" {
		t.Fatalf("background opens still pending: running=%v next=%q", running, next)
	}
	// An opened actor reads exactly as it did closed.
	if after := readEverything(t, runtime, chats); !reflectEqualReads(after, before) {
		t.Fatal("a chat opened in the background reads differently from the closed one")
	}

	// Idle eviction still reclaims a background-opened actor.
	runtime.mu.Lock()
	opened := make([]*providerChatActor, 0, len(runtime.actors))
	for _, actor := range runtime.actors {
		opened = append(opened, actor)
	}
	runtime.mu.Unlock()
	for _, actor := range opened {
		actor.lastUsed.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	}
	retired := 0
	for attempt := 0; attempt < 100 && retired < len(opened); attempt++ {
		retired += runtime.retireIdleActors(context.Background(), time.Hour)
		if retired < len(opened) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if open, _ := runtime.lazyActorCounts(); open != 0 {
		t.Fatalf("%d background-opened actors survived idle eviction", open)
	}

	// After Close, a view starts nothing.
	if err := runtime.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.PrewarmChat("warm-a")
	runtime.mu.Lock()
	running = runtime.prewarm.running
	runtime.mu.Unlock()
	if running {
		t.Fatal("a closed runtime started a background open")
	}
}

func reflectEqualReads(a, b lazyReads) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
