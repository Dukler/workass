package procchild

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"workass/internal/durablefs"
)

// The spool: what the supervisor knows, written where the daemon can read it
// whether or not the daemon is alive.
//
// delegation-fabric.md §5: "It works while the daemon is down. Events are
// appended to a spool directory under the state root; the daemon ingests the
// spool on start and on a tick." That sentence is the whole reason this file
// exists rather than the supervisor simply calling the daemon: a supervisor
// that needed a live daemon to record a settle would lose the settle during a
// self-update, which is precisely the window in which the product is most
// likely to be restarting.
//
// The format is the same JSONL-append idiom the subagent receipts and the
// entity logs already use, with one difference that matters: every record is
// flushed and fsynced before the supervisor acts on it. A settle that is in a
// buffer is not a fact.

// The spool's event vocabulary. It is delegation-fabric.md §3's closed set,
// narrowed to what a supervisor can actually witness.
const (
	// KindCreated is the supervisor stating what it was asked to run, before it
	// has asked anybody for permission to run it.
	KindCreated = "created"
	// KindQueued is one position report while waiting for a resource. It is not
	// actionable and never notifies: a queue position is visible by reading.
	KindQueued = "queued"
	// KindStarted carries the child's pid and its process group.
	KindStarted = "started"
	// KindStalled is the ONE-SHOT declared stall: the child has produced no
	// output for the `--stale-after` the caller itself declared. It is a flag on
	// running, never a state, and it is emitted once per child however long the
	// silence lasts.
	KindStalled = "stalled"
	// KindAttention is a decision somebody is waiting on. A holder past its
	// declared max_hold raises one (§16.7: a host never kills a holder to be
	// fair, it says who is holding).
	KindAttention = "attention"
	// KindCancelRequested records that a stop-file was observed, and by then it
	// has already been acted on.
	KindCancelRequested = "cancel_requested"
	// KindSettled is terminal and carries the real outcome.
	KindSettled = "settled"
)

// The outcome vocabulary a settle carries. It is fleetlog's, deliberately: one
// stream, one set of words (§7).
const (
	OutcomeSettled  = "settled"
	OutcomeFailed   = "failed"
	OutcomeTimedOut = "timed_out"
	// OutcomeCancelled is a stop the owner asked for. It never notifies,
	// because the party that would be told is the party that asked.
	OutcomeCancelled = "cancelled"
	// OutcomeLost is never written by a supervisor. A supervisor that could
	// write "lost" would be alive, and a live supervisor knows what happened.
	// Only the daemon appends it, from the absence of this file's terminal
	// event beside a pid that is gone.
	OutcomeLost = "lost"
)

// Terminal reports whether an event ends the child's life.
func Terminal(kind string) bool { return kind == KindSettled }

// maxSpoolBytes bounds one child's spool. A supervisor writes a bounded number
// of transitions — a creation, a queue report or two, a start, at most one
// stall, a settle — so this is three orders of magnitude of headroom, and a
// file past it is evidence of a bug, not of a busy job.
const maxSpoolBytes = 1 << 20

// maxSpoolEvents is the same bound stated in records, for a reader.
const maxSpoolEvents = 4096

// Event is one spooled transition.
type Event struct {
	Seq     uint64         `json:"seq"`
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Spool appends a child's events durably. One supervisor process owns one
// spool, so the mutex guards only its own concurrent timers.
type Spool struct {
	path string

	mu  sync.Mutex
	seq uint64
}

// OpenSpool opens (or resumes) one child's spool. Resuming matters: a
// supervisor that was itself restarted by a wrapper must continue the sequence
// rather than restart it, or the daemon's cursor would skip everything written
// after the restart.
func OpenSpool(path string) (*Spool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("a process child spool needs a path")
	}
	existing, _ := ReadSpool(path)
	spool := &Spool{path: path}
	for _, event := range existing {
		if event.Seq > spool.seq {
			spool.seq = event.Seq
		}
	}
	return spool, nil
}

// Path is the spool's file.
func (s *Spool) Path() string { return s.path }

// Head is the highest sequence this spool holds.
func (s *Spool) Head() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Append writes one event and does not return until it is on the disk.
//
// The durability is the point, not caution: the supervisor's next action after
// a settle is to exit, and a settle still sitting in a page cache when the
// machine loses power is indistinguishable from a supervisor that vanished —
// which the daemon would correctly, and wrongly, call `lost`.
func (s *Spool) Append(kind string, payload map[string]any) (Event, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return Event{}, errors.New("a spooled event needs a kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq >= maxSpoolEvents {
		return Event{}, fmt.Errorf("process child spool already holds %d events", s.seq)
	}
	event := Event{Seq: s.seq + 1, At: time.Now().UTC(), Kind: kind, Payload: payload}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Event{}, err
	}
	defer file.Close()
	if info, statErr := file.Stat(); statErr == nil && info.Size()+int64(len(line)) > maxSpoolBytes {
		return Event{}, errors.New("process child spool is full")
	}
	if _, err := file.Write(line); err != nil {
		return Event{}, err
	}
	if err := file.Sync(); err != nil {
		return Event{}, err
	}
	s.seq = event.Seq
	return event, nil
}

// ReadSpool reads a child's events in sequence order, skipping any trailing
// partial line.
//
// A partial last line is expected rather than exceptional: the writer appends
// and fsyncs, so a reader that arrives mid-append sees a prefix. Dropping it
// silently is correct — the writer will finish, and the next read will find the
// whole record. Inventing an event from half a line would be the one way this
// file could lie.
func ReadSpool(path string) ([]Event, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	events := make([]Event, 0, 8)
	reader := bufio.NewReaderSize(io.LimitReader(file, maxSpoolBytes+1), 64*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] == '\n' {
			var event Event
			if json.Unmarshal(line, &event) == nil && event.Seq > 0 && strings.TrimSpace(event.Kind) != "" {
				events = append(events, event)
			}
		}
		if err != nil {
			break
		}
		if len(events) >= maxSpoolEvents {
			break
		}
	}
	return events, nil
}

// RemoveSpool deletes one child's directory once its outcome is durable
// elsewhere. It is called by the daemon, never by a supervisor: the supervisor
// does not know whether anybody has read what it wrote.
func RemoveSpool(stateDir, childID string) error {
	dir := Dir(stateDir, childID)
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return durablefs.SyncDirectory(Root(stateDir))
}
