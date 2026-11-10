//go:build !windows

package procchild

import (
	"strings"
	"syscall"
	"testing"
)

// A descendant that stays in the process group after the main process exits
// is killed by the sweep and the receipt says so.
func TestLeftoverInTheProcessGroupIsKilledAndSaid(t *testing.T) {
	scopeWas := scopeAvailable
	scopeAvailable = func() bool { return false }
	defer func() { scopeAvailable = scopeWas }()

	state := t.TempDir()
	spec, reporter := fastSpec(t, state, "/bin/sh", "-c", "sleep 31 </dev/null >/dev/null 2>&1 & echo $! ; exit 0")
	result, err := Run(spec)
	if err != nil {
		t.Fatal(err)
	}
	events := spoolOf(t, reporter)
	settled := events[len(events)-1]
	note, _ := settled.Payload["leftovers"].(string)
	if result.Outcome != OutcomeSettled || !strings.Contains(note, "were killed") {
		t.Fatalf("outcome %q payload %+v; want a leftovers note", result.Outcome, settled.Payload)
	}
	if !strings.Contains(settled.Payload["summary"].(string), "were killed") {
		t.Fatalf("the summary must carry the leftovers note: %+v", settled.Payload)
	}
}

// With scope isolation, an active scope after the main process ended is
// stopped (kill, then stop) and reported; an empty one is left alone.
func TestSweepStopsAnActiveScopeAndStaysQuietOtherwise(t *testing.T) {
	activeWas, stopWas := scopeActive, scopeStop
	defer func() { scopeActive, scopeStop = activeWas, stopWas }()
	active := true
	var calls []string
	scopeActive = func(string) bool { return active }
	scopeStop = func(unit string, kill bool) error {
		if kill {
			calls = append(calls, "kill")
			active = false
		} else {
			calls = append(calls, "stop")
		}
		return nil
	}
	g := &group{kind: GroupSystemdScope, unit: "workass-run-xtest"}
	note := g.sweep()
	if !strings.Contains(note, "workass-run-xtest") || !strings.Contains(note, "killed") {
		t.Fatalf("note = %q", note)
	}
	if strings.Join(calls, ",") != "kill,stop" {
		t.Fatalf("calls = %v", calls)
	}
	if got := g.sweep(); got != "" {
		t.Fatalf("an emptied scope must be silent, got %q", got)
	}
}

func TestSweepOfAnEmptyProcessGroupIsSilent(t *testing.T) {
	g := &group{kind: GroupProcessGroup, pgid: 1 << 22}
	if err := syscall.Kill(-g.pgid, 0); err == nil {
		t.Skip("improbable live group")
	}
	if got := g.sweep(); got != "" {
		t.Fatalf("got %q", got)
	}
}
