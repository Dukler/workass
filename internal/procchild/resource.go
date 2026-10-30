package procchild

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A resource lease: a visible FIFO queue, and a hold that dies with its holder.
//
// delegation-fabric.md §10 and fleet-runtime.md §16.7. Two properties decide
// the whole design, and both are about failure rather than fairness:
//
//   - "The hold is tied to the supervisor process, so a child that dies —
//     however it dies — releases it; the daemon never has to guess whether a
//     lock is abandoned." A lease with a lifetime, a renewal or a heartbeat
//     would reintroduce exactly the guess it exists to remove. So the hold is
//     the supervisor's pid: alive means held, gone means free, and SIGKILL,
//     a panic, an OOM and a power cut are all the same case.
//   - "A queue only the daemon honours is not a lock." The queue is on the
//     filesystem, so a `workass run` started from a shell with no daemon
//     running still excludes another one, and a human can read the queue with
//     `ls`.
//
// `max_hold` is deliberately a REPORT, not a kill (§16.7): a holder past its
// declared hold raises an attention naming it. Killing somebody's eight-hour
// training run to be fair is a worse outcome than saying who is holding the GPU.

// claim is one waiter's file in a resource's queue directory.
type claim struct {
	// Ticket orders the queue. It is the nanosecond the claim was created,
	// zero-padded so the lexical order of the FILENAMES is the FIFO order —
	// which is what lets a reader rank the queue without opening anything.
	Ticket  string `json:"ticket"`
	PID     int    `json:"pid"`
	ChildID string `json:"childId"`
	Label   string `json:"label"`
	Owner   string `json:"owner,omitempty"`
	Since   string `json:"since"`
	// MaxHoldMs is the hold the caller declared. Report-only.
	MaxHoldMs int64 `json:"maxHoldMs,omitempty"`
}

// QueueEntry is one waiter, as a reader sees it.
type QueueEntry struct {
	Resource string `json:"resource"`
	Position int    `json:"position"`
	ChildID  string `json:"childId"`
	Label    string `json:"label,omitempty"`
	Owner    string `json:"owner,omitempty"`
	PID      int    `json:"pid"`
	Since    string `json:"since"`
	Holding  bool   `json:"holding"`
	// HeldForMs is set for the holder, so `max_hold` can be reported without
	// the reporter needing the holder's declaration.
	HeldForMs int64 `json:"heldForMs,omitempty"`
	MaxHoldMs int64 `json:"maxHoldMs,omitempty"`
}

// Lease is one acquired hold.
type Lease struct {
	resource string
	path     string
}

// Resource is the resource this lease holds, empty for no lease.
func (l *Lease) Resource() string {
	if l == nil {
		return ""
	}
	return l.resource
}

// Release drops the hold. It is also what the supervisor's own death does, by
// a different route — so Release is tidiness, never the mechanism.
func (l *Lease) Release() {
	if l == nil || l.path == "" {
		return
	}
	_ = os.Remove(l.path)
	l.path = ""
}

// AcquireRequest is one waiter's claim on one resource.
type AcquireRequest struct {
	StateDir  string
	Resource  string
	ChildID   string
	Label     string
	Owner     string
	PID       int
	MaxHold   time.Duration
	Poll      time.Duration
	Alive     func(pid int) bool
	Now       func() time.Time
	OnQueued  func(position int, total int)
	Cancelled func() bool
}

// Acquire blocks until this claim is at the head of the queue.
//
// It polls the directory rather than waiting on a notification, and that is a
// deliberate choice rather than an omission: the thing being waited for is
// another PROCESS's death, which has no portable notification, and the poll is
// over a directory with one file per waiter — a handful of stats a second,
// bounded by the number of jobs a human queued. The spec's "nothing polls" is
// about liveness reporting across machines, which the monitor stream replaced;
// a local queue is not that.
func Acquire(req AcquireRequest) (*Lease, error) {
	resource := NormalizeResource(req.Resource)
	if resource == "" {
		return nil, nil
	}
	dir := ResourceQueueDir(req.StateDir, resource)
	if dir == "" {
		return nil, errors.New("this machine has no process-child state directory, so it cannot queue for a resource")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	now := req.Now
	if now == nil {
		now = time.Now
	}
	alive := req.Alive
	if alive == nil {
		alive = PIDAlive
	}
	poll := req.Poll
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	pid := req.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	ticket := fmt.Sprintf("%020d", now().UTC().UnixNano())
	entry := claim{
		Ticket: ticket, PID: pid, ChildID: strings.TrimSpace(req.ChildID),
		Label: strings.TrimSpace(req.Label), Owner: strings.TrimSpace(req.Owner),
		Since: now().UTC().Format(time.RFC3339Nano), MaxHoldMs: req.MaxHold.Milliseconds(),
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "q-"+ticket+"-"+entry.ChildID+".claim")
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		return nil, err
	}
	lease := &Lease{resource: resource, path: path}
	announced := -1
	for {
		if req.Cancelled != nil && req.Cancelled() {
			lease.Release()
			return nil, errors.New("the wait for " + resource + " was cancelled before the hold was granted")
		}
		entries := readQueue(dir, resource, alive, now())
		position, total := 0, len(entries)
		for _, candidate := range entries {
			if candidate.ChildID == entry.ChildID && candidate.PID == pid {
				position = candidate.Position
				break
			}
		}
		if position == 1 {
			return lease, nil
		}
		if position == 0 {
			// This claim is gone from the queue: another waiter reaped it
			// because this process looked dead, or the file was removed by
			// hand. Writing it again would jump the queue; refusing is honest.
			lease.path = ""
			return nil, fmt.Errorf("this claim on %s is no longer in the queue", resource)
		}
		if req.OnQueued != nil && position != announced {
			announced = position
			req.OnQueued(position, total)
		}
		time.Sleep(poll)
	}
}

// Queue is the visible FIFO for one resource, in order, holder first.
func Queue(stateDir, resource string) []QueueEntry {
	return queueWith(stateDir, resource, PIDAlive)
}

// queueWith is Queue with the liveness probe named, so a test can hold and
// kill a claim without holding and killing a process.
func queueWith(stateDir, resource string, alive func(int) bool) []QueueEntry {
	dir := ResourceQueueDir(stateDir, resource)
	if dir == "" {
		return nil
	}
	if alive == nil {
		alive = PIDAlive
	}
	return readQueue(dir, NormalizeResource(resource), alive, time.Now())
}

// Resources lists every resource that has a queue directory, so a reader can
// show them all without being told their names.
func Resources(stateDir string) []string {
	root := Root(stateDir)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(root, "resources"))
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if name := NormalizeResource(entry.Name()); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// readQueue ranks one resource's live claims and REAPS the dead ones.
//
// Reaping here, in the reader, rather than in a sweeper, is what makes "any
// death releases it" true without a daemon: the next process that looks at the
// queue is the one that notices, and a queue nobody is looking at has nobody
// waiting on it.
func readQueue(dir, resource string, alive func(int) bool, now time.Time) []QueueEntry {
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".claim") {
			continue
		}
		names = append(names, file.Name())
	}
	// Filename order IS ticket order, because the ticket is zero-padded.
	sort.Strings(names)
	out := make([]QueueEntry, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil || len(data) > 8*1024 {
			_ = os.Remove(path)
			continue
		}
		var entry claim
		if json.Unmarshal(data, &entry) != nil || entry.PID <= 0 || entry.ChildID == "" {
			_ = os.Remove(path)
			continue
		}
		if !alive(entry.PID) {
			// The holder is gone. However it went — SIGKILL, OOM, a power cut —
			// this is the same case, and the hold is free.
			_ = os.Remove(path)
			continue
		}
		row := QueueEntry{
			Resource: resource, Position: len(out) + 1, ChildID: entry.ChildID,
			Label: entry.Label, Owner: entry.Owner, PID: entry.PID,
			Since: entry.Since, Holding: len(out) == 0, MaxHoldMs: entry.MaxHoldMs,
		}
		if row.Holding {
			if since, err := time.Parse(time.RFC3339Nano, entry.Since); err == nil {
				row.HeldForMs = now.Sub(since).Milliseconds()
			}
		}
		out = append(out, row)
	}
	return out
}
