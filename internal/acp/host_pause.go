package acp

import (
	"sync"
	"time"

	"workass/internal/procchild"
)

// hostPauseClock measures how much wall time passed between two reconcile
// passes while this daemon was not running to watch (bug-20261117-8c8e56dc).
//
// A machine that sleeps stops the daemon, every supervisor and every job
// together, and its wall clock jumps forward on wake. The silence verdict reads
// wall time, so on the first pass after a seven-minute battery sleep every
// supervised job on the Mac was declared silent at once, although nothing had
// been running to report or to listen. Wall time the daemon was not awake for
// is not time a supervisor failed to report in.
//
// Go's monotonic clock stops while darwin and linux sleep, so the difference
// between the two elapsed times is the time asleep. Where the monotonic clock
// keeps running through a sleep (Windows), a pass that comes long after the
// last one shows the same gap: anything beyond one heartbeat between two passes
// is time nothing here was watching.
type hostPauseClock struct {
	mu   sync.Mutex
	seen bool
	wall time.Time
	mono time.Duration
}

// hostPauseFloor ignores the drift and scheduling jitter every pass has, so a
// clock is moved only for a pause worth saying.
const hostPauseFloor = 10 * time.Second

// observe records one pass and returns the wall time since the previous one
// that the daemon was not awake to watch. wall is the wall clock and mono a
// monotonic reading of the same instant.
func (c *hostPauseClock) observe(wall time.Time, mono time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	prevWall, prevMono, seen := c.wall, c.mono, c.seen
	c.wall, c.mono, c.seen = wall, mono, true
	if !seen {
		return 0
	}
	wallGap := wall.Sub(prevWall)
	watched := min(max(mono-prevMono, 0), procchild.HeartbeatInterval)
	paused := wallGap - watched
	if paused < hostPauseFloor {
		return 0
	}
	return paused
}

// hostMonotonicBase anchors monotonic readings for the reconcile loop:
// time.Since on a time.Now value reads the monotonic clock only.
var hostMonotonicBase = time.Now()

// observeHostPass is one reconcile pass at wall time now with monotonic
// reading mono: the pause since the previous pass, if any, is taken off the
// silence clocks before anything reads them.
func (m *Manager) observeHostPass(now time.Time, mono time.Duration) time.Duration {
	if m == nil {
		return 0
	}
	paused := m.hostPause.observe(now, mono)
	if paused > 0 {
		m.creditHostPause(paused, now)
	}
	return paused
}

// creditHostPause stops the silence clock of every running supervised job for
// the time this daemon was not awake to hear it. Each clock moves forward by
// the pause and never past now, so a job's verdict is read against the time
// someone was actually listening. The pass after the pause still judges, so a
// supervisor that really died while the machine slept is still said, only no
// earlier than it would have been had the machine stayed awake.
func (m *Manager) creditHostPause(paused time.Duration, now time.Time) int {
	if m == nil || paused <= 0 {
		return 0
	}
	credited := 0
	m.spawnedWorkMu.Lock()
	for _, rec := range m.spawnedWork {
		if rec == nil || rec.Item.Status != "running" || !rec.Item.Supervised() || rec.LastReportAt.IsZero() {
			continue
		}
		moved := rec.LastReportAt.Add(paused)
		if moved.After(now) {
			moved = now
		}
		if moved.After(rec.LastReportAt) {
			rec.LastReportAt = moved
			credited++
		}
	}
	m.spawnedWorkMu.Unlock()
	if credited > 0 && m.opts.Logf != nil {
		m.opts.Logf("supervised job silence clocks paused for a host pause", map[string]any{
			"pausedMs": paused.Milliseconds(),
			"jobs":     credited,
		})
	}
	return credited
}
