package machinebook

import (
	"context"
	"net"
	"net/http"
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
