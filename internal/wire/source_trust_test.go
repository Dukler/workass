package wire

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workass/internal/lease"
)

// Every "is this local?" gate fails closed when it cannot tell who is asking.
func TestAnUnreadableAddressIsNotLocal(t *testing.T) {
	for _, ip := range []string{"", " ", "not-an-ip", "192.168.1.5", "100.64.1.1", "8.8.8.8"} {
		if IsLocalIP(ip) {
			t.Fatalf("IsLocalIP(%q) = true", ip)
		}
	}
	for _, ip := range []string{"127.0.0.1", "::1", "localhost"} {
		if !IsLocalIP(ip) {
			t.Fatalf("IsLocalIP(%q) = false", ip)
		}
	}
}

func TestPrivateSourcesIncludeAVPNAndNotTheInternet(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1":   true,
		"::1":         true,
		"10.1.2.3":    true,
		"172.16.0.9":  true,
		"192.168.1.5": true,
		"169.254.1.1": true,
		"100.64.1.7":  true,
		"100.127.1.7": true,
		"fd7a::3":     true,
		"fe80::1":     true,
		"fe80::1%en0": true,
		"100.128.0.1": false,
		"8.8.8.8":     false,
		"203.0.113.9": false,
		"2606:4700::": false,
		"":            false,
		"junk":        false,
	} {
		if got := IsPrivateSource(ip); got != want {
			t.Fatalf("IsPrivateSource(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestAClientOverTheApprovalLimitsIsParkedWithoutACard(t *testing.T) {
	hub := NewHub()
	hub.accessTimeout = time.Hour
	t.Cleanup(func() {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		for _, rec := range hub.pending {
			rec.timer.Stop()
		}
	})
	park := func(ip string) string {
		t.Helper()
		id, _ := hub.addPendingAccess(&client{ip: ip})
		return id
	}
	for i := 0; i < maxPendingAccessPerSource; i++ {
		if id := park("192.168.1.66"); hub.pendingIsQuiet(id) {
			t.Fatalf("request %d from one source showed no card", i)
		}
	}
	if id := park("192.168.1.66"); !hub.pendingIsQuiet(id) {
		t.Fatal("one source kept adding approval cards")
	}
	for host := 0; len(hub.pendingAccessPayloads()) < maxPendingAccess; host++ {
		if id := park(fmt.Sprintf("192.168.2.%d", host)); hub.pendingIsQuiet(id) {
			t.Fatal("a new source got no card below the overall limit")
		}
	}

	// Strangers filled the list. The owner's real request must still show.
	owner := park("192.168.9.9")
	if hub.pendingIsQuiet(owner) {
		t.Fatal("a full list hid a request from a source that had none")
	}
	if got := len(hub.pendingAccessPayloads()); got != maxPendingAccess {
		t.Fatalf("%d cards listed, want %d", got, maxPendingAccess)
	}
	// A source already showing a card gets no more once the list is full.
	if id := park("192.168.9.9"); !hub.pendingIsQuiet(id) {
		t.Fatal("the list grew past its limit")
	}

	// Sockets held open from one address must not lock a key holder out. Every new
	// connection is parked, however many came before it.
	for len(hub.pending) < maxParkedAccess {
		park("192.168.9.9")
	}
	holder := park("192.168.9.9")
	hub.mu.RLock()
	_, parked := hub.pending[holder]
	total := len(hub.pending)
	hub.mu.RUnlock()
	if !parked {
		t.Fatal("the newest connection was the one refused")
	}
	if total > maxParkedAccess {
		t.Fatalf("%d parked, want at most %d", total, maxParkedAccess)
	}
}

// The gate itself, on the real connection path. A paired device still gets in with
// its token, which the other tests cover.
func TestPairingIsRefusedFromASourceOnTheOpenInternet(t *testing.T) {
	leases, err := lease.NewManager(lease.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("leases: %v", err)
	}
	hub := NewHub(Options{Lease: leases})
	t.Cleanup(func() {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		for _, rec := range hub.pending {
			rec.timer.Stop()
		}
	})
	state := func(remote string) accessState {
		t.Helper()
		conn, peer := net.Pipe()
		t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
		request := httptest.NewRequest(http.MethodGet, "https://example.test/?deviceName=x", nil)
		request.RemoteAddr = remote
		return hub.newClient(conn, request).accessSnapshot()
	}
	if got := state("203.0.113.9:5000"); got.State != "rejected" || got.Reason != "non-private-source" {
		t.Fatalf("public source: %#v", got)
	}
	for _, remote := range []string{"192.168.1.5:5000", "100.64.1.7:5000", "[fd7a::3]:5000", "[fe80::1%en0]:5000"} {
		if got := state(remote); got.State != "waiting" {
			t.Fatalf("%s must be able to ask: %#v", remote, got)
		}
	}
}
