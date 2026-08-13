package main

import (
	"fmt"
	"testing"

	"workass/internal/chat"
)

func TestActorHistoryProjectionMaterializesOnlyRequestedTail(t *testing.T) {
	state, err := chat.NewState("bounded-history-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "bounded-history-tab"
	for index := 0; index < 85; index++ {
		state.Ledger = append(state.Ledger, chat.LedgerEvent{
			MessageID: fmt.Sprintf("message-%02d", index), Role: "assistant",
			Text: fmt.Sprintf("row %d", index), Status: "done",
		})
	}

	bounded := map[string]any{}
	if err := projectActorChatWithMessageLimit(bounded, state, sessionProjectionMessageTail); err != nil {
		t.Fatal(err)
	}
	rows := anySlice(bounded["messages"])
	if len(rows) != sessionProjectionMessageTail || intValue(bounded["messageCount"]) != 85 || bounded["historyComplete"] != false {
		t.Fatalf("bounded history metadata = rows:%d count:%v complete:%v", len(rows), bounded["messageCount"], bounded["historyComplete"])
	}
	if first := fieldString(mapFromAnyMain(rows[0]), "id"); first != "message-25" {
		t.Fatalf("bounded history began at %q, want message-25", first)
	}

	full := map[string]any{}
	if err := projectActorChat(full, state); err != nil {
		t.Fatal(err)
	}
	if rows := anySlice(full["messages"]); len(rows) != 85 || fieldString(mapFromAnyMain(rows[0]), "id") != "message-00" {
		t.Fatalf("full actor history was not preserved: rows=%d", len(rows))
	}
}
