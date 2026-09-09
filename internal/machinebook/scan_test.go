package machinebook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanCandidatesUseOnlyPort80AndExcludeSelf(t *testing.T) {
	networks := []*net.IPNet{{IP: net.ParseIP("192.168.0.13"), Mask: net.CIDRMask(24, 32)}}
	candidates := scanCandidates(networks, DiscoveryPort)
	if len(candidates) != 253 {
		t.Fatalf("candidate count = %d, want 253", len(candidates))
	}
	found71 := false
	for _, address := range candidates {
		if !isPort80Address(address) {
			t.Fatalf("automatic candidate used another port: %s", address)
		}
		if address == "192.168.0.13:80" {
			t.Fatal("scanner included this machine")
		}
		if address == "192.168.0.71:80" {
			found71 = true
		}
	}
	if !found71 {
		t.Fatal("scanner omitted 192.168.0.71:80")
	}
}

func TestScanCandidatesRefusePublicRanges(t *testing.T) {
	networks := []*net.IPNet{{IP: net.ParseIP("203.0.113.7"), Mask: net.CIDRMask(24, 32)}}
	if candidates := scanCandidates(networks, DiscoveryPort); len(candidates) != 0 {
		t.Fatalf("public range produced candidates: %v", candidates)
	}
}

func TestScannerAutoDetectsWithoutManualAddressUI(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-port80", "managed windows", 1))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, daemon.address())
		},
	}
	book, err := Open(Options{
		StateDir:    t.TempDir(),
		SelfID:      "m-self",
		WireVersion: 1,
		HTTPClient:  &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	found := make(chan Entry, 1)
	scanner := &Scanner{
		Book:         book,
		Candidates:   []string{"192.168.0.71:80"},
		ProbeTimeout: time.Second,
		OnChange:     func(entry Entry) { found <- entry },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scanner.Run(ctx) }()
	select {
	case got := <-found:
		cancel()
		if got.MachineID != "m-port80" || got.Name != "managed windows" {
			t.Fatalf("found %+v", got)
		}
		if got.AddedBy != SourceProbe || len(got.Endpoints) != 1 || got.Endpoints[0].Address != "192.168.0.71:80" {
			t.Fatalf("automatic entry = %+v", got)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("automatic discovery did not surface the machine")
	}
	if err := <-done; err != nil {
		t.Fatalf("scanner stopped: %v", err)
	}
}

func TestStableDiscoveryBackoffKeepsScanningAndResetsOnChanges(t *testing.T) {
	now := time.Unix(1000, 0)
	schedule := scanSchedule{base: 10 * time.Second}
	if !schedule.due(now, "network-a") {
		t.Fatal("initial discovery was delayed")
	}
	for _, delay := range []time.Duration{10, 20, 40, 60, 60} {
		schedule.finished(now, false)
		want := delay * time.Second
		if schedule.delay != want || schedule.due(now.Add(want-time.Nanosecond), "network-a") {
			t.Fatalf("bad stable delay: %v, want %v", schedule.delay, want)
		}
		now = now.Add(want)
		if !schedule.due(now, "network-a") {
			t.Fatal("backoff stopped discovery of new machines")
		}
	}
	schedule.finished(now, true)
	if schedule.delay != 10*time.Second {
		t.Fatal("newly discovered machine did not reset backoff")
	}
	schedule.finished(now, false)
	if !schedule.due(now.Add(time.Second), "network-b") {
		t.Fatal("network change waited for the old discovery deadline")
	}
	schedule.finished(now, false)
	if schedule.delay != 10*time.Second {
		t.Fatal("network change did not restore fast scans")
	}
}

func TestDiscoveryRescanFindsMachineAppearingAfterStableScan(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-new-after-backoff", "new machine", 1))
	var online atomic.Bool
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		if !online.Load() {
			return nil, errors.New("not online yet")
		}
		return (&net.Dialer{}).DialContext(ctx, network, daemon.address())
	}}
	defer transport.CloseIdleConnections()
	book, err := Open(Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	scanner := &Scanner{Book: book, Candidates: []string{"192.168.0.71:80"}}
	if scanner.scan(t.Context()) {
		t.Fatal("offline host appeared in discovery")
	}
	online.Store(true)
	if !scanner.scan(t.Context()) {
		t.Fatal("later discovery missed a new machine")
	}
	if scanner.scan(t.Context()) {
		t.Fatal("unchanged endpoint prevents discovery backoff")
	}
}
