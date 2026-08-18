package main

import (
	"fmt"
	"testing"

	"workass/internal/chat"
	providercontract "workass/internal/provider"
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
		state.Ledger = append(state.Ledger, chat.LedgerEvent{
			MessageID: fmt.Sprintf("message-%02d", index), Role: "assistant",
			Text: fmt.Sprintf("row %d", index), Status: "done",
		})
	}

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
				UserMessageID: "running-user", AssistantMessageID: "running-assistant",
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
