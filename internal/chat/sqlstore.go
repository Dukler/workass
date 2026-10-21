package chat

import (
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
			var chatID string
			var stateJSON []byte
			var version, ledgerLen int
			if err := rows.Scan(&chatID, &version, &stateJSON, &ledgerLen); err != nil {
				return err
			}
			if version != currentStateEnvelopeVersion {
				return fmt.Errorf("chat actor %q: unsupported chat state version %d", chatID, version)
			}
			var state State
			if err := json.Unmarshal(stateJSON, &state); err != nil {
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
	err = a.db.Read(context.Background(), func(tx *store.Tx) error {
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
		rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq < ? ORDER BY seq DESC LIMIT ?`, chatID, int64(upper), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		var events []LedgerEvent
		expected := int64(upper) - 1
		for rows.Next() {
			var seq int64
			var raw []byte
			if err := rows.Scan(&seq, &raw); err != nil {
				return err
			}
			if seq != expected {
				return fmt.Errorf("chat ledger row %d is missing", expected)
			}
			var event LedgerEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return fmt.Errorf("decode chat ledger row %d: %w", seq, err)
			}
			events = append(events, event)
			expected--
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for left, right := 0, len(events)-1; left < right; left, right = left+1, right-1 {
			events[left], events[right] = events[right], events[left]
		}
		page.Events = events
		if len(events) > 0 {
			page.First = uint64(expected + 1)
		}
		return nil
	})
	if err != nil {
		return MessagePage{}, found, err
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
}

// sqlLedgerCache is this handle's private, exact copy of the ledger rows it
// last read or wrote (exactLedgerCopy: nil and empty stay distinct, as they
// are in the stored JSON). gen is the chats.ledger_gen those rows carry; another writer
// bumps it, which invalidates the cache (the next checkpoint rewrites all
// rows). head/length identify the committed Ledger slice: the engine shares
// that backing array with the next state when a streamed event leaves the
// ledger alone, so an identical slice header proves "unchanged" in O(1).
type sqlLedgerCache struct {
	gen    int64
	rows   []LedgerEvent
	head   unsafe.Pointer
	length int
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
	var (
		loaded sqlLoadedActor
		found  bool
	)
	err := s.actors.db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		loaded, found, err = loadSQLActorRows(tx, s.chatID)
		return err
	})
	if err != nil || !found {
		s.cache = nil
		return State{}, false, err
	}
	s.cache = &sqlLedgerCache{
		gen: loaded.ledgerGen, rows: exactLedgerCopy(loaded.rows),
		head: unsafe.Pointer(unsafe.SliceData(loaded.state.Ledger)), length: len(loaded.state.Ledger),
	}
	return loaded.state, true, nil
}

type sqlLoadedActor struct {
	state     State
	rows      []LedgerEvent // the stored rows, before any journal replay
	ledgerGen int64
}

// loadSQLActor reads the checkpoint, its ledger rows and journal, and returns
// the same State FileStore.Load would for the equivalent files.
func loadSQLActor(tx *store.Tx, chatID string) (State, bool, error) {
	loaded, found, err := loadSQLActorRows(tx, chatID)
	return loaded.state, found, err
}

func loadSQLActorRows(tx *store.Tx, chatID string) (sqlLoadedActor, bool, error) {
	var (
		version   int
		revision  uint64
		stateJSON []byte
		ledgerLen int
		ledgerGen int64
	)
	err := tx.QueryRow(`SELECT envelope_version, revision, state_json, ledger_len, ledger_gen FROM chats WHERE chat_id = ?`, chatID).
		Scan(&version, &revision, &stateJSON, &ledgerLen, &ledgerGen)
	if errors.Is(err, sql.ErrNoRows) {
		return sqlLoadedActor{}, false, nil
	}
	if err != nil {
		return sqlLoadedActor{}, false, err
	}
	if version != currentStateEnvelopeVersion {
		return sqlLoadedActor{}, false, fmt.Errorf("unsupported chat state version %d", version)
	}
	var state State
	if err := json.Unmarshal(stateJSON, &state); err != nil {
		return sqlLoadedActor{}, false, fmt.Errorf("decode chat state: %w", err)
	}
	stateJSON = nil
	if strings.TrimSpace(state.ChatID) != chatID {
		return sqlLoadedActor{}, false, errors.New("chat state row belongs to another chat")
	}
	if state.Revision != revision {
		return sqlLoadedActor{}, false, fmt.Errorf("chat state revision %d disagrees with its checkpoint row %d", state.Revision, revision)
	}
	var rows []LedgerEvent
	if ledgerLen > 0 {
		rows, err = loadSQLLedger(tx, chatID, ledgerLen)
		if err != nil {
			return sqlLoadedActor{}, false, err
		}
		state.Ledger = rows
	}
	normalizeStateMaps(&state)
	if err := state.Validate(); err != nil {
		return sqlLoadedActor{}, false, fmt.Errorf("validate chat state: %w", err)
	}
	state, err = replaySQLJournal(tx, chatID, state)
	if err != nil {
		return sqlLoadedActor{}, false, fmt.Errorf("replay provider event journal: %w", err)
	}
	return sqlLoadedActor{state: state, rows: rows, ledgerGen: ledgerGen}, true, nil
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

// Save is a checkpoint: the head, the ledger rows that differ from the ones
// this handle last saw, and an empty journal, in one transaction.
func (s *SQLStore) Save(state State) error {
	if err := state.Validate(); err != nil {
		return err
	}
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
	unchanged := cache != nil && cache.sameLedger(state.Ledger)
	var plan sqlLedgerPlan
	err = s.actors.db.Write(context.Background(), func(tx *store.Tx) error {
		var storedGen int64
		err := tx.QueryRow(`SELECT ledger_gen FROM chats WHERE chat_id = ?`, s.chatID).Scan(&storedGen)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		plan = sqlLedgerPlan{}
		if !exists || cache == nil || cache.gen != storedGen || storedGen == 0 {
			plan.full = true
		} else if !unchanged {
			plan.diff(cache.rows, state.Ledger)
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
	if err != nil {
		// The table may or may not hold the rows this commit tried to write:
		// forget the cache so the next checkpoint rewrites every row.
		s.cache = nil
		return err
	}
	s.cache = plan.nextCache(cache, state.Ledger)
	return nil
}

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
		if !reflect.DeepEqual(ledger[index], c.rows[index]) {
			return false
		}
	}
	return true
}

// sqlLedgerPlan is the row-level difference one checkpoint writes.
type sqlLedgerPlan struct {
	full    bool
	changed []int // positions (0-based) to upsert
	gen     int64
}

func (p *sqlLedgerPlan) diff(stored, next []LedgerEvent) {
	shared := len(stored)
	if len(next) < shared {
		shared = len(next)
	}
	for index := 0; index < shared; index++ {
		if !reflect.DeepEqual(stored[index], next[index]) {
			p.changed = append(p.changed, index)
		}
	}
	for index := shared; index < len(next); index++ {
		p.changed = append(p.changed, index)
	}
}

func (p *sqlLedgerPlan) apply(tx *store.Tx, chatID string, ledger []LedgerEvent) error {
	if p.full {
		if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id = ?`, chatID); err != nil {
			return err
		}
		p.changed = p.changed[:0]
		for index := range ledger {
			p.changed = append(p.changed, index)
		}
	} else if _, err := tx.Exec(`DELETE FROM messages WHERE chat_id = ? AND seq > ?`, chatID, len(ledger)); err != nil {
		return err
	}
	if len(p.changed) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`INSERT INTO messages(chat_id, seq, message_id, role, event_json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, seq) DO UPDATE SET message_id = excluded.message_id, role = excluded.role, event_json = excluded.event_json`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, index := range p.changed {
		event := ledger[index]
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(chatID, index+1, event.MessageID, event.Role, sqlText(raw)); err != nil {
			return err
		}
	}
	return nil
}

// nextCache is the cache after a committed plan: a private copy of exactly
// the rows now in the table, re-cloning only the rows the plan wrote.
func (p *sqlLedgerPlan) nextCache(previous *sqlLedgerCache, ledger []LedgerEvent) *sqlLedgerCache {
	next := &sqlLedgerCache{gen: p.gen, head: unsafe.Pointer(unsafe.SliceData(ledger)), length: len(ledger)}
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
// journal; used where no handle cache exists (import).
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
	if !s.cache.sameLedger(next.Ledger) {
		return true
	}
	// Equal content in a new slice (a reducer clone): remember its header so
	// the streamed events that follow take the O(1) path.
	s.cache.head, s.cache.length = unsafe.Pointer(unsafe.SliceData(next.Ledger)), len(next.Ledger)
	return false
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
