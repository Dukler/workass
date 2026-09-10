package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/provider"
)

func TestCancelJournalPreservesEveryDurableBoundaryWithoutHistoryRewrite(t *testing.T) {
	for _, claim := range []string{"exact", "next"} {
		t.Run(claim, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actor.json")
			engine, lane := newJournalReadyEngine(t, path)
			chunk := ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
				Kind:      provider.EventAssistantChunk,
				Identity:  provider.EventIdentity{ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn", TurnID: "journal-native-turn", Sequence: 1},
				Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent},
			}}
			frame, err := encodeProviderEventJournalFrame(providerEventJournalRecord{Version: providerEventJournalVersion, BaseRevision: engine.state.Revision, Revision: engine.state.Revision + 1, Command: chunk})
			if err != nil {
				t.Fatal(err)
			}
			chunk.Event.Assistant.Text = strings.Repeat("x", providerEventJournalCheckpointBytes-len(providerEventJournalMagic)-len(frame)-64)
			if err := engine.Apply(chunk); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			check := func() {
				t.Helper()
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("Stop rewrote the full history snapshot")
				}
				loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
				if err != nil || !found || !equalCancelDurableState(t, loaded, engine.Snapshot()) {
					t.Fatalf("journal readback lost a committed cancellation boundary: found=%v err=%v", found, err)
				}
			}
			if err := engine.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
				t.Fatal(err)
			}
			check()
			var effect Effect
			var ok bool
			if claim == "exact" {
				effect, ok, err = engine.ClaimEffect(cancelEffectID("cancel"))
			} else {
				effect, ok, err = engine.ClaimNext()
			}
			if _, cancel := effect.(CancelTurnEffect); err != nil || !ok || !cancel {
				t.Fatalf("cancel dispatch was not durably claimed: ok=%v err=%v", ok, err)
			}
			check()
			if err := engine.Apply(CancelAcknowledged{OperationID: "cancel"}); err != nil {
				t.Fatal(err)
			}
			check()
			info, err := os.Stat(providerEventJournalPath(path))
			if err != nil || info.Size() <= providerEventJournalCheckpointBytes {
				t.Fatal("fixture did not cross the ordinary journal checkpoint threshold")
			}
			terminal := ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
				Kind:     provider.EventTurnTerminal,
				Identity: provider.EventIdentity{ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn", TurnID: "journal-native-turn", Sequence: 2},
				Terminal: &provider.TerminalEvent{Status: "cancelled", StopReason: "cancelled", FinishedAt: "2026-09-10T15:00:00Z"},
			}}
			if err := engine.Apply(terminal); err != nil {
				t.Fatal(err)
			}
			check()
			if err := engine.Apply(CancelAcknowledged{OperationID: "cancel"}); err != nil {
				t.Fatal(err)
			}
			check()
			restarted, err := NewDurableEngine("journal-chat", FileStore{Path: path})
			if err != nil || restarted.state.Foreground != nil || !outboxHas(&restarted.state, cancelEffectID("cancel"), OutboxCompleted) {
				t.Fatalf("restart lost terminal cancellation: %v", err)
			}
		})
	}
}

func TestCancelJournalFailureDoesNotCommitOrDispatch(t *testing.T) {
	for _, boundary := range []string{"intent", "claim", "ack"} {
		t.Run(boundary, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actor.json")
			engine, _ := newJournalReadyEngine(t, path)
			if boundary != "intent" {
				if err := engine.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "ack" {
				if _, ok, err := engine.ClaimEffect(cancelEffectID("cancel")); err != nil || !ok {
					t.Fatal("cannot prepare dispatch fixture")
				}
			}
			before := engine.Snapshot()
			// Checkpoint before injecting an unwritable journal, preserving the last
			// successful boundary for readback after removing the fault.
			if err := engine.store.Save(before); err != nil {
				t.Fatal(err)
			}
			journal := providerEventJournalPath(path)
			if err := os.Mkdir(journal, 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			switch boundary {
			case "intent":
				err = engine.Apply(CancelTurn{OperationID: "cancel"})
			case "claim":
				var effect Effect
				var ok bool
				effect, ok, err = engine.ClaimEffect(cancelEffectID("cancel"))
				if effect != nil || ok {
					t.Fatal("failed persistence returned a provider effect")
				}
			case "ack":
				err = engine.Apply(CancelAcknowledged{OperationID: "cancel"})
			}
			if err == nil || !reflect.DeepEqual(before, engine.Snapshot()) {
				t.Fatal("failed cancellation persistence changed the actor")
			}
			if err := os.Remove(journal); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := (FileStore{Path: path}).Load("journal-chat")
			if err != nil || !equalCancelDurableState(t, before, loaded) {
				t.Fatal("failed write damaged the previous durable boundary")
			}
		})
	}
}

func equalCancelDurableState(t *testing.T, a, b State) bool {
	t.Helper()
	// Clone normalizes nil/empty slices; JSON normalizes RawMessage(nil) and
	// RawMessage("null"). Compare the complete state after those allocations.
	x, err := json.Marshal(a.Clone())
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b.Clone())
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

func TestCancelJournalRejectsWrongChatAndNonCancelClaim(t *testing.T) {
	for _, scenario := range []string{"wrong-chat", "non-cancel-claim"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actor.json")
			engine, _ := newJournalReadyEngine(t, path)
			command := &cancelJournalCommand{ChatID: "other-chat", Intent: &CancelTurn{OperationID: "cancel"}}
			if scenario == "non-cancel-claim" {
				command = &cancelJournalCommand{ChatID: "journal-chat", Claim: &ClaimEffect{EffectID: startTurnEffectID("journal-turn")}}
			}
			frame, err := encodeProviderEventJournalFrame(providerEventJournalRecord{Version: cancelJournalVersion, BaseRevision: engine.state.Revision, Revision: engine.state.Revision + 1, Cancel: command})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appendProviderEventJournalFrame(path, frame, true); err != nil {
				t.Fatal(err)
			}
			if _, _, err := (FileStore{Path: path}).Load("journal-chat"); err == nil {
				t.Fatal("invalid cancellation journal ownership was accepted")
			}
		})
	}
}

func TestCancelJournalPendingInputAndFailureReadback(t *testing.T) {
	for _, boundary := range []string{"pending-input", "provider-failure"} {
		t.Run(boundary, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actor.json")
			var engine *Engine
			var command Command
			if boundary == "pending-input" {
				var err error
				engine, err = NewDurableEngine("journal-chat", FileStore{Path: path})
				if err != nil {
					t.Fatal(err)
				}
				if err := engine.Apply(SelectLane{Identity: testLane("journal-chat", "codex")}); err != nil {
					t.Fatal(err)
				}
				if err := engine.Apply(Submit{OperationID: "pending", Text: "never dispatched", Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
					t.Fatal(err)
				}
				command = CancelPendingTurn{OperationID: "pending"}
			} else {
				engine, _ = newJournalReadyEngine(t, path)
				if err := engine.Apply(CancelTurn{OperationID: "cancel"}); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := engine.ClaimEffect(cancelEffectID("cancel")); err != nil || !ok {
					t.Fatal("cannot prepare cancellation failure")
				}
				command = CancelFailed{OperationID: "cancel", Kind: provider.ErrorTransientTransport}
			}
			before, _ := os.ReadFile(path)
			if err := engine.Apply(command); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("cancellation boundary rewrote the transcript")
			}
			loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
			if err != nil || !found || !equalCancelDurableState(t, loaded, engine.Snapshot()) {
				t.Fatalf("cancellation boundary did not survive readback: %v", err)
			}
		})
	}
}
