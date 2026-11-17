package acp

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"workass/internal/procchild"
)

func TestHostPauseClockMeasuresOnlyTimeTheDaemonWasNotAwake(t *testing.T) {
	start := time.Date(2026, 11, 17, 16, 22, 47, 0, time.UTC)
	cases := []struct {
		name       string
		wall, mono time.Duration
		want       time.Duration
	}{
		{"an ordinary pass", 2 * time.Second, 2 * time.Second, 0},
		{"drift and jitter", 2*time.Second + 300*time.Millisecond, 2 * time.Second, 0},
		// darwin and linux: the monotonic clock stops while the machine sleeps.
		{"a sleep the monotonic clock skipped", 457 * time.Second, 2 * time.Second, 455 * time.Second},
		// Windows: it keeps running, and the pass simply comes very late.
		{"a sleep the monotonic clock counted", 457 * time.Second, 457 * time.Second, 397 * time.Second},
		{"the wall clock stepped back", -time.Minute, 2 * time.Second, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var clock hostPauseClock
			if got := clock.observe(start, time.Hour); got != 0 {
				t.Fatalf("the first pass has nothing to compare with, got %s", got)
			}
			if got := clock.observe(start.Add(tc.wall), time.Hour+tc.mono); got != tc.want {
				t.Fatalf("paused = %s, want %s", got, tc.want)
			}
		})
	}
}

// bug-20261117-8c8e56dc: the Mac went into Low Power Sleep at 1% battery for
// 455 s. Every supervisor, every job and the daemon itself slept together,
// and the first pass on wake declared seven live jobs "not reporting" at once
// because it read the sleep as silence.
func TestASleepingMachineDoesNotMakeItsSupervisorsSilent(t *testing.T) {
	lifecycle := &recordedLifecycle{}
	manager := processChildManager(t, lifecycle)
	children := []string{}
	for i := 0; i < 7; i++ {
		childID, _ := seedProcessChild(t, manager, fmt.Sprintf("op-asleep-%d", i), os.Getpid(), nil)
		appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242 + i})
		children = append(children, childID)
	}

	// Last heard 40 s before the machine slept, which is inside one beat.
	start := time.Now().UTC()
	lastHeard := start.Add(-40 * time.Second)
	manager.spawnedWorkMu.Lock()
	for _, rec := range manager.spawnedWork {
		rec.LastReportAt = lastHeard
	}
	manager.spawnedWorkMu.Unlock()
	mono := time.Hour
	pass := func(now time.Time, monoStep time.Duration) {
		mono += monoStep
		manager.observeHostPass(now, mono)
		manager.reconcileProcessChildren(now)
	}
	pass(start, 0)

	wake := start.Add(455*time.Second + 2*time.Second)
	pass(wake, 2*time.Second)
	if kinds := lifecycle.kinds(); strings.Contains(kinds, "not_reporting") {
		t.Fatalf("a machine that slept heard nothing because nothing was running, got %q", kinds)
	}
	for _, childID := range children {
		if item := itemFor(t, manager, childID); item.Reporting != "" {
			t.Fatalf("%s is marked %q after a sleep", childID, item.Reporting)
		}
	}

	// The supervisors beat again soon after wake, as they did on the Mac.
	for _, childID := range children[1:] {
		appendSpool(t, manager, childID, procchild.KindHeartbeat, nil)
	}
	manager.spawnedWorkMu.Lock()
	for _, rec := range manager.spawnedWork {
		if rec.Item.ID != children[0] {
			// Heard on the test's clock, which is the wall clock after wake.
			rec.LastReportAt = wake
		}
	}
	manager.spawnedWorkMu.Unlock()

	// The one that never answers again is still said, against awake time:
	// its window runs from the 40 s it had used before the sleep.
	now := wake
	for now.Before(wake.Add(procchild.SilenceThreshold - 40*time.Second - 2*time.Second - 10*time.Second)) {
		now = now.Add(2 * time.Second)
		pass(now, 2*time.Second)
	}
	if kinds := lifecycle.kinds(); strings.Contains(kinds, "not_reporting") {
		t.Fatalf("still inside the awake window, got %q", kinds)
	}
	for i := 0; i < 15; i++ {
		now = now.Add(2 * time.Second)
		pass(now, 2*time.Second)
	}
	if got := strings.Count(lifecycle.kinds(), "attention:not_reporting"); got != 1 {
		t.Fatalf("the supervisor that stayed quiet is said once, got %d in %q", got, lifecycle.kinds())
	}
	if item := itemFor(t, manager, children[0]); item.Reporting != processReportingSilent {
		t.Fatalf("the quiet one is marked silent, got %q", item.Reporting)
	}
}

// The credit is for supervised rows that are running: a settled row, or one
// whose clock has not started, keeps what it has.
func TestAHostPauseNeverMovesAClockPastNow(t *testing.T) {
	manager := processChildManager(t, nil)
	childID, _ := seedProcessChild(t, manager, "op-pause-cap", os.Getpid(), nil)
	appendSpool(t, manager, childID, procchild.KindStarted, map[string]any{"pid": 4242})
	now := time.Now().UTC()
	manager.spawnedWorkMu.Lock()
	for _, rec := range manager.spawnedWork {
		rec.LastReportAt = now.Add(-30 * time.Second)
	}
	manager.spawnedWorkMu.Unlock()
	if credited := manager.creditHostPause(10*time.Minute, now); credited != 1 {
		t.Fatalf("credited %d rows, want 1", credited)
	}
	manager.spawnedWorkMu.Lock()
	defer manager.spawnedWorkMu.Unlock()
	for _, rec := range manager.spawnedWork {
		if !rec.LastReportAt.Equal(now) {
			t.Fatalf("the clock moved to %s, want no later than now %s", rec.LastReportAt, now)
		}
	}
}
