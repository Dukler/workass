package chat

import (
	"errors"
	"strings"
)

// LedgerReader is the bounded transcript read surface behind every renderer
// history path: one page of committed rows before a sequence, and one row by
// its stable message id (a folded tool's full output). Neither read copies
// more than it returns, and neither has write authority.
//
// The Engine answers both from its resident State today. A store that keeps
// the ledger as rows (messages(chat_id, seq, ...)) answers the same calls with
// `WHERE seq < ? ORDER BY seq DESC LIMIT ?` and a message-id index, without
// materializing the whole actor first.
type LedgerReader interface {
	// ReadLedgerPageBefore is the renderer's cursor form: the page ending
	// immediately before the row with this stable message id.
	ReadLedgerPageBefore(beforeMessageID string, limit int) (LedgerPageSnapshot, error)
	// ReadMessages returns up to limit committed rows ending immediately
	// before beforeSequence, in ledger order. beforeSequence 0 means "after
	// the newest row". LedgerCount is the committed row count.
	ReadMessages(beforeSequence uint64, limit int) (LedgerPageSnapshot, error)
	// ReadMessage returns one committed row by its stable message id.
	ReadMessage(messageID string) (LedgerEvent, bool)
}

var _ LedgerReader = (*Engine)(nil)

func (e *Engine) ReadMessages(beforeSequence uint64, limit int) (LedgerPageSnapshot, error) {
	if limit < 1 {
		return LedgerPageSnapshot{}, errors.New("ledger page limit must be positive")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.readMessagesLocked(beforeSequence, limit)
}

func (e *Engine) readMessagesLocked(beforeSequence uint64, limit int) (LedgerPageSnapshot, error) {
	count := len(e.state.Ledger)
	end := count
	if beforeSequence > 0 {
		// Ledger sequences are contiguous from one (State.Validate), so a
		// sequence is its own index.
		if beforeSequence > uint64(count)+1 {
			return LedgerPageSnapshot{}, errors.New("ledger page boundary is ahead of the ledger")
		}
		end = int(beforeSequence - 1)
	}
	start := max(0, end-limit)
	return LedgerPageSnapshot{
		Events: cloneLedgerEvents(e.state.Ledger[start:end]), Start: start, End: end,
		LedgerCount: count, Deleted: e.state.Deleted,
	}, nil
}

func (e *Engine) ReadMessage(messageID string) (LedgerEvent, bool) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return LedgerEvent{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.Deleted {
		return LedgerEvent{}, false
	}
	for index := len(e.state.Ledger) - 1; index >= 0; index-- {
		if e.state.Ledger[index].MessageID == messageID {
			return cloneLedgerEvents(e.state.Ledger[index : index+1])[0], true
		}
	}
	return LedgerEvent{}, false
}
