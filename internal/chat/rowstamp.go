package chat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"sync"

	"workass/internal/store"
)

// Row stamps: what a reader that keeps something derived from one stored
// ledger row (a folded row's calls, encoded once: cmd/workass
// provider_chat_work_group_pack.go) needs to know that the row is still the
// one it read. A row is stamped with the CRC-32C of its stored JSON.
//
// The rows this process writes are stamped as they commit, so a reader asks
// RowStamp for free; a row this process has not written is as it was when
// the process started, and a reader that kept it from an earlier process
// compares the stamp of its stored bytes once (RowCRC).

var rowCRCTable = crc32.MakeTable(crc32.Castagnoli)

// RowCRC is the stamp of a row's stored JSON.
func RowCRC(raw []byte) uint32 { return crc32.Checksum(raw, rowCRCTable) }

type rowStamps struct {
	mu   sync.Mutex
	rows map[string]map[int]uint32
}

// note records a committed plan's rows (seq is position + 1) and forgets the
// rows past the ledger's end.
func (s *rowStamps) note(chatID string, plan *sqlLedgerPlan, ledgerLen int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rows == nil {
		s.rows = make(map[string]map[int]uint32)
	}
	rows := s.rows[chatID]
	if rows == nil || plan.full {
		rows = make(map[int]uint32, len(plan.changed))
		s.rows[chatID] = rows
	}
	for seq := range rows {
		if seq > ledgerLen {
			delete(rows, seq)
		}
	}
	for position, index := range plan.changed {
		if position < len(plan.encoded) {
			rows[index+1] = RowCRC(plan.encoded[position])
		}
	}
	if plan.full {
		// Every row was rewritten; one not in the plan no longer exists.
		for seq := range rows {
			if seq > ledgerLen {
				delete(rows, seq)
			}
		}
	}
}

// RowStamp is the stamp of the row at seq if this process wrote it, and
// written reports whether it did. A full rewrite this process committed
// counts every row it holds as written: written is then true and ok false
// for a row past its end.
func (a *SQLActors) RowStamp(chatID string, seq int) (stamp uint32, written bool) {
	if a == nil {
		return 0, false
	}
	a.stamps.mu.Lock()
	defer a.stamps.mu.Unlock()
	stamp, written = a.stamps.rows[strings.TrimSpace(chatID)][seq]
	return stamp, written
}

// ReadLedgerMessageRawAt is ReadLedgerMessageRaw with the row's sequence.
func (a *SQLActors) ReadLedgerMessageRawAt(chatID string, view LedgerView, messageID string) ([]byte, int, bool, error) {
	chatID, messageID = strings.TrimSpace(chatID), strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, 0, false, nil
	}
	var raw []byte
	var seq int64
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raw = nil
		if err := checkSQLRevision(tx, chatID, view); err != nil {
			return err
		}
		var err error
		seq, err = sqlMessageSequence(tx, chatID, messageID)
		if err != nil || seq == 0 {
			return err
		}
		return tx.QueryRow(`SELECT event_json FROM messages WHERE chat_id = ? AND seq = ?`, chatID, seq).Scan(&raw)
	})
	if err != nil || raw == nil {
		return nil, 0, false, err
	}
	return raw, int(seq), true, nil
}

// ReadLedgerRowAt reads the stored row at seq if it still holds messageID.
func (a *SQLActors) ReadLedgerRowAt(chatID string, seq int, messageID string) ([]byte, bool, error) {
	var raw []byte
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		raw = nil
		var id string
		err := tx.QueryRow(`SELECT message_id, event_json FROM messages WHERE chat_id = ? AND seq = ?`, strings.TrimSpace(chatID), seq).Scan(&id, &raw)
		if err != nil {
			raw = nil
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if id != strings.TrimSpace(messageID) {
			raw = nil
		}
		return nil
	})
	if err != nil || raw == nil {
		return nil, false, err
	}
	return raw, true, nil
}

// MessageStamp is where a committed row sits and the actor's revision,
// without copying the row.
func (e *Engine) MessageStamp(messageID string) (seq int, revision uint64, ok bool) {
	messageID = strings.TrimSpace(messageID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Deleted || messageID == "" {
		return 0, 0, false
	}
	for index := len(e.state.Ledger) - 1; index >= 0; index-- {
		if e.state.Ledger[index].MessageID == messageID {
			return index + 1, e.state.Revision, true
		}
	}
	return 0, 0, false
}

// ReadMessageAt is ReadMessage with the row's sequence and the actor's
// revision.
func (e *Engine) ReadMessageAt(messageID string) (LedgerEvent, int, uint64, bool) {
	messageID = strings.TrimSpace(messageID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Deleted || messageID == "" {
		return LedgerEvent{}, 0, 0, false
	}
	for index := len(e.state.Ledger) - 1; index >= 0; index-- {
		if e.state.Ledger[index].MessageID == messageID {
			return cloneLedgerEvents(e.state.Ledger[index : index+1])[0], index + 1, e.state.Revision, true
		}
	}
	return LedgerEvent{}, 0, 0, false
}

// ReadLedgerMessageForPack reads the newest row with this message id, its
// sequence and the chat's revision, in one read.
func (a *SQLActors) ReadLedgerMessageForPack(chatID, messageID string) (raw []byte, seq int, revision uint64, found bool, err error) {
	chatID, messageID = strings.TrimSpace(chatID), strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, 0, 0, false, nil
	}
	err = a.db.Read(context.Background(), func(tx *store.Tx) error {
		raw = nil
		var stored, journal int64
		if err := tx.QueryRow(`SELECT revision, coalesce((SELECT max(revision) FROM chat_events WHERE chat_events.chat_id = chats.chat_id), 0) FROM chats WHERE chat_id = ?`, chatID).
			Scan(&stored, &journal); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		revision = uint64(max(stored, journal, 0))
		sequence, err := sqlMessageSequence(tx, chatID, messageID)
		if err != nil || sequence == 0 {
			return err
		}
		seq = int(sequence)
		return tx.QueryRow(`SELECT event_json FROM messages WHERE chat_id = ? AND seq = ?`, chatID, sequence).Scan(&raw)
	})
	if err != nil || raw == nil {
		return nil, 0, 0, false, err
	}
	return raw, seq, revision, true, nil
}

// Revision is the actor's committed revision.
func (e *Engine) Revision() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state.Revision
}

// NextMessageID is the id of the row after the newest row with messageID,
// "" when it is the newest.
func (a *SQLActors) NextMessageID(chatID, messageID string) (string, bool, error) {
	chatID, messageID = strings.TrimSpace(chatID), strings.TrimSpace(messageID)
	var next sql.NullString
	found := false
	err := a.db.Read(context.Background(), func(tx *store.Tx) error {
		seq, err := sqlMessageSequence(tx, chatID, messageID)
		if err != nil || seq == 0 {
			return err
		}
		found = true
		err = tx.QueryRow(`SELECT message_id FROM messages WHERE chat_id = ? AND seq = ?`, chatID, seq+1).Scan(&next)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return next.String, found, err
}

// NextMessageID is SQLActors.NextMessageID of an open actor.
func (e *Engine) NextMessageID(messageID string) (string, bool) {
	messageID = strings.TrimSpace(messageID)
	e.mu.Lock()
	defer e.mu.Unlock()
	for index := len(e.state.Ledger) - 1; index >= 0; index-- {
		if e.state.Ledger[index].MessageID == messageID {
			if index+1 < len(e.state.Ledger) {
				return e.state.Ledger[index+1].MessageID, true
			}
			return "", true
		}
	}
	return "", false
}

// DecodeLedgerWindowDesc reads the window's rows in one read and decodes them
// in parallel (decode nil: json.Unmarshal), newest first. For a first screen
// a few rows of megabytes each are most of its time; read and decoded one
// after another they took 9 of its 16 ms.
func (a *SQLActors) DecodeLedgerWindowDesc(chatID string, view LedgerView, window LedgerWindow, decode func([]byte) (LedgerEvent, error)) ([]LedgerEvent, error) {
	raws, err := a.readSQLLedgerChunkDesc(strings.TrimSpace(chatID), view, window.Start, window.End)
	if err != nil {
		return nil, err
	}
	events := make([]LedgerEvent, len(raws))
	errs := make([]error, len(raws))
	var wg sync.WaitGroup
	for index := range raws {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if decode != nil {
				events[index], errs[index] = decode(raws[index].eventJSON)
			} else {
				errs[index] = json.Unmarshal(raws[index].eventJSON, &events[index])
			}
			if errs[index] != nil {
				errs[index] = fmt.Errorf("decode chat ledger row %d: %w", raws[index].seq, errs[index])
			}
		}(index)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return events, nil
}
