package chat

import (
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
