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
	// The lane has seen its whole history, as a long native chat's lane has.
	laneState := state.Lanes[lane.ID]
	for _, event := range state.Ledger {
		laneState.Coverage[event.Sequence] = CoverageRecord{Sequence: event.Sequence, EventID: event.EventID, Status: CoverageNativeSeen}
	}
	laneState.CoveredThrough, laneState.InitialSeedPending = state.LedgerHead(), false
	state.Lanes[lane.ID] = laneState
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

// startLongChatTurn sends a prompt on the long chat and admits it, leaving a
// running foreground turn ready to stream.
func startLongChatTurn(b *testing.B, engine *Engine, operationID provider.OperationID) (string, func(provider.Event)) {
	b.Helper()
	if err := engine.Apply(Submit{OperationID: operationID, Text: "continue", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		b.Fatal(err)
	}
	for {
		effect, claimed, err := engine.ClaimNext()
		if err != nil || !claimed {
			b.Fatal(claimed, err)
		}
		resume, resuming := effect.(ResumeLaneEffect)
		if !resuming {
			break
		}
		// The daemon restarted since the lane last ran: reattach it exactly.
		if err := engine.Apply(LaneOpened{
			LaneID: resume.Identity.ID, Identity: resume.Identity, Thread: resume.Thread,
			ConnectionGeneration: resume.Generation, Context: exactContext(provider.ContextImportUnsupported),
		}); err != nil {
			b.Fatal(err)
		}
	}
	nativeID := "native-" + string(operationID)
	if err := engine.Apply(TurnAdmitted{OperationID: operationID, Accepted: true, Turn: provider.TurnRef{OperationID: operationID, NativeID: nativeID}}); err != nil {
		b.Fatal(err)
	}
	laneID := engine.state.Foreground.LaneID
	generation := engine.state.Lanes[laneID].ConnectionGeneration
	emit := func(event provider.Event) {
		engine.mu.Lock()
		sequence := engine.state.Lanes[laneID].LastEventSequence + 1
		engine.mu.Unlock()
		event.Identity = provider.EventIdentity{
			ChatID: engine.state.ChatID, LaneID: laneID, OperationID: operationID, TurnID: nativeID,
			Sequence: sequence, ObservedAtUnixMS: 1_791_677_100_000 + int64(sequence),
		}
		if err := engine.Apply(ProviderEventReceived{ConnectionGeneration: generation, Event: event}); err != nil {
			b.Fatalf("%s: %v", event.Kind, err)
		}
	}
	emit(provider.Event{Kind: provider.EventInputConsumed, Input: &provider.InputEvent{OperationID: operationID}})
	return nativeID, emit
}

// The per-event cost of streaming a long reply into the long chat: text
// deltas, thinking deltas and tool updates, each a journaled commit.
func BenchmarkLongChatStreaming(b *testing.B) {
	defer func(hook func(Command, []LedgerEvent, []LedgerEvent, *State)) { checkLedgerSharing = hook }(checkLedgerSharing)
	checkLedgerSharing = nil
	engine := longChatEngine(b)
	_, emit := startLongChatTurn(b, engine, "streamed-turn")
	tools := 0
	b.Run("text-delta", func(b *testing.B) {
		for range b.N {
			emit(chunk("more of the reply, a sentence at a time. "))
		}
	})
	b.Run("thinking-delta", func(b *testing.B) {
		for range b.N {
			emit(provider.Event{Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: "considering the next step "}})
		}
	})
	b.Run("tool-update", func(b *testing.B) {
		for range b.N {
			tools++
			emit(provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
				ToolCallID: fmt.Sprintf("toolu-live-%d", tools), Status: "running", Title: "Read a file",
			}})
		}
	})
	emit(completed())
	turns := 0
	b.Run("turn-end", func(b *testing.B) {
		for range b.N {
			turns++
			index := turns
			b.StopTimer()
			_, next := startLongChatTurn(b, engine, provider.OperationID(fmt.Sprintf("ending-turn-%d", index)))
			next(chunk("done"))
			b.StartTimer()
			next(completed())
		}
	})
}
