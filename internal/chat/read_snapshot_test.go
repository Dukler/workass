package chat

import (
	"encoding/json"
	"reflect"
	"testing"

	"workass/internal/provider"
)

// ReadSnapshot shares the committed ledger rows and copies everything else;
// later commits, which never edit committed rows in place, leave the rows an
// earlier reader holds exactly as they were.
func TestReadSnapshotSharesOnlyImmutableLedgerRows(t *testing.T) {
	f := newProviderTurnFixture(t)
	engine := &Engine{state: f.state}
	held := engine.ReadSnapshot()
	if len(held.Ledger) == 0 || &held.Ledger[0] != &engine.state.Ledger[0] {
		t.Fatal("ReadSnapshot copied the ledger rows")
	}
	if !reflect.DeepEqual(held, engine.state.cloneSharingLedger()) {
		t.Fatal("ReadSnapshot is not the committed state")
	}
	before, err := json.Marshal(held.Ledger)
	if err != nil {
		t.Fatal(err)
	}

	// The copy's maps and slices other than the rows are its own.
	held.Tools["reader-tool"] = ToolState{}
	held.Operations["reader-operation"] = struct{}{}
	held.Lanes[f.lane.ID] = LaneState{}
	if _, leaked := engine.state.Tools["reader-tool"]; leaked {
		t.Fatal("a reader's map write reached the committed state")
	}
	if _, leaked := engine.state.Operations["reader-operation"]; leaked {
		t.Fatal("a reader's map write reached the committed state")
	}
	if engine.state.Lanes[f.lane.ID].Identity.ID != f.lane.ID {
		t.Fatal("a reader's lane write reached the committed state")
	}

	// A turn that rewrites and appends ledger rows commits new rows.
	f.state = engine.state
	f.start("reader-turn")
	f.emit(provider.ProviderTurnOperationID("reader-turn"), "reader-turn", chunk("a later reply"))
	f.emit(provider.ProviderTurnOperationID("reader-turn"), "reader-turn", completed())
	if len(f.state.Ledger) <= len(held.Ledger) {
		t.Fatal("the turn did not add ledger rows")
	}
	after, err := json.Marshal(held.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a later commit changed rows an earlier reader holds")
	}
}
