// Package fleetlog is the transport of the fleet runtime: a per-entity,
// append-only, sequence-numbered log at the entity's home, plus the durable
// monitor registry that says who is owed what.
//
// docs/specs/fleet-runtime.md §5 names the shape and §8 the consistency model,
// and both are chosen to remove one specific dishonesty. Before this, a child
// on another machine was observed by POLLING it (`clampControllerWait`, a 2 s
// re-ask) and a notice was a side effect of whichever process happened to be
// alive at the moment the child finished. So a disconnect lost events, a
// restart lost them again, and nothing could say whether "running" meant
// running or meant unknown.
//
// The log fixes that by being the authoritative history rather than a cache of
// one:
//
//   - One writer per entity, at its home. There is no merge, no CRDT and no
//     conflict resolution, because ownership IS the resolution (§8).
//   - Per-entity total order by seq, and no cross-entity atomicity. A
//     subscriber applies in order, deduplicates by seq, and keeps the last seq
//     it applied — so redelivery is free and a partition is a postponement.
//   - Bounded, and honest about it. Retention is 7 days or 1,000 events per
//     entity, whichever comes first (§16.3c); compaction records `first_seq`
//     and appends `truncated{to_seq}`, so a subscriber below the watermark
//     RESYNCS from the head and says so rather than failing or silently
//     skipping. Eviction is logged, never silent.
//   - A terminal event survives the cap until it is acknowledged. "Nothing is
//     forgotten before it is consumed" is the one invariant that cannot be
//     traded for a byte count, so the retention sweep asks the monitor registry
//     what is still owed before it drops an `exit`.
//
// Storage is one JSONL file per entity, because the whole point is that the
// file IS the history: it is appended to, read from a cursor, and rewritten
// only by compaction. The format is deliberately the same shape the subagent
// receipts already use (`internal/acp/subagent_receipts.go`), so there is one
// durable-append idiom in this daemon rather than two.
package fleetlog

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/durablefs"
)

// The event kinds a monitor stream speaks (§7). They are a closed vocabulary on
// purpose: one stream, one set of words, local and remote identical.
const (
	// KindExit is terminal: settled | failed | lost | timed_out | cancelled.
	KindExit = "exit"
	// KindAttention is a child-owned decision the watcher must see.
	KindAttention = "attention"
	// KindWaiting is a declared wait with its conditions.
	KindWaiting = "waiting"
	// KindResumed follows a declared wait that ended.
	KindResumed = "resumed"
	// KindProgress is opt-in and is never a wake.
	KindProgress = "progress"
	// KindTruncated is compaction's own record: everything at or below to_seq is
	// gone, so a subscriber below it must resync from the head.
	KindTruncated = "truncated"
	// KindSpawned records that the entity came into being. It is not actionable
	// and never notifies (`started` is visible by reading).
	KindSpawned = "spawned"
)

// The outcome vocabulary carried by an exit event.
const (
	OutcomeSettled   = "settled"
	OutcomeFailed    = "failed"
	OutcomeLost      = "lost"
	OutcomeTimedOut  = "timed_out"
	OutcomeCancelled = "cancelled"
)

// Terminal reports whether an event ends the entity's life, which is what the
// retention sweep must not drop before it is acknowledged.
func Terminal(kind string) bool { return kind == KindExit }

// Event is one record in one entity's log.
type Event struct {
	Entity  string         `json:"entity"`
	Seq     uint64         `json:"seq"`
	Kind    string         `json:"kind"`
	At      time.Time      `json:"at"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Retention bounds one entity's log. Both limits apply; whichever comes first
// wins (§16.3c).
type Retention struct {
	MaxAge    time.Duration
	MaxEvents int
}

// DefaultRetention is §16.3c's decided pair, fixed rather than configurable:
// the operator accepted three numbers, and a knob beside a decided number is
// how a decided number stops being one.
var DefaultRetention = Retention{MaxAge: 7 * 24 * time.Hour, MaxEvents: 1000}

// Options configures a store.
type Options struct {
	Retention Retention
	Now       func() time.Time
	Logf      func(string, ...any)
	// Owed answers, for one entity, the lowest sequence every monitor has
	// acknowledged, and whether any monitor exists at all. A terminal event
	// above that sequence is still owed to somebody and is NOT evicted.
	//
	// It is a function rather than a field because the registry that knows the
	// answer is the home's, and a log that reached into it directly could not be
	// opened without one.
	Owed func(entity string) (through uint64, monitored bool)
}

// Store is this daemon's set of entity logs. It holds only entities homed here:
// a daemon never writes another home's log (§8).
type Store struct {
	dir       string
	retention Retention
	now       func() time.Time
	logf      func(string, ...any)
	owed      func(entity string) (uint64, bool)

	mu    sync.Mutex
	state map[string]*entityState
}

type entityState struct {
	path     string
	head     uint64
	firstSeq uint64
	loaded   bool
}

// Slice is what a subscriber receives for one cursor: the events it has not
// applied, plus the two watermarks that let it tell "current" from "behind".
type Slice struct {
	Entity string  `json:"entity"`
	Events []Event `json:"events"`
	// Head is the highest seq the home has appended.
	Head uint64 `json:"head"`
	// FirstSeq is the lowest seq the home still holds. A cursor below it cannot
	// be served event by event.
	FirstSeq uint64 `json:"firstSeq"`
	// Truncated says the cursor was below FirstSeq, so this slice starts at the
	// head rather than at the cursor and the subscriber must resync.
	Truncated bool `json:"truncated,omitempty"`
	// TruncatedTo is the to_seq compaction recorded.
	TruncatedTo uint64 `json:"truncatedTo,omitempty"`
}

// maxSliceEvents bounds one delivery. A four-day backlog is delivered in order
// across several slices rather than in one frame, so nothing depends on a
// single oversized message surviving the link.
const maxSliceEvents = 256

// Open prepares the log directory. It creates no file: an entity's log exists
// because something was appended to it.
func Open(stateDir string, opts Options) (*Store, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("fleet logs need a state directory")
	}
	dir := filepath.Join(stateDir, "fleet-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	store := &Store{dir: dir, retention: opts.Retention, now: opts.Now, logf: opts.Logf, owed: opts.Owed, state: map[string]*entityState{}}
	if store.retention.MaxAge <= 0 {
		store.retention.MaxAge = DefaultRetention.MaxAge
	}
	if store.retention.MaxEvents <= 0 {
		store.retention.MaxEvents = DefaultRetention.MaxEvents
	}
	if store.now == nil {
		store.now = time.Now
	}
	if store.logf == nil {
		store.logf = func(string, ...any) {}
	}
	return store, nil
}

// Dir is where the logs live, for a receipt that names a path.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// Append writes one event and returns it with its assigned seq.
func (s *Store) Append(entity, kind string, payload map[string]any) (Event, error) {
	entity, kind = strings.TrimSpace(entity), strings.TrimSpace(kind)
	if s == nil {
		return Event{}, errors.New("fleet log store is unavailable")
	}
	if entity == "" || kind == "" {
		return Event{}, errors.New("an entity log event needs an entity and a kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, events, err := s.loadLocked(entity)
	if err != nil {
		return Event{}, err
	}
	event := Event{Entity: entity, Seq: state.head + 1, Kind: kind, At: s.now().UTC(), Payload: payload}
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	file, err := os.OpenFile(state.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Event{}, err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return Event{}, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Event{}, err
	}
	if err := file.Close(); err != nil {
		return Event{}, err
	}
	_ = durablefs.SyncDirectory(s.dir)
	state.head = event.Seq
	if state.firstSeq == 0 {
		state.firstSeq = event.Seq
	}
	s.compactLocked(entity, state, append(events, event))
	return event, nil
}

// Since serves one cursor. A cursor at or above the head gets an empty slice
// and the watermarks, which is how a subscriber answers "am I current?" without
// a poll of its own.
func (s *Store) Since(entity string, afterSeq uint64, limit int) (Slice, error) {
	entity = strings.TrimSpace(entity)
	if s == nil || entity == "" {
		return Slice{}, errors.New("an entity log read needs an entity")
	}
	if limit <= 0 || limit > maxSliceEvents {
		limit = maxSliceEvents
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, events, err := s.loadLocked(entity)
	if err != nil {
		return Slice{}, err
	}
	slice := Slice{Entity: entity, Events: []Event{}, Head: state.head, FirstSeq: state.firstSeq}
	// A cursor below the watermark cannot be served event by event: those events
	// are gone. Say so — the subscriber resyncs from the head and reports the
	// gap, which is strictly better than a silent skip (§5).
	if state.firstSeq > 0 && afterSeq+1 < state.firstSeq {
		slice.Truncated = true
		slice.TruncatedTo = state.firstSeq - 1
		for _, event := range events {
			if event.Kind == KindTruncated {
				if to := uint64Field(event.Payload, "to_seq"); to > slice.TruncatedTo {
					slice.TruncatedTo = to
				}
			}
		}
		afterSeq = 0
	}
	for _, event := range events {
		if event.Seq <= afterSeq {
			continue
		}
		slice.Events = append(slice.Events, event)
		if len(slice.Events) >= limit {
			break
		}
	}
	return slice, nil
}

// Head is the highest seq appended for this entity, or zero for an entity with
// no log.
func (s *Store) Head(entity string) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, _, err := s.loadLocked(strings.TrimSpace(entity))
	if err != nil {
		return 0
	}
	return state.head
}

// Entities is every entity this home has a log for.
func (s *Store) Entities() []string {
	if s == nil {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		events := readEventFile(filepath.Join(s.dir, entry.Name()))
		if len(events) == 0 {
			continue
		}
		out = append(out, events[0].Entity)
	}
	sort.Strings(out)
	return out
}

// Prune applies retention to every entity, and removes the file of an entity
// whose log has nothing left in it and nothing owed on it.
//
// The removal matters more than it looks. A subagent is an entity, so a busy
// week is thousands of entities, and a log that only ever compacted would leave
// one empty file per subagent forever. An entity whose every event has expired
// and whose last monitor is gone has no history to be honest about.
func (s *Store) Prune() {
	if s == nil {
		return
	}
	for _, entity := range s.Entities() {
		s.mu.Lock()
		state, events, err := s.loadLocked(entity)
		if err == nil {
			s.compactLocked(entity, state, events)
			s.removeIfSpentLocked(entity, state)
		}
		s.mu.Unlock()
	}
}

// removeIfSpentLocked deletes a log with no surviving events and no monitor.
// A monitored entity is never removed, however empty: the monitor is a standing
// claim, and an absent file would answer head 0 where the truth is "everything
// you were owed has been delivered".
func (s *Store) removeIfSpentLocked(entity string, state *entityState) {
	if s.owed != nil {
		if _, monitored := s.owed(entity); monitored {
			return
		}
	}
	for _, event := range readEventFile(state.path) {
		if event.Kind != KindTruncated {
			return
		}
	}
	if err := os.Remove(state.path); err != nil && !os.IsNotExist(err) {
		s.logf("[fleet] could not remove the spent log for %s: %v", entity, err)
		return
	}
	delete(s.state, entity)
	s.logf("[fleet] removed the spent log for %s", entity)
}

// compactLocked enforces retention and records what it dropped.
//
// Terminal events are held back from eviction while a monitor still owes an
// acknowledgement for them, which is the one case where "bounded" yields to
// "nothing is forgotten before it is consumed".
func (s *Store) compactLocked(entity string, state *entityState, events []Event) {
	if len(events) == 0 {
		return
	}
	// The previous truncation marker is bookkeeping, not history: it is never
	// kept and never counted, because this sweep rewrites it. Keeping it would
	// also put a seq-0 record at the front of the kept set, which is how the
	// watermark arithmetic below would underflow.
	real := make([]Event, 0, len(events))
	priorTo := uint64(0)
	for _, event := range events {
		if event.Kind == KindTruncated {
			if to := uint64Field(event.Payload, "to_seq"); to > priorTo {
				priorTo = to
			}
			continue
		}
		real = append(real, event)
	}
	cutoff := s.now().UTC().Add(-s.retention.MaxAge)
	ackedThrough, monitored := uint64(0), false
	if s.owed != nil {
		ackedThrough, monitored = s.owed(entity)
	}
	keep := make([]Event, 0, len(real))
	dropped := 0
	for index, event := range real {
		overCount := len(real)-index > s.retention.MaxEvents
		tooOld := event.At.Before(cutoff)
		if !overCount && !tooOld {
			keep = append(keep, event)
			continue
		}
		if Terminal(event.Kind) && monitored && event.Seq > ackedThrough {
			// Still owed. A terminal log outliving its retention window is the
			// honest answer; dropping it would turn a delivered outcome into
			// silence on the watcher's side (§5).
			keep = append(keep, event)
			continue
		}
		dropped++
	}
	if dropped == 0 {
		// Nothing went, so the existing marker — and the watermark it carries —
		// is left exactly as it is. priorTo is read above only so a rewrite can
		// never lose it.
		_ = priorTo
		return
	}
	lowestKept := state.head + 1
	if len(keep) > 0 {
		lowestKept = keep[0].Seq
	}
	if lowestKept == 0 {
		lowestKept = 1
	}
	marker := Event{
		Entity: entity, Seq: 0, Kind: KindTruncated, At: s.now().UTC(),
		Payload: map[string]any{"to_seq": lowestKept - 1, "dropped": dropped},
	}
	rewritten := append([]Event{marker}, keep...)
	if err := s.rewriteLocked(state.path, rewritten); err != nil {
		s.logf("[fleet] could not compact the log for %s: %v", entity, err)
		return
	}
	state.firstSeq = lowestKept
	// Eviction is logged, never silent (§16.3c).
	s.logf("[fleet] compacted %s: dropped %d event(s) at or below seq %d", entity, dropped, lowestKept-1)
}

func (s *Store) rewriteLocked(path string, events []Event) error {
	var builder strings.Builder
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		builder.Write(line)
		builder.WriteByte('\n')
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(builder.String()), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(path))
}

func (s *Store) loadLocked(entity string) (*entityState, []Event, error) {
	if entity == "" {
		return nil, nil, errors.New("an entity log needs an entity id")
	}
	state := s.state[entity]
	if state == nil {
		state = &entityState{path: filepath.Join(s.dir, entityFileName(entity))}
		s.state[entity] = state
	}
	events := readEventFile(state.path)
	if !state.loaded {
		state.loaded = true
	}
	state.head, state.firstSeq = 0, 0
	for _, event := range events {
		if event.Seq > state.head {
			state.head = event.Seq
		}
		if event.Seq > 0 && (state.firstSeq == 0 || event.Seq < state.firstSeq) {
			state.firstSeq = event.Seq
		}
		if event.Kind == KindTruncated {
			if to := uint64Field(event.Payload, "to_seq"); to > state.head {
				state.head = to
			}
		}
	}
	return state, events, nil
}

func readEventFile(path string) []Event {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	out := make([]Event, 0, 32)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		out = append(out, event)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// entityFileName is readable AND collision-free: the sanitized id so a human
// reading the directory knows what they are looking at, and a digest of the
// exact id so two entities whose ids sanitize alike cannot share a file.
func entityFileName(entity string) string {
	sum := sha256.Sum256([]byte(entity))
	var builder strings.Builder
	for _, r := range entity {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('~')
		}
	}
	name := builder.String()
	if len(name) > 96 {
		name = name[len(name)-96:]
	}
	return fmt.Sprintf("%s-%x.jsonl", name, sum[:6])
}

func uint64Field(payload map[string]any, key string) uint64 {
	if payload == nil {
		return 0
	}
	switch value := payload[key].(type) {
	case uint64:
		return value
	case int64:
		if value > 0 {
			return uint64(value)
		}
	case int:
		if value > 0 {
			return uint64(value)
		}
	case float64:
		if value > 0 {
			return uint64(value)
		}
	case json.Number:
		if parsed, err := value.Int64(); err == nil && parsed > 0 {
			return uint64(parsed)
		}
	case string:
		var parsed uint64
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return 0
}
