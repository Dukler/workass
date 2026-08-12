package chat

import (
	"strings"
	"testing"

	providercontract "workass/internal/provider"
)

func TestWorkspaceMutationReceiptReplaysWithoutApplyingEpochTwice(t *testing.T) {
	state, err := NewState("workspace-receipt-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "workspace-receipt-tab"
	oldCWD := "/workspace/old"
	state.Presentation.CWD = &oldCWD

	const (
		operationID = providercontract.OperationID("workspace-move-1")
		digest      = "digest-workspace-move-1"
	)
	command := ChangeWorkspace{
		OperationID: operationID, Digest: digest, CWD: "/workspace/new", ExpectedRevision: 0,
	}
	first, _, err := Reduce(state, command)
	if err != nil {
		t.Fatalf("first workspace change: %v", err)
	}
	receipt, ok := first.WorkspaceMutationReceipts[operationID]
	if !ok || receipt.Digest != digest || receipt.CWD != "/workspace/new" || receipt.Revision != 1 {
		t.Fatalf("workspace receipt = %#v", first.WorkspaceMutationReceipts)
	}
	if first.Presentation.WorkspaceRevision != 1 || first.Presentation.CWD == nil || *first.Presentation.CWD != "/workspace/new" {
		t.Fatalf("first workspace state = %#v", first.Presentation)
	}

	retry, _, err := Reduce(first, command)
	if err != nil {
		t.Fatalf("same workspace operation retry: %v", err)
	}
	retried := retry.WorkspaceMutationReceipts[operationID]
	if retried != receipt || retry.Presentation.WorkspaceRevision != first.Presentation.WorkspaceRevision || retry.Presentation.CWD == nil || *retry.Presentation.CWD != receipt.CWD {
		t.Fatalf("workspace retry changed the committed result: first=%#v retry=%#v", first.Presentation, retry.Presentation)
	}

	conflict := command
	conflict.CWD = "/workspace/other"
	conflict.Digest = "different-digest"
	if _, _, err := Reduce(first, conflict); err == nil || !strings.Contains(err.Error(), "reused for different content") {
		t.Fatalf("conflicting workspace operation was accepted: %v", err)
	}
}

func TestWorkspaceMutationReceiptSupportsCommittedNoopAndDurableRoundTrip(t *testing.T) {
	state, err := NewState("workspace-noop-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "workspace-noop-tab"
	cwd := "/workspace/same"
	state.Presentation.CWD = &cwd

	first, _, err := Reduce(state, ChangeWorkspace{
		OperationID: "workspace-noop", Digest: "workspace-noop-digest", CWD: cwd, ExpectedRevision: 0,
	})
	if err != nil {
		t.Fatalf("noop workspace change: %v", err)
	}
	receipt, ok := first.WorkspaceMutationReceipts["workspace-noop"]
	if !ok || receipt.Revision != 0 || receipt.CWD != cwd {
		t.Fatalf("noop workspace receipt = %#v", first.WorkspaceMutationReceipts)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("noop receipt invalid: %v", err)
	}
}
