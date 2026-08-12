package chat

import "testing"

func TestDeleteChatRetryRequiresTheOriginalOperationIdentity(t *testing.T) {
	state, _ := NewState("delete-retry-chat")
	initialized, _, err := Reduce(state, InitializeChat{
		Presentation: PresentationState{TabID: "delete-retry-tab"},
		OperationID:  "create-delete-retry",
		Digest:       "delete-retry-digest",
	})
	if err != nil {
		t.Fatalf("initialize chat: %v", err)
	}

	const operationID = "delete-retry-operation"
	deleted, effects, err := Reduce(initialized, DeleteChat{OperationID: operationID, Force: true})
	if err != nil || !deleted.Deleted || len(effects) != 1 {
		t.Fatalf("delete chat: state=%#v effects=%#v err=%v", deleted, effects, err)
	}

	retried, effects, err := Reduce(deleted, DeleteChat{OperationID: operationID, Force: true})
	if err != nil || len(effects) != 0 || retried.DeletionOperationID != operationID {
		t.Fatalf("same-operation delete retry: state=%#v effects=%#v err=%v", retried, effects, err)
	}
	if _, _, err := Reduce(deleted, DeleteChat{OperationID: "delete-retry-substitute", Force: true}); err == nil {
		t.Fatal("a different operation id was accepted against the durable tombstone")
	}
}
