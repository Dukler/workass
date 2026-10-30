package procchild

import "time"

// The event vocabulary, and nothing else.
//
// This file used to be the spool: a JSONL append log the supervisor wrote and
// the daemon read. It is not any more. The spool was the PRIMARY record, and
// the first live two-machine test showed what that costs — four jobs ran to
// completion, every transition was written down, and no chat was ever told,
// because the only thing that could have told it was a file read nobody
// performed. Events now go to the daemon over the Agent API and are
// acknowledged (outbox.go); the write-ahead copy is a SQLite outbox.
//
// What survives here is the vocabulary, unchanged, because it is the contract:
// the kinds a supervisor can witness, the outcomes a settle can carry, and the
// Event shape both halves speak. Renaming any of it would rename what is
// already on the wire and in every stored row.

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
	// KindHeartbeat is the supervisor saying only that it is still there.
	//
	// The spool design needed no such thing: the daemon probed the pid, and
	// absence was the evidence. Reporting over the API needs it for the
	// question the pid cannot answer — a long job that is healthy and quiet
	// looks exactly like a supervisor that has lost its daemon, and the
	// operator's rule is that the second case must never look like the first.
	// It is never a notice and never an entry in the entity log; it moves one
	// timestamp.
	KindHeartbeat = "heartbeat"
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
	// OutcomeLost is never reported by a supervisor. A supervisor that could
	// report "lost" would be alive, and a live supervisor knows what happened.
	// Only the daemon reaches it, from a supervisor pid that is gone with no
	// terminal event delivered and none left in its outbox.
	OutcomeLost = "lost"
)

// Terminal reports whether an event ends the child's life.
func Terminal(kind string) bool { return kind == KindSettled }

// Event is one transition: what happened, when, and the facts that go with
// it. The seq is per child and is allocated by the outbox write that records
// it, so it is dense, ordered and unique without a coordinator.
type Event struct {
	Seq     uint64         `json:"seq"`
	At      time.Time      `json:"at"`
	Kind    string         `json:"kind"`
	Payload map[string]any `json:"payload,omitempty"`
}

// HighestSeq is the latest event in a batch, and zero for an empty one.
func HighestSeq(events []Event) uint64 {
	highest := uint64(0)
	for _, event := range events {
		if event.Seq > highest {
			highest = event.Seq
		}
	}
	return highest
}
