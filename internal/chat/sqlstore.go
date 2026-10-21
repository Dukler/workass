package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"workass/internal/store"
)

// SQLActors is the SQLite home of every durable chat actor: the
// replacement for provider-chats/<sha256>.json and its .events journal.
//
// Layout (internal/store/migrations/0002_chat_actors.sql):
//   - chats.state_json is the actor's State in its usual JSON encoding,
//     without the ledger whenever the ledger has rows;
//   - messages holds the ledger, one row per LedgerEvent, keyed by its 1-based
//     position;
//   - chat_events holds the journal records committed after the checkpoint,
//     exactly the payloads the file journal framed.
//
// A checkpoint (Save) replaces all three for one chat in one transaction,
// which also makes the file store's two-step "write snapshot, then delete
// journal" atomic.
type SQLActors struct {
	db *store.DB
}

// NewSQLActors binds the chat actor tables of an open state database.
func NewSQLActors(db *store.DB) *SQLActors {
	return &SQLActors{db: db}
}

// DB is the underlying state database.
func (a *SQLActors) DB() *store.DB { return a.db }

// Store returns the StateStore of one chat actor. It is cheap; the engine
// keeps the value it was constructed with.
func (a *SQLActors) Store(chatID string) *SQLStore {
	return &SQLStore{actors: a, chatID: strings.TrimSpace(chatID)}
}

// ActorHead is what boot discovery needs from one actor without decoding its
// history: the State with no Ledger, and how many ledger rows it has.
type ActorHead struct {
	State        State
	LedgerLength int
}

// Discover lists every actor's head, ordered by ChatID. It decodes only
// state_json (never messages), so boot no longer reads every transcript; full
// validation and journal replay happen when the actor is opened.
func (a *SQLActors) Discover() ([]ActorHead, error) {
	var heads []ActorHead
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT chat_id, envelope_version, state_json, ledger_len FROM chats ORDER BY chat_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var chatID, stateJSON string
			var version, ledgerLen int
			if err := rows.Scan(&chatID, &version, &stateJSON, &ledgerLen); err != nil {
				return err
			}
			if version != currentStateEnvelopeVersion {
				return fmt.Errorf("chat actor %q: unsupported chat state version %d", chatID, version)
			}
			var state State
			if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
				return fmt.Errorf("decode chat actor %q: %w", chatID, err)
			}
			if strings.TrimSpace(state.ChatID) != chatID {
				return fmt.Errorf("chat actor %q holds state for another chat", chatID)
			}
			if ledgerLen > 0 {
				state.Ledger = nil
			}
			heads = append(heads, ActorHead{State: state, LedgerLength: ledgerLen})
		}
		return rows.Err()
	})
	return heads, err
}

// SQLStore is one chat actor's StateStore over SQLActors. It implements the
// same journal fast paths as FileStore (streamed provider events, terminal
// receipts, cancellation) through commitProviderEventViaJournal.
type SQLStore struct {
	actors *SQLActors
	chatID string
}

var (
	_ StateStore              = (*SQLStore)(nil)
	_ providerEventStateStore = (*SQLStore)(nil)
	_ cancelCommandStateStore = (*SQLStore)(nil)
)

func (s *SQLStore) Load(chatID string) (State, bool, error) {
	if strings.TrimSpace(chatID) != s.chatID || s.chatID == "" {
		return State{}, false, errors.New("chat state store belongs to another chat")
	}
	var (
		state State
		found bool
	)
	err := s.actors.db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		state, found, err = loadSQLActor(tx, s.chatID)
		return err
	})
	if err != nil {
		return State{}, false, err
	}
	return state, found, nil
}

// loadSQLActor reads the checkpoint, its ledger rows and journal, and returns
// the same State FileStore.Load would for the equivalent files.
func loadSQLActor(tx *store.Tx, chatID string) (State, bool, error) {
	var (
		version   int
		revision  uint64
		stateJSON string
		ledgerLen int
	)
	err := tx.QueryRow(`SELECT envelope_version, revision, state_json, ledger_len FROM chats WHERE chat_id = ?`, chatID).
		Scan(&version, &revision, &stateJSON, &ledgerLen)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if version != currentStateEnvelopeVersion {
		return State{}, false, fmt.Errorf("unsupported chat state version %d", version)
	}
	var state State
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return State{}, false, fmt.Errorf("decode chat state: %w", err)
	}
	if strings.TrimSpace(state.ChatID) != chatID {
		return State{}, false, errors.New("chat state row belongs to another chat")
	}
	if state.Revision != revision {
		return State{}, false, fmt.Errorf("chat state revision %d disagrees with its checkpoint row %d", state.Revision, revision)
	}
	if ledgerLen > 0 {
		ledger, err := loadSQLLedger(tx, chatID, ledgerLen)
		if err != nil {
			return State{}, false, err
		}
		state.Ledger = ledger
	}
	normalizeStateMaps(&state)
	if err := state.Validate(); err != nil {
		return State{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	state, err = replaySQLJournal(tx, chatID, state)
	if err != nil {
		return State{}, false, fmt.Errorf("replay provider event journal: %w", err)
	}
	return state, true, nil
}

func loadSQLLedger(tx *store.Tx, chatID string, ledgerLen int) ([]LedgerEvent, error) {
	rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? ORDER BY seq`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ledger := make([]LedgerEvent, 0, ledgerLen)
	for rows.Next() {
		var seq int
		var raw []byte
		if err := rows.Scan(&seq, &raw); err != nil {
			return nil, err
		}
		if seq != len(ledger)+1 {
			return nil, fmt.Errorf("chat ledger row %d is out of position %d", seq, len(ledger)+1)
		}
		var event LedgerEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, fmt.Errorf("decode chat ledger row %d: %w", seq, err)
		}
		ledger = append(ledger, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ledger) != ledgerLen {
		return nil, fmt.Errorf("chat ledger has %d rows, its checkpoint expects %d", len(ledger), ledgerLen)
	}
	return ledger, nil
}

func replaySQLJournal(tx *store.Tx, chatID string, state State) (State, error) {
	rows, err := tx.Query(`SELECT record FROM chat_events WHERE chat_id = ? ORDER BY revision`, chatID)
	if err != nil {
		return State{}, err
	}
	defer rows.Close()
	replayed, err := replayProviderEventRecords(state, func() ([]byte, error) {
		if !rows.Next() {
			return nil, rows.Err()
		}
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if len(payload) == 0 {
			return nil, errors.New("provider event journal record is empty")
		}
		return payload, nil
	})
	if err != nil {
		return State{}, err
	}
	return replayed, rows.Err()
}

// Save is a full checkpoint: state_json, every ledger row and an empty
// journal, in one transaction.
func (s *SQLStore) Save(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(state.ChatID) != s.chatID || s.chatID == "" {
		return errors.New("chat state store belongs to another chat")
	}
	head, rows, err := encodeSQLActor(state)
	if err != nil {
		return err
	}
	return s.actors.db.Write(context.Background(), func(tx *store.Tx) error {
		return writeSQLCheckpoint(tx, s.chatID, state.Revision, head, rows)
	})
}

// sqlLedgerRow is one encoded LedgerEvent.
type sqlLedgerRow struct {
	messageID string
	role      string
	json      []byte
}

// encodeSQLActor splits a State into its head (State without a non-empty
// Ledger) and its encoded ledger rows. An empty ledger stays in the head so
// nil and [] round-trip exactly.
func encodeSQLActor(state State) ([]byte, []sqlLedgerRow, error) {
	head := state
	rows := make([]sqlLedgerRow, 0, len(state.Ledger))
	if len(state.Ledger) > 0 {
		head.Ledger = nil
		for _, event := range state.Ledger {
			raw, err := json.Marshal(event)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, sqlLedgerRow{messageID: event.MessageID, role: event.Role, json: raw})
		}
	}
	raw, err := json.Marshal(head)
	if err != nil {
		return nil, nil, err
	}
	return raw, rows, nil
}

func writeSQLCheckpoint(tx *store.Tx, chatID string, revision uint64, head []byte, rows []sqlLedgerRow) error {
	if err := upsertSQLChatHead(tx, chatID, revision, head, len(rows)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	if err := insertSQLLedgerRows(tx, chatID, 1, rows); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM chat_events WHERE chat_id = ?`, chatID)
	return err
}

func upsertSQLChatHead(tx *store.Tx, chatID string, revision uint64, head []byte, ledgerLen int) error {
	_, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, journal_bytes, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(chat_id) DO UPDATE SET envelope_version = excluded.envelope_version, revision = excluded.revision,
			state_json = excluded.state_json, ledger_len = excluded.ledger_len, journal_bytes = 0, updated_at = excluded.updated_at`,
		chatID, currentStateEnvelopeVersion, int64(revision), sqlText(head), ledgerLen, sqlNow())
	return err
}

func insertSQLLedgerRows(tx *store.Tx, chatID string, firstSeq int, rows []sqlLedgerRow) error {
	if len(rows) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for index, row := range rows {
		if _, err := stmt.Exec(chatID, firstSeq+index, row.messageID, row.role, sqlText(row.json)); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLStore) commitProviderEvent(baseRevision uint64, next State, command ProviderEventReceived) error {
	return commitProviderEventViaJournal(s, baseRevision, next, command)
}

func (s *SQLStore) commitCancelCommand(baseRevision uint64, next State, command Command) error {
	return commitCancelCommandViaJournal(s, baseRevision, next, command)
}

// appendJournalRecord commits one record after the checkpoint, charging the
// journal the same framed size the file journal would so both stores
// checkpoint at the same cadence.
func (s *SQLStore) appendJournalRecord(record providerEventJournalRecord, payload []byte, urgent bool) (bool, error) {
	appended := false
	err := s.actors.db.Write(context.Background(), func(tx *store.Tx) error {
		appended = false
		var journalBytes int64
		err := tx.QueryRow(`SELECT journal_bytes FROM chats WHERE chat_id = ?`, s.chatID).Scan(&journalBytes)
		if errors.Is(err, sql.ErrNoRows) {
			// No checkpoint to journal against: the caller checkpoints instead.
			return nil
		}
		if err != nil {
			return err
		}
		size, fits := providerEventJournalAppendFits(journalBytes, providerEventJournalFrameLen(payload), urgent)
		if !fits {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`,
			s.chatID, int64(record.Revision), int64(record.BaseRevision), sqlText(payload)); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE chats SET journal_bytes = ? WHERE chat_id = ?`, size, s.chatID); err != nil {
			return err
		}
		appended = true
		return nil
	})
	return appended, err
}

// sqlText binds JSON bytes as TEXT (so the sqlite3 CLI's json functions
// work on the columns) without a Go-heap copy: the driver copies the value
// into SQLite's memory during the statement, and raw is not touched until
// it returns. A single ledger row can be tens of megabytes.
func sqlText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(raw), len(raw))
}

func sqlNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
