package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	providercontract "workass/internal/provider"
)

func TestRecordCancelReceiptIsIdempotentAndFenced(t *testing.T) {
	state, err := NewState("cancel-receipt-chat")
	if err != nil {
		t.Fatal(err)
	}
	command := RecordCancelReceipt{OperationID: "idle-cancel", Cancelled: false, Reason: "idle"}

	first, effects, err := Reduce(state, command)
	if err != nil {
		t.Fatalf("record idle cancel: %v", err)
	}
	if len(effects) != 0 {
		t.Fatalf("idle cancel produced provider effects: %#v", effects)
	}
	receipt, ok := first.CancelMutationReceipts[providercontract.OperationID("idle-cancel")]
	if !ok || receipt.Cancelled || receipt.Reason != "idle" {
		t.Fatalf("idle cancel receipt = %#v", first.CancelMutationReceipts)
	}
	if _, ok := first.Operations[providercontract.OperationID("idle-cancel")]; !ok {
		t.Fatal("idle cancel did not reserve its operation id")
	}

	retry, effects, err := Reduce(first, command)
	if err != nil {
		t.Fatalf("retry idle cancel: %v", err)
	}
	if len(effects) != 0 || retry.CancelMutationReceipts["idle-cancel"] != receipt {
		t.Fatalf("idle cancel retry changed receipt: %#v", retry.CancelMutationReceipts)
	}

	conflict := command
	conflict.Reason = "cancelled"
	if _, _, err := Reduce(first, conflict); err == nil || !strings.Contains(err.Error(), "different result") {
		t.Fatalf("conflicting idle cancel receipt was accepted: %v", err)
	}

	cloned := first.Clone()
	cloned.CancelMutationReceipts["idle-cancel"] = CancelMutationReceipt{Reason: "changed"}
	if first.CancelMutationReceipts["idle-cancel"].Reason != "idle" {
		t.Fatal("state clone shares cancel receipt map storage")
	}
	invalid := first.Clone()
	invalid.CancelMutationReceipts["invalid"] = CancelMutationReceipt{Cancelled: true, Reason: "idle"}
	if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "cancel mutation receipt") {
		t.Fatalf("invalid cancel receipt survived validation: %v", err)
	}
}

func TestFileStoreNormalizesPreV17CancelReceiptMap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat", "state.json")
	state, err := NewState("legacy-cancel-receipt-chat")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a v16 actor, whose JSON had no cancel receipt map.
	state.CancelMutationReceipts = nil
	raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion - 1, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, ok, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load pre-v17 actor: ok=%v err=%v", ok, err)
	}
	if loaded.CancelMutationReceipts == nil {
		t.Fatal("pre-v17 actor did not receive an initialized cancel receipt map")
	}
	if err := (FileStore{Path: path}).Save(loaded); err != nil {
		t.Fatal(err)
	}
	savedRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved stateEnvelope
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Version != currentStateEnvelopeVersion {
		t.Fatalf("saved actor version = %d, want %d", saved.Version, currentStateEnvelopeVersion)
	}
}
