package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"workass/internal/provider"
	"workass/internal/store"
)

// totalChanges is SQLite's count of rows inserted, updated or deleted on the
// connection: the row-level cost of one commit.
func totalChanges(t testing.TB, db *store.DB) int64 {
	t.Helper()
	var changes int64
	if err := db.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT total_changes()`).Scan(&changes)
	}); err != nil {
		t.Fatal(err)
	}
	return changes
}

func largeLedgerState(t testing.TB, chatID string, rows int) State {
	t.Helper()
	state, err := NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	for len(state.Ledger) < rows {
		state.Ledger = append(state.Ledger, historyRow(len(state.Ledger)+1))
	}
	state.Revision = 1
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	return state
}

func historyRow(sequence int) LedgerEvent {
	return LedgerEvent{
		EventID: fmt.Sprintf("history-event-%d", sequence), MessageID: fmt.Sprintf("history-message-%d", sequence),
		Sequence: uint64(sequence), Role: "assistant", Status: "done", Text: fmt.Sprintf("row %d <&>", sequence),
		OperationID: provider.OperationID(fmt.Sprintf("history-operation-%d", sequence)), ContextExcluded: true,
	}
}

func assertRowsMatch(t testing.TB, actors *SQLActors, chatID string, ledger []LedgerEvent) {
	t.Helper()
	page, found, err := actors.ReadMessages(chatID, 0, len(ledger)+1)
	if err != nil || !found {
		t.Fatalf("read rows: found=%v err=%v", found, err)
	}
	if int(page.Total) != len(ledger) || !reflect.DeepEqual(page.Events, ledger) && !(len(ledger) == 0 && len(page.Events) == 0) {
		got, _ := json.Marshal(page.Events)
		want, _ := json.Marshal(ledger)
		t.Fatalf("rows differ from the saved ledger: %s", firstJSONDifference(got, want))
	}
}

func TestSQLCheckpointWritesOnlyTheRowsThatChanged(t *testing.T) {
	const chatID = "incremental-chat"
	actors := openTestActors(t)
	sqlStore := actors.Store(chatID)
	state := largeLedgerState(t, chatID, 4096)
	if err := sqlStore.Save(state); err != nil {
		t.Fatal(err)
	}

	measure := func(label string, next State, maxChanges int64) {
		t.Helper()
		before := totalChanges(t, actors.db)
		if err := sqlStore.Save(next); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if changes := totalChanges(t, actors.db) - before; changes > maxChanges {
			t.Fatalf("%s wrote %d rows, want at most %d", label, changes, maxChanges)
		}
		assertRowsMatch(t, actors, chatID, next.Ledger)
		loaded, found, err := actors.Store(chatID).Load(chatID)
		if err != nil || !found || !reflect.DeepEqual(loaded.Ledger, next.Ledger) || loaded.Revision != next.Revision {
			t.Fatalf("%s: reload differs: found=%v err=%v", label, found, err)
		}
	}

	// Same ledger slice (a streamed event's shape): the head only.
	next := state
	next.Revision++
	measure("unchanged ledger", next, 1)

	// A reducer clone with equal rows: still the head only.
	cloned := next.Clone()
	cloned.Revision++
	cloned.Ledger = exactLedgerCopy(next.Ledger)
	measure("cloned equal ledger", cloned, 1)

	// Append one message: the head and one row.
	appended := cloned
	appended.Revision++
	appended.Ledger = append(exactLedgerCopy(cloned.Ledger), historyRow(4097))
	measure("append", appended, 2)

	// Edit one old row (a settled subagent tool update): head and that row.
	edited := appended
	edited.Revision++
	edited.Ledger = exactLedgerCopy(appended.Ledger)
	edited.Ledger[100].Text = "edited in place"
	measure("edit one row", edited, 2)

	// Truncate (a fork-style rebuild): head plus the deleted rows.
	truncated := edited
	truncated.Revision++
	truncated.Ledger = exactLedgerCopy(edited.Ledger[:4000])
	measure("truncate", truncated, 1+97)

	// Empty the ledger: [] stays [] (not null) through the head.
	emptied := truncated
	emptied.Revision++
	emptied.Ledger = []LedgerEvent{}
	measure("empty", emptied, 1+4000)
	loaded, _, err := actors.Store(chatID).Load(chatID)
	if err != nil || loaded.Ledger == nil {
		t.Fatalf("an empty ledger reloaded as %#v (%v)", loaded.Ledger, err)
	}
}

func TestSQLCheckpointRewritesWhenAnotherHandleWroteTheChat(t *testing.T) {
	const chatID = "two-handle-chat"
	actors := openTestActors(t)
	first, second := actors.Store(chatID), actors.Store(chatID)
	state := largeLedgerState(t, chatID, 50)
	if err := first.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, _, err := second.Load(chatID); err != nil {
		t.Fatal(err)
	}
	// first writes a different row 10; second's cache still holds the old
	// row 10 and would otherwise conclude "unchanged" for its own state.
	fromFirst := state.Clone()
	fromFirst.Revision++
	fromFirst.Ledger[9].Text = "first handle"
	if err := first.Save(fromFirst); err != nil {
		t.Fatal(err)
	}
	fromSecond := state.Clone()
	fromSecond.Revision += 2
	if err := second.Save(fromSecond); err != nil {
		t.Fatal(err)
	}
	assertRowsMatch(t, actors, chatID, fromSecond.Ledger)
	// And first, now stale, does the same.
	again := fromFirst.Clone()
	again.Revision += 5
	again.Ledger = append(again.Ledger, historyRow(51))
	if err := first.Save(again); err != nil {
		t.Fatal(err)
	}
	assertRowsMatch(t, actors, chatID, again.Ledger)
}

func TestReadMessagesPagesBackwardsConsistently(t *testing.T) {
	const chatID = "paged-chat"
	actors := openTestActors(t)
	if _, found, err := actors.ReadMessages(chatID, 0, 10); err != nil || found {
		t.Fatalf("unknown chat: found=%v err=%v", found, err)
	}
	state := largeLedgerState(t, chatID, 23)
	if err := actors.Store(chatID).Save(state); err != nil {
		t.Fatal(err)
	}
	var collected []LedgerEvent
	before := uint64(0)
	pages := 0
	for {
		page, found, err := actors.ReadMessages(chatID, before, 5)
		if err != nil || !found {
			t.Fatalf("page %d: found=%v err=%v", pages, found, err)
		}
		if page.Total != 23 || page.Revision != state.Revision {
			t.Fatalf("page %d: total=%d revision=%d", pages, page.Total, page.Revision)
		}
		if len(page.Events) == 0 {
			break
		}
		if page.Events[0].Sequence != page.First || page.Events[len(page.Events)-1].Sequence != page.First+uint64(len(page.Events))-1 {
			t.Fatalf("page %d is not contiguous oldest-first: first=%d", pages, page.First)
		}
		collected = append(append([]LedgerEvent(nil), page.Events...), collected...)
		pages++
		if !page.HasOlder() {
			break
		}
		before = page.First
	}
	if pages != 5 || !reflect.DeepEqual(collected, state.Ledger) {
		t.Fatalf("paged %d pages, %d events", pages, len(collected))
	}
	if page, _, err := actors.ReadMessages(chatID, 1_000, 3); err != nil || page.First != 21 || len(page.Events) != 3 {
		t.Fatalf("beforeSeq past the end = first %d, %d events, %v", page.First, len(page.Events), err)
	}
	if page, _, err := actors.ReadMessages(chatID, 1, 3); err != nil || len(page.Events) != 0 || page.HasOlder() {
		t.Fatalf("beforeSeq 1 = %d events, %v", len(page.Events), err)
	}
	if _, _, err := actors.ReadMessages(chatID, 0, 0); err == nil {
		t.Fatal("a zero limit was accepted")
	}
	// A journal record after the checkpoint moves the page revision.
	if err := actors.db.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, '{}')`, chatID, state.Revision+1, state.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if page, _, err := actors.ReadMessages(chatID, 0, 1); err != nil || page.Revision != state.Revision+1 {
		t.Fatalf("revision with a pending journal = %d, %v", page.Revision, err)
	}
}

// TestPhase1aDatabaseUpgradesAndHealsOnFirstOpen builds a database the way
// the phase-1a store left it (schema 2, terminal and cancel records still in
// chat_events, so the rows lag the actor), upgrades it, and checks that the
// rows are refused for paging until the actor's first open checkpoints them.
func TestPhase1aDatabaseUpgradesAndHealsOnFirstOpen(t *testing.T) {
	const chatID = "phase1a-chat"
	dir := t.TempDir()
	path := filepath.Join(dir, "provider-chats", actorFileName(chatID))
	engine, err := NewDurableEngine(chatID, FileStore{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range actorLifecycleScript(chatID) {
		step.run(t, engine)
		if step.name == "terminal" {
			break
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope stateEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	checkpoint := envelope.State
	normalizeStateMaps(&checkpoint)

	migrations, err := store.EmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, store.FileName)
	old, err := store.Open(store.Options{Path: dbPath, Migrations: migrations[:2]})
	if err != nil {
		t.Fatal(err)
	}
	// The head as phase 1a wrote it: the State without its ledger, tool
	// owners included (migration 0005 moves them to chat_tools).
	legacyHead := checkpoint
	legacyHead.Ledger = nil
	if len(legacyHead.Tools) == 0 {
		t.Fatal("fixture has no tool owners for the upgrade to move")
	}
	head, err := json.Marshal(legacyHead)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := os.Open(providerEventJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := newProviderEventJournalFrameReader(journal)
	if err != nil {
		t.Fatal(err)
	}
	records := 0
	if err := old.Write(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, journal_bytes, updated_at) VALUES (?, ?, ?, ?, ?, 0, 'then')`,
			chatID, currentStateEnvelopeVersion, int64(checkpoint.Revision), string(head), len(checkpoint.Ledger)); err != nil {
			return err
		}
		for index, event := range checkpoint.Ledger {
			row, _ := json.Marshal(event)
			if _, err := tx.Exec(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, ?, ?)`, chatID, index+1, event.MessageID, event.Role, string(row)); err != nil {
				return err
			}
		}
		for {
			payload, err := frames.next()
			if err != nil {
				return err
			}
			if payload == nil {
				return nil
			}
			record, err := decodeProviderEventJournalRecord(payload)
			if err != nil {
				return err
			}
			if record.Revision <= checkpoint.Revision {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`, chatID, int64(record.Revision), int64(record.BaseRevision), string(payload)); err != nil {
				return err
			}
			records++
		}
	}); err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if records == 0 {
		t.Fatal("fixture has no journal records")
	}

	upgraded, err := store.Open(store.Options{Path: dbPath, BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if upgraded.SchemaVersion() != len(migrations) {
		t.Fatalf("schema %d after upgrade", upgraded.SchemaVersion())
	}
	actors := NewSQLActors(upgraded)
	if _, _, err := actors.ReadMessages(chatID, 0, 10); !errors.Is(err, ErrLedgerNotCheckpointed) {
		t.Fatalf("rows behind a ledger-changing journal were served: %v", err)
	}
	sqlEngine, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	fileEngine, err := NewDurableEngine(chatID, FileStore{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	assertSameDurableState(t, "upgraded actor", fileEngine.Snapshot(), sqlEngine.Snapshot(), true)
	page, _, err := actors.ReadMessages(chatID, 0, 100)
	if err != nil || !reflect.DeepEqual(cloneLedgerEvents(page.Events), sqlEngine.Snapshot().Ledger) {
		t.Fatalf("rows after the healing open: %d events, %v", len(page.Events), err)
	}
}

// bigRowsState is a valid state whose rows are about rowBytes each.
func bigRowsState(t testing.TB, chatID string, rows, rowBytes int) State {
	t.Helper()
	state := largeLedgerState(t, chatID, rows)
	text := strings.Repeat("x", rowBytes)
	for index := range state.Ledger {
		state.Ledger[index].Text = text + state.Ledger[index].Text
	}
	return state
}

// Every chat shares one SQLite connection. Opening a large chat must not hold
// it for the whole read and decode: another chat's commits keep landing in
// about the time of one chunk read, not of the open.
func TestSQLLargeLoadDoesNotStallOtherChatsCommits(t *testing.T) {
	actors := openTestActors(t)
	big := bigRowsState(t, "big-chat", 400, 100_000)
	if err := actors.Store("big-chat").Save(big); err != nil {
		t.Fatal(err)
	}
	small := largeLedgerState(t, "small-chat", 4)
	smallStore := actors.Store("small-chat")
	if err := smallStore.Save(small); err != nil {
		t.Fatal(err)
	}

	done := make(chan time.Duration)
	go func() {
		started := time.Now()
		loaded, found, err := actors.Store("big-chat").Load("big-chat")
		if err != nil || !found || len(loaded.Ledger) != len(big.Ledger) {
			t.Errorf("big load: found=%v rows=%d err=%v", found, len(loaded.Ledger), err)
		}
		done <- time.Since(started)
	}()
	var loadTook time.Duration
	var commits []time.Duration
	for loadTook == 0 {
		select {
		case loadTook = <-done:
			continue
		default:
		}
		small.Revision++
		started := time.Now()
		if err := smallStore.Save(small); err != nil {
			t.Fatal(err)
		}
		commits = append(commits, time.Since(started))
	}
	if len(commits) == 0 {
		t.Fatal("no small commit ran during the load")
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i] < commits[j] })
	worst := commits[len(commits)-1]
	// The slowest commit includes SQLite's periodic WAL checkpoint, which an
	// idle loop of the same commits shows too (~9 ms with fullfsync here).
	t.Logf("%d MB chat load %v; %d small commits during it: median %v, p99 %v, slowest %v",
		len(big.Ledger)*100_000>>20, loadTook, len(commits), commits[len(commits)/2], commits[len(commits)*99/100], worst)
	if loadTook < 40*time.Millisecond {
		t.Skipf("the load took %v: too fast on this machine to show a stall", loadTook)
	}
	if worst > loadTook/3 {
		t.Fatalf("a small chat's commit waited %v during a %v load of another chat", worst, loadTook)
	}
}

// A chunked load spans several read transactions. A checkpoint by another
// handle in between must not mix two checkpoints: the load restarts and
// returns the newer one exactly.
func TestSQLChunkedLoadRestartsWhenTheActorIsCheckpointedMidLoad(t *testing.T) {
	const chatID = "chunked-chat"
	actors := openTestActors(t)
	writer := actors.Store(chatID)
	state := bigRowsState(t, chatID, 12, sqlLoadChunkBytes/2)
	if err := writer.Save(state); err != nil {
		t.Fatal(err)
	}
	newer := state.Clone()
	newer.Revision++
	newer.Ledger[0].Text = "rewritten while another handle was loading"
	newer.Ledger = append(newer.Ledger, historyRow(len(newer.Ledger)+1))

	var pauses []int
	sqlLoadBetweenChunks = func(id string, decoded int) {
		pauses = append(pauses, decoded)
		if len(pauses) == 1 {
			if err := writer.Save(newer); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { sqlLoadBetweenChunks = nil })
	reader := actors.Store(chatID)
	loaded, found, err := reader.Load(chatID)
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if len(pauses) < 4 {
		t.Fatalf("the load paused %d times; want several chunks", len(pauses))
	}
	got, _ := json.Marshal(loaded.Ledger)
	want, _ := json.Marshal(newer.Ledger)
	if loaded.Revision != newer.Revision || string(got) != string(want) {
		t.Fatalf("load mixed checkpoints: revision %d, %s", loaded.Revision, firstJSONDifference(got, want))
	}
	// The reader's cache describes the newer rows: its next checkpoint is
	// incremental and leaves the table equal to its state.
	next := loaded
	next.Revision++
	next.Ledger = append(exactLedgerCopy(loaded.Ledger), historyRow(len(loaded.Ledger)+1))
	if err := reader.Save(next); err != nil {
		t.Fatal(err)
	}
	assertRowsMatch(t, actors, chatID, next.Ledger)

	// A writer that never lets the load finish exhausts the restarts and
	// fails closed instead of returning a mixed state.
	sqlLoadBetweenChunks = func(string, int) {
		next.Revision++
		if err := writer.Save(next); err != nil {
			t.Error(err)
		}
	}
	if _, _, err := actors.Store(chatID).Load(chatID); !errors.Is(err, errSQLActorCheckpointedDuringLoad) {
		t.Fatalf("endless checkpoints during a load: %v", err)
	}
}
