package machinebook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanCandidatesUseOnlyPort80AndExcludeSelf(t *testing.T) {
	networks := []*net.IPNet{{IP: net.ParseIP("192.168.50.13"), Mask: net.CIDRMask(24, 32)}}
	candidates := scanCandidates(networks, DiscoveryPort)
	if len(candidates) != 253 {
		t.Fatalf("candidate count = %d, want 253", len(candidates))
	}
	found71 := false
	for _, address := range candidates {
		if !isPort80Address(address) {
			t.Fatalf("automatic candidate used another port: %s", address)
		}
		if address == "192.168.50.13:80" {
			t.Fatal("scanner included this machine")
		}
		if address == "192.168.50.71:80" {
			found71 = true
		}
	}
	if !found71 {
		t.Fatal("scanner omitted 192.168.50.71:80")
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
		Candidates:   []string{"192.168.50.71:80"},
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
		if got.AddedBy != SourceProbe || len(got.Endpoints) != 1 || got.Endpoints[0].Address != "192.168.50.71:80" {
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
	scanner := &Scanner{Book: book, Candidates: []string{"192.168.50.71:80"}}
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

func TestSweepPortsCoverTheDaemonDefaultAndNothingElse(t *testing.T) {
	for address, want := range map[string]bool{
		"192.168.50.71:80":   true,
		"192.168.50.71:8788": true,
		"192.168.50.71:443":  false,
		"192.168.50.71:9000": false,
	} {
		if got := isSweepAddress(address); got != want {
			t.Fatalf("isSweepAddress(%s) = %v, want %v", address, got, want)
		}
	}
}

// A sweep of port 80 alone could never find a macOS or Linux daemon, which is the
// reason this port is swept at all.
func TestBookAcceptsADaemonOnItsDefaultPortFromASweep(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-mac", "mac", 1))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, daemon.address())
		},
	}
	book, err := Open(Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	if _, _, err := book.Discover(context.Background(), "192.168.50.20:8788"); err != nil {
		t.Fatalf("a daemon on its default port was refused: %v", err)
	}
	if _, _, err := book.Discover(context.Background(), "192.168.50.20:9000"); err == nil {
		t.Fatal("a sweep reached a port it has no reason to probe")
	}
}

func TestGateStopsTheLoopButNotAnAskedForSweep(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-other", "other", 1))
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, daemon.address())
		},
	}
	book, err := Open(Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	var gateCalls atomic.Int32
	scanner := &Scanner{
		Book: book, Interval: time.Millisecond, ProbeTimeout: time.Second,
		Candidates: []string{"192.168.50.20:8788"},
		Gate:       func(context.Context) bool { gateCalls.Add(1); return false },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scanner.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for gateCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if gateCalls.Load() == 0 {
		t.Fatal("the loop never consulted its gate")
	}
	if probes := daemon.probes.Load(); probes != 0 {
		t.Fatalf("a gated loop probed %d times", probes)
	}
	if !scanner.SweepNow(context.Background()) {
		t.Fatal("an asked-for sweep found nothing")
	}
	if entries := book.List(); len(entries) != 1 || entries[0].MachineID != "m-other" {
		t.Fatalf("book after asked-for sweep: %#v", entries)
	}
}

func TestAssociationRequiresTheStoredCertificateOnAnAttachedNetwork(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-home", "home", 1))
	sum := sha256.Sum256(daemon.server.Certificate().Raw)
	real := hex.EncodeToString(sum[:])
	loopback := []*net.IPNet{{IP: net.IPv4(127, 0, 0, 0), Mask: net.CIDRMask(8, 32)}}
	elsewhere := []*net.IPNet{{IP: net.IPv4(192, 168, 50, 0), Mask: net.CIDRMask(24, 32)}}

	book := func(fingerprint string) *Book {
		t.Helper()
		b, err := Open(Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 1})
		if err != nil {
			t.Fatalf("open book: %v", err)
		}
		b.entries["m-home"] = Entry{
			MachineID: "m-home", CertFingerprint: fingerprint,
			Endpoints: []Endpoint{{Kind: KindLAN, Address: daemon.address()}},
		}
		return b
	}
	ctx := context.Background()
	if !book(real).associated(ctx, loopback) {
		t.Fatal("the stored certificate answering on an attached network must count")
	}
	if book(strings.Repeat("0", 64)).associated(ctx, loopback) {
		t.Fatal("a different certificate at a remembered address must not count")
	}
	if book(real).associated(ctx, elsewhere) {
		t.Fatal("an address on no attached network must not count")
	}
	if book("").associated(ctx, loopback) {
		t.Fatal("a machine with no stored certificate must not count")
	}
}
