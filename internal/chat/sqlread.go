package chat

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"workass/internal/store"
)

// Reads of a chat that is not resident. A daemon keeps only quiescent actors
// closed (Quiescent), and a closed actor's rows are exactly its committed
// ledger: every ledger change checkpoints, and an actor leaves memory only
// after its journal is folded into a checkpoint (checkpointForRetire). These
// reads therefore answer what the engine's ReadMessages, ReadLedgerPageBefore
// and ReadMessage would, row for row, from messages(chat_id, seq) and its
// (chat_id, message_id) index (migration 0004), without opening the actor.
//
// Each read is one read transaction and returns the actor revision it saw, so
// a caller can tell that the actor was opened and changed underneath it.

// DiscoverEach visits every actor's head, ordered by ChatID. It decodes only
// state_json (never messages), so boot reads no transcript. Heads are copied
// out in read transactions of about sqlDiscoverChunkBytes and decoded one at
// a time between them, so boot never holds every head's JSON, or every
// decoded head, at once, and visit may read the store itself. Full
// validation and journal replay happen when the actor is opened.
func (a *SQLActors) DiscoverEach(visit func(ActorHead) error) error {
	type rawHead struct {
		chatID       string
		version      int
		stateJSON    []byte
		ledgerLen    int
		journalBytes int64
		ledgerGen    int64
	}
	after, more := "", true
	for more {
		var raws []rawHead
		err := a.db.Read(context.Background(), func(tx *store.Tx) error {
			raws, more = raws[:0], false
			rows, err := tx.Query(`SELECT chat_id, envelope_version, state_json, ledger_len, journal_bytes, ledger_gen FROM chats WHERE chat_id > ? ORDER BY chat_id`, after)
			if err != nil {
				return err
			}
			defer rows.Close()
			copied := 0
			for rows.Next() {
				if copied >= sqlDiscoverChunkBytes {
					more = true
					break
				}
				var raw rawHead
				if err := rows.Scan(&raw.chatID, &raw.version, &raw.stateJSON, &raw.ledgerLen, &raw.journalBytes, &raw.ledgerGen); err != nil {
					return err
				}
				copied += len(raw.stateJSON)
				raws = append(raws, raw)
			}
			return rows.Err()
		})
		if err != nil {
			return err
		}
		// Decoding is most of discovery; the heads of a chunk decode in
		// parallel and are visited in order.
		states := make([]State, len(raws))
		errs := make([]error, len(raws))
		var next atomic.Int64
		var workers sync.WaitGroup
		for worker := 0; worker < min(len(raws), runtime.GOMAXPROCS(0), 8); worker++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for index := int(next.Add(1)) - 1; index < len(raws); index = int(next.Add(1)) - 1 {
					if raws[index].version == currentStateEnvelopeVersion {
						errs[index] = json.Unmarshal(raws[index].stateJSON, &states[index])
					}
					raws[index].stateJSON = nil
				}
			}()
		}
		workers.Wait()
		for index := range raws {
			raw := &raws[index]
			after = raw.chatID
			if raw.version != currentStateEnvelopeVersion {
				return fmt.Errorf("chat actor %q: unsupported chat state version %d", raw.chatID, raw.version)
			}
			if errs[index] != nil {
				return fmt.Errorf("decode chat actor %q: %w", raw.chatID, errs[index])
			}
			state := states[index]
			states[index] = State{}
			if strings.TrimSpace(state.ChatID) != raw.chatID {
				return fmt.Errorf("chat actor %q holds state for another chat", raw.chatID)
			}
			if raw.ledgerLen > 0 {
				state.Ledger = nil
			}
			if !state.Initialized {
				// Only an uninitialized head's acceptance depends on its tool
				// owners (chat_tools); an accepted head is summarized without
				// them, and opening the actor reads them.
				var rows []sqlToolRow
				if err := a.db.Read(context.Background(), func(tx *store.Tx) error {
					var err error
					rows, err = readSQLTools(tx, raw.chatID)
					return err
				}); err != nil {
					return err
				}
				if _, err := withSQLTools(&state, rows); err != nil {
					return err
				}
			}
			if err := visit(ActorHead{State: state, LedgerLength: raw.ledgerLen, JournalBytes: raw.journalBytes, LedgerGen: raw.ledgerGen}); err != nil {
				return err
			}
		}
	}
	return nil
}

// sqlDiscoverChunkBytes bounds the head JSON one discovery read copies.
const sqlDiscoverChunkBytes = 16 << 20

// NewestLedgerStamp is the stamp of a chat's newest row and its sequence.
type NewestLedgerStamp struct {
	Sequence int
	LedgerStamp
}

// ReadNewestStamps returns the stamp of every chat's newest ledger row, in
// one read: what SummarizeHead needs for almost every chat (the rest, whose
// newest row has no timestamp, read further back with ReadLedgerStamps). The
// newest sequences come from the messages key alone; reading chats.ledger_len
// would walk every head's overflow pages.
func (a *SQLActors) ReadNewestStamps() (map[string]NewestLedgerStamp, error) {
	type rawStamp struct {
		chatID    string
		seq       int
		eventJSON []byte
	}
	var raws []rawStamp
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		rows, err := tx.Query(`SELECT chat_id, max(seq) FROM messages GROUP BY chat_id`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var raw rawStamp
			if err := rows.Scan(&raw.chatID, &raw.seq); err != nil {
				rows.Close()
				return err
			}
			raws = append(raws, raw)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for index := range raws {
			if err := tx.QueryRow(`SELECT event_json FROM messages WHERE chat_id = ? AND seq = ?`, raws[index].chatID, raws[index].seq).Scan(&raws[index].eventJSON); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	stamps := make(map[string]NewestLedgerStamp, len(raws))
	for index := range raws {
		var stamp LedgerStamp
		if err := json.Unmarshal(raws[index].eventJSON, &stamp); err != nil {
			return nil, fmt.Errorf("decode chat %q newest ledger row: %w", raws[index].chatID, err)
		}
		raws[index].eventJSON = nil
		stamps[raws[index].chatID] = NewestLedgerStamp{Sequence: raws[index].seq, LedgerStamp: stamp}
	}
	return stamps, nil
}

// HasLedgerTimestamp reports whether a stamp's timestamp parses (the session
// timestamp rule).
func (s LedgerStamp) HasLedgerTimestamp() bool {
	_, ok := ledgerStampTime(s.At)
	return ok
}

// errSQLChatUnknown: the chat has no checkpoint row.
var errSQLChatUnknown = errors.New("chat actor has no stored checkpoint")

// LedgerView is the committed ledger a closed actor's reads expect: its row
// count and its revision (journal included), from the actor's summary. A
// closed actor's rows are its committed ledger and change only when it is
// opened and commits, which moves the revision, so every read checks the
// revision alone. That reads chats.revision, which precedes state_json in
// the row: a head can be tens of megabytes of overflow pages, and reading
// ledger_len or ledger_gen (after it) walked them on every chunk.
type LedgerView struct {
	Count    int
	Revision uint64
}

// ErrLedgerChanged: the actor committed since the reader's view of it.
var ErrLedgerChanged = errors.New("chat ledger changed since it was summarized")

// errLedgerChangedDuringRead is ErrLedgerChanged by its older name.
var errLedgerChangedDuringRead = ErrLedgerChanged

// checkSQLRevision fails unless the actor is still at view.Revision.
func checkSQLRevision(tx *store.Tx, chatID string, view LedgerView) error {
	var revision, journal int64
	err := tx.QueryRow(`SELECT revision, coalesce((SELECT max(revision) FROM chat_events WHERE chat_events.chat_id = chats.chat_id), 0) FROM chats WHERE chat_id = ?`, chatID).
		Scan(&revision, &journal)
	if errors.Is(err, sql.ErrNoRows) {
		return errSQLChatUnknown
	}
	if err != nil {
		return err
	}
	if current := max(revision, journal); current < 0 || uint64(current) != view.Revision {
		return ErrLedgerChanged
	}
	return nil
}

// sqlMessageSequence is the sequence of the newest row with this message id
// (the engine searches from the newest row), or 0.
func sqlMessageSequence(tx *store.Tx, chatID, messageID string) (int64, error) {
	var seq sql.NullInt64
	if err := tx.QueryRow(`SELECT max(seq) FROM messages WHERE chat_id = ? AND message_id = ?`, chatID, messageID).Scan(&seq); err != nil {
		return 0, err
	}
	return seq.Int64, nil
}

// LedgerWindow is the row range a page read covers: rows (Start, End],
// 1-based, of a ledger of Count rows.
type LedgerWindow struct {
	Start, End, Count int
}

// sqlWindowBefore resolves, inside tx, the window Engine.ReadMessages
// (beforeMessageID empty) or Engine.ReadLedgerPageBefore would read.
func sqlWindowBefore(tx *store.Tx, chatID string, view LedgerView, beforeSequence uint64, beforeMessageID string, limit int) (LedgerWindow, error) {
	if err := checkSQLRevision(tx, chatID, view); err != nil {
		return LedgerWindow{}, err
	}
	before := beforeSequence
	if beforeMessageID != "" {
		seq, err := sqlMessageSequence(tx, chatID, beforeMessageID)
		if err != nil {
			return LedgerWindow{}, err
		}
		if seq == 0 {
			return LedgerWindow{}, errors.New("ledger page boundary does not exist")
		}
		before = uint64(seq)
	}
	window := LedgerWindow{Count: view.Count, End: view.Count}
	if before > 0 {
		if before > uint64(view.Count)+1 {
			return LedgerWindow{}, errors.New("ledger page boundary is ahead of the ledger")
		}
		window.End = int(before - 1)
	}
	window.Start = max(0, window.End-limit)
	return window, nil
}

// LedgerWindowBefore resolves the window ReadLedgerPage would read, without
// reading a row.
func (a *SQLActors) LedgerWindowBefore(chatID string, view LedgerView, beforeSequence uint64, beforeMessageID string, limit int) (LedgerWindow, error) {
	chatID, beforeMessageID = strings.TrimSpace(chatID), strings.TrimSpace(beforeMessageID)
	if limit < 1 {
		return LedgerWindow{}, errors.New("ledger page limit must be positive")
	}
	var window LedgerWindow
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		window, err = sqlWindowBefore(tx, chatID, view, beforeSequence, beforeMessageID, limit)
		return err
	})
	return window, err
}

// ReadLedgerPage is Engine.ReadMessages (beforeMessageID empty) or
// Engine.ReadLedgerPageBefore (beforeMessageID set) for a chat that is not
// resident, as one read. Deleted is never set: the caller knows the tombstone
// from the actor's summary.
func (a *SQLActors) ReadLedgerPage(chatID string, view LedgerView, beforeSequence uint64, beforeMessageID string, limit int) (LedgerPageSnapshot, error) {
	chatID, beforeMessageID = strings.TrimSpace(chatID), strings.TrimSpace(beforeMessageID)
	if limit < 1 {
		return LedgerPageSnapshot{}, errors.New("ledger page limit must be positive")
	}
	var (
		window LedgerWindow
		raws   []sqlRawRow
	)
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		var err error
		if window, err = sqlWindowBefore(tx, chatID, view, beforeSequence, beforeMessageID, limit); err != nil || window.End == window.Start {
			return err
		}
		rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq > ? AND seq <= ? ORDER BY seq`, chatID, window.Start, window.End)
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
		return LedgerPageSnapshot{}, err
	}
	// Decoding happens after the read, so it never holds the connection
	// every chat shares.
	events := make([]LedgerEvent, 0, len(raws))
	for index := range raws {
		raw := &raws[index]
		if raw.seq != int64(window.Start+index+1) {
			return LedgerPageSnapshot{}, fmt.Errorf("chat ledger row %d is missing", window.Start+index+1)
		}
		var event LedgerEvent
		if err := json.Unmarshal(raw.eventJSON, &event); err != nil {
			return LedgerPageSnapshot{}, fmt.Errorf("decode chat ledger row %d: %w", raw.seq, err)
		}
		raw.eventJSON = nil
		events = append(events, event)
	}
	if len(events) != window.End-window.Start {
		return LedgerPageSnapshot{}, fmt.Errorf("chat ledger has %d of rows %d..%d", len(events), window.Start+1, window.End)
	}
	return LedgerPageSnapshot{Events: cloneLedgerEvents(events), Start: window.Start, End: window.End, LedgerCount: view.Count}, nil
}

// sqlLedgerChunkRows is how many rows one read transaction of a row walk
// copies: rows can be tens of megabytes, and every chat shares the
// connection.
const sqlLedgerChunkRows = 4

// readSQLLedgerChunkDesc copies rows (lo, hi] newest first, after checking
// that the actor is still at the view's revision.
func (a *SQLActors) readSQLLedgerChunkDesc(chatID string, view LedgerView, lo, hi int) ([]sqlRawRow, error) {
	var raws []sqlRawRow
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		if err := checkSQLRevision(tx, chatID, view); err != nil {
			return err
		}
		rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq > ? AND seq <= ? ORDER BY seq DESC`, chatID, lo, hi)
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
		return nil, err
	}
	if len(raws) != hi-lo {
		return nil, fmt.Errorf("chat ledger rows %d..%d are missing", lo+1, hi)
	}
	for index := range raws {
		if raws[index].seq != int64(hi-index) {
			return nil, fmt.Errorf("chat ledger row %d is missing", hi-index)
		}
	}
	return raws, nil
}

// WalkLedgerBackward decodes the window's rows newest first, a few per read
// transaction, until visit returns false. A history page keeps only the
// newest rows that fit its byte budget, so a reader that stops there decodes
// the rows it keeps plus one, not the whole window. A commit to the actor
// ends the walk with ErrLedgerChanged.
func (a *SQLActors) WalkLedgerBackward(chatID string, view LedgerView, window LedgerWindow, visit func(LedgerEvent) (bool, error)) error {
	chatID = strings.TrimSpace(chatID)
	for hi := window.End; hi > window.Start; hi -= sqlLedgerChunkRows {
		lo := max(window.Start, hi-sqlLedgerChunkRows)
		raws, err := a.readSQLLedgerChunkDesc(chatID, view, lo, hi)
		if err != nil {
			return err
		}
		for index := range raws {
			var event LedgerEvent
			if err := json.Unmarshal(raws[index].eventJSON, &event); err != nil {
				return fmt.Errorf("decode chat ledger row %d: %w", raws[index].seq, err)
			}
			raws[index].eventJSON = nil
			more, err := visit(cloneLedgerEvents([]LedgerEvent{event})[0])
			if err != nil || !more {
				return err
			}
		}
	}
	return nil
}

// ScanLedgerLiveRows returns the rows in (lo, hi] whose own status is
// pending or running, newest first. Such a row binds a history page together
// (actorHistorySuffixWithin keeps everything from it on), so a reader that
// stops at the byte budget must know where they are. The reducer never
// commits one; only the few rows whose JSON contains such a status anywhere
// (a tool event's, say) are decoded to tell.
func (a *SQLActors) ScanLedgerLiveRows(chatID string, view LedgerView, lo, hi int) ([]int, error) {
	chatID = strings.TrimSpace(chatID)
	var live []int
	for top := hi; top > lo; top -= sqlLedgerChunkRows {
		bottom := max(lo, top-sqlLedgerChunkRows)
		raws, err := a.readSQLLedgerChunkDesc(chatID, view, bottom, top)
		if err != nil {
			return nil, err
		}
		for index := range raws {
			raw := raws[index].eventJSON
			if !bytes.Contains(raw, []byte(`"Status":"pending"`)) && !bytes.Contains(raw, []byte(`"Status":"running"`)) {
				continue
			}
			var row struct{ Status string }
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, fmt.Errorf("decode chat ledger row %d: %w", raws[index].seq, err)
			}
			if row.Status == "pending" || row.Status == "running" {
				live = append(live, int(raws[index].seq))
			}
		}
	}
	return live, nil
}

// ReadLedgerMessage is Engine.ReadMessage for a chat that is not resident.
func (a *SQLActors) ReadLedgerMessage(chatID string, view LedgerView, messageID string) (LedgerEvent, bool, error) {
	chatID, messageID = strings.TrimSpace(chatID), strings.TrimSpace(messageID)
	if messageID == "" {
		return LedgerEvent{}, false, nil
	}
	var raw []byte
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raw = nil
		if err := checkSQLRevision(tx, chatID, view); err != nil {
			return err
		}
		seq, err := sqlMessageSequence(tx, chatID, messageID)
		if err != nil || seq == 0 {
			return err
		}
		return tx.QueryRow(`SELECT event_json FROM messages WHERE chat_id = ? AND seq = ?`, chatID, seq).Scan(&raw)
	})
	if err != nil || raw == nil {
		return LedgerEvent{}, false, err
	}
	var event LedgerEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return LedgerEvent{}, false, fmt.Errorf("decode chat ledger row: %w", err)
	}
	return cloneLedgerEvents([]LedgerEvent{event})[0], true, nil
}

// ReadLedgerStamps returns the identity and timestamp of a chat's trailing
// rows, oldest first, newest last: back from the newest row until one has a
// parseable timestamp, at most window rows. That is all SummarizeHead needs.
func (a *SQLActors) ReadLedgerStamps(chatID string, ledgerLength, window int) ([]LedgerStamp, error) {
	chatID = strings.TrimSpace(chatID)
	if ledgerLength <= 0 || window <= 0 {
		return nil, nil
	}
	var stamps []LedgerStamp
	from := ledgerLength
	for from > 0 && ledgerLength-from < window {
		// Rows can be megabytes; read a few at a time and stop at the first
		// timestamp.
		batch := min(4, window-(ledgerLength-from), from)
		var raws []sqlRawRow
		err := a.db.Read(context.Background(), func(tx *store.Tx) error {
			raws = raws[:0]
			rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq > ? AND seq <= ? ORDER BY seq DESC`, chatID, from-batch, from)
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
			return nil, err
		}
		if len(raws) != batch {
			return nil, fmt.Errorf("chat ledger rows %d..%d are missing", from-batch+1, from)
		}
		found := false
		for index := range raws {
			if raws[index].seq != int64(from-index) {
				return nil, fmt.Errorf("chat ledger row %d is missing", from-index)
			}
			var stamp LedgerStamp
			if err := json.Unmarshal(raws[index].eventJSON, &stamp); err != nil {
				return nil, fmt.Errorf("decode chat ledger row %d: %w", raws[index].seq, err)
			}
			raws[index].eventJSON = nil
			stamps = append(stamps, stamp)
			if _, ok := ledgerStampTime(stamp.At); ok {
				found = true
				break
			}
		}
		if found {
			break
		}
		from -= batch
	}
	for left, right := 0, len(stamps)-1; left < right; left, right = left+1, right-1 {
		stamps[left], stamps[right] = stamps[right], stamps[left]
	}
	return stamps, nil
}

// needsCheckpointAfterLoad: the loaded state is not the stored checkpoint.
// Journal records were replayed onto the head (and may have edited an old row
// the rows still lag), or the rows predate incremental checkpoints.
func (s *SQLStore) needsCheckpointAfterLoad() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache == nil || s.cache.gen == 0 || s.journalRecords > 0
}

// checkpointForRetire folds the journal into one checkpoint so the closed
// actor's head and rows are exactly its state, then drops this handle's copy
// of the rows: the actor leaves memory, and so does its cache.
func (s *SQLStore) checkpointForRetire(state State) error {
	if strings.TrimSpace(state.ChatID) != s.chatID || s.chatID == "" {
		return errors.New("chat state store belongs to another chat")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil || s.cache.gen == 0 || s.journalRecords > 0 {
		if err := s.checkpointLocked(state); err != nil {
			return err
		}
	}
	s.cache = nil
	return nil
}

// LedgerHasNativeTurn reports whether a row of a closed chat names this
// native turn id: the "finished turn" answer Stop gives for a job id it
// cannot cancel.
func (a *SQLActors) LedgerHasNativeTurn(chatID, jobID string) (bool, error) {
	chatID, jobID = strings.TrimSpace(chatID), strings.TrimSpace(jobID)
	if jobID == "" {
		return false, nil
	}
	encoded, err := json.Marshal(jobID)
	if err != nil {
		return false, err
	}
	needle := strings.Trim(string(encoded), `"`)
	var raws [][]byte
	err = a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		rows, err := tx.Query(`SELECT event_json FROM messages WHERE chat_id = ? AND instr(event_json, ?) > 0`, chatID, needle)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			raws = append(raws, raw)
		}
		return rows.Err()
	})
	if err != nil {
		return false, err
	}
	for _, raw := range raws {
		var row struct{ NativeTurnID string }
		if json.Unmarshal(raw, &row) == nil && strings.TrimSpace(row.NativeTurnID) == jobID {
			return true, nil
		}
	}
	return false, nil
}

// sqlRowDecoder decodes an actor's ledger rows on worker goroutines while
// its load keeps reading chunks: decoding is most of opening a large actor,
// and rows decode independently. Each row lands in its position, so the
// result is what decoding them in order gives. In-flight raw bytes are
// bounded.
type sqlRowDecoder struct {
	ledger []LedgerEvent
	read   int
	work   chan sqlRawRow
	wg     sync.WaitGroup

	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	err      error
	stopped  bool
}

// sqlDecodeInflightBytes bounds the raw row bytes waiting for a decoder.
const sqlDecodeInflightBytes = 64 << 20

func newSQLRowDecoder(rows int) *sqlRowDecoder {
	decoder := &sqlRowDecoder{ledger: make([]LedgerEvent, rows), work: make(chan sqlRawRow, 64)}
	decoder.cond = sync.NewCond(&decoder.mu)
	for worker := 0; worker < min(runtime.GOMAXPROCS(0), 8); worker++ {
		decoder.wg.Add(1)
		go func() {
			defer decoder.wg.Done()
			for raw := range decoder.work {
				err := json.Unmarshal(raw.eventJSON, &decoder.ledger[raw.seq-1])
				decoder.mu.Lock()
				decoder.inflight -= len(raw.eventJSON)
				if err != nil && decoder.err == nil {
					decoder.err = fmt.Errorf("decode chat ledger row %d: %w", raw.seq, err)
				}
				decoder.cond.Broadcast()
				decoder.mu.Unlock()
			}
		}()
	}
	return decoder
}

// submit queues rows that continue the ledger without a gap.
func (d *sqlRowDecoder) submit(rows []sqlRawRow) error {
	for index := range rows {
		raw := rows[index]
		rows[index].eventJSON = nil
		if raw.seq != int64(d.read+1) || d.read >= len(d.ledger) {
			return fmt.Errorf("chat ledger row %d is out of position %d", raw.seq, d.read+1)
		}
		d.read++
		d.mu.Lock()
		for d.inflight > 0 && d.inflight+len(raw.eventJSON) > sqlDecodeInflightBytes && d.err == nil {
			d.cond.Wait()
		}
		err := d.err
		d.inflight += len(raw.eventJSON)
		d.mu.Unlock()
		if err != nil {
			return err
		}
		d.work <- raw
	}
	return nil
}

// finish waits for every queued row and returns the decoded ledger.
func (d *sqlRowDecoder) finish() ([]LedgerEvent, error) {
	d.stop()
	if d.err != nil {
		return nil, d.err
	}
	if d.read != len(d.ledger) {
		return nil, fmt.Errorf("chat ledger has %d rows, its checkpoint expects %d", d.read, len(d.ledger))
	}
	return d.ledger, nil
}

// stop ends the workers; it is safe to call more than once.
func (d *sqlRowDecoder) stop() {
	if d == nil || d.stopped {
		return
	}
	d.stopped = true
	close(d.work)
	d.wg.Wait()
}

// LedgerAttachmentRefs returns every attachment reference the ledger owns in
// the sense of Engine.AttachmentRefOwnership: a row's own attachments and the
// tool attachments in its timeline. It reads the rows once, in chunks checked
// against revision, decoding only those fields; a caller keeps the set to
// answer image reads of a closed chat without scanning its rows per image.
func (a *SQLActors) LedgerAttachmentRefs(chatID string, view LedgerView) (map[string]struct{}, error) {
	chatID = strings.TrimSpace(chatID)
	type refs struct {
		Attachments []struct{ Ref string }
		Timeline    []struct {
			Tool *struct{ Attachments []struct{ Ref string } }
		}
	}
	owned := make(map[string]struct{})
	after := int64(0)
	for done := false; !done; {
		var chunk []sqlRawRow
		err := a.db.Read(context.Background(), func(tx *store.Tx) error {
			if err := checkSQLRevision(tx, chatID, view); err != nil {
				return err
			}
			var err error
			chunk, done, err = readSQLLedgerChunk(tx, chatID, after, sqlLoadChunkBytes, nil)
			return err
		})
		if err != nil {
			return nil, err
		}
		for index := range chunk {
			if chunk[index].seq != after+1 {
				return nil, fmt.Errorf("chat ledger row %d is missing", after+1)
			}
			after = chunk[index].seq
			raw := chunk[index].eventJSON
			chunk[index].eventJSON = nil
			if !bytes.Contains(raw, []byte(`"Ref":"`)) {
				continue
			}
			var row refs
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, fmt.Errorf("decode chat ledger row %d: %w", after, err)
			}
			for _, attachment := range row.Attachments {
				owned[attachment.Ref] = struct{}{}
			}
			for _, entry := range row.Timeline {
				if entry.Tool == nil {
					continue
				}
				for _, attachment := range entry.Tool.Attachments {
					owned[attachment.Ref] = struct{}{}
				}
			}
		}
	}
	if after != int64(view.Count) {
		return nil, fmt.Errorf("chat ledger has %d rows, its summary %d", after, view.Count)
	}
	delete(owned, "")
	return owned, nil
}
