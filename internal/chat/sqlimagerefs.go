package chat

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"workass/internal/store"
)

// ImageReferenceChat is what the image store needs to know about one durable
// chat: its identity and whether it is a tombstone. UpdatedAt is when its
// durable state last changed; for a tombstone it is no earlier than the
// deletion.
type ImageReferenceChat struct {
	ChatID    string
	TabID     string
	Deleted   bool
	UpdatedAt string
}

// Image reference row kinds.
const (
	ImageReferenceHead    = "head"
	ImageReferenceMessage = "message"
	ImageReferenceJournal = "journal"
	ImageReferenceTool    = "tool"
)

// ErrImageReferenceScanChanged reports that a chat kept changing while its
// rows were read, so the read cannot vouch for what the chat references.
var ErrImageReferenceScanChanged = errors.New("chat changed throughout its image reference scan")

// ImageReferenceChats lists every chat in the database, ordered by id.
func (a *SQLActors) ImageReferenceChats(ctx context.Context) ([]ImageReferenceChat, error) {
	var chats []ImageReferenceChat
	err := a.db.Read(ctx, func(tx *store.Tx) error {
		chats = chats[:0]
		rows, err := tx.Query(`SELECT chat_id,
				coalesce(json_extract(state_json, '$.Deleted'), 0),
				coalesce(json_extract(state_json, '$.Presentation.TabID'), ''),
				updated_at
			FROM chats ORDER BY chat_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var chat ImageReferenceChat
			var deleted int
			if err := rows.Scan(&chat.ChatID, &deleted, &chat.TabID, &chat.UpdatedAt); err != nil {
				return err
			}
			chat.Deleted = deleted != 0
			chats = append(chats, chat)
		}
		return rows.Err()
	})
	return chats, err
}

type imageReferenceStamp struct {
	revision, ledgerGen, journal int64
}

func readImageReferenceStamp(tx *store.Tx, chatID string) (imageReferenceStamp, error) {
	var stamp imageReferenceStamp
	err := tx.QueryRow(`SELECT revision, ledger_gen,
			coalesce((SELECT max(revision) FROM chat_events WHERE chat_events.chat_id = chats.chat_id), 0)
		FROM chats WHERE chat_id = ?`, chatID).Scan(&stamp.revision, &stamp.ledgerGen, &stamp.journal)
	if errors.Is(err, sql.ErrNoRows) {
		return stamp, errSQLChatUnknown
	}
	return stamp, err
}

// imageReferenceScanAttempts bounds the restarts of one chat's scan when the
// chat commits while it is being read.
const imageReferenceScanAttempts = 4

// EachImageReferenceRow visits every durable serialized value of one chat
// that can name an image: its head, its ledger rows, its journal records and
// its tool owners (kinds selects some; nil is all). Each read transaction
// copies at most sqlLoadChunkBytes, so a scan never holds the shared
// connection long, and pause (when set) runs between them.
//
// With stable set, the scan must describe one committed state: a chat that
// commits while it is read is read again from the start, after restart tells
// the caller to forget what it saw, and one that keeps changing fails with
// ErrImageReferenceScanChanged. Without it, rows are read as they are (a
// best-effort reader such as clone hints).
func (a *SQLActors) EachImageReferenceRow(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, restart func(), visit func(kind string, data []byte) error) error {
	chatID = strings.TrimSpace(chatID)
	for attempt := 0; attempt < imageReferenceScanAttempts; attempt++ {
		if attempt > 0 && restart != nil {
			restart()
		}
		err := a.eachImageReferenceRowOnce(ctx, chatID, kinds, stable, pause, visit)
		if !errors.Is(err, ErrLedgerChanged) {
			return err
		}
	}
	return fmt.Errorf("%w: %s", ErrImageReferenceScanChanged, chatID)
}

func (a *SQLActors) eachImageReferenceRowOnce(ctx context.Context, chatID string, kinds map[string]bool, stable bool, pause func() error, visit func(kind string, data []byte) error) error {
	want := func(kind string) bool { return kinds == nil || kinds[kind] }
	var first imageReferenceStamp
	var head []byte
	if err := a.db.Read(ctx, func(tx *store.Tx) error {
		var err error
		if first, err = readImageReferenceStamp(tx, chatID); err != nil {
			return err
		}
		if want(ImageReferenceHead) {
			return tx.QueryRow(`SELECT state_json FROM chats WHERE chat_id = ?`, chatID).Scan(&head)
		}
		return nil
	}); err != nil {
		return err
	}
	if len(head) > 0 {
		if err := visit(ImageReferenceHead, head); err != nil {
			return err
		}
	}
	// checked reads one bounded chunk and proves the chat did not commit
	// since the scan began.
	checked := func(read func(tx *store.Tx) error) error {
		if pause != nil {
			if err := pause(); err != nil {
				return err
			}
		}
		return a.db.Read(ctx, func(tx *store.Tx) error {
			stamp, err := readImageReferenceStamp(tx, chatID)
			if err != nil {
				return err
			}
			if stable && stamp != first {
				return ErrLedgerChanged
			}
			return read(tx)
		})
	}
	if want(ImageReferenceMessage) {
		after := int64(0)
		for done := false; !done; {
			var chunk []sqlRawRow
			if err := checked(func(tx *store.Tx) error {
				var err error
				chunk, done, err = readSQLLedgerChunk(tx, chatID, after, sqlLoadChunkBytes, nil)
				return err
			}); err != nil {
				return err
			}
			for index := range chunk {
				after = chunk[index].seq
				if err := visit(ImageReferenceMessage, chunk[index].eventJSON); err != nil {
					return err
				}
				chunk[index].eventJSON = nil
			}
		}
	}
	if want(ImageReferenceJournal) {
		after := int64(0)
		for done := false; !done; {
			var records [][]byte
			if err := checked(func(tx *store.Tx) error {
				var err error
				records, after, done, err = readImageReferenceChunk(tx,
					`SELECT revision, record FROM chat_events WHERE chat_id = ? AND revision > ? ORDER BY revision`, chatID, after)
				return err
			}); err != nil {
				return err
			}
			for _, record := range records {
				if err := visit(ImageReferenceJournal, record); err != nil {
					return err
				}
			}
		}
	}
	if want(ImageReferenceTool) {
		after := ""
		for done := false; !done; {
			var owners [][]byte
			if err := checked(func(tx *store.Tx) error {
				rows, err := tx.Query(`SELECT tool_id, owner_json FROM chat_tools WHERE chat_id = ? AND tool_id > ? ORDER BY tool_id`, chatID, after)
				if err != nil {
					return err
				}
				defer rows.Close()
				copied := 0
				owners, done = owners[:0], true
				for rows.Next() {
					if copied >= sqlLoadChunkBytes {
						done = false
						break
					}
					var toolID string
					var owner []byte
					if err := rows.Scan(&toolID, &owner); err != nil {
						return err
					}
					after = toolID
					copied += len(owner)
					owners = append(owners, owner)
				}
				return rows.Err()
			}); err != nil {
				return err
			}
			for _, owner := range owners {
				if err := visit(ImageReferenceTool, owner); err != nil {
					return err
				}
			}
		}
	}
	// The rows read belong to the head read first only if nothing committed
	// after the last chunk either.
	return checked(func(*store.Tx) error { return nil })
}

func readImageReferenceChunk(tx *store.Tx, query, chatID string, after int64) ([][]byte, int64, bool, error) {
	rows, err := tx.Query(query, chatID, after)
	if err != nil {
		return nil, after, false, err
	}
	defer rows.Close()
	var out [][]byte
	copied := 0
	for rows.Next() {
		if copied >= sqlLoadChunkBytes {
			return out, after, false, rows.Err()
		}
		var key int64
		var data []byte
		if err := rows.Scan(&key, &data); err != nil {
			return nil, after, false, err
		}
		after = key
		copied += len(data)
		out = append(out, data)
	}
	return out, after, true, rows.Err()
}
