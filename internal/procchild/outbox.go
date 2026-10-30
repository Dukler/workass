package procchild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/store"
)

// The outbox: a transactional queue of events on their way to the local daemon.
//
// This replaces the JSONL spool, and the reason is the one the operator gave
// when the first live test failed: the spool had become the PRIMARY path, so a
// supervisor that could not reach its daemon still looked like it had reported,
// and nothing but a file on disk knew otherwise. The direction is now the other
// way round (docs/specs/fleet-runtime.md §13 P4):
//
//   - The supervisor REPORTS every transition to its own daemon over the
//     authenticated Agent API, the same surface an agent calls, and the daemon
//     ACKNOWLEDGES each one. That ack is what makes a report delivered.
//   - This outbox is the write-ahead log for the window when the daemon is
//     unreachable — a restart, a self-update, a crash. An event is INSERTed
//     here before it is sent, and marked delivered only when the daemon says
//     it applied it, so a crash between the insert and the send replays once
//     and exactly once, ordered by seq.
//   - The daemon never DISCOVERS a job by scanning anything. It learns about a
//     job from the report, and it reads this file only for a child it already
//     holds a row for, by id, when that child's supervisor has died with
//     events still undelivered.
//
// It is SQLite rather than another JSONL file because the failure modes of a
// hand-rolled append log are exactly the ones a queue cannot afford: a torn
// last line, a reader that has to parse to find the end, a seq allocated
// outside the write that records it, and a directory listing standing in for an
// index. One `INSERT` in one transaction gives the seq and the durability
// together, and `delivered_at IS NULL` is the queue.
//
// One database per machine, not one per job: `state/process-outbox.db`, shared
// by every `workass run` on the box. It is deliberately NOT state.db — that one
// is the daemon's single-writer ledger and holds its exclusive lock, and a
// supervisor must be able to write while the daemon is down.

// OutboxFileName is the outbox database inside the daemon state directory.
const OutboxFileName = "process-outbox.db"

// maxOutboxEvents bounds one child's event count. A supervisor writes a
// bounded number of transitions — a creation, a queue report or two, a start,
// at most one stall, a settle, and one heartbeat a minute — so this is a
// week of heartbeats of headroom, and a child past it is evidence of a bug.
const maxOutboxEvents = 16384

// maxOutboxPayloadBytes bounds one event's payload.
const maxOutboxPayloadBytes = 64 << 10

// outboxRetention is how long a delivered event is kept after the daemon
// acknowledged it. It is kept at all because it is the only record of what the
// supervisor said, which is what a disagreement between the row and the job is
// settled with.
const outboxRetention = 7 * 24 * time.Hour

// outboxMigrations is this database's own schema, carried through the one
// opener internal/store already is.
var outboxMigrations = []store.Migration{{
	Version: 1,
	Name:    "process_outbox",
	SQL: `
CREATE TABLE process_events (
  child_id     TEXT    NOT NULL,
  seq          INTEGER NOT NULL,
  kind         TEXT    NOT NULL,
  at           TEXT    NOT NULL,
  payload      TEXT    NOT NULL,
  -- NULL is the queue: every undelivered event, in seq order, is what a
  -- reconnecting supervisor replays.
  delivered_at TEXT,
  PRIMARY KEY (child_id, seq)
) WITHOUT ROWID;
CREATE INDEX process_events_pending ON process_events (delivered_at, child_id, seq);
`,
}}

// Outbox is one machine's supervised-job outbox.
type Outbox struct {
	db   *store.DB
	path string
}

// OutboxPath is the outbox database for a state directory.
func OutboxPath(stateDir string) string {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, OutboxFileName)
}

// OpenOutbox opens (creating if needed) the outbox for one state directory.
//
// The state directory is a parameter and never a default, because the one bug
// this whole redesign started from was a supervisor resolving `state` relative
// to the JOB's working directory and spooling four jobs into /tmp/state, where
// no daemon would ever look. Callers resolve it from the credential or from an
// explicit flag; this function refuses anything that is not an absolute path.
func OpenOutbox(stateDir string) (*Outbox, error) { return openOutbox(stateDir, false) }

// OpenOutboxForReading opens the outbox for a reader — the daemon draining a
// dead supervisor's undelivered events, or a diagnostic. It is the same file
// with the same pragmas; the distinction is in what the caller then does.
func OpenOutboxForReading(stateDir string) (*Outbox, error) { return openOutbox(stateDir, true) }

func openOutbox(stateDir string, readOnly bool) (*Outbox, error) {
	path := OutboxPath(stateDir)
	if path == "" {
		return nil, errors.New("a supervised job needs this machine's daemon state directory, and none was resolved")
	}
	if !filepath.IsAbs(path) {
		// A relative state directory is the bug that started this: it resolves
		// against whatever the job's cwd happens to be.
		return nil, fmt.Errorf("supervised-job state directory %q is not absolute, so it would resolve against the job's working directory", stateDir)
	}
	if readOnly {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, os.ErrNotExist
			}
			return nil, err
		}
	}
	db, err := store.Open(store.Options{
		Path:       path,
		Shared:     true,
		Migrations: outboxMigrations,
		// FULL, unlike state.db's NORMAL, and for the reason the supervisor's
		// own fsync had: the next thing a supervisor does after recording a
		// settle is exit, and a settle lost to a power cut is indistinguishable
		// from a supervisor that vanished — which the daemon would correctly,
		// and wrongly, call `lost`.
		Synchronous: store.SynchronousFull,
		QuickCheck:  false,
	})
	if err != nil {
		// Loud, and naming the file: a corrupt or unopenable outbox means this
		// machine cannot report supervised jobs at all, which is never
		// something to discover later from a missing notice.
		return nil, fmt.Errorf("supervised-job outbox %s is unusable: %w", path, err)
	}
	return &Outbox{db: db, path: path}, nil
}

// Path is the outbox database file.
func (o *Outbox) Path() string {
	if o == nil {
		return ""
	}
	return o.path
}

// Close releases the outbox.
func (o *Outbox) Close() error {
	if o == nil || o.db == nil {
		return nil
	}
	return o.db.Close()
}

// Append records one event for one child and returns it with the seq the
// transaction allocated.
//
// The seq is allocated INSIDE the write, from the rows already there, which is
// what lets several supervisors share one file and what makes a resumed
// supervisor continue a sequence rather than restart it. A caller that reads a
// maximum and then inserts would have two supervisors agree on the same number.
func (o *Outbox) Append(childID, kind string, payload map[string]any) (Event, error) {
	if o == nil || o.db == nil {
		return Event{}, errors.New("this supervisor has no outbox")
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return Event{}, errors.New("an outbox event needs a valid process child id")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return Event{}, errors.New("an outbox event needs a kind")
	}
	encoded := []byte("{}")
	if len(payload) > 0 {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return Event{}, err
		}
	}
	if len(encoded) > maxOutboxPayloadBytes {
		return Event{}, fmt.Errorf("an outbox event payload may not exceed %d bytes", maxOutboxPayloadBytes)
	}
	event := Event{At: time.Now().UTC(), Kind: kind, Payload: payload}
	stamp := event.At.Format(time.RFC3339Nano)
	err := o.db.Write(context.Background(), func(tx *store.Tx) error {
		var count, head int64
		if err := tx.QueryRow(
			`SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM process_events WHERE child_id = ?`, childID,
		).Scan(&count, &head); err != nil {
			return err
		}
		if count >= maxOutboxEvents {
			return fmt.Errorf("process child %s already holds %d outbox events", childID, count)
		}
		event.Seq = uint64(head) + 1
		_, err := tx.Exec(
			`INSERT INTO process_events (child_id, seq, kind, at, payload) VALUES (?, ?, ?, ?, ?)`,
			childID, int64(event.Seq), kind, stamp, string(encoded),
		)
		return err
	})
	if err != nil {
		return Event{}, err
	}
	return event, nil
}

// Pending is one child's undelivered events, in seq order.
func (o *Outbox) Pending(childID string, limit int) ([]Event, error) {
	if o == nil || o.db == nil {
		return nil, errors.New("this supervisor has no outbox")
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return nil, errors.New("an outbox read needs a valid process child id")
	}
	if limit <= 0 || limit > maxOutboxEvents {
		limit = maxOutboxEvents
	}
	events := make([]Event, 0, 8)
	err := o.db.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(
			`SELECT seq, kind, at, payload FROM process_events
			  WHERE child_id = ? AND delivered_at IS NULL
			  ORDER BY seq LIMIT ?`, childID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			event, err := scanOutboxEvent(rows)
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// All is one child's events, delivered or not, past a cursor. It is the read
// the `transitions` list of a job read is built from.
func (o *Outbox) All(childID string, afterSeq uint64, limit int) ([]Event, error) {
	if o == nil || o.db == nil {
		return nil, nil
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return nil, errors.New("an outbox read needs a valid process child id")
	}
	if limit <= 0 || limit > maxOutboxEvents {
		limit = maxOutboxEvents
	}
	events := make([]Event, 0, 8)
	err := o.db.Read(context.Background(), func(tx *store.Tx) error {
		rows, err := tx.Query(
			`SELECT seq, kind, at, payload FROM process_events
			  WHERE child_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
			childID, int64(afterSeq), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			event, err := scanOutboxEvent(rows)
			if err != nil {
				return err
			}
			events = append(events, event)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

type outboxRowScanner interface {
	Scan(dest ...any) error
}

func scanOutboxEvent(rows outboxRowScanner) (Event, error) {
	var seq int64
	var kind, stamp, payload string
	if err := rows.Scan(&seq, &kind, &stamp, &payload); err != nil {
		return Event{}, err
	}
	event := Event{Seq: uint64(seq), Kind: kind}
	if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		event.At = at.UTC()
	}
	if strings.TrimSpace(payload) != "" && payload != "{}" {
		fields := map[string]any{}
		if json.Unmarshal([]byte(payload), &fields) == nil {
			event.Payload = fields
		}
	}
	return event, nil
}

// MarkDelivered records the daemon's acknowledgement up to and including
// through. It is the ONLY thing that makes an event delivered: a send with no
// ack leaves the row in the queue, which is what makes the replay exactly-once
// rather than at-most-once.
func (o *Outbox) MarkDelivered(childID string, through uint64) error {
	if o == nil || o.db == nil || through == 0 {
		return nil
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return errors.New("an outbox ack needs a valid process child id")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	return o.db.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(
			`UPDATE process_events SET delivered_at = ?
			  WHERE child_id = ? AND seq <= ? AND delivered_at IS NULL`,
			stamp, childID, int64(through))
		return err
	})
}

// PendingCount is how many events are still queued for one child, which is
// what a receipt reports when a run ends without being fully acknowledged.
func (o *Outbox) PendingCount(childID string) (int, error) {
	if o == nil || o.db == nil {
		return 0, nil
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return 0, errors.New("an outbox read needs a valid process child id")
	}
	count := 0
	err := o.db.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(
			`SELECT COUNT(*) FROM process_events WHERE child_id = ? AND delivered_at IS NULL`,
			childID).Scan(&count)
	})
	return count, err
}

// Forget removes one child's events once its outcome is durable elsewhere.
func (o *Outbox) Forget(childID string) error {
	if o == nil || o.db == nil {
		return nil
	}
	childID = strings.TrimSpace(childID)
	if !ValidID(childID) {
		return errors.New("an outbox delete needs a valid process child id")
	}
	return o.db.Write(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM process_events WHERE child_id = ? AND delivered_at IS NOT NULL`, childID)
		return err
	})
}

// Prune removes delivered events past the retention window and reports how
// many rows went. An undelivered event is never pruned however old it is: it
// is the only copy of something nobody has been told.
func (o *Outbox) Prune(now time.Time) (int64, error) {
	if o == nil || o.db == nil {
		return 0, nil
	}
	cutoff := now.UTC().Add(-outboxRetention).Format(time.RFC3339Nano)
	var removed int64
	err := o.db.Write(context.Background(), func(tx *store.Tx) error {
		result, err := tx.Exec(
			`DELETE FROM process_events WHERE delivered_at IS NOT NULL AND delivered_at < ?`, cutoff)
		if err != nil {
			return err
		}
		removed, _ = result.RowsAffected()
		return nil
	})
	return removed, err
}
