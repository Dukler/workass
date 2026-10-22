package chat

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"workass/internal/provider"
	"workass/internal/store"
)

func TestSQLCheckpointWritesOnlyTheToolOwnersThatChanged(t *testing.T) {
	actors := openTestActors(t)
	const chatID = "tool-owners"
	state := quiescentScriptedActor(t, actors, chatID).Snapshot()
	first := state.Tools["tool-1"]
	second := first
	second.Owner.OperationID, second.Owner.TurnID = provider.OperationID("turn-2"), "native-2"
	for index := 0; index < 2_000; index++ {
		state.Tools[fmt.Sprintf("tool-%04d", index)] = first
	}
	state.Revision++
	handle := actors.Store(chatID)
	if err := handle.Save(state); err != nil {
		t.Fatal(err)
	}
	var head string
	if err := actors.DB().Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT state_json FROM chats WHERE chat_id = ?`, chatID).Scan(&head)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(head, "tool-0001") {
		t.Fatal("the head still carries tool owners")
	}

	next := state.Clone()
	next.Revision++
	next.Tools["tool-0007"] = second
	next.Tools["tool-new"] = first
	delete(next.Tools, "tool-0010")
	before := totalChanges(t, actors.DB())
	if err := handle.Save(next); err != nil {
		t.Fatal(err)
	}
	// The head, one changed, one added and one removed owner, the journal
	// sweep: nothing proportional to the 2,000 owners.
	if changed := totalChanges(t, actors.DB()) - before; changed > 8 {
		t.Fatalf("a checkpoint changing three owners changed %d rows", changed)
	}
	loaded, found, err := actors.Store(chatID).Load(chatID)
	if err != nil || !found || !reflect.DeepEqual(loaded.Tools, next.Tools) {
		t.Fatalf("owners after reload differ: found=%v err=%v (%d owners)", found, err, len(loaded.Tools))
	}
	// A fresh handle has no copy of the stored owners: it rewrites them all,
	// and the result is the same.
	if err := actors.Store(chatID).Save(next); err != nil {
		t.Fatal(err)
	}
	if loaded, _, err = actors.Store(chatID).Load(chatID); err != nil || !reflect.DeepEqual(loaded.Tools, next.Tools) {
		t.Fatalf("owners after a full rewrite differ: %v", err)
	}
	// Discovery summarizes an accepted head without its owners.
	heads, err := actors.Discover()
	if err != nil || len(heads) != 1 || len(heads[0].State.Tools) != 0 || !heads[0].State.Initialized {
		t.Fatalf("discovered heads = %d (%v)", len(heads), err)
	}
}
