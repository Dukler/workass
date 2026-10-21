package chat

import (
	"context"
	"strings"
	"testing"

	"workass/internal/store"
)

// A journal append never touches the chats row (rewriting it rewrote the
// whole actor head), and the journal budget still decides exactly what the
// stored journal_bytes column decided: across a handle restart, another
// writer's rows, and the checkpoint threshold.
func TestSQLJournalAppendKeepsTheBudgetWithoutRewritingTheHead(t *testing.T) {
	actors := openTestActors(t)
	state, err := NewState("journal-chat")
	if err != nil {
		t.Fatal(err)
	}
	handle := actors.Store(state.ChatID)
	if err := handle.Save(state); err != nil {
		t.Fatal(err)
	}
	head := func() (journalBytes int64, updatedAt string) {
		t.Helper()
		if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
			return tx.QueryRow(`SELECT journal_bytes, updated_at FROM chats WHERE chat_id = ?`, state.ChatID).Scan(&journalBytes, &updatedAt)
		}); err != nil {
			t.Fatal(err)
		}
		return journalBytes, updatedAt
	}
	_, savedAt := head()

	// The reference is the column the store used to keep: the file journal's
	// size, magic header included.
	var expected int64
	revision := state.Revision
	appendOne := func(handle *SQLStore, size int, urgent bool) bool {
		t.Helper()
		payload := []byte(strings.Repeat("x", size))
		next, fits := providerEventJournalAppendFits(expected, providerEventJournalFrameLen(payload), urgent)
		record := providerEventJournalRecord{Version: providerEventJournalVersion, BaseRevision: revision, Revision: revision + 1}
		appended, err := handle.appendJournalRecord(record, payload, urgent)
		if err != nil {
			t.Fatal(err)
		}
		if appended != fits {
			t.Fatalf("revision %d: appended=%v, the budget says %v (journal %d bytes)", record.Revision, appended, fits, expected)
		}
		if appended {
			expected, revision = next, record.Revision
		}
		return appended
	}
	for range 40 {
		appendOne(handle, 10_000, false)
	}
	// A restarted handle recounts the rows it did not write.
	restarted := actors.Store(state.ChatID)
	for range 40 {
		appendOne(restarted, 10_000, false)
	}
	// Another writer's row moves the fence; the account is recounted.
	foreign := []byte(strings.Repeat("y", 5_000))
	if err := actors.db.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`,
			state.ChatID, int64(revision+1), int64(revision), string(foreign))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	expected += int64(providerEventJournalFrameLen(foreign))
	revision++
	filled := false
	for range 40 {
		if !appendOne(restarted, 10_000, false) {
			filled = true
			break
		}
	}
	if !filled {
		t.Fatal("the journal never reached its checkpoint threshold")
	}
	// Past the ordinary threshold only an urgent record still fits.
	if !appendOne(restarted, 10_000, true) {
		t.Fatal("an urgent record did not fit the urgent region")
	}
	if journalBytes, updatedAt := head(); journalBytes != 0 || updatedAt != savedAt {
		t.Fatalf("journal appends rewrote the chats row: journal_bytes=%d updated_at %q -> %q", journalBytes, savedAt, updatedAt)
	}
	// A checkpoint empties the journal; the account starts again from zero.
	state.Revision = revision
	if err := restarted.Save(state); err != nil {
		t.Fatal(err)
	}
	expected = 0
	for appended := 0; appendOne(restarted, 10_000, false); appended++ {
		if appended > 200 {
			t.Fatal("the journal never refilled to its checkpoint threshold")
		}
	}
}
