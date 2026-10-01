package chat

import (
	"encoding/json"
	"reflect"
	"testing"

	"workass/internal/provider"
)

func rapidSteerState(t *testing.T) State {
	t.Helper()
	state, _ := NewState("rapid-steer-chat")
	lane := testLane(state.ChatID, "alpha")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID: lane.ID, Thread: provider.ThreadRef{ProviderID: "alpha", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: exactContext(provider.ContextImportNonSampling),
	})
	state, _ = apply(t, state, Submit{OperationID: "turn", Text: "work", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{OperationID: "turn", Accepted: true, Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"}})
	for _, id := range []provider.OperationID{"first", "second"} {
		state, _ = apply(t, state, Steer{OperationID: id, Text: string(id), Attachments: []provider.Attachment{{ID: string(id) + "-image", MIMEType: "image/png", Ref: "fixture.png"}},
			Presentation: provider.TurnPresentation{Origin: "human", UserMessageID: string(id), AssistantMessageID: string(id) + "-assistant"}})
		state, _ = apply(t, state, SteerAdmitted{OperationID: id, Accepted: true, AwaitConsumption: true})
	}
	return state
}

func TestQueuedSteerTransfersAndRestoresTheExactOwner(t *testing.T) {
	for _, outcome := range []string{"consumed", "rejected", "uncertain", "stale-revision"} {
		t.Run(outcome, func(t *testing.T) {
			state := rapidSteerState(t)
			rows := []StagedQueueEntry{
				{ID: "before", Text: "before"},
				{ID: "selected", Text: "daemon-owned direction", Attachments: []provider.Attachment{{ID: "image", MIMEType: "image/png", Ref: "fixture.png"}}, AttachmentNames: []string{"fixture.png"}, TargetProviderID: "alpha", ModelID: "model", ModeID: "agent", Permission: "allow"},
				{ID: "after", Text: "after"},
			}
			state, _ = apply(t, state, ReplaceStagedQueue{OperationID: "save", Digest: "rows", Entries: rows, ExpectedRevision: state.Presentation.AgentQueueRevision})
			command := Steer{OperationID: "queued-steer", Text: "renderer text", QueueID: "selected", QueueRevision: state.Presentation.AgentQueueRevision, Presentation: provider.TurnPresentation{Origin: "human", UserMessageID: "queued-user", AssistantMessageID: "queued-assistant"}}
			if outcome == "stale-revision" {
				command.QueueRevision--
				if _, _, err := Reduce(state, command); err == nil {
					t.Fatal("stale queue revision claimed a row")
				}
				if !reflect.DeepEqual(state.StagedQueue, rows) {
					t.Fatal("rejected claim changed its owner")
				}
				return
			}
			state, effects := apply(t, state, command)
			pending := state.PendingSteerFor("queued-steer")
			if pending == nil || pending.Text != rows[1].Text || !reflect.DeepEqual(pending.Attachments, rows[1].Attachments) || pending.Presentation.QueueID != "selected" || len(effects) != 1 {
				t.Fatal("transfer lost immutable daemon-owned input")
			}
			if len(state.StagedQueue) != 2 || state.StagedQueue[0].ID != "before" || state.StagedQueue[1].ID != "after" {
				t.Fatal("transfer removed another FIFO row")
			}
			if _, _, err := Reduce(state, ReplaceStagedQueue{OperationID: "resurrect", Digest: "rows", Entries: rows, ExpectedRevision: state.Presentation.AgentQueueRevision}); err == nil {
				t.Fatal("queue save duplicated a steering owner")
			}
			data, _ := json.Marshal(state)
			var restored State
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if err := restored.Validate(); err != nil {
				t.Fatal(err)
			}
			clone := restored.Clone()
			clone.PendingSteerFor("queued-steer").QueuedEntry.Attachments[0].Ref = "mutated"
			if restored.PendingSteerFor("queued-steer").QueuedEntry.Attachments[0].Ref != "fixture.png" {
				t.Fatal("snapshot aliases queue restore owner")
			}
			state = restored
			switch outcome {
			case "rejected":
				state, _ = apply(t, state, SteerFailed{OperationID: "queued-steer", Kind: provider.ErrorAdmissionRejected})
				if !reflect.DeepEqual(state.StagedQueue, rows) || state.PendingSteerFor("first") == nil {
					t.Fatal("rejection failed to restore the same row alongside direct steering")
				}
			case "uncertain":
				state, _ = apply(t, state, SteerFailed{OperationID: "queued-steer", Ambiguous: true})
				if state.PendingSteerFor("queued-steer") == nil || len(state.StagedQueue) != 2 {
					t.Fatal("ambiguous acknowledgement replayed or dropped a row")
				}
			case "consumed":
				state, _ = apply(t, state, SteerAdmitted{OperationID: "queued-steer", Accepted: true, AwaitConsumption: true})
				state, _ = apply(t, state, InputConsumed{OperationID: "queued-steer"})
				state, _ = apply(t, state, TurnCompleted{OperationID: "turn", Assistant: "done"})
				found := 0
				for _, event := range state.Ledger {
					if event.Role == "user" && event.QueueID == "selected" {
						found++
					}
				}
				if found != 1 || len(state.StagedQueue) != 2 {
					t.Fatal("consumption did not retain exactly one canonical owner")
				}
			}
		})
	}
}

func TestRapidSteersRetainIndependentOwnersThroughReceiptsAndTerminal(t *testing.T) {
	for _, boundary := range []string{"later-receipt-first", "terminal", "cancelled", "host-lost", "reject-second"} {
		t.Run(boundary, func(t *testing.T) {
			state := rapidSteerState(t)
			// Exercise the actual durable envelope and both snapshot clone paths.
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var restored State
			if err := json.Unmarshal(data, &restored); err != nil {
				t.Fatal(err)
			}
			if err := restored.Validate(); err != nil {
				t.Fatal(err)
			}
			engine := &Engine{state: restored}
			for _, snapshot := range []State{engine.Snapshot(), engine.ReadProjectionSnapshot(60).State} {
				second := snapshot.PendingSteerFor("second")
				if second == nil || len(second.Attachments) != 1 {
					t.Fatal("second steer lost its durable input")
				}
				second.Text = "changed snapshot"
				second.Attachments[0].Ref = "changed snapshot"
				if engine.state.PendingSteerFor("second").Text != "second" || engine.state.PendingSteerFor("second").Attachments[0].Ref != "fixture.png" {
					t.Fatal("snapshot aliases the second steer owner")
				}
			}
			state = restored
			switch boundary {
			case "later-receipt-first":
				state, _ = apply(t, state, InputConsumed{OperationID: "second"})
				state, _ = apply(t, state, InputConsumed{OperationID: "first"})
				state, _ = apply(t, state, SteerAdmitted{OperationID: "first", Accepted: true, AwaitConsumption: true})
				state, _ = apply(t, state, TurnCompleted{OperationID: "turn", Assistant: "done"})
			case "terminal":
				state, _ = apply(t, state, TurnCompleted{OperationID: "turn", Assistant: "done"})
			case "cancelled":
				state, _ = apply(t, state, TurnTerminated{OperationID: "turn", Status: "cancelled"})
			case "host-lost":
				store := &memoryStateStore{state: state, ok: true}
				restarted, err := NewDurableEngine(state.ChatID, store)
				if err != nil {
					t.Fatal(err)
				}
				state = restarted.Snapshot()
			case "reject-second":
				// Rejection applies only to an input still awaiting admission.
				state.PendingSteerFor("second").Status = SteerDispatching
				state, _ = apply(t, state, SteerFailed{OperationID: "second", Kind: provider.ErrorAdmissionRejected})
				if state.PendingSteerFor("first") == nil {
					t.Fatal("second rejection erased first owner")
				}
				state, _ = apply(t, state, InputConsumed{OperationID: "first"})
				state, _ = apply(t, state, TurnCompleted{OperationID: "turn", Assistant: "done"})
			}
			if state.PendingSteer != nil || state.Foreground != nil || len(state.Queue) != 0 {
				t.Fatal("terminal left or replayed a steering owner")
			}
			var ids []provider.OperationID
			for _, event := range state.Ledger {
				if event.Role == "user" && (event.OperationID == "first" || event.OperationID == "second") {
					ids = append(ids, event.OperationID)
					if len(event.Attachments) != 1 {
						t.Fatal("settlement lost steering attachments")
					}
				}
			}
			want := 2
			if boundary == "reject-second" {
				want = 1
			}
			if len(ids) != want || ids[0] != "first" || (want == 2 && ids[1] != "second") {
				t.Fatalf("steer ownership/order = %v", ids)
			}
		})
	}
}
