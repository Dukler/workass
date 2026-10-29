package fleetclient

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/fleet"
	"workass/internal/httpserve"
	"workass/internal/lease"
	"workass/internal/machinebook"
	"workass/internal/tlscert"
	"workass/internal/wire"
)

// peerDaemon is one receiving daemon: the real hub, the real device store, the
// real fleet key store and a real TLS listener serving the real identity card.
//
// Nothing here is a stand-in for the production path. The point of P2 is that a
// daemon can dial a copy of itself, so a test that mocked either end would be
// testing the mock. The one thing it deliberately does NOT have is a renderer —
// that absence is the phase's whole operator-visible claim.
type peerDaemon struct {
	machineID   string
	address     string
	lease       *lease.Manager
	keys        *fleet.Store
	hub         *wire.Hub
	fingerprint string
	stateDir    string

	listener net.Listener
	server   *http.Server
	echoes   atomic.Int64

	connMu sync.Mutex
	conns  []net.Conn
}

// severing is why the harness tracks accepted connections. http.Server.Shutdown
// closes the listener and idle connections, but a websocket has been HIJACKED —
// it is no longer the server's to close — so a home that "stopped" would keep
// answering heartbeats forever and the link would never go down.
type trackingListener struct {
	net.Listener
	daemon *peerDaemon
}

func (l *trackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.daemon.connMu.Lock()
	l.daemon.conns = append(l.daemon.conns, conn)
	l.daemon.connMu.Unlock()
	return conn, nil
}

func newPeerDaemon(t *testing.T, machineID, secret string) *peerDaemon {
	t.Helper()
	stateDir := t.TempDir()
	certificate, err := tlscert.Ensure(stateDir)
	if err != nil {
		t.Fatalf("daemon %s certificate: %v", machineID, err)
	}
	leases, err := lease.NewManager(lease.Options{StateDir: stateDir, Logf: t.Logf})
	if err != nil {
		t.Fatalf("daemon %s device store: %v", machineID, err)
	}
	keys, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatalf("daemon %s fleet store: %v", machineID, err)
	}
	if strings.TrimSpace(secret) != "" {
		if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
			t.Fatalf("daemon %s join fleet: %v", machineID, err)
		}
	}
	// TrustLocalhost is off on purpose: a loopback client that auto-pairs never
	// performs the fleet ceremony, and the ceremony is what is under test.
	hub := wire.NewHub(wire.Options{Lease: leases, TrustLocalhost: false, Logf: t.Logf})
	hub.SetMachineID(machineID)
	hub.SetFleet(keys, machineID)
	hub.SetTLSFingerprint(certificate.Fingerprint)

	daemon := &peerDaemon{
		machineID: machineID, lease: leases, keys: keys, hub: hub,
		fingerprint: certificate.Fingerprint, stateDir: stateDir,
	}
	hub.Register("test:echo", func(args []any) (any, error) {
		daemon.echoes.Add(1)
		return map[string]any{"ok": true, "machineId": machineID, "args": args}, nil
	})

	server := httpserve.New("", hub, nil)
	server.Version = "test"
	server.Name = machineID
	server.Identity = func() map[string]any {
		return map[string]any{
			"machineId": machineID, "displayName": machineID, "name": machineID,
			"wireVersion": 1, "secure": true, "certFingerprint": certificate.Fingerprint,
			"fleetIds": keys.KeyIDs(),
		}
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("daemon %s listen: %v", machineID, err)
	}
	daemon.listener = &trackingListener{Listener: tls.NewListener(raw, &tls.Config{
		Certificates: []tls.Certificate{certificate.TLS}, MinVersion: tls.VersionTLS13,
	}), daemon: daemon}
	daemon.address = raw.Addr().String()
	daemon.server = &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = daemon.server.Serve(daemon.listener) }()
	t.Cleanup(daemon.stop)
	return daemon
}

func (d *peerDaemon) stop() {
	if d.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = d.server.Shutdown(ctx)
	}
	d.connMu.Lock()
	conns := d.conns
	d.conns = nil
	d.connMu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// restart serves the same state directory on the same address again, which is
// what a daemon coming back from a restart or a self-update looks like to a peer.
func (d *peerDaemon) restart(t *testing.T) {
	t.Helper()
	raw, err := net.Listen("tcp", d.address)
	if err != nil {
		t.Fatalf("daemon %s relisten: %v", d.machineID, err)
	}
	certificate, err := tlscert.Ensure(d.stateDir)
	if err != nil {
		t.Fatalf("daemon %s certificate: %v", d.machineID, err)
	}
	d.listener = &trackingListener{Listener: tls.NewListener(raw, &tls.Config{
		Certificates: []tls.Certificate{certificate.TLS}, MinVersion: tls.VersionTLS13,
	}), daemon: d}
	server := httpserve.New("", d.hub, nil)
	server.Version = "test"
	server.Name = d.machineID
	server.Identity = func() map[string]any {
		return map[string]any{
			"machineId": d.machineID, "displayName": d.machineID, "name": d.machineID,
			"wireVersion": 1, "secure": true, "certFingerprint": certificate.Fingerprint,
			"fleetIds": d.keys.KeyIDs(),
		}
	}
	d.server = &http.Server{Handler: server, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = d.server.Serve(d.listener) }()
	t.Cleanup(d.stop)
}

// peerLinks is the receiving side's list, which is what §4 asks to be visible.
func (d *peerDaemon) peerLinks() []lease.Device { return d.lease.PeerLinks() }

// dialer is one daemon's outbound half, with nothing mocked but the clock.
func newDialer(t *testing.T, machineID, secret string, opts ...func(*Options)) (*Mesh, *Store) {
	t.Helper()
	stateDir := t.TempDir()
	store, err := Open(stateDir)
	if err != nil {
		t.Fatalf("dialer %s peer store: %v", machineID, err)
	}
	keys, err := fleet.Open(stateDir)
	if err != nil {
		t.Fatalf("dialer %s fleet store: %v", machineID, err)
	}
	if strings.TrimSpace(secret) != "" {
		if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
			t.Fatalf("dialer %s join fleet: %v", machineID, err)
		}
	}
	options := Options{
		SelfMachineID: machineID, SelfName: machineID, Store: store, Keys: keys,
		Logf: t.Logf, Heartbeat: 25 * time.Millisecond, DialTimeout: 10 * time.Second,
		InvokeTimeout: 10 * time.Second,
	}
	for _, apply := range opts {
		apply(&options)
	}
	mesh := New(options)
	t.Cleanup(mesh.Close)
	return mesh, store
}

// A key-holder links itself. No pairing code, no approval, no human: the whole
// reason one key defines "my machines" (§4, layer 1).
func TestKeyHolderEnrolsWithNoHuman(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	mesh, store := newDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}

	result, err := mesh.Invoke(context.Background(), home.machineID, "test:echo", map[string]any{"hello": "fleet"})
	if err != nil {
		t.Fatalf("invoke over a fresh link: %v", err)
	}
	if answer, _ := result.(map[string]any); answer == nil || answer["machineId"] != home.machineID {
		t.Fatalf("the home answered %#v", result)
	}

	links := home.peerLinks()
	if len(links) != 1 {
		t.Fatalf("the home recorded %d peer links, want exactly one: %#v", len(links), links)
	}
	if links[0].Peer != "m-away" || links[0].Kind != lease.KindPeer {
		t.Fatalf("peer link record = %#v", links[0])
	}
	if links[0].KeyID == "" {
		t.Fatalf("the link does not record which fleet key let it in: %#v", links[0])
	}
	peer, ok := store.Peer(home.machineID)
	if !ok || !peer.Linked() {
		t.Fatalf("the dialler kept no credential: %#v", peer)
	}
	if peer.CertFingerprint != home.fingerprint {
		t.Fatalf("the dialler pinned %q, the home serves %q", peer.CertFingerprint, home.fingerprint)
	}
	if state := mesh.State(home.machineID); state.Status != StatusUp {
		t.Fatalf("link state = %#v, want up", state)
	}
}

// Identity before credentials. The address answers as a different daemon, so
// the dial is refused and nothing is presented to it — the impostor records no
// device and sees no enrolment attempt at all.
func TestWrongIdentityIsRefusedBeforeAnyCredentialIsSent(t *testing.T) {
	secret := newTestSecret(t)
	impostor := newPeerDaemon(t, "m-impostor", secret)
	mesh, store := newDialer(t, "m-away", secret)
	// The route for m-home points at the impostor, which is exactly what a
	// hijacked DNS name, a reused address or a swapped machine looks like.
	if err := mesh.AddRoute("m-home", "home", KindLAN, impostor.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}

	_, err := mesh.Invoke(context.Background(), "m-home", "test:echo", map[string]any{})
	if !errors.Is(err, ErrWrongIdentity) {
		t.Fatalf("dial to an impostor err = %v, want a wrong-identity refusal", err)
	}
	if devices := impostor.lease.Devices(); len(devices) != 0 {
		t.Fatalf("the impostor was given %d device(s): %#v", len(devices), devices)
	}
	if peer, _ := store.Peer("m-home"); peer.Linked() {
		t.Fatalf("a credential was derived for an impostor: %#v", peer)
	}
	// A refusal is not a cooldown. The route stays refused until a human changes
	// it, because retrying it could only mean presenting a credential again.
	peer, _ := store.Peer("m-home")
	if len(peer.Routes) != 1 || !peer.Routes[0].Refused {
		t.Fatalf("the refused address was not recorded: %#v", peer.Routes)
	}
	if !strings.Contains(peer.Routes[0].RefusedReason, "m-impostor") {
		t.Fatalf("the refusal does not name who answered: %q", peer.Routes[0].RefusedReason)
	}
	if _, err := mesh.Invoke(context.Background(), "m-home", "test:echo", map[string]any{}); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("a refused route was tried again: %v", err)
	}
}

// A certificate that is not the pinned one is the same refusal, even when the
// machine id matches: identity is the pair.
func TestPinnedCertificateMismatchIsRefused(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	mesh, store := newDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := store.SaveLink(home.machineID, "device-x", "key-x", "not-the-derived-token",
		"0000000000000000000000000000000000000000000000000000000000000000", "home"); err != nil {
		t.Fatalf("seed a pinned certificate: %v", err)
	}
	if _, err := mesh.Invoke(context.Background(), home.machineID, "test:echo"); !errors.Is(err, ErrWrongIdentity) {
		t.Fatalf("a swapped certificate was accepted: %v", err)
	}
	if home.echoes.Load() != 0 {
		t.Fatalf("a command reached a daemon whose certificate was not the pinned one")
	}
}

// A forged or expired credential buys nothing. Membership is what admits a
// daemon, so a key-holder presenting a stale token is refused and then enrols
// afresh in the same dial — one link, one record, under a token it derived —
// and a daemon with no key is simply refused.
func TestAForgedTokenIsRefusedAndOnlyMembershipAdmits(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	mesh, store := newDialer(t, "m-away", secret)
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	const forged = "deadbeefdeadbeef"
	if _, err := store.SaveLink(home.machineID, "device-x", "key-x", forged, home.fingerprint, "home"); err != nil {
		t.Fatalf("seed a forged token: %v", err)
	}

	if _, err := mesh.Invoke(context.Background(), home.machineID, "test:echo"); err != nil {
		t.Fatalf("a key-holder could not recover from a stale token: %v", err)
	}
	peer, _ := store.Peer(home.machineID)
	if peer.DeviceToken == forged {
		t.Fatal("the forged token was accepted as it stood")
	}
	if peer.RevokedByPeer {
		t.Fatal("a stale token was mistaken for a revoked link")
	}
	// The recovery replaced the credential rather than accumulating a second
	// identity for one machine.
	if links := home.peerLinks(); len(links) != 1 || links[0].Peer != "m-away" {
		t.Fatalf("the home holds %#v, want exactly one link for m-away", links)
	}

	// The same forged token, held by a daemon that is not in the fleet, gets
	// nowhere: there is no key to enrol with.
	stranger, strangerStore := newDialer(t, "m-stranger", "")
	if err := stranger.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := strangerStore.SaveLink(home.machineID, "device-y", "key-y", forged, home.fingerprint, "home"); err != nil {
		t.Fatalf("seed a forged token: %v", err)
	}
	if _, err := stranger.Invoke(context.Background(), home.machineID, "test:echo"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a forged token admitted a daemon with no fleet key: %v", err)
	}
	if links := home.peerLinks(); len(links) != 1 {
		t.Fatalf("the home holds %d links after a stranger tried a forged token", len(links))
	}
}

// A daemon that holds no key cannot link, and says so rather than parking.
func TestADaemonWithNoFleetKeyCannotLink(t *testing.T) {
	home := newPeerDaemon(t, "m-home", newTestSecret(t))
	mesh, _ := newDialer(t, "m-away", "")
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := mesh.Invoke(context.Background(), home.machineID, "test:echo"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("a keyless daemon linked anyway: %v", err)
	}
}

// A key for a different fleet is refused, and the refusal does not leave a
// usable challenge behind.
func TestAKeyFromAnotherFleetIsRefused(t *testing.T) {
	home := newPeerDaemon(t, "m-home", newTestSecret(t))
	mesh, store := newDialer(t, "m-away", newTestSecret(t))
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	if _, err := mesh.Invoke(context.Background(), home.machineID, "test:echo"); err == nil {
		t.Fatal("a stranger's key linked to this fleet")
	}
	if peer, _ := store.Peer(home.machineID); peer.Linked() {
		t.Fatalf("a credential survived a refused enrolment: %#v", peer)
	}
	if links := home.peerLinks(); len(links) != 0 {
		t.Fatalf("the home recorded %d link(s) for a stranger", len(links))
	}
}

// Revoking one link cuts exactly one peer. The other link keeps working, the
// fleet key is untouched, and the cut peer stays cut rather than re-enrolling
// itself on the next heartbeat under the very key that was left in place.
func TestRevokingOneLinkCutsExactlyOnePeer(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	first, firstStore := newDialer(t, "m-first", secret)
	second, _ := newDialer(t, "m-second", secret)
	for _, dialer := range []*Mesh{first, second} {
		if err := dialer.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
			t.Fatalf("add route: %v", err)
		}
		if _, err := dialer.Invoke(context.Background(), home.machineID, "test:echo"); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	if links := home.peerLinks(); len(links) != 2 {
		t.Fatalf("the home holds %d links, want two", len(links))
	}
	keysBefore := strings.Join(home.keys.KeyIDs(), ",")

	found, err := home.hub.RevokePeerLink("m-first", "test")
	if err != nil || !found {
		t.Fatalf("revoke one link: found=%v err=%v", found, err)
	}
	links := home.peerLinks()
	if len(links) != 1 || links[0].Peer != "m-second" {
		t.Fatalf("revoking one link left %#v", links)
	}
	if strings.Join(home.keys.KeyIDs(), ",") != keysBefore {
		t.Fatalf("revoking a link changed the fleet key set")
	}
	if !home.lease.PeerBlocked("m-first") || home.lease.PeerBlocked("m-second") {
		t.Fatalf("blocked peers = %#v", home.lease.BlockedPeers())
	}

	// The untouched link still works.
	if _, err := second.Invoke(context.Background(), home.machineID, "test:echo"); err != nil {
		t.Fatalf("the other link was cut too: %v", err)
	}
	// The cut one is refused by name, and stops trying. Its token is not cleared
	// by the test: the revocation itself fenced the socket, so the next dial
	// presents the very credential the peer no longer honours — which is the
	// case that must end in a refusal rather than a silent re-pairing.
	_, err = first.Invoke(context.Background(), home.machineID, "test:echo")
	if !errors.Is(err, ErrLinkRevoked) {
		t.Fatalf("a revoked peer re-enrolled: %v", err)
	}
	if peer, _ := firstStore.Peer(home.machineID); !peer.RevokedByPeer {
		t.Fatalf("the revocation was not recorded on the dialling side: %#v", peer)
	}
	// Re-approving one link is as narrow as cutting it was.
	if lifted, err := home.lease.AllowPeer("m-first"); err != nil || !lifted {
		t.Fatalf("re-approve one link: lifted=%v err=%v", lifted, err)
	}
	if err := firstStore.MarkRevokedByPeer("m-home", ""); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := firstStore.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("re-add route: %v", err)
	}
	revived, _ := newDialerFor(t, "m-first", secret, firstStore)
	if _, err := revived.Invoke(context.Background(), home.machineID, "test:echo"); err == nil {
		t.Fatal("a peer the dialler still believes is revoked dialled anyway")
	}
}

// Routes are tried in order, a failed one cools down instead of spinning, and a
// learned route never outranks a human-entered one until it has worked.
func TestRoutesAreTriedInOrderAndAFailureCoolsDown(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	dead := deadAddress(t)
	var probed []string
	var probeMu sync.Mutex
	mesh, store := newDialer(t, "m-away", secret, func(opts *Options) {
		inner := opts.Probe
		opts.Probe = func(ctx context.Context, address string) (machinebook.Card, error) {
			probeMu.Lock()
			probed = append(probed, address)
			probeMu.Unlock()
			if inner != nil {
				return inner(ctx, address)
			}
			client := &http.Client{Timeout: time.Second, Transport: machinebook.NewProbeTransport()}
			return machinebook.ProbeAddress(ctx, address, client)
		}
	})
	// A human typed the dead one first. It is tried first precisely because a
	// human typed it, and the live one behind it still gets used.
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, dead, SourceManual); err != nil {
		t.Fatalf("add dead route: %v", err)
	}
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add live route: %v", err)
	}
	if _, err := mesh.Invoke(context.Background(), home.machineID, "test:echo"); err != nil {
		t.Fatalf("invoke across an ordered route list: %v", err)
	}
	probeMu.Lock()
	order := append([]string(nil), probed...)
	probeMu.Unlock()
	if len(order) != 2 || order[0] != dead || order[1] != home.address {
		t.Fatalf("routes were not tried in order: %#v", order)
	}
	peer, _ := store.Peer(home.machineID)
	if peer.Routes[0].CooldownUntil == "" || peer.Routes[0].LastError == "" {
		t.Fatalf("a failed route did not cool down: %#v", peer.Routes[0])
	}
	if peer.Routes[1].LastSuccessAt == "" {
		t.Fatalf("a working route recorded no success: %#v", peer.Routes[1])
	}

	// The next dial skips the cooling route entirely rather than hammering it.
	live := peer.OrderedRoutes(time.Now())
	if len(live) != 1 || live[0].Address != home.address {
		t.Fatalf("a cooling route is still in the dial order: %#v", live)
	}
	if later := peer.OrderedRoutes(time.Now().Add(RouteCooldown + time.Second)); len(later) != 2 {
		t.Fatalf("a cooldown never expires: %#v", later)
	}
}

func TestALearnedRouteNeverOutranksAHumanEnteredOneWithoutASuccess(t *testing.T) {
	peer := Peer{MachineID: "m-home", Routes: []Route{
		{Kind: KindLAN, Address: "typed:8788", Source: SourceManual},
		{Kind: KindLAN, Address: "observed:8788", Source: SourceLearned},
	}}
	order := peer.OrderedRoutes(time.Now())
	if len(order) != 2 || order[0].Address != "typed:8788" {
		t.Fatalf("a learned route outranked a typed one with no success: %#v", order)
	}
	// One success is what earns it the same standing, and recency decides from
	// there — not provenance.
	peer.Routes[1].LastSuccessAt = time.Now().UTC().Format(time.RFC3339)
	order = peer.OrderedRoutes(time.Now())
	if order[0].Address != "observed:8788" {
		t.Fatalf("a proven learned route never gets promoted: %#v", order)
	}
}

// Concurrent demand for one peer converges on one link rather than a socket per
// caller, which is the dial race that actually happens.
func TestConcurrentDemandConvergesOnOneLink(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	var dials atomic.Int64
	mesh, _ := newDialer(t, "m-away", secret, func(opts *Options) {
		opts.Dial = func(ctx context.Context, request DialRequest) (Invoker, error) {
			if request.DeviceToken != "" {
				dials.Add(1)
			}
			return defaultDial(ctx, request)
		}
	})
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	var wait sync.WaitGroup
	links := make([]Invoker, 8)
	errs := make([]error, 8)
	for i := range links {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			links[index], errs[index] = mesh.EnsureLink(context.Background(), home.machineID)
		}(i)
	}
	wait.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureLink %d: %v", i, err)
		}
		if links[i] != links[0] {
			t.Fatalf("caller %d got a second link to one peer", i)
		}
	}
	if opened := dials.Load(); opened != 1 {
		t.Fatalf("%d authenticated sockets were opened for one pair, want one", opened)
	}
}

// The heartbeat is the only timer, and it is what makes link state true: a home
// that goes away is `down(since)`, and one that comes back is `up` again,
// without anything polling.
func TestHeartbeatTakesTheLinkDownAndBackUp(t *testing.T) {
	secret := newTestSecret(t)
	home := newPeerDaemon(t, "m-home", secret)
	states := make(chan State, 32)
	mesh, _ := newDialer(t, "m-away", secret, func(opts *Options) {
		opts.OnState = func(state State) { states <- state }
	})
	if err := mesh.AddRoute(home.machineID, "home", KindLAN, home.address, SourceManual); err != nil {
		t.Fatalf("add route: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mesh.Start(ctx)
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err != nil {
		t.Fatalf("link: %v", err)
	}
	mesh.Supervise(ctx, home.machineID)
	home.stop()
	if state := waitForStatus(t, states, StatusDown); state.Since.IsZero() {
		t.Fatalf("down state carries no since: %#v", state)
	}
	downAt := mesh.State(home.machineID).Since
	// down(since) is since it WENT down, not since the last failed retry.
	time.Sleep(60 * time.Millisecond)
	if again := mesh.State(home.machineID); again.Status == StatusDown && !again.Since.Equal(downAt) {
		t.Fatalf("retries moved the since stamp from %v to %v", downAt, again.Since)
	}
	home.restart(t)
	waitForStatus(t, states, StatusUp)
	if _, err := mesh.Invoke(ctx, home.machineID, "test:echo"); err != nil {
		t.Fatalf("invoke after the link came back: %v", err)
	}
}

func waitForStatus(t *testing.T, states <-chan State, want string) State {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case state := <-states:
			if state.Status == want {
				return state
			}
		case <-deadline:
			t.Fatalf("link never reached %q", want)
		}
	}
}

// newDialerFor builds a mesh over an existing peer store, for the half of a
// test that has to survive the dialler restarting.
func newDialerFor(t *testing.T, machineID, secret string, store *Store) (*Mesh, *Store) {
	t.Helper()
	keys, err := fleet.Open(t.TempDir())
	if err != nil {
		t.Fatalf("fleet store: %v", err)
	}
	if _, err := keys.Join(secret, fleet.LocalOwner, "test"); err != nil {
		t.Fatalf("join fleet: %v", err)
	}
	mesh := New(Options{
		SelfMachineID: machineID, SelfName: machineID, Store: store, Keys: keys, Logf: t.Logf,
		Heartbeat: 25 * time.Millisecond, DialTimeout: 5 * time.Second, InvokeTimeout: 5 * time.Second,
	})
	t.Cleanup(mesh.Close)
	return mesh, store
}

func newTestSecret(t *testing.T) string {
	t.Helper()
	secret, err := fleet.NewSecret()
	if err != nil {
		t.Fatalf("mint a fleet key: %v", err)
	}
	return secret
}

// deadAddress reserves a port and releases it, so the address is well-formed
// and nothing is listening on it.
func deadAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a dead port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the dead port: %v", err)
	}
	return address
}
