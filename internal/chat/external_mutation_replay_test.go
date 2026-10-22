package chat

import (
	"encoding/json"
	"reflect"
	"testing"

	"workass/internal/provider"
)

type countingStateStore struct {
	memoryStateStore
	saves int
}

func (s *countingStateStore) Save(state State) error {
	s.saves++
	return s.memoryStateStore.Save(state)
}

// externalMutationReplayEngine holds one journaled operation in each outbox
// status an external mutation can reach.
func externalMutationReplayEngine(t *testing.T) (*Engine, *countingStateStore) {
	t.Helper()
	store := &countingStateStore{}
	engine, err := NewDurableEngine("replay-chat", store)
	if err != nil {
		t.Fatal(err)
	}
	apply := func(command Command) {
		t.Helper()
		if err := engine.Apply(command); err != nil {
			t.Fatalf("%T: %v", command, err)
		}
	}
	apply(InitializeChat{Presentation: PresentationState{TabID: "replay-tab"}, OperationID: "create", Digest: "create-digest"})
	for _, status := range []string{"pending", "dispatched", "completed", "completed-result", "failed", "ambiguous"} {
		record := replayRecord(status)
		apply(record)
		if status == "pending" {
			continue
		}
		if _, claimed, err := engine.ClaimEffect(externalMutationEffectID(record.OperationID)); err != nil || !claimed {
			t.Fatalf("claim %s: %v %v", status, claimed, err)
		}
		receipt := replayReceipt(record)
		switch status {
		case "completed":
		case "completed-result":
			receipt.Result = json.RawMessage(`{"url":"/workass/artifacts/a"}`)
		case "failed":
			receipt.Failed, receipt.ErrorKind = true, provider.ErrorAdmissionRejected
		case "ambiguous":
			receipt.Ambiguous = true
		default:
			continue
		}
		apply(receipt)
	}
	return engine, store
}

func replayRecord(status string) RecordExternalMutation {
	return RecordExternalMutation{
		OperationID: provider.OperationID("op-" + status), Kind: "visualize:host", Method: "visualize-html-v1:" + status,
		TabID: "replay-tab", Digest: "digest-" + status,
	}
}

func replayReceipt(record RecordExternalMutation) ExternalMutationReceipt {
	return ExternalMutationReceipt{
		OperationID: record.OperationID, Kind: record.Kind, Method: record.Method, TabID: record.TabID, Digest: record.Digest,
	}
}

// Every record and receipt a caller can send for a journaled operation, exact
// or altered in each field: the engine either commits exactly what Reduce
// produces or, only when Reduce would change nothing but the revision, commits
// nothing at all. Rejections are unchanged.
func TestExternalMutationReplayChangesNothingOnlyWhenReduceWouldNot(t *testing.T) {
	statuses := []string{"pending", "dispatched", "completed", "completed-result", "failed", "ambiguous", "missing"}
	var commands []Command
	for _, status := range statuses {
		record := replayRecord(status)
		variants := []RecordExternalMutation{record}
		for _, alter := range []func(*RecordExternalMutation){
			func(c *RecordExternalMutation) { c.Kind = "browser:navigate" },
			func(c *RecordExternalMutation) { c.Method = "other" },
			func(c *RecordExternalMutation) { c.TabID = "other-tab" },
			func(c *RecordExternalMutation) { c.Digest = "other" },
			func(c *RecordExternalMutation) { c.Digest = " DIGEST-" + status + " " },
			func(c *RecordExternalMutation) { c.OperationID = " " + c.OperationID + " " },
			func(c *RecordExternalMutation) { c.Method = "" },
		} {
			variant := record
			alter(&variant)
			variants = append(variants, variant)
		}
		for _, variant := range variants {
			commands = append(commands, variant)
			receipt := replayReceipt(variant)
			commands = append(commands, receipt)
			for _, alter := range []func(*ExternalMutationReceipt){
				func(c *ExternalMutationReceipt) { c.Failed = true },
				func(c *ExternalMutationReceipt) { c.Ambiguous = true },
				func(c *ExternalMutationReceipt) { c.Failed, c.Ambiguous = true, true },
				func(c *ExternalMutationReceipt) { c.Result = json.RawMessage(`{"url":"/workass/artifacts/a"}`) },
				func(c *ExternalMutationReceipt) { c.Result = json.RawMessage(`{"url":"/workass/artifacts/b"}`) },
				func(c *ExternalMutationReceipt) { c.Result = json.RawMessage(`not json`) },
				func(c *ExternalMutationReceipt) {
					c.Ambiguous, c.Result = true, json.RawMessage(`{"url":"/workass/artifacts/a"}`)
				},
			} {
				altered := receipt
				alter(&altered)
				commands = append(commands, altered)
			}
		}
	}
	skipped, committed, rejected := 0, 0, 0
	for index, command := range commands {
		engine, store := externalMutationReplayEngine(t)
		before := engine.Snapshot()
		saves := store.saves
		expected, _, expectedErr := Reduce(before, command)
		err := engine.Apply(command)
		if (err == nil) != (expectedErr == nil) || err != nil && err.Error() != expectedErr.Error() {
			t.Fatalf("command %d %#v: Apply error %v, Reduce error %v", index, command, err, expectedErr)
		}
		after := engine.Snapshot()
		switch {
		case err != nil:
			rejected++
			if !reflect.DeepEqual(after, before) || store.saves != saves {
				t.Fatalf("command %d: rejected command changed the actor", index)
			}
		case after.Revision == before.Revision:
			skipped++
			expected.Revision = before.Revision
			if !reflect.DeepEqual(expected, before) {
				t.Fatalf("command %d %#v: skipped a transition that changes the actor", index, command)
			}
			if store.saves != saves {
				t.Fatalf("command %d: unchanged replay was written", index)
			}
		default:
			committed++
			if !reflect.DeepEqual(after, expected) || store.saves != saves+1 {
				t.Fatalf("command %d %#v: committed state differs from Reduce", index, command)
			}
		}
	}
	if skipped == 0 || committed == 0 || rejected == 0 {
		t.Fatalf("cases did not cover every outcome: skipped=%d committed=%d rejected=%d", skipped, committed, rejected)
	}
	if testing.Verbose() {
		t.Logf("%d commands: %d unchanged and skipped, %d committed, %d rejected", len(commands), skipped, committed, rejected)
	}
}

func TestExternalMutationReplayOnDeletedChatIsStillRefused(t *testing.T) {
	engine, _ := externalMutationReplayEngine(t)
	if err := engine.Apply(DeleteChat{OperationID: "delete"}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"completed", "failed"} {
		record := replayRecord(status)
		for _, command := range []Command{record, replayReceipt(record)} {
			if err := engine.Apply(command); err == nil {
				t.Fatalf("%s %T on a deleted chat was accepted", status, command)
			}
		}
	}
}
