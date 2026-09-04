package main

import (
	"fmt"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
	"workass/internal/wire"
)

func TestActorHistoryProjectionMaterializesOnlyRequestedTail(t *testing.T) {
	state, err := chat.NewState("bounded-history-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "bounded-history-tab"
	state.Presentation.Settled = "settled"
	state.Presentation.SettledAt = 1_787_000_000_000
	for index := 0; index < 85; index++ {
		at := ""
		if index == 84 {
			at = "2026-08-12T12:59:00Z"
		}
		state.Ledger = append(state.Ledger, chat.LedgerEvent{
			MessageID: fmt.Sprintf("message-%02d", index), Role: "assistant",
			Text: fmt.Sprintf("row %d", index), Status: "done", At: at,
		})
	}
	wantLastActivity := time.Date(2026, time.August, 12, 12, 59, 0, 0, time.UTC).UnixMilli()

	bounded := map[string]any{}
	if err := projectActorChatWithHistory(bounded, state, actorHistoryTail); err != nil {
		t.Fatal(err)
	}
	rows := anySlice(bounded["messages"])
	if len(rows) != sessionProjectionMessageTail || intValue(bounded["messageCount"]) != 85 || bounded["historyComplete"] != false {
		t.Fatalf("bounded history metadata = rows:%d count:%v complete:%v", len(rows), bounded["messageCount"], bounded["historyComplete"])
	}
	if first := fieldString(mapFromAnyMain(rows[0]), "id"); first != "message-25" {
		t.Fatalf("bounded history began at %q, want message-25", first)
	}
	recent := map[string]any{}
	if err := projectActorChatWithHistoryLimit(recent, state, actorHistoryTail, 10); err != nil {
		t.Fatal(err)
	}
	recentRows := anySlice(recent["messages"])
	if len(recentRows) != 10 || intValue(recent["messageCount"]) != 85 || recent["historyComplete"] != false {
		t.Fatalf("recent history metadata = rows:%d count:%v complete:%v", len(recentRows), recent["messageCount"], recent["historyComplete"])
	}
	if first := fieldString(mapFromAnyMain(recentRows[0]), "id"); first != "message-75" {
		t.Fatalf("recent history began at %q, want message-75", first)
	}
	if err := projectActorChatWithHistoryLimit(map[string]any{}, state, actorHistoryTail, 0); err == nil {
		t.Fatal("zero recent history limit must be rejected instead of falling back to a full projection")
	}
	if err := projectActorChatWithHistoryLimit(map[string]any{}, state, actorHistoryTail, sessionProjectionMessageTail+1); err == nil {
		t.Fatal("oversized recent history limit must be rejected")
	}
	if fieldString(bounded, "settled") != "settled" || intValue(bounded["settledAt"]) != 1_787_000_000_000 {
		t.Fatalf("bounded presentation archive state = %#v", bounded)
	}

	metadataOnly := map[string]any{}
	if err := projectActorChatWithHistory(metadataOnly, state, actorHistoryMetadataOnly); err != nil {
		t.Fatal(err)
	}
	if rows := anySlice(metadataOnly["messages"]); len(rows) != 0 || intValue(metadataOnly["messageCount"]) != 85 || metadataOnly["historyComplete"] != false {
		t.Fatalf("metadata-only history = rows:%d count:%v complete:%v", len(rows), metadataOnly["messageCount"], metadataOnly["historyComplete"])
	}
	if got := int64(intValue(metadataOnly["lastActivityAt"])); got != wantLastActivity {
		t.Fatalf("metadata-only last activity = %d, want %d", got, wantLastActivity)
	}

	full := map[string]any{}
	if err := projectActorChat(full, state); err != nil {
		t.Fatal(err)
	}
	if rows := anySlice(full["messages"]); len(rows) != 85 || fieldString(mapFromAnyMain(rows[0]), "id") != "message-00" {
		t.Fatalf("full actor history was not preserved: rows=%d", len(rows))
	}
	if fieldString(full, "settled") != "settled" || intValue(full["settledAt"]) != 1_787_000_000_000 {
		t.Fatalf("full presentation archive state = %#v", full)
	}
}

func TestMetadataOnlyProjectionRetainsUncommittedForegroundRows(t *testing.T) {
	state, err := chat.NewState("running-metadata-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "running-metadata-tab"
	state.Ledger = append(state.Ledger, chat.LedgerEvent{
		MessageID: "settled-row", Role: "assistant", Text: "already settled", Status: "done",
	})
	state.Foreground = &chat.ForegroundTurn{
		OperationID: "running-operation",
		Status:      chat.ForegroundRunning,
		Input: chat.QueueEntry{
			Text: "current input",
			Presentation: providercontract.TurnPresentation{
				UserMessageID: "running-user", AssistantMessageID: "running-assistant", Origin: "human",
			},
		},
		CurrentAssistantMessageID: "running-assistant",
		RootAssistantMessageID:    "running-assistant",
		AssistantContent:          "current output",
	}

	projected := map[string]any{}
	if err := projectActorChatWithHistory(projected, state, actorHistoryMetadataOnly); err != nil {
		t.Fatal(err)
	}
	rows := anySlice(projected["messages"])
	if len(rows) != 2 || fieldString(mapFromAnyMain(rows[0]), "id") != "running-user" || fieldString(mapFromAnyMain(rows[1]), "id") != "running-assistant" {
		t.Fatalf("metadata-only running rows = %#v", rows)
	}
	if intValue(projected["messageCount"]) != 3 || projected["historyComplete"] != false {
		t.Fatalf("metadata-only running history metadata = count:%v complete:%v", projected["messageCount"], projected["historyComplete"])
	}
}

func TestSessionProjectionCarriesHistoryOnlyForActiveOrRunningChats(t *testing.T) {
	idle, err := chat.NewState("idle-chat")
	if err != nil {
		t.Fatal(err)
	}
	idle.Initialized = true
	idle.Presentation.TabID = "idle-tab"
	if got := sessionHistoryProjection(idle, "active-tab"); got != actorHistoryMetadataOnly {
		t.Fatalf("idle inactive history projection = %d", got)
	}
	if got := sessionHistoryProjection(idle, "idle-tab"); got != actorHistoryTail {
		t.Fatalf("active history projection = %d", got)
	}
	idle.Foreground = &chat.ForegroundTurn{Status: chat.ForegroundRunning}
	if got := sessionHistoryProjection(idle, "active-tab"); got != actorHistoryTail {
		t.Fatalf("running inactive history projection = %d", got)
	}
}

func TestBoundedActorSnapshotPreservesCanonicalHistoryCount(t *testing.T) {
	state, err := chat.NewState("bounded-snapshot-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "bounded-snapshot-tab"
	for index := 75; index < 85; index++ {
		state.Ledger = append(state.Ledger, chat.LedgerEvent{
			MessageID: fmt.Sprintf("message-%02d", index), Role: "assistant", Text: fmt.Sprintf("row %d", index), Status: "done",
		})
	}

	projected := map[string]any{}
	window := actorLedgerWindow{offset: 75, count: 85}
	if err := projectActorChatWithHistoryWindow(projected, state, actorHistoryTail, 10, window); err != nil {
		t.Fatal(err)
	}
	rows := anySlice(projected["messages"])
	if len(rows) != 10 || intValue(projected["messageCount"]) != 85 || projected["historyComplete"] != false {
		t.Fatalf("bounded projection metadata = rows:%d count:%v complete:%v", len(rows), projected["messageCount"], projected["historyComplete"])
	}
	if first := fieldString(mapFromAnyMain(rows[0]), "id"); first != "message-75" {
		t.Fatalf("bounded projection began at %q", first)
	}

	metadata := map[string]any{}
	if err := projectActorChatWithHistoryWindow(metadata, state, actorHistoryMetadataOnly, sessionProjectionMessageTail, window); err != nil {
		t.Fatal(err)
	}
	if rows := anySlice(metadata["messages"]); len(rows) != 0 || intValue(metadata["messageCount"]) != 85 {
		t.Fatalf("metadata projection = rows:%d count:%v", len(rows), metadata["messageCount"])
	}
}

func TestArchiveWirePagesBeforeStableActorMessageWithoutChangingLegacyFullRead(t *testing.T) {
	stateDir := t.TempDir()
	engine, err := chat.NewEngine("paged-wire-chat")
	if err != nil {
		t.Fatal(err)
	}
	messages := make([]chat.LedgerEvent, 100)
	for index := range messages {
		messages[index] = chat.LedgerEvent{
			EventID: fmt.Sprintf("event-%03d", index), MessageID: fmt.Sprintf("message-%03d", index),
			OperationID: providercontract.OperationID(fmt.Sprintf("operation-%03d", index)),
			Role:        "assistant", Text: fmt.Sprintf("row %d", index), Status: "done",
		}
	}
	if err := engine.Apply(chat.InitializeFork{
		Presentation: chat.PresentationState{TabID: "paged-wire-tab", Title: "Paged"},
		SourceChatID: "source-chat", OperationID: "paged-wire-create", Digest: "paged-wire-digest",
		Messages: messages,
	}); err != nil {
		t.Fatal(err)
	}
	runtime := &providerChatRuntime{
		manager: &acp.Manager{}, sessions: sharedSessionStore(stateDir), stateDir: stateDir,
		actors: map[string]*providerChatActor{"paged-wire-chat": {engine: engine}},
		known:  map[string]struct{}{"paged-wire-chat": {}},
	}
	hub := wire.NewHub()
	registerArchiveHandlers(hub, nil, runtime)

	rawPage, err := hub.Invoke("chat:archive-load", []any{
		"paged-wire-tab", map[string]any{"beforeMessageId": "message-060", "limit": 40},
	})
	if err != nil {
		t.Fatal(err)
	}
	page := anySlice(rawPage)
	if len(page) != 40 || fieldString(mapFromAnyMain(page[0]), "id") != "message-020" || fieldString(mapFromAnyMain(page[39]), "id") != "message-059" {
		t.Fatalf("paged archive range = %#v", page)
	}

	rawFull, err := hub.Invoke("chat:archive-load", []any{"paged-wire-tab"})
	if err != nil {
		t.Fatal(err)
	}
	full := anySlice(rawFull)
	if len(full) != 100 || fieldString(mapFromAnyMain(full[0]), "id") != "message-000" || fieldString(mapFromAnyMain(full[99]), "id") != "message-099" {
		t.Fatalf("legacy full archive range = rows:%d", len(full))
	}
	if _, err := hub.Invoke("chat:archive-load", []any{
		"paged-wire-tab", map[string]any{"beforeMessageId": "missing", "limit": 40},
	}); err == nil {
		t.Fatal("missing page boundary must fail closed")
	}
}
