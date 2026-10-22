package chat

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Re-observing the same Entorno commits nothing; any difference, and every
// rejection, is exactly what Reduce decides.
func TestUnchangedEnvironmentObservationIsNotCommitted(t *testing.T) {
	observed := UpdateEnvironment{
		ExpectedTabID: "env-tab", CWD: "/work/repo",
		Payload: json.RawMessage(`{"repos":[]}`), Checkpoints: json.RawMessage(`[]`), Reference: json.RawMessage(`{"base":"a"}`),
	}
	variants := []UpdateEnvironment{observed}
	for _, alter := range []func(*UpdateEnvironment){
		func(c *UpdateEnvironment) { c.CWD = "/work/other" },
		func(c *UpdateEnvironment) { c.CWD = " /work/repo " },
		func(c *UpdateEnvironment) { c.Payload = json.RawMessage(`{"repos":[{"path":"x"}]}`) },
		func(c *UpdateEnvironment) { c.Checkpoints = json.RawMessage(`[{"turnSeq":1}]`) },
		func(c *UpdateEnvironment) { c.Reference = nil },
		func(c *UpdateEnvironment) { c.Reference = json.RawMessage(`{"base":"b"}`) },
		func(c *UpdateEnvironment) { c.ExpectedTabID = "other-tab" },
		func(c *UpdateEnvironment) { c.Payload = nil },
		func(c *UpdateEnvironment) { c.Payload = json.RawMessage(`{`) },
	} {
		variant := observed
		alter(&variant)
		variants = append(variants, variant)
	}
	skipped, committed, rejected := 0, 0, 0
	for index, command := range variants {
		store := &countingStateStore{}
		engine, err := NewDurableEngine("env-chat", store)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(InitializeChat{Presentation: PresentationState{TabID: "env-tab"}, OperationID: "create", Digest: "d"}); err != nil {
			t.Fatal(err)
		}
		if err := engine.Apply(observed); err != nil {
			t.Fatal(err)
		}
		before, saves := engine.Snapshot(), store.saves
		expected, _, expectedErr := Reduce(before, command)
		err = engine.Apply(command)
		if (err == nil) != (expectedErr == nil) || err != nil && err.Error() != expectedErr.Error() {
			t.Fatalf("variant %d: Apply error %v, Reduce error %v", index, err, expectedErr)
		}
		after := engine.Snapshot()
		switch {
		case err != nil:
			rejected++
			if !reflect.DeepEqual(after, before) || store.saves != saves {
				t.Fatalf("variant %d: a rejected update changed the actor", index)
			}
		case after.Revision == before.Revision:
			skipped++
			expected.Revision, expected.Environment.Revision = before.Revision, before.Environment.Revision
			if !reflect.DeepEqual(expected, before) || store.saves != saves {
				t.Fatalf("variant %d: skipped an update that changes the actor", index)
			}
		default:
			committed++
			if !reflect.DeepEqual(after, expected) || store.saves != saves+1 {
				t.Fatalf("variant %d: committed state differs from Reduce", index)
			}
		}
	}
	// Skipped: the identical observation, and the one whose CWD differs only
	// by the padding the reducer trims.
	if skipped != 2 || committed == 0 || rejected == 0 {
		t.Fatalf("skipped=%d committed=%d rejected=%d", skipped, committed, rejected)
	}
}
