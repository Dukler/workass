package chat

import (
	"context"
	"reflect"
	"testing"

	"workass/internal/provider"
	"workass/internal/store"
)

// A checkpoint compares the actor's rows with the store's copy of what is
// persisted. When the rows moved past the stored ones before the first
// checkpoint (a journal record that edits a row, as an imported file journal
// can carry), that copy must describe the stored rows, taken before the
// replay, or the edited row would look unchanged and never be written.
func TestCheckpointWritesARowAJournalReplayEdited(t *testing.T) {
	f := newProviderTurnFixture(t)
	actors := openTestActors(t)
	if err := actors.Store("chat").Save(f.state); err != nil {
		t.Fatal(err)
	}
	// A subagent's progress on the settled turn edits that turn's row.
	event := f.event("launch", "launch-job", provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{
		ToolCallID: "child-1", ToolKind: "agent", Title: "reviewer", Status: "completed",
		SubagentID: "child-1", SubagentLabel: "reviewer", SubagentHeader: true,
	}})
	record := providerEventJournalRecord{
		Version: providerEventJournalVersion, BaseRevision: f.state.Revision, Revision: f.state.Revision + 1,
		Command: ProviderEventReceived{ConnectionGeneration: 1, Event: event},
	}
	payload, err := encodeProviderEventJournalPayload(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := actors.db.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`,
			"chat", int64(record.Revision), int64(record.BaseRevision), string(payload))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Opening replays the record and checkpoints the result.
	engine, err := NewDurableEngine("chat", actors.Store("chat"))
	if err != nil {
		t.Fatal(err)
	}
	ledger := engine.Snapshot().Ledger
	if timeline := ledger[1].Timeline; len(timeline) != 1 || timeline[0].Tool == nil || timeline[0].Tool.SubagentID != "child-1" {
		t.Fatalf("replay did not edit the settled row: %#v", timeline)
	}
	page, found, err := actors.ReadMessages("chat", 0, len(ledger)+1)
	if err != nil || !found {
		t.Fatalf("read rows: %v %v", found, err)
	}
	if !reflect.DeepEqual(cloneLedgerEvents(page.Events), ledger) {
		t.Fatal("the checkpoint after the replay did not write the edited row")
	}
}
