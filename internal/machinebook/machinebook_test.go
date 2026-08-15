package machinebook

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDaemon serves an identity document the way a real daemon does, and counts
// how many times it was asked — several tests turn on the probe count rather
// than the answer.
type fakeDaemon struct {
	server *httptest.Server
	probes atomic.Int64
	doc    atomic.Value // map[string]any
}

func newFakeDaemon(t *testing.T, doc map[string]any) *fakeDaemon {
	t.Helper()
	daemon := &fakeDaemon{}
	daemon.doc.Store(doc)
	daemon.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			http.NotFound(w, r)
			return
		}
		daemon.probes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(daemon.doc.Load().(map[string]any))
	}))
	t.Cleanup(daemon.server.Close)
	return daemon
}

func (f *fakeDaemon) address() string {
	return strings.TrimPrefix(f.server.URL, "https://")
}

func identityDoc(id, name string, wireVersion int) map[string]any {
	return map[string]any{
		"app":             "workass",
		"version":         "1.2.3",
		"name":            name,
		"displayName":     name,
		"machineId":       id,
		"wireVersion":     wireVersion,
		"secure":          true,
		"certFingerprint": "test-certificate-fingerprint",
	}
}

func openBook(t *testing.T, selfID string) *Book {
	t.Helper()
	book, err := Open(Options{StateDir: t.TempDir(), SelfID: selfID, WireVersion: 1})
	if err != nil {
		t.Fatalf("open book: %v", err)
	}
	return book
}

func TestNormalizeAddress(t *testing.T) {
	cases := []struct{ in, want string }{
		{"192.168.1.50", "192.168.1.50:8788"},
		{"192.168.1.50:18788", "192.168.1.50:18788"},
		{"http://192.168.1.50:18788/", "192.168.1.50:18788"},
		{"https://builder:80/workass/health", "builder:80"},
		{"  builder  ", "builder:8788"},
		{"[fe80::1]:8788", "[fe80::1]:8788"},
		{"fe80::1", "[fe80::1]:8788"},
	}
	for _, tc := range cases {
		got, err := NormalizeAddress(tc.in)
		if err != nil {
			t.Fatalf("NormalizeAddress(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("NormalizeAddress(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", "http://", "://x"} {
		if _, err := NormalizeAddress(bad); err == nil {
			t.Fatalf("NormalizeAddress(%q) accepted a non-address", bad)
		}
	}
}

func TestAddRecordsProbedMachine(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")

	entry, err := book.Add(context.Background(), daemon.address())
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if entry.MachineID != "m-remote" || entry.Name != "builder" {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.Status != StatusOK || entry.Reason != "" {
		t.Fatalf("status = %q reason = %q", entry.Status, entry.Reason)
	}
	if entry.Owner != LocalOwner {
		t.Fatalf("owner = %q, want the single-owner placeholder", entry.Owner)
	}
	if entry.AddedBy != SourceManual {
		t.Fatalf("addedBy = %q, want %q", entry.AddedBy, SourceManual)
	}
	if len(entry.Endpoints) != 1 || entry.Endpoints[0].Kind != KindLAN {
		t.Fatalf("endpoints = %+v", entry.Endpoints)
	}
	if entry.AddedAt == "" || entry.LastSeenAt == "" {
		t.Fatalf("timestamps missing: %+v", entry)
	}
}

// The same machine reached a second way is one machine with two endpoints —
// the shape D7 exists for, and what makes a VPN a later address rather than a
// later rewrite.
func TestAddSecondAddressJoinsOneEntry(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")

	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("first add: %v", err)
	}
	host, port, err := net.SplitHostPort(daemon.address())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if host != "127.0.0.1" {
		t.Skipf("httptest bound %s, not loopback", host)
	}
	// Same daemon, a name for the same address: a second endpoint, not a
	// second machine.
	if _, err := book.Add(context.Background(), net.JoinHostPort("localhost", port)); err != nil {
		t.Fatalf("second add: %v", err)
	}

	list := book.List()
	if len(list) != 1 {
		t.Fatalf("book has %d entries, want 1: %+v", len(list), list)
	}
	if len(list[0].Endpoints) != 2 {
		t.Fatalf("endpoints = %+v, want 2", list[0].Endpoints)
	}
}

func TestVerifiedEndpointMovesToTheMachineIDThatAnswered(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-old", "old identity", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add old identity: %v", err)
	}

	daemon.doc.Store(identityDoc("m-new", "current identity", 1))
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add current identity: %v", err)
	}

	list := book.List()
	if len(list) != 1 || list[0].MachineID != "m-new" {
		t.Fatalf("verified endpoint remained duplicated: %+v", list)
	}
	book.mu.Lock()
	_, staleExists := book.entries["m-old"]
	book.mu.Unlock()
	if staleExists {
		t.Fatal("stale machine id still owns the verified endpoint")
	}
}

func TestVerifiedEndpointTransferRetainsASeparateOldEndpoint(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-old", "old identity", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add old identity: %v", err)
	}
	host, port, err := net.SplitHostPort(daemon.address())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if host != "127.0.0.1" {
		t.Skipf("httptest bound %s, not loopback", host)
	}
	secondAddress := net.JoinHostPort("localhost", port)
	if _, err := book.Add(context.Background(), secondAddress); err != nil {
		t.Fatalf("add second endpoint: %v", err)
	}

	daemon.doc.Store(identityDoc("m-new", "current identity", 1))
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("transfer first endpoint: %v", err)
	}

	book.mu.Lock()
	old := book.entries["m-old"]
	current := book.entries["m-new"]
	book.mu.Unlock()
	if len(old.Endpoints) != 1 || old.Endpoints[0].Address != secondAddress {
		t.Fatalf("old machine lost its distinct endpoint or kept the transferred one: %+v", old.Endpoints)
	}
	if len(current.Endpoints) != 1 || current.Endpoints[0].Address != daemon.address() {
		t.Fatalf("new machine did not receive the verified endpoint: %+v", current.Endpoints)
	}
}

func TestListCoalescesLegacyOfflineCopiesOfOneEndpoint(t *testing.T) {
	book := openBook(t, "m-self")
	endpoint := []Endpoint{{Kind: KindLAN, Address: "192.168.0.71:80"}}
	book.mu.Lock()
	book.entries["m-stale-a"] = Entry{MachineID: "m-stale-a", Name: "node", Status: StatusUnreachable, LastSeenAt: "2026-07-01T00:00:00Z", Endpoints: endpoint}
	book.entries["m-stale-b"] = Entry{MachineID: "m-stale-b", Name: "node", Status: StatusUnreachable, LastSeenAt: "2026-07-02T00:00:00Z", Endpoints: endpoint}
	book.entries["m-current"] = Entry{MachineID: "m-current", Name: "node", Status: StatusOK, LastSeenAt: "2026-07-01T00:00:00Z", Endpoints: endpoint}
	book.mu.Unlock()

	list := book.List()
	if len(list) != 1 || list[0].MachineID != "m-current" {
		t.Fatalf("legacy copies rendered as separate nodes: %+v", list)
	}
}

func TestAddRefusesSelf(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-self", "this mac", 1))
	book := openBook(t, "m-self")

	if _, err := book.Add(context.Background(), daemon.address()); !errors.Is(err, ErrSelf) {
		t.Fatalf("adding self returned %v, want ErrSelf", err)
	}
	if len(book.List()) != 0 {
		t.Fatalf("book recorded itself: %+v", book.List())
	}
}

func TestAddRejectsStrangers(t *testing.T) {
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>hello</html>"))
	}))
	defer web.Close()
	book := openBook(t, "m-self")

	_, err := book.Add(context.Background(), strings.TrimPrefix(web.URL, "https://"))
	if !errors.Is(err, ErrNotWorkass) {
		t.Fatalf("adding a web server returned %v, want ErrNotWorkass", err)
	}
}

func TestAddRejectsDaemonWithoutIdentity(t *testing.T) {
	daemon := newFakeDaemon(t, map[string]any{"app": "workass", "version": "0.9", "name": "old"})
	book := openBook(t, "m-self")

	_, err := book.Add(context.Background(), daemon.address())
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("adding a pre-identity daemon returned %v, want ErrNoIdentity", err)
	}
}

// Unreachable is not forgotten: a machine that is off is still a machine you
// know about, so the endpoints and the last time it answered both survive.
func TestRefreshMarksDeadMachineUnreachable(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}
	before := book.List()[0]
	daemon.server.Close()

	list, changed := book.Refresh(context.Background())
	if !changed {
		t.Fatal("a machine going dark is a change worth reporting")
	}
	if len(list) != 1 {
		t.Fatalf("book has %d entries, want 1", len(list))
	}
	after := list[0]
	if after.Status != StatusUnreachable {
		t.Fatalf("status = %q, want %q", after.Status, StatusUnreachable)
	}
	if !strings.Contains(after.Reason, "refused the connection") {
		t.Fatalf("reason = %q, want it to say plainly that nobody is listening", after.Reason)
	}
	if strings.Contains(after.Reason, "http://") {
		t.Fatalf("reason = %q — a dial chain is true and unreadable", after.Reason)
	}
	if after.LastSeenAt != before.LastSeenAt {
		t.Fatalf("last seen moved on a failed probe: %q -> %q", before.LastSeenAt, after.LastSeenAt)
	}
	if len(after.Endpoints) != 1 {
		t.Fatalf("endpoints dropped: %+v", after.Endpoints)
	}
}

// With several endpoints, quoting one failed address reads as if the others
// might have worked.
func TestUnreachableAcrossSeveralAddressesSaysSo(t *testing.T) {
	book := openBook(t, "m-self")
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}
	_, port, _ := net.SplitHostPort(daemon.address())
	if _, err := book.Add(context.Background(), net.JoinHostPort("localhost", port)); err != nil {
		t.Fatalf("second endpoint: %v", err)
	}
	daemon.server.Close()

	list, _ := book.Refresh(context.Background())
	if !strings.Contains(list[0].Reason, "none of its 2 addresses") {
		t.Fatalf("reason = %q", list[0].Reason)
	}
}

func TestRefreshStaysQuietWhenNothingMoved(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, changed := book.Refresh(context.Background()); changed {
		t.Fatal("a machine that is still exactly where it was is not news")
	}
}

func TestRefreshRekeysAnEndpointWhenItsHealthIdentityChanged(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-old", "old identity", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}
	daemon.doc.Store(identityDoc("m-new", "current identity", 1))

	list, changed := book.Refresh(context.Background())
	if !changed {
		t.Fatal("identity change was not reported")
	}
	if len(list) != 1 || list[0].MachineID != "m-new" || list[0].Name != "current identity" {
		t.Fatalf("refresh kept the endpoint under its stale id: %+v", list)
	}
}

func TestWireVersionGapNamesTheSideThatIsBehind(t *testing.T) {
	older := newFakeDaemon(t, identityDoc("m-old", "old box", 1))
	newer := newFakeDaemon(t, identityDoc("m-new", "new box", 9))
	book, err := Open(Options{StateDir: t.TempDir(), SelfID: "m-self", WireVersion: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	behind, err := book.Add(context.Background(), older.address())
	if err != nil {
		t.Fatalf("add older: %v", err)
	}
	if behind.Status != StatusNeedsUpdate || !strings.Contains(behind.Reason, "that machine") {
		t.Fatalf("older machine: status %q reason %q", behind.Status, behind.Reason)
	}
	ahead, err := book.Add(context.Background(), newer.address())
	if err != nil {
		t.Fatalf("add newer: %v", err)
	}
	if ahead.Status != StatusNeedsUpdate || !strings.Contains(ahead.Reason, "this machine") {
		t.Fatalf("newer machine: status %q reason %q", ahead.Status, ahead.Reason)
	}
}

func TestBookSurvivesRestart(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	stateDir := t.TempDir()
	book, err := Open(Options{StateDir: stateDir, SelfID: "m-self", WireVersion: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}

	reopened, err := Open(Options{StateDir: stateDir, SelfID: "m-self", WireVersion: 1})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	list := reopened.List()
	if len(list) != 1 || list[0].MachineID != "m-remote" || list[0].Name != "builder" {
		t.Fatalf("reopened book = %+v", list)
	}
	if list[0].Owner != LocalOwner {
		t.Fatalf("owner lost across restart: %+v", list[0])
	}
}

func TestOpenRefusesCorruptBook(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, FileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Open(Options{StateDir: stateDir}); err == nil {
		t.Fatal("a corrupt book opened silently — the machines would vanish without a word")
	}
}

func TestForget(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")
	if _, err := book.Add(context.Background(), daemon.address()); err != nil {
		t.Fatalf("add: %v", err)
	}

	forgotten, err := book.Forget("m-remote")
	if err != nil || !forgotten {
		t.Fatalf("forget = %v, %v", forgotten, err)
	}
	if len(book.List()) != 0 {
		t.Fatalf("still listed: %+v", book.List())
	}
	again, err := book.Forget("m-remote")
	if err != nil || again {
		t.Fatalf("second forget = %v, %v; want false, nil", again, err)
	}
}

// A machine that is simply still there must not cost a connection, or an idle
// network with a handful of daemons on it never goes quiet.
func TestSightedProbesOnlyWhenSomethingChanged(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-remote", "builder", 1))
	book := openBook(t, "m-self")

	entry, changed, err := book.Sighted(context.Background(), "m-remote", daemon.address())
	if err != nil || !changed {
		t.Fatalf("first sighting = %+v, %v, %v", entry, changed, err)
	}
	if entry.AddedBy != SourceBeacon {
		t.Fatalf("addedBy = %q, want %q", entry.AddedBy, SourceBeacon)
	}
	first := daemon.probes.Load()

	for range 5 {
		if _, changed, err := book.Sighted(context.Background(), "m-remote", daemon.address()); err != nil || changed {
			t.Fatalf("repeat sighting = %v, %v", changed, err)
		}
	}
	if got := daemon.probes.Load(); got != first {
		t.Fatalf("repeat sightings probed %d extra times", got-first)
	}
}

func TestSightedIgnoresSelf(t *testing.T) {
	book := openBook(t, "m-self")
	if _, _, err := book.Sighted(context.Background(), "m-self", "192.168.0.13:8788"); !errors.Is(err, ErrSelf) {
		t.Fatalf("hearing itself returned %v, want ErrSelf", err)
	}
}

// The id inside a UDP packet decides whether to look; the id on the card
// decides what gets written. A machine announcing someone else's id must not be
// able to overwrite that someone else's entry.
func TestSightedTrustsTheCardNotThePacket(t *testing.T) {
	daemon := newFakeDaemon(t, identityDoc("m-actual", "actual box", 1))
	book := openBook(t, "m-self")

	entry, changed, err := book.Sighted(context.Background(), "m-claimed", daemon.address())
	if err != nil || !changed {
		t.Fatalf("sighting = %v, %v", changed, err)
	}
	if entry.MachineID != "m-actual" {
		t.Fatalf("recorded %q, want the id the machine itself returned", entry.MachineID)
	}
	list := book.List()
	if len(list) != 1 || list[0].MachineID != "m-actual" {
		t.Fatalf("book = %+v", list)
	}
}

// The first acceptance gate, run for real: two daemons on one host find each
// other by beacon alone, with nobody typing an address.
func TestBeaconFindsPeer(t *testing.T) {
	lanIP := firstLANIPv4(t)
	remote := lanDaemon(t, lanIP, identityDoc("m-peer", "peer daemon", 1))
	book := openBook(t, "m-listener")

	// Off the group real daemons listen on. These are real packets on a real
	// network, and a test must not be able to write an invented machine into
	// the book of a daemon someone is actually using.
	const testGroup = "239.87.87.99:48799"

	found := make(chan Entry, 4)
	listener := &Beacon{
		Book:        book,
		MachineID:   "m-listener",
		Name:        "listener",
		Port:        remote.port,
		Interval:    200 * time.Millisecond,
		Group:       testGroup,
		ReceiveOnly: true,
		OnChange:    func(entry Entry) { found <- entry },
	}
	announcer := &Beacon{
		MachineID: "m-peer",
		Name:      "peer daemon",
		Port:      remote.port,
		Interval:  200 * time.Millisecond,
		Group:     testGroup,
		// A listening book is not needed on the announcing side; this beacon
		// exists only to put packets on the wire.
		Book: openBook(t, "m-peer"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() {
		if err := listener.Run(ctx); err != nil {
			t.Errorf("listener beacon: %v", err)
		}
	}()
	go func() {
		if err := announcer.Run(ctx); err != nil {
			t.Errorf("announcer beacon: %v", err)
		}
	}()

	select {
	case entry := <-found:
		if entry.MachineID != "m-peer" {
			t.Fatalf("found %+v, want the announcing peer", entry)
		}
		if entry.AddedBy != SourceBeacon {
			t.Fatalf("addedBy = %q", entry.AddedBy)
		}
	case <-ctx.Done():
		t.Skip("no multicast between processes on this host — the live gate covers this path")
	}
}

// The beacon must not announce onto a VPN. Flags are the portable tell: on
// macOS a WireGuard or Tailscale interface is POINTOPOINT, and on Linux it
// simply is not BROADCAST — either way it is not somewhere this daemon gets to
// advertise itself.
func TestUsableInterfaceExcludesTunnels(t *testing.T) {
	const (
		up    = net.FlagUp | net.FlagMulticast
		lan   = up | net.FlagBroadcast
		vpnV4 = up | net.FlagPointToPoint // utun6 / utun10 on this Mac
	)
	cases := []struct {
		name  string
		flags net.Flags
		want  bool
	}{
		{"en0 wifi", lan, true},
		{"bridge100 vm host", lan, true},
		{"utun tailscale", vpnV4, false},
		{"linux wg (no broadcast)", up, false},
		{"loopback", lan | net.FlagLoopback, false},
		{"down", net.FlagBroadcast | net.FlagMulticast, false},
		{"no multicast", net.FlagUp | net.FlagBroadcast, false},
	}
	for _, tc := range cases {
		if got := usableInterface(net.Interface{Flags: tc.flags}); got != tc.want {
			t.Fatalf("%s: usableInterface = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBeaconBoundsFailedUnknownProbesWithoutThrottlingKnownPeers(t *testing.T) {
	used := 0
	for range maxNewPerInterval {
		if !reserveProbe(true, &used) {
			t.Fatalf("unknown probe %d was rejected before the interval limit", used+1)
		}
	}
	if reserveProbe(true, &used) {
		t.Fatal("unknown probe beyond the interval limit was accepted")
	}
	for range maxNewPerInterval * 2 {
		if !reserveProbe(false, &used) {
			t.Fatal("known peer refresh was throttled by the unknown-peer limit")
		}
	}
}

func TestReceiveOnlyBeaconNeverAnnounces(t *testing.T) {
	for _, tc := range []struct {
		name        string
		receiveOnly bool
		want        int
	}{
		{name: "receive only", receiveOnly: true, want: 0},
		{name: "announcer", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			announcements := 0
			beacon := &Beacon{
				Book: openBook(t, "m-self"), MachineID: "m-self", Name: "self", Port: 80,
				ReceiveOnly: tc.receiveOnly, Interval: time.Hour,
				announceFn: func(*net.UDPAddr) { announcements++ },
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := beacon.Run(ctx); err != nil {
				t.Fatalf("beacon run: %v", err)
			}
			if announcements != tc.want {
				t.Fatalf("announcements = %d, want %d", announcements, tc.want)
			}
		})
	}
}

// lanDaemon serves an identity document on a LAN address rather than loopback,
// because a beacon probes the source address of the packet it heard.
type fakeLANDaemon struct{ port int }

func lanDaemon(t *testing.T, ip net.IP, doc map[string]any) fakeLANDaemon {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Skipf("cannot bind %s: %v", ip, err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return fakeLANDaemon{port: listener.Addr().(*net.TCPAddr).Port}
}

func firstLANIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs := multicastAddrs()
	if len(addrs) == 0 {
		t.Skip("no multicast-capable IPv4 interface here")
	}
	return addrs[0].IP
}
