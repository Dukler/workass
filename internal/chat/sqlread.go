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
			if err := visit(ActorHead{State: state, LedgerLength: raw.ledgerLen, JournalBytes: raw.journalBytes, LedgerGen: raw.ledgerGen}); err != nil {
				return err
			}
		}
	}
	return nil
}

// sqlDiscoverChunkBytes bounds the head JSON one discovery read copies.
const sqlDiscoverChunkBytes = 16 << 20

// ReadNewestStamps returns the stamp of every chat's newest ledger row, in
// one read: what SummarizeHead needs for almost every chat (the rest, whose
// newest row has no timestamp, read further back with ReadLedgerStamps).
func (a *SQLActors) ReadNewestStamps() (map[string]LedgerStamp, error) {
	type rawStamp struct {
		chatID    string
		eventJSON []byte
	}
	var raws []rawStamp
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		rows, err := tx.Query(`SELECT messages.chat_id, messages.event_json FROM chats JOIN messages ON messages.chat_id = chats.chat_id AND messages.seq = chats.ledger_len`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw rawStamp
			if err := rows.Scan(&raw.chatID, &raw.eventJSON); err != nil {
				return err
			}
			raws = append(raws, raw)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	stamps := make(map[string]LedgerStamp, len(raws))
	for index := range raws {
		var stamp LedgerStamp
		if err := json.Unmarshal(raws[index].eventJSON, &stamp); err != nil {
			return nil, fmt.Errorf("decode chat %q newest ledger row: %w", raws[index].chatID, err)
		}
		raws[index].eventJSON = nil
		stamps[raws[index].chatID] = stamp
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

// sqlLedgerHead is the part of a chats row a ledger read checks.
type sqlLedgerHead struct {
	ledgerLen int
	revision  uint64
}

func readSQLLedgerHead(tx *store.Tx, chatID string) (sqlLedgerHead, error) {
	var head sqlLedgerHead
	var ledgerGen, journalRevision int64
	err := tx.QueryRow(`SELECT ledger_len, ledger_gen, revision, coalesce((SELECT max(revision) FROM chat_events WHERE chat_events.chat_id = chats.chat_id), 0) FROM chats WHERE chat_id = ?`, chatID).
		Scan(&head.ledgerLen, &ledgerGen, &head.revision, &journalRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return head, errSQLChatUnknown
	}
	if err != nil {
		return head, err
	}
	if uint64(journalRevision) > head.revision {
		head.revision = uint64(journalRevision)
	}
	if ledgerGen == 0 {
		return head, ErrLedgerNotCheckpointed
	}
	return head, nil
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

// ReadLedgerPage is Engine.ReadMessages (beforeMessageID empty) or
// Engine.ReadLedgerPageBefore (beforeMessageID set) for a chat that is not
// resident, with the actor revision the rows belong to. Deleted is never set:
// the caller knows the tombstone from the actor's summary.
func (a *SQLActors) ReadLedgerPage(chatID string, beforeSequence uint64, beforeMessageID string, limit int) (LedgerPageSnapshot, uint64, error) {
	chatID, beforeMessageID = strings.TrimSpace(chatID), strings.TrimSpace(beforeMessageID)
	if limit < 1 {
		return LedgerPageSnapshot{}, 0, errors.New("ledger page limit must be positive")
	}
	var (
		page     LedgerPageSnapshot
		revision uint64
		raws     []sqlRawRow
	)
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		page, raws = LedgerPageSnapshot{}, raws[:0]
		head, err := readSQLLedgerHead(tx, chatID)
		if err != nil {
			return err
		}
		revision = head.revision
		before := beforeSequence
		if beforeMessageID != "" {
			seq, err := sqlMessageSequence(tx, chatID, beforeMessageID)
			if err != nil {
				return err
			}
			if seq == 0 {
				return errors.New("ledger page boundary does not exist")
			}
			before = uint64(seq)
		}
		count := head.ledgerLen
		end := count
		if before > 0 {
			if before > uint64(count)+1 {
				return errors.New("ledger page boundary is ahead of the ledger")
			}
			end = int(before - 1)
		}
		start := max(0, end-limit)
		page = LedgerPageSnapshot{Start: start, End: end, LedgerCount: count}
		if end == start {
			return nil
		}
		rows, err := tx.Query(`SELECT seq, event_json FROM messages WHERE chat_id = ? AND seq > ? AND seq <= ? ORDER BY seq`, chatID, start, end)
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
		return LedgerPageSnapshot{}, 0, err
	}
	// Decoding happens after the read, so it never holds the connection
	// every chat shares.
	events := make([]LedgerEvent, 0, len(raws))
	for index := range raws {
		raw := &raws[index]
		if raw.seq != int64(page.Start+index+1) {
			return LedgerPageSnapshot{}, 0, fmt.Errorf("chat ledger row %d is missing", page.Start+index+1)
		}
		var event LedgerEvent
		if err := json.Unmarshal(raw.eventJSON, &event); err != nil {
			return LedgerPageSnapshot{}, 0, fmt.Errorf("decode chat ledger row %d: %w", raw.seq, err)
		}
		raw.eventJSON = nil
		events = append(events, event)
	}
	if len(events) != page.End-page.Start {
		return LedgerPageSnapshot{}, 0, fmt.Errorf("chat ledger has %d of rows %d..%d", len(events), page.Start+1, page.End)
	}
	page.Events = cloneLedgerEvents(events)
	return page, revision, nil
}

// LedgerWindow is the row range a page read covers: rows (Start, End],
// 1-based, of a ledger of Count rows.
type LedgerWindow struct {
	Start, End, Count int
}

// LedgerWindowBefore resolves the window ReadLedgerPage(beforeSequence,
// beforeMessageID, limit) reads, with the actor revision, without reading a
// row.
func (a *SQLActors) LedgerWindowBefore(chatID string, beforeSequence uint64, beforeMessageID string, limit int) (LedgerWindow, uint64, error) {
	chatID, beforeMessageID = strings.TrimSpace(chatID), strings.TrimSpace(beforeMessageID)
	if limit < 1 {
		return LedgerWindow{}, 0, errors.New("ledger page limit must be positive")
	}
	var (
		window   LedgerWindow
		revision uint64
	)
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		head, err := readSQLLedgerHead(tx, chatID)
		if err != nil {
			return err
		}
		revision = head.revision
		before := beforeSequence
		if beforeMessageID != "" {
			seq, err := sqlMessageSequence(tx, chatID, beforeMessageID)
			if err != nil {
				return err
			}
			if seq == 0 {
				return errors.New("ledger page boundary does not exist")
			}
			before = uint64(seq)
		}
		window = LedgerWindow{Count: head.ledgerLen, End: head.ledgerLen}
		if before > 0 {
			if before > uint64(head.ledgerLen)+1 {
				return errors.New("ledger page boundary is ahead of the ledger")
			}
			window.End = int(before - 1)
		}
		window.Start = max(0, window.End-limit)
		return nil
	})
	return window, revision, err
}

// sqlLedgerChunkRows is how many rows one read transaction of a row walk
// copies: rows can be tens of megabytes, and every chat shares the
// connection.
const sqlLedgerChunkRows = 4

// readSQLLedgerChunkDesc copies rows (lo, hi] newest first, after checking
// that the actor is still at revision with count rows.
func (a *SQLActors) readSQLLedgerChunkDesc(chatID string, lo, hi, count int, revision uint64) ([]sqlRawRow, error) {
	var raws []sqlRawRow
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		head, err := readSQLLedgerHead(tx, chatID)
		if err != nil {
			return err
		}
		if head.revision != revision || head.ledgerLen != count {
			return errLedgerChangedDuringRead
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
// between two chunks ends the walk with an error.
func (a *SQLActors) WalkLedgerBackward(chatID string, window LedgerWindow, revision uint64, visit func(LedgerEvent) (bool, error)) error {
	chatID = strings.TrimSpace(chatID)
	for hi := window.End; hi > window.Start; hi -= sqlLedgerChunkRows {
		lo := max(window.Start, hi-sqlLedgerChunkRows)
		raws, err := a.readSQLLedgerChunkDesc(chatID, lo, hi, window.Count, revision)
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
func (a *SQLActors) ScanLedgerLiveRows(chatID string, lo, hi, count int, revision uint64) ([]int, error) {
	chatID = strings.TrimSpace(chatID)
	var live []int
	for top := hi; top > lo; top -= sqlLedgerChunkRows {
		bottom := max(lo, top-sqlLedgerChunkRows)
		raws, err := a.readSQLLedgerChunkDesc(chatID, bottom, top, count, revision)
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

// errLedgerChangedDuringRead: the actor committed between two chunks of a
// backward read.
var errLedgerChangedDuringRead = errors.New("chat ledger changed during the read")

// ReadLedgerMessage is Engine.ReadMessage for a chat that is not resident.
func (a *SQLActors) ReadLedgerMessage(chatID, messageID string) (LedgerEvent, bool, uint64, error) {
	chatID, messageID = strings.TrimSpace(chatID), strings.TrimSpace(messageID)
	if messageID == "" {
		return LedgerEvent{}, false, 0, nil
	}
	var (
		raw      []byte
		revision uint64
	)
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raw = nil
		head, err := readSQLLedgerHead(tx, chatID)
		if err != nil {
			return err
		}
		revision = head.revision
		seq, err := sqlMessageSequence(tx, chatID, messageID)
		if err != nil || seq == 0 {
			return err
		}
		return tx.QueryRow(`SELECT event_json FROM messages WHERE chat_id = ? AND seq = ?`, chatID, seq).Scan(&raw)
	})
	if err != nil || raw == nil {
		return LedgerEvent{}, false, revision, err
	}
	var event LedgerEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return LedgerEvent{}, false, 0, fmt.Errorf("decode chat ledger row: %w", err)
	}
	return cloneLedgerEvents([]LedgerEvent{event})[0], true, revision, nil
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

// LedgerOwnsAttachment is the ledger half of Engine.AttachmentRefOwnership
// for a chat that is not resident (a closed actor has no foreground turn or
// pending steer): whether a row's own attachment or a tool attachment in its
// timeline is ref. Only rows whose JSON contains ref are decoded.
func (a *SQLActors) LedgerOwnsAttachment(chatID, ref string) (bool, uint64, error) {
	chatID = strings.TrimSpace(chatID)
	if ref == "" {
		return false, 0, nil
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return false, 0, err
	}
	needle := string(encoded)
	var (
		raws     [][]byte
		revision uint64
	)
	err = a.db.Read(context.Background(), func(tx *store.Tx) error {
		raws = raws[:0]
		head, err := readSQLLedgerHead(tx, chatID)
		if err != nil {
			return err
		}
		revision = head.revision
		rows, err := tx.Query(`SELECT event_json FROM messages WHERE chat_id = ? AND instr(event_json, ?) > 0 ORDER BY seq`, chatID, needle)
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
		return false, 0, err
	}
	for _, raw := range raws {
		var row LedgerEvent
		if err := json.Unmarshal(raw, &row); err != nil {
			return false, 0, fmt.Errorf("decode chat ledger row: %w", err)
		}
		if timelineOwnsAttachment(row.Timeline, ref) || attachmentsOwn(row.Attachments, ref) {
			return true, revision, nil
		}
	}
	return false, revision, nil
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
