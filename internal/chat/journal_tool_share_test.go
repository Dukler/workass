package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"workass/internal/provider"
)

func journalToolEvent(lane provider.LaneIdentity, sequence uint64, operationID provider.OperationID, toolCallID, status string) ProviderEventReceived {
	return ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind: provider.EventToolUpdate,
		Identity: provider.EventIdentity{
			ChatID: "journal-chat", LaneID: lane.ID, OperationID: operationID,
			TurnID: "journal-native-turn", Sequence: sequence, ObservedAtUnixMS: int64(1000 + sequence),
		},
		Tool: &provider.ToolEvent{ToolCallID: toolCallID, Title: "Run tests", Status: status},
	}}
}

// The engine writes a tool update's owner into the committed map in place.
// Its result must be the pure reducer's, and a failed reduction or a failed
// commit must leave the committed state byte for byte as it was, for a new
// tool call and for an update to a known one.
func TestEngineToolUpdateSharesToolsAndRollsBack(t *testing.T) {
	base, lane := largeJournalReadyState(t)
	base.Tools["known-tool"] = ToolState{Owner: ProviderActivityOwner{
		LaneID: lane.ID, OperationID: "journal-turn", TurnID: "journal-native-turn", ConnectionGeneration: 1,
	}}
	encode := func(state State) []byte {
		raw, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, toolCallID := range []string{"new-tool", "known-tool"} {
		t.Run(toolCallID, func(t *testing.T) {
			good := journalToolEvent(lane, 1, "journal-turn", " "+toolCallID+" ", "running")
			expected, err := reduceJournaledProviderEvent(base.Clone(), good)
			if err != nil {
				t.Fatalf("reference: %v", err)
			}

			engine := &Engine{state: base.Clone(), store: &failingProviderEventStore{err: errors.New("injected commit failure")}}
			before := encode(engine.state)
			if err := engine.Apply(good); err == nil {
				t.Fatal("injected commit failure was ignored")
			}
			if !bytes.Equal(encode(engine.state), before) {
				t.Fatal("a failed commit left the tool update in the committed state")
			}
			// Wrong owner: the reducer rejects the event after nothing else.
			if err := engine.Apply(journalToolEvent(lane, 1, "someone-elses-turn", toolCallID, "running")); err == nil {
				t.Fatal("a tool update for another operation was accepted")
			}
			if !bytes.Equal(encode(engine.state), before) {
				t.Fatal("a rejected tool update changed the committed state")
			}

			engine.store = &failingProviderEventStore{}
			if err := engine.Apply(good); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(engine.state, expected) {
				t.Fatal("the shared-map tool update diverged from the pure reducer")
			}
		})
	}
}
