package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/fleetclient"
	"workass/internal/lease"
)

// B8's third half: the output itself was the bug report.
//
// `workass fleet links` printed the inbound records in one block and the
// outbound ones in another, with no verdict in either. So a `lastUse` coalesced
// to five minutes read as a dead link, while the inbound half further down
// carried a timestamp from thirty seconds ago for the same pair — and an
// operator reading it concluded, reasonably, that a live link was down. One row
// per pair with the verdict on it is the fix, and these are the four readings
// that have to be unmistakable.

// fleetLinksStateDir builds a state dir holding exactly the records a pair
// leaves behind, with the clock in the test's hands: `lastSeen` is whatever the
// lease manager is told the time is.
func fleetLinksStateDir(t *testing.T, seenAt time.Time, peers ...string) string {
	t.Helper()
	stateDir := t.TempDir()
	leases, err := lease.NewManager(lease.Options{StateDir: stateDir, Now: func() time.Time { return seenAt }})
	if err != nil {
		t.Fatalf("device store: %v", err)
	}
	for _, peer := range peers {
		if _, err := leases.EnrollPeerDevice("daemon "+peer, "192.168.0.134", "token-"+peer, "local", "key-1", peer, ""); err != nil {
			t.Fatalf("enrol %s: %v", peer, err)
		}
	}
	return stateDir
}

// writePeerFile writes the outbound half directly, which is the one way to hand
// the reader a lastUse of a chosen age: the peer store stamps its own clock.
func writePeerFile(t *testing.T, stateDir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stateDir, fleetclient.FileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write peer file: %v", err)
	}
}

func TestFleetLinksReadsUpOrDownAtAGlance(t *testing.T) {
	now := time.Now().UTC()
	stamp := func(ago time.Duration) string { return now.Add(-ago).Format(time.RFC3339) }

	t.Run("a coalesced lastUse is not a dead link", func(t *testing.T) {
		// The exact reading that opened B8: the beat was written down four
		// minutes ago because that is how often it is written down at all.
		stateDir := fleetLinksStateDir(t, now.Add(-30*time.Second), "m-duklerver")
		writePeerFile(t, stateDir, `{"version":1,"peers":[{"machineId":"m-duklerver","name":"duklerver",
			"routes":[{"kind":"lan","address":"192.168.0.134:8788","source":"manual","lastSuccessAt":"`+stamp(time.Hour)+`"}],
			"deviceToken":"a-derived-token","pairedAt":"`+stamp(2*time.Hour)+`","lastUseAt":"`+stamp(4*time.Minute)+`"}]}`)

		out := fleetLinks(t, stateDir)
		if !strings.Contains(out, "pair m-duklerver") {
			t.Fatalf("no row for the pair: %q", out)
		}
		if !strings.Contains(out, "UP") || strings.Contains(out, "DOWN") {
			t.Fatalf("a link whose peer was seen 30s ago did not read as up: %q", out)
		}
		// The coalescing window is named on the line carrying the timestamp, so
		// a five-minute-old lastUse cannot be misread as five minutes of silence.
		if !strings.Contains(out, "recorded at most every") {
			t.Fatalf("lastUse is printed without saying how often it is recorded: %q", out)
		}
		// B4's rule still holds, and no credential is ever printed.
		if !strings.Contains(out, "paired") || strings.Contains(out, "known, not linked") {
			t.Fatalf("a paired peer printed as unlinked: %q", out)
		}
		if strings.Contains(out, "a-derived-token") {
			t.Fatalf("fleet links printed a credential: %q", out)
		}
	})

	t.Run("an inbound-only pair is up and says it cannot be dialled", func(t *testing.T) {
		// duklerver's own state at the time of the report: a live link in, and
		// no peer file at all. The absence IS the finding, so it prints.
		stateDir := fleetLinksStateDir(t, now.Add(-20*time.Second), "m-mac")
		out := fleetLinks(t, stateDir)
		if !strings.Contains(out, "pair m-mac") || !strings.Contains(out, "UP") {
			t.Fatalf("a live inbound link did not read as up: %q", out)
		}
		if !strings.Contains(out, "not dialled") {
			t.Fatalf("the missing outbound half is invisible: %q", out)
		}
		// The sentence an operator greps for is kept verbatim, and it is about
		// the outbound half only — it must not read as "this pair is down".
		if !strings.Contains(out, "this machine has dialled no peer") {
			t.Fatalf("the outbound sentinel sentence is gone: %q", out)
		}
	})

	t.Run("real silence reads as down and names the threshold", func(t *testing.T) {
		stateDir := fleetLinksStateDir(t, now.Add(-3*time.Hour), "m-dark")
		out := fleetLinks(t, stateDir)
		if !strings.Contains(out, "DOWN") {
			t.Fatalf("a pair silent for three hours read as up: %q", out)
		}
		// The number is home_down's own, so the CLI and the watcher cannot
		// disagree about the same pair.
		if !strings.Contains(out, fleetHomeDownThreshold.String()) {
			t.Fatalf("down is printed without the threshold it is measured against: %q", out)
		}
	})

	t.Run("paired but never beaten is unknown, not down", func(t *testing.T) {
		stateDir := t.TempDir()
		writePeerFile(t, stateDir, `{"version":1,"peers":[{"machineId":"m-fresh","name":"fresh",
			"routes":[{"kind":"lan","address":"192.168.0.9:8788","source":"manual"}],
			"deviceToken":"t","pairedAt":"`+stamp(time.Minute)+`"}]}`)
		out := fleetLinks(t, stateDir)
		if !strings.Contains(out, "UNKNOWN") {
			t.Fatalf("a pair with no beat yet was given a verdict anyway: %q", out)
		}
		if strings.Contains(out, "DOWN") {
			t.Fatalf("no evidence was reported as down, which is the one thing home_down must never mean: %q", out)
		}
	})
}

func fleetLinks(t *testing.T, stateDir string) string {
	t.Helper()
	var out strings.Builder
	if err := printFleetLinks(stateDir, &out); err != nil {
		t.Fatalf("print fleet links: %v", err)
	}
	t.Log("\n" + out.String())
	return out.String()
}
