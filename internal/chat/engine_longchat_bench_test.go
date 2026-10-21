package chat

import (
	"fmt"
	"strings"
	"testing"

	"workass/internal/provider"
)

// longChatEngine is a durable SQLite actor shaped like the longest real chat:
// 1,434 rows with multi-kilobyte bodies and tool timelines, ~61k tool owners,
// background work and outbox history, on an established, idle lane.
func longChatEngine(b *testing.B) *Engine {
	b.Helper()
	state, _ := NewState("long-chat")
	lane := testLane("long-chat", "claude")
	var err error
	step := func(command Command) {
		b.Helper()
		if state, _, err = Reduce(state, command); err != nil {
			b.Fatalf("%T: %v", command, err)
		}
	}
	step(InitializeChat{Presentation: PresentationState{TabID: "long-tab"}, OperationID: "create", Digest: "create-digest"})
	step(SelectLane{Identity: lane})
	step(LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "claude", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportUnsupported),
	})
	body := strings.Repeat("A long reply with code, tables and prose. ", 300)
	const rows, toolsPerTurn = 1434, 85
	for index := range rows {
		turn := index / 2
		operationID := provider.OperationID(fmt.Sprintf("operation-%d", turn))
		nativeTurnID := fmt.Sprintf("native-turn-%d", turn)
		event := LedgerEvent{
			Sequence: uint64(len(state.Ledger) + 1), EventID: fmt.Sprintf("event-%d", index), MessageID: fmt.Sprintf("message-%d", index),
			OperationID: operationID, Role: "user", Text: "question", NativeTurnID: nativeTurnID,
			LaneID: lane.ID, ProviderID: "claude", Status: "done",
		}
		if index%2 == 1 {
			event.Role, event.Text = "assistant", body
			owner := ProviderActivityOwner{LaneID: lane.ID, OperationID: operationID, TurnID: nativeTurnID, ConnectionGeneration: 1}
			for tool := range toolsPerTurn {
				id := fmt.Sprintf("toolu-%d-%d", turn, tool)
				state.Tools[id] = ToolState{Owner: owner}
				if tool < 40 {
					event.Timeline = append(event.Timeline, TimelineEntry{
						Key: "tool:" + id, At: tool, Kind: provider.EventToolUpdate,
						Tool: &provider.ToolEvent{ToolCallID: id, Status: "completed", Title: "Read a file"},
					})
				}
			}
			if turn%4 == 0 {
				workID := fmt.Sprintf("work-%d", turn)
				state.Background[workID] = BackgroundState{Owner: owner}
			}
		}
		state.Ledger = append(state.Ledger, event)
	}
	if err := state.Validate(); err != nil {
		b.Fatal(err)
	}
	actors := openTestActors(b)
	if err := actors.Store(state.ChatID).Save(state); err != nil {
		b.Fatal(err)
	}
	engine, err := NewDurableEngine(state.ChatID, actors.Store(state.ChatID))
	if err != nil {
		b.Fatal(err)
	}
	return engine
}

// The cost of one ordinary command on a long chat, end to end through the
// SQLite checkpoint: a queue edit, and the record/claim/receipt sequence of
// hosting one visualization, which a chat open runs per visualization.
func BenchmarkLongChatCommand(b *testing.B) {
	defer func(hook func(Command, []LedgerEvent, []LedgerEvent, *State)) { checkLedgerSharing = hook }(checkLedgerSharing)
	checkLedgerSharing = nil
	engine := longChatEngine(b)
	next := 0 // operation ids stay unique across benchmark rounds
	b.Run("queue-edit", func(b *testing.B) {
		for range b.N {
			next++
			index := next
			revision := engine.state.Presentation.AgentQueueRevision
			if err := engine.Apply(ReplaceStagedQueue{
				OperationID: provider.OperationID(fmt.Sprintf("queue-%d", index)), Digest: fmt.Sprintf("queue-digest-%d", index),
				ExpectedRevision: revision, Entries: []StagedQueueEntry{{ID: fmt.Sprintf("row-%d", index), Text: "next"}},
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("host-visualization", func(b *testing.B) {
		for range b.N {
			next++
			index := next
			record := RecordExternalMutation{
				OperationID: provider.OperationID(fmt.Sprintf("visualize-%d", index)), Kind: "visualize:host",
				Method: "visualize-html-v1:bench", TabID: "long-tab", Digest: "bench-digest",
			}
			if err := engine.Apply(record); err != nil {
				b.Fatal(err)
			}
			if _, claimed, err := engine.ClaimEffect(externalMutationEffectID(record.OperationID)); err != nil || !claimed {
				b.Fatal(claimed, err)
			}
			if err := engine.Apply(ExternalMutationReceipt{
				OperationID: record.OperationID, Kind: record.Kind, Method: record.Method, TabID: record.TabID, Digest: record.Digest,
			}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("rehost-visualization", func(b *testing.B) {
		record := RecordExternalMutation{
			OperationID: provider.OperationID(fmt.Sprintf("visualize-%d", next)), Kind: "visualize:host", Method: "visualize-html-v1:bench", TabID: "long-tab", Digest: "bench-digest",
		}
		for range b.N {
			if err := engine.Apply(record); err != nil {
				b.Fatal(err)
			}
		}
	})
}
