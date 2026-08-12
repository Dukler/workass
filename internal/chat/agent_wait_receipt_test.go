package chat

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	providercontract "workass/internal/provider"
)

func TestAgentWaitObservationReceiptIsDurableAndRejectsChangedIntent(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "agent-wait.json")
	engine, err := NewDurableEngine("agent-wait-chat", FileStore{Path: statePath})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(InitializeChat{
		Presentation: PresentationState{TabID: "agent-wait-tab"},
		OperationID:  "agent-wait-create", Digest: "agent-wait-create-digest",
	}); err != nil {
		t.Fatal(err)
	}
	command := RecordAgentWaitObservation{
		OperationID: providercontract.OperationID("agent-wait-once"),
		TabID:       "agent-wait-tab", Digest: strings.Repeat("a", 64),
	}
	if err := engine.Apply(command); err != nil {
		t.Fatal(err)
	}
	first := engine.Snapshot()
	receipt, ok := first.AgentWaitObservationReceipts[command.OperationID]
	if !ok || receipt.Digest != command.Digest {
		t.Fatalf("wait observation receipt = %#v", first.AgentWaitObservationReceipts)
	}
	if _, ok := first.Operations[command.OperationID]; !ok {
		t.Fatal("wait observation did not reserve its operation id")
	}
	invalidDigest := first.Clone()
	invalidReceipt := invalidDigest.AgentWaitObservationReceipts[command.OperationID]
	invalidReceipt.Digest = "valid-prefix\n"
	invalidDigest.AgentWaitObservationReceipts[command.OperationID] = invalidReceipt
	if err := invalidDigest.Validate(); err == nil {
		t.Fatal("state validation accepted a non-printable wait observation digest")
	}

	if err := engine.Apply(command); err != nil {
		t.Fatalf("same wait observation retry: %v", err)
	}
	retried := engine.Snapshot()
	if retried.AgentWaitObservationReceipts[command.OperationID] != receipt {
		t.Fatalf("same wait observation retry changed receipt: %#v", retried.AgentWaitObservationReceipts)
	}
	changed := command
	changed.Digest = strings.Repeat("b", 64)
	if err := engine.Apply(changed); err == nil || !strings.Contains(err.Error(), "different request") {
		t.Fatalf("changed wait observation was accepted: %v", err)
	}

	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "ResultExcerpt") || strings.Contains(string(encoded), "tail") {
		t.Fatalf("wait receipt persisted executor output fields: %s", encoded)
	}
	reloaded, err := NewDurableEngine("agent-wait-chat", FileStore{Path: statePath})
	if err != nil {
		t.Fatal(err)
	}
	if durable := reloaded.Snapshot().AgentWaitObservationReceipts[command.OperationID]; durable != receipt {
		t.Fatalf("reloaded wait observation receipt = %#v", durable)
	}
}
