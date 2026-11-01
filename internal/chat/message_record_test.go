package chat

import (
	"encoding/json"
	"testing"

	"workass/internal/provider"
)

func TestUserMessageRecordReadsOnlyTheDaemonsOwnRecord(t *testing.T) {
	state := State{
		ChatID: "chat-1",
		Ledger: []LedgerEvent{
			{MessageID: "human", Role: "user", Text: "update it", At: "2026-11-01T00:00:00Z", Origin: "human"},
			{MessageID: "agent", Role: "user", Text: "update it", Origin: "agent"},
			{MessageID: "legacy", Role: "user", Text: "update it"},
			{MessageID: "notice", Role: "user", Text: "update it", Origin: "agent", NoticeKind: "subagent"},
			{MessageID: "fork", Role: "user", Text: "update it", Origin: "human", Inherited: true},
			{MessageID: "fork-excluded", Role: "user", Text: "update it", Origin: "human", ContextExcluded: true},
			{MessageID: "stopped", Role: "user", Text: "update it", Origin: "human", TerminalState: "cancelled_before_admission"},
			{MessageID: "assistant", Role: "assistant", Text: "update it", Origin: "human"},
		},
		Queue: []QueueEntry{{OperationID: "op-q", Text: "update it", Presentation: provider.TurnPresentation{UserMessageID: "queued", Origin: "human"}}},
		Foreground: &ForegroundTurn{
			StartedAt: "2026-11-01T01:00:00Z",
			Input:     QueueEntry{Text: "update running", Presentation: provider.TurnPresentation{UserMessageID: "running", Origin: "human"}},
		},
		PendingSteer: &PendingSteer{
			Text: "first", Presentation: provider.TurnPresentation{UserMessageID: "steer-1", Origin: "agent"},
			Next: &PendingSteer{Text: "update second", Presentation: provider.TurnPresentation{UserMessageID: "steer-2", Origin: "human", StartedAt: "2026-11-01T02:00:00Z"}},
		},
	}
	for id, want := range map[string]UserMessageRecord{
		"human":         {MessageID: "human", Text: "update it", At: "2026-11-01T00:00:00Z", Origin: "human"},
		"agent":         {MessageID: "agent", Text: "update it", Origin: "agent"},
		"legacy":        {MessageID: "legacy", Text: "update it"},
		"notice":        {MessageID: "notice", Text: "update it", Origin: "agent", Notice: true},
		"fork":          {MessageID: "fork", Text: "update it", Origin: "human", Copied: true},
		"fork-excluded": {MessageID: "fork-excluded", Text: "update it", Origin: "human", Copied: true},
		"stopped":       {MessageID: "stopped", Text: "update it", Origin: "human", Withdrawn: true},
		"running":       {MessageID: "running", Text: "update running", At: "2026-11-01T01:00:00Z", Origin: "human"},
		"steer-1":       {MessageID: "steer-1", Text: "first", Origin: "agent"},
		"steer-2":       {MessageID: "steer-2", Text: "update second", At: "2026-11-01T02:00:00Z", Origin: "human"},
	} {
		got, ok := state.UserMessageRecord(id)
		if !ok || got != want {
			t.Fatalf("%s = %#v ok=%v, want %#v", id, got, ok, want)
		}
	}
	for _, id := range []string{"", "assistant", "queued", "elsewhere"} {
		if got, ok := state.UserMessageRecord(id); ok {
			t.Fatalf("%q was found: %#v", id, got)
		}
	}
	state.Deleted = true
	if _, ok := state.UserMessageRecord("human"); ok {
		t.Fatal("a deleted chat still answered for its messages")
	}
}

// Every path that writes an input's row carries who wrote it. A path that forgot
// would read as unknown and refuse, which is safe but would lose a real permission.
func TestEveryPathThatWritesAnInputRecordsItsOrigin(t *testing.T) {
	presentation := provider.TurnPresentation{UserMessageID: "m", Origin: "human", StartedAt: "2026-11-01T00:00:00Z"}
	foreground := func() *ForegroundTurn {
		return &ForegroundTurn{OperationID: "op-m", Input: QueueEntry{OperationID: "op-m", Text: "update it", Presentation: presentation}}
	}
	empty := func() State {
		return State{ChatID: "c", Foreground: foreground(), Lanes: map[provider.LaneID]LaneState{}}
	}
	for name, write := range map[string]func(*State){
		"consumed turn": func(state *State) { _, _ = appendForegroundUser(state) },
		"consumed steer": func(state *State) {
			_, _ = appendConsumedSteer(state, &PendingSteer{OperationID: "op-m", Text: "update it", Presentation: presentation})
		},
		"unconsumed steer": func(state *State) {
			_ = appendUnconsumedSteerAtTerminal(state, &PendingSteer{OperationID: "op-m", Text: "update it", Presentation: presentation})
		},
		"cancelled before admission": func(state *State) {
			state.Lanes["lane"] = LaneState{}
			_ = appendPreAdmissionCancellation(state, QueueEntry{OperationID: "op-m", LaneID: "lane", Text: "update it", Presentation: presentation})
		},
	} {
		state := empty()
		write(&state)
		if len(state.Ledger) == 0 {
			t.Fatalf("%s wrote no row", name)
		}
		if state.Ledger[0].Origin != "human" {
			t.Fatalf("%s lost the origin: %#v", name, state.Ledger[0])
		}
	}
}

func TestRecordedOriginSurvivesTheStoreAndIsAdditive(t *testing.T) {
	raw, err := json.Marshal(LedgerEvent{MessageID: "m", Role: "user", Origin: "human"})
	if err != nil {
		t.Fatal(err)
	}
	var restored LedgerEvent
	if err := json.Unmarshal(raw, &restored); err != nil || restored.Origin != "human" {
		t.Fatalf("restored = %#v err=%v", restored, err)
	}
	// A row from before the field reads as unknown, and a row with no origin adds
	// no key to the stored document.
	var legacy LedgerEvent
	if err := json.Unmarshal([]byte(`{"MessageID":"m","Role":"user"}`), &legacy); err != nil || legacy.Origin != "" {
		t.Fatalf("legacy = %#v err=%v", legacy, err)
	}
	empty, _ := json.Marshal(LedgerEvent{MessageID: "m"})
	var keys map[string]any
	if err := json.Unmarshal(empty, &keys); err != nil {
		t.Fatal(err)
	}
	if _, present := keys["Origin"]; present {
		t.Fatal("an empty origin was written to the store")
	}
}
