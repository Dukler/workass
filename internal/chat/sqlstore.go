package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	"workass/internal/store"
)

// SQLActors is the SQLite home of every durable chat actor: the
// replacement for provider-chats/<sha256>.json and its .events journal.
//
// Layout (internal/store/migrations/0002_chat_actors.sql, 0003):
//   - chats.state_json is the actor's State in its usual JSON encoding,
//     without the ledger whenever the ledger has rows;
//   - messages holds the ledger, one row per LedgerEvent, keyed by its 1-based
//     position. Its rows always equal the committed ledger: a commit that
//     changes the ledger writes the changed rows in the same transaction, so
//     a reader can page history without the actor;
//   - chat_events holds journal records committed after the checkpoint
//     (exactly the payloads the file journal framed). Every record this store
//     journals leaves the ledger unchanged.
//
// A checkpoint (Save) writes the head, only the ledger rows that changed and
// an empty journal in one transaction, so appending a message costs one row,
// not a rewrite of the transcript.
type SQLActors struct {
	db *store.DB
}

// NewSQLActors binds the chat actor tables of an open state database.
func NewSQLActors(db *store.DB) *SQLActors {
	return &SQLActors{db: db}
}

// DB is the underlying state database.
func (a *SQLActors) DB() *store.DB { return a.db }

// Store returns the StateStore of one chat actor. The engine keeps the value
// it was constructed with; the store remembers the rows it last wrote so its
// next checkpoint writes only the difference.
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
// validation and journal replay happen when the actor is opened. The heads
// are copied out in one read and decoded after it.
func (a *SQLActors) Discover() ([]ActorHead, error) {
	type rawHead struct {
		chatID    string
		version   int
		stateJSON []byte
		ledgerLen int
	}
	var raws []rawHead
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		rows, err := tx.Query(`SELECT chat_id, envelope_version, state_json, ledger_len FROM chats ORDER BY chat_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw rawHead
			if err := rows.Scan(&raw.chatID, &raw.version, &raw.stateJSON, &raw.ledgerLen); err != nil {
				return err
			}
			raws = append(raws, raw)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	heads := make([]ActorHead, 0, len(raws))
	for index := range raws {
		raw := &raws[index]
		if raw.version != currentStateEnvelopeVersion {
			return nil, fmt.Errorf("chat actor %q: unsupported chat state version %d", raw.chatID, raw.version)
		}
		var state State
		if err := json.Unmarshal(raw.stateJSON, &state); err != nil {
			return nil, fmt.Errorf("decode chat actor %q: %w", raw.chatID, err)
		}
		raw.stateJSON = nil
		if strings.TrimSpace(state.ChatID) != raw.chatID {
			return nil, fmt.Errorf("chat actor %q holds state for another chat", raw.chatID)
		}
		if raw.ledgerLen > 0 {
			state.Ledger = nil
		}
		heads = append(heads, ActorHead{State: state, LedgerLength: raw.ledgerLen})
	}
	return heads, nil
}

// ErrLedgerNotCheckpointed: the chat's rows were written by a build that
// could journal ledger changes, and the actor has not been opened since. Open
// the actor (any open checkpoints it) and read again.
var ErrLedgerNotCheckpointed = errors.New("chat ledger rows predate the current checkpoint format")

// MessagePage is one page of a chat's ledger, oldest first.
type MessagePage struct {
	// Events are LedgerEvents with Sequence in [First, First+len(Events)).
	Events []LedgerEvent
	// First is the Sequence of Events[0]; 0 for an empty page.
	First uint64
	// Total is the chat's ledger length; the newest event's Sequence.
	Total uint64
	// Revision is the actor revision the page belongs to (journal included).
	Revision uint64
}

// HasOlder reports whether events before this page exist.
func (p MessagePage) HasOlder() bool { return p.First > 1 }

// ReadMessages pages a chat's committed ledger without loading the actor.
// It returns at most limit events whose Sequence is < beforeSeq, the newest
// of those first in selection and returned oldest-first; beforeSeq == 0
// means "from the newest event". Page backwards by passing the previous
// page's First as the next beforeSeq. found is false for an unknown chat.
//
// The page reflects every committed actor transition, including those whose
// journal records are not yet checkpointed. It is consistent: rows and
// Total/Revision come from one read transaction.
func (a *SQLActors) ReadMessages(chatID string, beforeSeq uint64, limit int) (page MessagePage, found bool, err error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return MessagePage{}, false, errors.New("read messages requires a chat id")
	}
	if limit <= 0 {
		return MessagePage{}, false, errors.New("read messages requires a positive limit")
	}
	var raws []sqlRawRow
	expected := int64(0)
	err = a.db.Read(context.Background(), func(tx *store.Tx) error {
		page, found, raws = MessagePage{}, false, raws[:0]
		var ledgerLen, ledgerGen int64
		var revision uint64
		err := tx.QueryRow(`SELECT ledger_len, ledger_gen, revision FROM chats WHERE chat_id = ?`, chatID).Scan(&ledgerLen, &ledgerGen, &revision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if ledgerGen == 0 {
			return ErrLedgerNotCheckpointed
		}
		var journalRevision sql.NullInt64
		if err := tx.QueryRow(`SELECT max(revision) FROM chat_events WHERE chat_id = ?`, chatID).Scan(&journalRevision); err != nil {
			return err
		}
		page.Total = uint64(ledgerLen)
		page.Revision = revision
		if journalRevision.Valid && uint64(journalRevision.Int64) > revision {
			page.Revision = uint64(journalRevision.Int64)
		}
		upper := beforeSeq
		if upper == 0 || upper > page.Total+1 {
			upper = page.Total + 1
		}
		expected = int64(upper) - 1
		rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`, chatID, int64(upper), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw sqlRawRow
			if err := rows.Scan(&raw.seq, &raw.eventJSON); err != nil {
				return err
			}
			raws = append(raws, raw)
		}
		return rows.Err()
	})
	if err != nil {
		return MessagePage{}, found, err
	}
	// Decoding a page of large rows happens after the read, so it never holds
	// the connection every chat shares.
	events := make([]LedgerEvent, 0, len(raws))
	for index := range raws {
		raw := &raws[index]
		if raw.seq != expected {
			return MessagePage{}, found, fmt.Errorf("chat ledger row %d is missing", expected)
		}
		var event LedgerEvent
		if err := json.Unmarshal(raw.eventJSON, &event); err != nil {
			return MessagePage{}, found, fmt.Errorf("decode chat ledger row %d: %w", raw.seq, err)
		}
		raw.eventJSON = nil
		events = append(events, event)
		expected--
	}
	for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
		events[left], events[right] = events[right], events[left]
	}
	if len(events) > 0 {
		page.Events = events
		page.First = uint64(expected + 1)
	}
	return page, found, nil
}

// SQLStore is one chat actor's StateStore over SQLActors. It implements the
// same journal fast paths as FileStore (streamed provider events, terminal
// receipts, cancellation) through commitProviderEventViaJournal, except that
// a record which changes the ledger becomes a checkpoint so the message rows
// never lag the actor.
type SQLStore struct {
	actors *SQLActors
	chatID string

	mu    sync.Mutex
	cache *sqlLedgerCache
	// journal is this handle's account of the chat's journal records after
	// its checkpoint (see appendJournalRecord); nil when unknown.
	journal *sqlJournalAccount
}

// sqlJournalAccount is the size the journal budget charges for the journal
// rows this handle last saw: the magic header once there is a row, plus each
// record's file frame, exactly what the import and every append put in
// chats.journal_bytes. last is the newest row's revision (0 for none), the
// fence that proves no other writer added or removed rows since.
type sqlJournalAccount struct {
	bytes int64
	last  uint64
}

// sqlLedgerCache is this handle's private, exact copy of the ledger rows it
// last read or wrote (exactLedgerCopy: nil and empty stay distinct, as they
// are in the stored JSON). gen is the chats.ledger_gen those rows carry; another writer
// bumps it, which invalidates the cache (the next checkpoint rewrites all
// rows). head/length identify the committed Ledger slice: the engine shares
// that backing array with the next state when a streamed event leaves the
// ledger alone, so an identical slice header proves "unchanged" in O(1).
type sqlLedgerCache struct {
	gen  int64
	rows []LedgerEvent
	// shadow is a shallow copy of the committed rows: each row's own fields,
	// sharing its slices and pointers. Committed row data is never edited in
	// place (a transition copies the rows into a new array and replaces any
	// slice it changes), so a row whose memory still equals its shadow is the
	// stored row and needs no deep comparison. Anything else is compared with
	// rows.
	shadow []LedgerEvent
	head   unsafe.Pointer
	length int
}

// unchangedRow reports whether ledger[index] is, field for field and
// pointer for pointer, the row this cache last stored at that position.
func (c *sqlLedgerCache) unchangedRow(ledger []LedgerEvent, index int) bool {
	if index >= len(c.shadow) {
		return false
	}
	size := unsafe.Sizeof(ledger[index])
	return bytes.Equal(
		unsafe.Slice((*byte)(unsafe.Pointer(&ledger[index])), size),
		unsafe.Slice((*byte)(unsafe.Pointer(&c.shadow[index])), size),
	)
}

// shadowLedger is the shallow copy a cache keeps of the rows it stored.
func shadowLedger(ledger []LedgerEvent) []LedgerEvent {
	return append(make([]LedgerEvent, 0, len(ledger)), ledger...)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.journal = nil
	loaded, found, err := loadSQLActorInChunks(s.actors.db, s.chatID)
	if err != nil || !found {
		s.cache = nil
		return State{}, false, err
	}
	s.cache = &sqlLedgerCache{
		gen: loaded.ledgerGen, rows: exactLedgerCopy(loaded.rows), shadow: shadowLedger(loaded.rows),
		head: unsafe.Pointer(unsafe.SliceData(loaded.state.Ledger)), length: len(loaded.state.Ledger),
	}
	return loaded.state, true, nil
}

type sqlLoadedActor struct {
	state     State
	rows      []LedgerEvent // the stored rows, before any journal replay
	ledgerGen int64
}

// sqlActorHeadRow is a chats row as stored. Its checkpoint fields identify
// one committed checkpoint: every checkpoint bumps ledger_gen, so a reader
// that sees the same values again knows no checkpoint happened in between.
type sqlActorHeadRow struct {
	version   int
	revision  uint64
	stateJSON []byte
	ledgerLen int
	ledgerGen int64
	updatedAt string
}

func (h sqlActorHeadRow) sameCheckpoint(other sqlActorHeadRow) bool {
	return h.version == other.version && h.revision == other.revision && h.ledgerLen == other.ledgerLen &&
		h.ledgerGen == other.ledgerGen && h.updatedAt == other.updatedAt
}

// sqlRawRow is one messages row as stored.
type sqlRawRow struct {
	seq       int64
	eventJSON []byte
}

const (
	// sqlLoadChunkBytes bounds the ledger bytes one read transaction copies
	// out while an actor loads. Every chat shares one connection: a 150 MB
	// chat read and decoded in one transaction stalled every other chat's
	// commits for the whole open (review 2026-10-21: a 0.2 ms commit waited
	// 440 ms).
	sqlLoadChunkBytes = 1 << 20
	// sqlLoadAttempts bounds the restarts when another handle checkpoints the
	// actor between two chunks. Each actor has one writer, so a restart is
	// rare and a second one rarer still.
	sqlLoadAttempts = 8
)

// errSQLActorCheckpointedDuringLoad restarts a chunked load.
var errSQLActorCheckpointedDuringLoad = errors.New("chat actor was checkpointed during its load")

// sqlLoadBetweenChunks, tests only, runs between two read transactions of a
// chunked load, with the number of rows decoded so far.
var sqlLoadBetweenChunks func(chatID string, decoded int)

// loadSQLActorInChunks returns what loadSQLActorRows returns in one
// transaction, without holding the connection for the whole read or for any
// decoding. The head and the first rows are copied out in one short read
// transaction; each later one re-reads the head's checkpoint fields and
// copies the next sqlLoadChunkBytes of rows, and the one that reaches the
// last row also copies the journal. Decoding, validation and journal replay
// run between and after those transactions.
//
// The result is one committed checkpoint: rows change only through a
// checkpoint, which bumps ledger_gen, so an unchanged head proves the rows
// already copied still belong to it, and the journal copied with the last
// rows is that checkpoint's journal at that moment. A checkpoint in between
// restarts the load.
func loadSQLActorInChunks(db *store.DB, chatID string) (sqlLoadedActor, bool, error) {
	for attempt := 1; ; attempt++ {
		loaded, found, err := loadSQLActorChunkedOnce(db, chatID)
		if errors.Is(err, errSQLActorCheckpointedDuringLoad) && attempt < sqlLoadAttempts {
			continue
		}
		return loaded, found, err
	}
}

func loadSQLActorChunkedOnce(db *store.DB, chatID string) (sqlLoadedActor, bool, error) {
	var (
		head    sqlActorHeadRow
		state   State
		ledger  []LedgerEvent
		chunk   []sqlRawRow
		journal [][]byte
	)
	for first, done := true, false; !done; first = false {
		found := false
		err := db.Read(context.Background(), func(tx *store.Tx) error {
			chunk, journal, done = chunk[:0], nil, false
			current, ok, err := readSQLActorHead(tx, chatID)
			if err != nil {
				return err
			}
			found = ok
			if first {
				head = current
				if !ok {
					return nil
				}
			} else if !ok || !current.sameCheckpoint(head) {
				return errSQLActorCheckpointedDuringLoad
			}
			done = true
			if head.ledgerLen > 0 {
				chunk, done, err = readSQLLedgerChunk(tx, chatID, int64(len(ledger)), sqlLoadChunkBytes, chunk)
				if err != nil {
					return err
				}
			}
			if done {
				journal, err = readSQLJournal(tx, chatID)
			}
			return err
		})
		if err != nil || !found {
			return sqlLoadedActor{}, false, err
		}
		if first {
			if state, err = decodeSQLHead(head, chatID); err != nil {
				return sqlLoadedActor{}, false, err
			}
			head.stateJSON = nil
			if head.ledgerLen > 0 {
				ledger = make([]LedgerEvent, 0, head.ledgerLen)
			}
		}
		if ledger, err = appendSQLLedgerRows(ledger, chunk); err != nil {
			return sqlLoadedActor{}, false, err
		}
		if !done && sqlLoadBetweenChunks != nil {
			sqlLoadBetweenChunks(chatID, len(ledger))
		}
	}
	return finishSQLActor(state, head.ledgerLen, ledger, journal, head.ledgerGen)
}

// readSQLActorHead reads one chats row as stored.
func readSQLActorHead(tx *store.Tx, chatID string) (sqlActorHeadRow, bool, error) {
	var head sqlActorHeadRow
	err := tx.QueryRow(`SELECT envelope_version, revision, state_json, ledger_len, ledger_gen, updated_at FROM chats WHERE chat_id = ?`, chatID).
		Scan(&head.version, &head.revision, &head.stateJSON, &head.ledgerLen, &head.ledgerGen, &head.updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlActorHeadRow{}, false, nil
	}
	if err != nil {
		return sqlActorHeadRow{}, false, err
	}
	return head, true, nil
}

// readSQLLedgerChunk appends to into the rows after seq `after`, in order,
// until at least budget bytes are copied (budget <= 0: every row). done
// reports that the last row was read.
func readSQLLedgerChunk(tx *store.Tx, chatID string, after int64, budget int, into []sqlRawRow) ([]sqlRawRow, bool, error) {
	rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq > ? ORDER BY seq`, chatID, after)
	if err != nil {
		return into, false, err
	}
	defer rows.Close()
	copied := 0
	for budget <= 0 || copied < budget {
		if !rows.Next() {
			return into, true, rows.Err()
		}
		var raw sqlRawRow
		if err := rows.Scan(&raw.seq, &raw.eventJSON); err != nil {
			return into, false, err
		}
		copied += len(raw.eventJSON)
		into = append(into, raw)
	}
	return into, false, rows.Err()
}

func readSQLJournal(tx *store.Tx, chatID string) ([][]byte, error) {
	rows, err := tx.Query(`SELECT record FROM chat_events WHERE chat_id = ? ORDER BY revision`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if len(payload) == 0 {
			return nil, errors.New("provider event journal record is empty")
		}
		records = append(records, payload)
	}
	return records, rows.Err()
}

func decodeSQLHead(head sqlActorHeadRow, chatID string) (State, error) {
	if head.version != currentStateEnvelopeVersion {
		return State{}, fmt.Errorf("unsupported chat state version %d", head.version)
	}
	var state State
	if err := json.Unmarshal(head.stateJSON, &state); err != nil {
		return State{}, fmt.Errorf("decode chat state: %w", err)
	}
	if strings.TrimSpace(state.ChatID) != chatID {
		return State{}, errors.New("chat state row belongs to another chat")
	}
	if state.Revision != head.revision {
		return State{}, fmt.Errorf("chat state revision %d disagrees with its checkpoint row %d", state.Revision, head.revision)
	}
	return state, nil
}

// appendSQLLedgerRows decodes rows that continue ledger without a gap,
// dropping each row's bytes once it is decoded.
func appendSQLLedgerRows(ledger []LedgerEvent, rows []sqlRawRow) ([]LedgerEvent, error) {
	for index := range rows {
		raw := &rows[index]
		if raw.seq != int64(len(ledger)+1) {
			return nil, fmt.Errorf("chat ledger row %d is out of position %d", raw.seq, len(ledger)+1)
		}
		var event LedgerEvent
		if err := json.Unmarshal(raw.eventJSON, &event); err != nil {
			return nil, fmt.Errorf("decode chat ledger row %d: %w", raw.seq, err)
		}
		raw.eventJSON = nil
		ledger = append(ledger, event)
	}
	return ledger, nil
}

// finishSQLActor turns a decoded checkpoint into the State FileStore.Load
// returns for the equivalent files: ledger rows in place, maps normalized,
// validated, journal replayed.
func finishSQLActor(state State, ledgerLen int, ledger []LedgerEvent, journal [][]byte, ledgerGen int64) (sqlLoadedActor, bool, error) {
	var rows []LedgerEvent
	if ledgerLen > 0 {
		if len(ledger) != ledgerLen {
			return sqlLoadedActor{}, false, fmt.Errorf("chat ledger has %d rows, its checkpoint expects %d", len(ledger), ledgerLen)
		}
		rows = ledger
		state.Ledger = rows
	}
	normalizeStateMaps(&state)
	if err := state.Validate(); err != nil {
		return sqlLoadedActor{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	next := 0
	state, err := replayProviderEventRecords(state, func() ([]byte, error) {
		if next == len(journal) {
			return nil, nil
		}
		payload := journal[next]
		journal[next] = nil
		next++
		return payload, nil
	})
	if err != nil {
		return sqlLoadedActor{}, false, fmt.Errorf("replay provider event journal: %w", err)
	}
	return sqlLoadedActor{state: state, rows: rows, ledgerGen: ledgerGen}, true, nil
}

// loadSQLActor reads the checkpoint, its ledger rows and journal, and returns
// the same State FileStore.Load would for the equivalent files.
func loadSQLActor(tx *store.Tx, chatID string) (State, bool, error) {
	loaded, found, err := loadSQLActorRows(tx, chatID)
	return loaded.state, found, err
}

// loadSQLActorRows is the whole load inside the caller's transaction: the
// import, which runs at boot before any chat can commit. Runtime opens use
// loadSQLActorInChunks.
func loadSQLActorRows(tx *store.Tx, chatID string) (sqlLoadedActor, bool, error) {
	head, found, err := readSQLActorHead(tx, chatID)
	if err != nil || !found {
		return sqlLoadedActor{}, false, err
	}
	state, err := decodeSQLHead(head, chatID)
	if err != nil {
		return sqlLoadedActor{}, false, err
	}
	head.stateJSON = nil
	var ledger []LedgerEvent
	if head.ledgerLen > 0 {
		// Chunks here only bound the raw bytes held next to the decoded rows.
		ledger = make([]LedgerEvent, 0, head.ledgerLen)
		var chunk []sqlRawRow
		for done := false; !done; {
			if chunk, done, err = readSQLLedgerChunk(tx, chatID, int64(len(ledger)), sqlLoadChunkBytes, chunk[:0]); err != nil {
				return sqlLoadedActor{}, false, err
			}
			if ledger, err = appendSQLLedgerRows(ledger, chunk); err != nil {
				return sqlLoadedActor{}, false, err
			}
		}
	}
	journal, err := readSQLJournal(tx, chatID)
	if err != nil {
		return sqlLoadedActor{}, false, err
	}
	return finishSQLActor(state, head.ledgerLen, ledger, journal, head.ledgerGen)
}

// Save is a checkpoint: the head, the ledger rows that differ from the ones
// this handle last saw, and an empty journal, in one transaction.
func (s *SQLStore) Save(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	return s.saveValidated(state)
}

// saveValidated is Save for a state the caller has just validated.
func (s *SQLStore) saveValidated(state State) error {
	if strings.TrimSpace(state.ChatID) != s.chatID || s.chatID == "" {
		return errors.New("chat state store belongs to another chat")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointLocked(state)
}

func (s *SQLStore) checkpointLocked(state State) error {
	head, err := encodeSQLHead(state)
	if err != nil {
		return err
	}
	cache := s.cache
	// The plan, the row diff and every changed row's JSON are worked out
	// before the write transaction. On a large ledger they cost far more than
	// the writes, and every chat shares the one connection. The transaction
	// only confirms the plan still applies (the stored ledger_gen is the one
	// the cache describes) and writes. A full plan (no usable cache: a new
	// chat, a failed commit, another writer) still writes every row in one
	// transaction, which a checkpoint must; an open actor's ordinary
	// checkpoints are incremental.
	plan := sqlLedgerPlan{full: cache == nil || cache.gen == 0}
	if !plan.full && !cache.sameLedger(state.Ledger) {
		plan.diff(cache, state.Ledger)
	}
	if err := plan.encode(state.Ledger); err != nil {
		return err
	}
	for {
		err = s.actors.db.Write(context.Background(), func(tx *store.Tx) error {
			var storedGen int64
			err := tx.QueryRow(`SELECT ledger_gen FROM chats WHERE chat_id = ?`, s.chatID).Scan(&storedGen)
			exists := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if !plan.full && (!exists || storedGen != cache.gen) {
				return errSQLPlanStale
			}
			plan.gen = storedGen + 1
			if err := upsertSQLChatHead(tx, s.chatID, state.Revision, head, len(state.Ledger), plan.gen); err != nil {
				return err
			}
			if err := plan.apply(tx, s.chatID, state.Ledger); err != nil {
				return err
			}
			_, err = tx.Exec(`DELETE FROM chat_events WHERE chat_id = ?`, s.chatID)
			return err
		})
		if !errors.Is(err, errSQLPlanStale) || plan.full {
			break
		}
		// Another writer moved the rows since this handle read them: rewrite
		// every row, as the in-transaction plan always did in this case.
		plan = sqlLedgerPlan{full: true}
		if err = plan.encode(state.Ledger); err != nil {
			break
		}
	}
	if err != nil {
		// The table may or may not hold the rows this commit tried to write:
		// forget the cache so the next checkpoint rewrites every row.
		s.cache = nil
		s.journal = nil
		return err
	}
	s.cache = plan.nextCache(cache, state.Ledger)
	s.journal = &sqlJournalAccount{}
	return nil
}

// errSQLPlanStale: the incremental plan was made against rows another writer
// has since replaced.
var errSQLPlanStale = errors.New("chat ledger rows changed since this handle read them")

// sameLedger reports whether ledger is the committed ledger this cache
// describes. The O(1) proof is the identical slice header (the engine shares
// the array whenever an event leaves the ledger alone); the newest row is
// still compared, cheaply, as a guard against an in-place edit of a shared
// array. Anything else falls back to comparing every row.
func (c *sqlLedgerCache) sameLedger(ledger []LedgerEvent) bool {
	if len(ledger) != len(c.rows) {
		return false
	}
	if unsafe.Pointer(unsafe.SliceData(ledger)) == c.head && len(ledger) == c.length {
		return len(ledger) == 0 || reflect.DeepEqual(ledger[len(ledger)-1], c.rows[len(c.rows)-1])
	}
	for index := range ledger {
		if !c.unchangedRow(ledger, index) && !reflect.DeepEqual(ledger[index], c.rows[index]) {
			return false
		}
	}
	return true
}

// sqlLedgerPlan is the row-level difference one checkpoint writes.
type sqlLedgerPlan struct {
	full    bool
	changed []int    // positions (0-based) to upsert
	encoded [][]byte // the JSON of each changed row, in changed order
	gen     int64
}

func (p *sqlLedgerPlan) diff(cache *sqlLedgerCache, next []LedgerEvent) {
	stored := cache.rows
	shared := len(stored)
	if len(next) < shared {
		shared = len(next)
	}
	for index := 0; index < shared; index++ {
		if !cache.unchangedRow(next, index) && !reflect.DeepEqual(stored[index], next[index]) {
			p.changed = append(p.changed, index)
		}
	}
	for index := shared; index < len(next); index++ {
		p.changed = append(p.changed, index)
	}
}

// encode settles which rows the plan writes (every row for a full plan) and
// marshals them, so the write transaction does no encoding.
func (p *sqlLedgerPlan) encode(ledger []LedgerEvent) error {
	if p.full {
		p.changed = p.changed[:0]
		for index := range ledger {
			p.changed = append(p.changed, index)
		}
	}
	p.encoded = make([][]byte, len(p.changed))
	for position, index := range p.changed {
		raw, err := json.Marshal(ledger[index])
		if err != nil {
			return err
		}
		p.encoded[position] = raw
	}
	return nil
}

func (p *sqlLedgerPlan) apply(tx *store.Tx, chatID string, ledger []LedgerEvent) error {
	if p.full {
		if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id = ?`, chatID); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id = ? AND seq > ?`, chatID, len(ledger)); err != nil {
		return err
	}
	if len(p.changed) == 0 {
		return nil
	}
	if len(p.encoded) != len(p.changed) {
		return errors.New("chat ledger plan was not encoded")
	}
	stmt, err := tx.Prepare(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, seq) DO UPDATE SET message_id = excluded.message_id, role = excluded.role, event_json = excluded.event_json`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for position, index := range p.changed {
		event := ledger[index]
		if _, err := stmt.Exec(chatID, index+1, event.MessageID, event.Role, sqlText(p.encoded[position])); err != nil {
			return err
		}
	}
	return nil
}

// nextCache is the cache after a committed plan: a private copy of exactly
// the rows now in the table, re-cloning only the rows the plan wrote.
func (p *sqlLedgerPlan) nextCache(previous *sqlLedgerCache, ledger []LedgerEvent) *sqlLedgerCache {
	next := &sqlLedgerCache{gen: p.gen, shadow: shadowLedger(ledger), head: unsafe.Pointer(unsafe.SliceData(ledger)), length: len(ledger)}
	if p.full || previous == nil {
		next.rows = exactLedgerCopy(ledger)
		return next
	}
	rows := previous.rows
	if len(rows) > len(ledger) {
		rows = rows[:len(ledger)]
	}
	for _, index := range p.changed {
		row := exactLedgerCopy(ledger[index : index+1])[0]
		if index < len(rows) {
			rows[index] = row
		} else {
			rows = append(rows, row)
		}
	}
	next.rows = rows
	return next
}

// exactLedgerCopy deep-copies events so that reflect.DeepEqual against the
// copy decides exactly whether a row's JSON would change. Strings are shared
// (immutable); every slice, map and pointer is copied, and nil stays nil.
// cloneLedgerEvents is not exact for this purpose: it turns a nil Timeline
// into an empty one and an empty slice into nil.
func exactLedgerCopy(events []LedgerEvent) []LedgerEvent {
	if events == nil {
		return nil
	}
	out := make([]LedgerEvent, len(events))
	for index := range events {
		exactCopyInto(reflect.ValueOf(&out[index]).Elem(), reflect.ValueOf(events[index]))
	}
	return out
}

func exactCopyInto(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		if src.IsNil() {
			dst.SetZero()
			return
		}
		copied := reflect.New(src.Type().Elem())
		exactCopyInto(copied.Elem(), src.Elem())
		dst.Set(copied)
	case reflect.Slice:
		if src.IsNil() {
			dst.SetZero()
			return
		}
		copied := reflect.MakeSlice(src.Type(), src.Len(), src.Len())
		if src.Type().Elem().Kind() == reflect.Uint8 {
			reflect.Copy(copied, src)
		} else {
			for index := 0; index < src.Len(); index++ {
				exactCopyInto(copied.Index(index), src.Index(index))
			}
		}
		dst.Set(copied)
	case reflect.Map:
		if src.IsNil() {
			dst.SetZero()
			return
		}
		copied := reflect.MakeMapWithSize(src.Type(), src.Len())
		entries := src.MapRange()
		for entries.Next() {
			value := reflect.New(src.Type().Elem()).Elem()
			exactCopyInto(value, entries.Value())
			copied.SetMapIndex(entries.Key(), value)
		}
		dst.Set(copied)
	case reflect.Struct:
		// Copy every field (unexported ones by value), then replace each
		// settable reference field with its own copy.
		dst.Set(src)
		for index := 0; index < src.NumField(); index++ {
			if field := dst.Field(index); field.CanSet() {
				exactCopyInto(field, src.Field(index))
			}
		}
	default:
		dst.Set(src)
	}
}

// encodeSQLHead encodes the State without a non-empty Ledger (the rows hold
// it). An empty ledger stays in the head so nil and [] round-trip exactly.
func encodeSQLHead(state State) ([]byte, error) {
	head := state
	if len(state.Ledger) > 0 {
		head.Ledger = nil
	}
	return json.Marshal(head)
}

// writeSQLFullCheckpoint replaces one chat's head, every ledger row and its
// journal; used where no handle cache exists (import, which owns the
// connection at boot).
func writeSQLFullCheckpoint(tx *store.Tx, chatID string, state State) error {
	head, err := encodeSQLHead(state)
	if err != nil {
		return err
	}
	var storedGen int64
	if err := tx.QueryRow(`SELECT ledger_gen FROM chats WHERE chat_id = ?`, chatID).Scan(&storedGen); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	plan := sqlLedgerPlan{full: true, gen: storedGen + 1}
	if err := plan.encode(state.Ledger); err != nil {
		return err
	}
	if err := upsertSQLChatHead(tx, chatID, state.Revision, head, len(state.Ledger), plan.gen); err != nil {
		return err
	}
	if err := plan.apply(tx, chatID, state.Ledger); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM chat_events WHERE chat_id = ?`, chatID)
	return err
}

func upsertSQLChatHead(tx *store.Tx, chatID string, revision uint64, head []byte, ledgerLen int, ledgerGen int64) error {
	_, err := tx.Exec(`INSERT INTO chats(chat_id, envelope_version, revision, state_json, ledger_len, journal_bytes, updated_at, ledger_gen)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET envelope_version = excluded.envelope_version, revision = excluded.revision,
			state_json = excluded.state_json, ledger_len = excluded.ledger_len, journal_bytes = 0,
			updated_at = excluded.updated_at, ledger_gen = excluded.ledger_gen`,
		chatID, currentStateEnvelopeVersion, int64(revision), sqlText(head), ledgerLen, sqlNow(), ledgerGen)
	return err
}

func (s *SQLStore) commitProviderEvent(baseRevision uint64, next State, command ProviderEventReceived) error {
	if s.ledgerChanged(next) {
		// Terminal receipts and settled-subagent updates edit the ledger. A
		// checkpoint here writes only the changed rows, and keeps the message
		// rows equal to the actor for readers that never open it.
		return s.journalCheckpoint(next)
	}
	return commitProviderEventViaJournal(s, baseRevision, next, command)
}

func (s *SQLStore) commitCancelCommand(baseRevision uint64, next State, command Command) error {
	if s.ledgerChanged(next) {
		return s.journalCheckpoint(next)
	}
	return commitCancelCommandViaJournal(s, baseRevision, next, command)
}

// journalCheckpoint is the checkpoint that stands in for a journal record.
// Like the file store's journal append it does not re-validate the whole
// next state (on a 300 MB actor that alone costs ~160 ms, and the reducer
// validated the state it came from); Load validates every state it returns,
// exactly as file journal replay does.
func (s *SQLStore) journalCheckpoint(next State) error {
	if strings.TrimSpace(next.ChatID) != s.chatID || s.chatID == "" {
		return errors.New("chat state store belongs to another chat")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkpointLocked(next)
}

// ledgerChanged reports whether next's ledger differs from the stored rows.
// An unknown cache counts as changed (the checkpoint then rewrites).
func (s *SQLStore) ledgerChanged(next State) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return true
	}
	newSlice := unsafe.Pointer(unsafe.SliceData(next.Ledger)) != s.cache.head || len(next.Ledger) != s.cache.length
	if !s.cache.sameLedger(next.Ledger) {
		return true
	}
	// Equal content in a new slice (a reducer clone): remember its header so
	// the streamed events that follow take the O(1) path, and its rows so the
	// next comparison recognises them without a deep one.
	if newSlice {
		s.cache.shadow = shadowLedger(next.Ledger)
	}
	s.cache.head, s.cache.length = unsafe.Pointer(unsafe.SliceData(next.Ledger)), len(next.Ledger)
	return false
}

// appendJournalRecord commits one record after the checkpoint, charging the
// journal the same framed size the file journal would so both stores
// checkpoint at the same cadence.
//
// The journal's size is kept by this handle rather than read from and
// written back to chats.journal_bytes: SQLite rewrites a whole row to update
// one column, and the chats row carries the actor head, megabytes on a long
// chat, so every streamed token rewrote the head. The column keeps the value
// the last checkpoint or import wrote. The account is fenced by the newest
// journal revision and recomputed from the rows whenever it is unknown or
// another writer moved them.
func (s *SQLStore) appendJournalRecord(record providerEventJournalRecord, payload []byte, urgent bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	appended := false
	var account sqlJournalAccount
	err := s.actors.db.Write(context.Background(), func(tx *store.Tx) error {
		appended = false
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM chats WHERE chat_id = ?`, s.chatID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			// No checkpoint to journal against: the caller checkpoints instead.
			return nil
		}
		if err != nil {
			return err
		}
		var last sql.NullInt64
		if err := tx.QueryRow(`SELECT max(revision) FROM chat_events WHERE chat_id = ?`, s.chatID).Scan(&last); err != nil {
			return err
		}
		if s.journal != nil && s.journal.last == uint64(last.Int64) {
			account = *s.journal
		} else {
			var rows, recordBytes int64
			if err := tx.QueryRow(`SELECT count(*), coalesce(sum(length(CAST(record AS BLOB))), 0) FROM chat_events WHERE chat_id = ?`, s.chatID).Scan(&rows, &recordBytes); err != nil {
				return err
			}
			account = sqlJournalAccount{last: uint64(last.Int64)}
			if rows > 0 {
				account.bytes = int64(len(providerEventJournalMagic)) + recordBytes + rows*int64(providerEventJournalFrameLen(nil))
			}
		}
		size, fits := providerEventJournalAppendFits(account.bytes, providerEventJournalFrameLen(payload), urgent)
		if !fits {
			return nil
		}
		if _, err := tx.Exec(`INSERT INTO chat_events(chat_id, revision, base_revision, record) VALUES (?, ?, ?, ?)`,
			s.chatID, int64(record.Revision), int64(record.BaseRevision), sqlText(payload)); err != nil {
			return err
		}
		account = sqlJournalAccount{bytes: size, last: record.Revision}
		appended = true
		return nil
	})
	switch {
	case err != nil:
		s.journal = nil
	case appended:
		s.journal = &account
	}
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
