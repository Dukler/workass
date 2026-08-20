package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

// These tests intentionally pin the actor boundaries that are easy to bypass
// when a receipt arrives before its durable effect claim or carries a forged
// provider owner. They are fail-first review tests for the current refactor.

func TestReceiptReviewExternalAmbiguousReceiptRequiresDispatch(t *testing.T) {
	state, err := NewState("external-receipt-review-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "external-receipt-review-tab"
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	command := RecordExternalMutation{
		OperationID: "external-review-once",
		Kind:        "browser",
		Method:      "click",
		TabID:       state.Presentation.TabID,
		Digest:      digest,
	}
	state, _, err = Reduce(state, command)
	if err != nil {
		t.Fatalf("journal external mutation: %v", err)
	}
	_, _, err = Reduce(state, ExternalMutationReceipt{
		OperationID: command.OperationID,
		Kind:        command.Kind,
		Method:      command.Method,
		TabID:       command.TabID,
		Digest:      digest,
		Ambiguous:   true,
	})
	if err == nil || !strings.Contains(err.Error(), "before durable dispatch") {
		t.Fatalf("pre-dispatch ambiguous external receipt was accepted: %v", err)
	}
}

func TestReceiptReviewDetachFailureRequiresDispatch(t *testing.T) {
	state, identity, owner, _, connectionID := newDetachTestState(t)
	operationID := DetachOperationID(state.ChatID, identity.ID, connectionID, 7)
	var err error
	state, _, err = Reduce(state, DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: owner,
		ConnectionID: connectionID, ConnectionGeneration: 7,
	})
	if err != nil {
		t.Fatalf("journal detach: %v", err)
	}
	_, _, err = Reduce(state, DetachLaneFailed{
		OperationID: operationID, LaneID: identity.ID, ConnectionID: connectionID,
		ConnectionGeneration: 7, Kind: provider.ErrorTransientTransport,
	})
	if err == nil {
		t.Fatal("pre-dispatch detach failure receipt was accepted")
	}
}

func TestReceiptReviewCancelReceiptMustReserveOperation(t *testing.T) {
	state, err := NewState("cancel-receipt-review-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.CancelMutationReceipts["unreserved-cancel"] = CancelMutationReceipt{
		Cancelled: false,
		Reason:    "idle",
	}
	if err := state.Validate(); err == nil || !strings.Contains(err.Error(), "cancel mutation receipt") {
		t.Fatalf("unreserved cancel receipt survived validation: %v", err)
	}
}

func TestReceiptReviewAgentWaitReceiptSchemaKeepsCommandBounds(t *testing.T) {
	state, err := NewState("agent-wait-receipt-review-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Operations["agent-wait-review"] = struct{}{}
	state.AgentWaitObservationReceipts["agent-wait-review"] = AgentWaitObservationReceipt{
		Digest: strings.Repeat("a", 257),
	}
	if err := state.Validate(); err == nil || !strings.Contains(err.Error(), "agent wait observation receipt") {
		t.Fatalf("oversized agent wait receipt survived validation: %v", err)
	}
}

func TestReceiptReviewAgentWaitReceiptDigestMatchesReducerBounds(t *testing.T) {
	tests := []struct {
		name      string
		digest    string
		wantValid bool
	}{
		{name: "empty", digest: "", wantValid: false},
		{name: "space", digest: " ", wantValid: false},
		{name: "control", digest: "valid\n", wantValid: false},
		{name: "delete", digest: string([]byte{0x7f}), wantValid: false},
		{name: "one-byte-lower-bound", digest: string([]byte{0x21}), wantValid: true},
		{name: "one-byte-upper-bound", digest: string([]byte{0x7e}), wantValid: true},
		{name: "maximum", digest: strings.Repeat("~", 256), wantValid: true},
		{name: "too-long", digest: strings.Repeat("~", 257), wantValid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, err := NewState("agent-wait-digest-bounds-" + test.name)
			if err != nil {
				t.Fatal(err)
			}
			state.Operations["agent-wait-bounds"] = struct{}{}
			state.AgentWaitObservationReceipts["agent-wait-bounds"] = AgentWaitObservationReceipt{Digest: test.digest}
			err = state.Validate()
			if test.wantValid && err != nil {
				t.Fatalf("valid reducer digest rejected by persisted schema: %v", err)
			}
			if !test.wantValid && err == nil {
				t.Fatal("invalid reducer digest accepted by persisted schema")
			}
		})
	}
}

func TestReceiptReviewDeleteTombstonePersistsAndRejectsTampering(t *testing.T) {
	state, err := NewState("delete-receipt-review-chat")
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = Reduce(state, InitializeChat{
		Presentation: PresentationState{TabID: "delete-receipt-review-tab"},
		OperationID:  "delete-receipt-review-create", Digest: "delete-receipt-review-create-digest",
	})
	if err != nil {
		t.Fatalf("initialize chat: %v", err)
	}
	const deletionOperationID provider.OperationID = "delete-receipt-review-operation"
	state, _, err = Reduce(state, DeleteChat{OperationID: deletionOperationID, Force: true})
	if err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("valid deleted state rejected: %v", err)
	}

	store := FileStore{Path: filepath.Join(t.TempDir(), "chat-state.json")}
	if err := store.Save(state); err != nil {
		t.Fatalf("persist deleted state: %v", err)
	}
	reloaded, ok, err := store.Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("reload deleted state: ok=%v err=%v", ok, err)
	}
	if reloaded.DeletionOperationID != deletionOperationID || len(reloaded.Outbox) != 1 {
		t.Fatalf("reloaded delete receipt = operation=%q outbox=%d", reloaded.DeletionOperationID, len(reloaded.Outbox))
	}
	deleteEffect := reloaded.Outbox[0]
	if deleteEffect.ID != deleteChatEffectID(reloaded.ChatID) ||
		deleteEffect.OperationID != reloaded.DeletionOperationID ||
		deleteEffect.ChatID != reloaded.ChatID ||
		deleteEffect.TabID != reloaded.Presentation.TabID || deleteEffect.LaneID != "" {
		t.Fatalf("reloaded delete effect identity = %#v", deleteEffect)
	}

	tamperCases := []struct {
		name   string
		mutate func(*State)
	}{
		{name: "empty tombstone operation", mutate: func(state *State) { state.DeletionOperationID = "" }},
		{name: "unnormalized tombstone operation", mutate: func(state *State) {
			state.DeletionOperationID = provider.OperationID(" " + string(deletionOperationID))
		}},
		{name: "missing delete effect", mutate: func(state *State) { state.Outbox = nil }},
		{name: "wrong effect id", mutate: func(state *State) { state.Outbox[0].ID = "chat-delete:forged" }},
		{name: "wrong operation id", mutate: func(state *State) { state.Outbox[0].OperationID = "forged-delete-operation" }},
		{name: "wrong chat id", mutate: func(state *State) { state.Outbox[0].ChatID = "forged-delete-chat" }},
		{name: "wrong tab id", mutate: func(state *State) { state.Outbox[0].TabID = "forged-delete-tab" }},
		{name: "unexpected lane id", mutate: func(state *State) { state.Outbox[0].LaneID = "forged-delete-lane" }},
	}
	for _, test := range tamperCases {
		t.Run(test.name, func(t *testing.T) {
			tampered := reloaded.Clone()
			test.mutate(&tampered)
			raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion, State: tampered})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "tampered-state.json")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := (FileStore{Path: path}).Load(reloaded.ChatID); err == nil {
				t.Fatal("tampered persisted delete state was accepted")
			}
		})
	}
}

func TestReceiptReviewDetachOperationIdentityIsSchemaFenced(t *testing.T) {
	state, identity, owner, _, connectionID := newDetachTestState(t)
	state.Operations["wrong-detach-operation"] = struct{}{}
	state.Outbox = append(state.Outbox, OutboxEntry{
		ID: "wrong-detach-operation", Kind: EffectDetachLane, Status: OutboxPending,
		LaneID: identity.ID, OperationID: "wrong-detach-operation", Owner: owner,
		ConnectionID: connectionID, Generation: 7,
	})
	if err := state.Validate(); err == nil || !strings.Contains(err.Error(), "detach") {
		t.Fatalf("detach receipt with a mismatched operation identity survived validation: %v", err)
	}
}

func TestReceiptReviewProviderBackgroundOwnerMustBeKnown(t *testing.T) {
	state, identity, _, _, _ := newDetachTestState(t)
	next, _, err := Reduce(state, ProviderEventReceived{
		ConnectionGeneration: 7,
		Event: provider.Event{
			Kind: provider.EventBackgroundWork,
			Identity: provider.EventIdentity{
				ChatID: state.ChatID, LaneID: identity.ID,
				OperationID: "forged-background-operation", TurnID: "forged-native-turn", Sequence: 1,
			},
			Background: &provider.BackgroundEvent{WorkID: "forged-work", Status: "running"},
		},
	})
	if err == nil {
		t.Fatalf("provider background activity with an unknown operation owner was accepted: %#v", next.Background)
	}
}
