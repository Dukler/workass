package fleetlog

import (
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

// Monitors is the durable registry behind §7's Erlang semantics.
//
// `monitor(watcher, target, kinds)` is durable at BOTH ends, and for one reason
// each: the watcher's home records it so the monitor survives the watcher's own
// restart, and the target's home records it so it knows WHO IS OWED. Neither
// end can be inferred from the other, which is why the same type is opened
// twice under two names rather than shared.
//
// It is deliberately not a subscription registry. A subscription is a cursor on
// a live link and dies with the socket (§5); a monitor is a standing claim on
// an entity's outcome and outlives both daemons. Conflating them is how a
// restart used to lose an outcome.
type Monitors struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	rows map[string]Registration
}

// Registration is one monitor. The same record serves both ends; each end reads
// the fields that are its business.
type Registration struct {
	// Ref is the monitor reference, derived from (watcher, target) so that a
	// replayed registration is the same monitor rather than a second one.
	Ref string `json:"ref"`
	// WatcherID and TargetID are global ids (§3), so each names its own home.
	WatcherID string `json:"watcherId"`
	TargetID  string `json:"targetId"`
	// Kinds restricts what may be read through this monitor. Empty means the
	// actionable set. A monitor carries a capability and nothing else can be
	// read through it (§11).
	Kinds []string `json:"kinds,omitempty"`
	// Label is the target's display name, carried so a notice can name the work
	// even when the target's home is unreachable (`last_known`).
	Label string `json:"label,omitempty"`
	// The watcher's own coordinates, as the watcher's chat knows them. The
	// target's home keeps them opaque: they are the watcher's business, and the
	// home only ever hands them back.
	WatcherChatID     string    `json:"watcherChatId,omitempty"`
	WatcherTabID      string    `json:"watcherTabId,omitempty"`
	OriginLaneID      string    `json:"originLaneId,omitempty"`
	OriginOperationID string    `json:"originOperationId,omitempty"`
	CreatedAt         time.Time `json:"createdAt"`
	// AckedSeq is the home's record of what this watcher has acknowledged. It is
	// what holds a terminal event back from retention.
	AckedSeq uint64 `json:"ackedSeq,omitempty"`
	// AppliedSeq is the watcher's own cursor: the last seq it applied. Dedup by
	// seq is this field and nothing more (§5).
	AppliedSeq uint64 `json:"appliedSeq,omitempty"`
	// Consumed marks a monitor whose terminal event has been applied and
	// acknowledged. It is kept rather than deleted until the ack is confirmed,
	// so a crash between apply and ack cannot lose the record that says which
	// outcome was already delivered.
	Consumed bool `json:"consumed,omitempty"`
}

// WatcherHome is the machine that owns the watcher, read out of its global id.
func (r Registration) WatcherHome() string { return homeOf(r.WatcherID) }

// TargetHome is the machine that owns the target.
func (r Registration) TargetHome() string { return homeOf(r.TargetID) }

// Allows reports whether this monitor may carry one event kind. Progress is
// opt-in and never implied, because an opt-in that leaks by default is not one.
func (r Registration) Allows(kind string) bool {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return false
	}
	if len(r.Kinds) == 0 {
		return kind != KindProgress
	}
	for _, allowed := range r.Kinds {
		if strings.EqualFold(strings.TrimSpace(allowed), kind) {
			return true
		}
	}
	return false
}

// MonitorRef derives a monitor's reference from the pair it is about, so that
// registering the same monitor twice — a retry, a reconnect, a replayed spawn —
// produces one monitor and not two.
func MonitorRef(watcherID, targetID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(watcherID) + "\x00" + strings.TrimSpace(targetID)))
	return fmt.Sprintf("mon-%x", sum[:12])
}

type monitorFile struct {
	Version  int            `json:"version"`
	Monitors []Registration `json:"monitors"`
}

const monitorFileVersion = 1

// OpenMonitors opens one registry file by name. The two names a daemon uses are
// MonitorsHeld (what this home owes to others) and MonitorsWatched (what this
// daemon is watching elsewhere).
func OpenMonitors(stateDir, name string) (*Monitors, error) {
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return nil, errors.New("the monitor registry needs a state directory")
	}
	dir := filepath.Join(stateDir, "fleet-monitors")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	monitors := &Monitors{path: filepath.Join(dir, name+".json"), now: time.Now, rows: map[string]Registration{}}
	data, err := os.ReadFile(monitors.path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return monitors, nil
	}
	var file monitorFile
	if err := json.Unmarshal(data, &file); err != nil {
		// A registry this daemon cannot read is not a reason to refuse to start,
		// but it IS a reason to keep the file: it is renamed aside rather than
		// overwritten, because a monitor nobody can read is still evidence.
		_ = os.Rename(monitors.path, monitors.path+".unreadable")
		return monitors, nil
	}
	for _, row := range file.Monitors {
		if strings.TrimSpace(row.Ref) == "" {
			continue
		}
		monitors.rows[row.Ref] = row
	}
	return monitors, nil
}

// The two registry names.
const (
	MonitorsHeld    = "held"
	MonitorsWatched = "watched"
)

// Add registers one monitor, or returns the existing one unchanged. Registering
// twice is a no-op and not an error: a monitor is re-established automatically
// on link up (§7), so the idempotent path is the ordinary one.
func (m *Monitors) Add(row Registration) (Registration, error) {
	if m == nil {
		return Registration{}, errors.New("the monitor registry is unavailable")
	}
	watcher, target := strings.TrimSpace(row.WatcherID), strings.TrimSpace(row.TargetID)
	if watcher == "" || target == "" {
		return Registration{}, errors.New("a monitor needs a watcher id and a target id")
	}
	row.WatcherID, row.TargetID = watcher, target
	row.Ref = MonitorRef(watcher, target)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.rows[row.Ref]; ok {
		return existing, nil
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = m.now().UTC()
	}
	m.rows[row.Ref] = row
	return row, m.saveLocked()
}

// Get reads one monitor by ref.
func (m *Monitors) Get(ref string) (Registration, bool) {
	if m == nil {
		return Registration{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[strings.TrimSpace(ref)]
	return row, ok
}

// All is every monitor in this registry, ordered by ref so a receipt is stable.
func (m *Monitors) All() []Registration {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Registration, 0, len(m.rows))
	for _, row := range m.rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// ByTarget is every monitor owed an event about one entity.
func (m *Monitors) ByTarget(targetID string) []Registration {
	targetID = strings.TrimSpace(targetID)
	out := make([]Registration, 0, 2)
	for _, row := range m.All() {
		if row.TargetID == targetID {
			out = append(out, row)
		}
	}
	return out
}

// ByWatcherHome is every monitor whose watcher lives on one machine. It is how
// a home answers "what do you hold for me?" on link up, which is the one
// question that lets a watcher adopt a monitor whose registration reply it
// never received and drop one whose watcher is gone.
func (m *Monitors) ByWatcherHome(machineID string) []Registration {
	machineID = strings.TrimSpace(machineID)
	out := make([]Registration, 0, 4)
	for _, row := range m.All() {
		if row.WatcherHome() == machineID {
			out = append(out, row)
		}
	}
	return out
}

// ByTargetHome is every monitor watching entities on one machine, which is what
// a watcher resubscribes for when that machine's link comes up.
func (m *Monitors) ByTargetHome(machineID string) []Registration {
	machineID = strings.TrimSpace(machineID)
	out := make([]Registration, 0, 4)
	for _, row := range m.All() {
		if row.TargetHome() == machineID {
			out = append(out, row)
		}
	}
	return out
}

// Homes is every distinct machine this registry's targets are homed on.
func (m *Monitors) Homes() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	for _, row := range m.All() {
		home := row.TargetHome()
		if home == "" {
			continue
		}
		if _, ok := seen[home]; ok {
			continue
		}
		seen[home] = struct{}{}
		out = append(out, home)
	}
	sort.Strings(out)
	return out
}

// Update applies one change to one monitor under the registry lock.
func (m *Monitors) Update(ref string, apply func(*Registration)) (Registration, bool) {
	if m == nil || apply == nil {
		return Registration{}, false
	}
	ref = strings.TrimSpace(ref)
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[ref]
	if !ok {
		return Registration{}, false
	}
	apply(&row)
	m.rows[ref] = row
	if err := m.saveLocked(); err != nil {
		return row, false
	}
	return row, true
}

// MinAcked is the lowest acknowledgement across every monitor on one entity,
// and whether any monitor exists. It is exactly the Owed hook the retention
// sweep asks: a terminal event above this is still owed to somebody.
func (m *Monitors) MinAcked(entity string) (uint64, bool) {
	rows := m.ByTarget(entity)
	if len(rows) == 0 {
		return 0, false
	}
	lowest := rows[0].AckedSeq
	for _, row := range rows[1:] {
		if row.AckedSeq < lowest {
			lowest = row.AckedSeq
		}
	}
	return lowest, true
}

// Remove drops one monitor. demonitor(ref) on consume, and a monitor whose
// watcher no longer exists is dropped by the target's home, so nothing leaks
// (§7).
func (m *Monitors) Remove(ref string) (Registration, bool, error) {
	if m == nil {
		return Registration{}, false, nil
	}
	ref = strings.TrimSpace(ref)
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[ref]
	if !ok {
		return Registration{}, false, nil
	}
	delete(m.rows, ref)
	return row, true, m.saveLocked()
}

func (m *Monitors) saveLocked() error {
	file := monitorFile{Version: monitorFileVersion, Monitors: make([]Registration, 0, len(m.rows))}
	for _, row := range m.rows {
		file.Monitors = append(file.Monitors, row)
	}
	sort.Slice(file.Monitors, func(i, j int) bool { return file.Monitors[i].Ref < file.Monitors[j].Ref })
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	temporary := m.path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, m.path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(m.path))
}

// homeOf reads the home machine out of a global id without importing fleetid:
// this package is below it, and the one thing it needs is the first segment.
// fleetid remains the only place that VALIDATES an id.
func homeOf(id string) string {
	id = strings.TrimSpace(id)
	rest, ok := strings.CutPrefix(id, "wa:")
	if !ok {
		return ""
	}
	home, _, ok := strings.Cut(rest, "/")
	if !ok {
		return ""
	}
	return home
}
