// Package fleetclient is the daemon's outbound half: the one capability
// Workass never had.
//
// `internal/wire` only ever ACCEPTED websockets, so the only way one daemon
// could reach another was to ask the Electron renderer to forward the call over
// a socket the renderer owned. The app was the fleet's only bridge, and with it
// closed there was no fleet (docs/specs/fleet-runtime.md §1). This package is
// the other half of that socket: a daemon dials a peer itself, over TLS 1.3,
// authenticated by the same derived device token every other client uses.
//
// Four rules shape it, all from §4, and each one is a failure this design
// exists to remove:
//
//   - Identity before credentials. The address is read first — the identity
//     card at /workass/health must answer as the expected machine id, and its
//     certificate must match the fingerprint pinned at pairing. A wrong id is a
//     refusal, logged with the address, and never a retry: a retry would mean
//     presenting a credential to an impostor a second time.
//   - Ordered routes with cooldowns. A peer has a LIST of addresses in
//     preference order; a failed one cools down instead of being hammered, and
//     a route merely observed never outranks one a human typed until it has
//     worked.
//   - One link per pair, and the heartbeat is the only timer in the runtime.
//     Link state is connecting | up | down(since), published locally so
//     everything above it reads one vocabulary.
//   - Enrolment needs no human. A daemon holding the fleet key proves it over
//     nonces and DERIVES the device token both sides compute; nothing reusable
//     crosses the wire in either direction.
//
// What it deliberately does not hold is Workass semantics. A link carries the
// command surface that already exists — a channel name and its args — which is
// why a new tool reaches the whole fleet the day it exists locally (§6).
package fleetclient

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"workass/internal/fleet"
	"workass/internal/machinebook"
)

// HeartbeatInterval is the one timer in the runtime. §16's ten-minute silence
// threshold is what it is derived from, so this is not a second knob.
const HeartbeatInterval = time.Minute

// beatRecordInterval is how often a live link's heartbeat is written down. It
// is deliberately much longer than the beat itself: what a reader needs is
// "this link was alive within the last few minutes", not a timestamp accurate
// to the second bought with a disk write per minute per peer.
const beatRecordInterval = 5 * time.Minute

// DialTimeout bounds one dial attempt, identity probe included.
const DialTimeout = 20 * time.Second

// InvokeTimeout bounds one forwarded command. It matches the renderer relay's
// own ceiling so a tool does not behave differently depending on which
// transport carried it.
const InvokeTimeout = 2 * time.Minute

// Link states, as published.
const (
	StatusConnecting = "connecting"
	StatusUp         = "up"
	StatusDown       = "down"
)

// ErrWrongIdentity reports an address that answered as a different daemon. It
// is a refusal, not a failure to retry: no credential was sent and none will be.
var ErrWrongIdentity = errors.New("that address is not the daemon it claims to be")

// ErrLinkRevoked reports a peer that has cut this link off. Re-enrolling would
// undo a human's decision, so nothing here retries it.
var ErrLinkRevoked = errors.New("that peer revoked this link")

// ErrNoRoute reports a peer with no address left to try.
var ErrNoRoute = errors.New("no usable route to that peer")

// ErrRefusedCredential reports a peer that would not accept the credential this
// daemon presented. It is not the route's fault, which is why it never cools a
// route down: the address answered, proved its identity, and then said no.
var ErrRefusedCredential = errors.New("peer refused this daemon's device token")

// ErrNoKey reports a daemon that cannot enrol because it holds no fleet key.
var ErrNoKey = errors.New("this daemon holds no fleet key, so it cannot link to a peer")

// ErrNotSent wraps every failure that happened BEFORE a command left this
// machine — no route, a refused identity, a revoked link, a dial that never
// completed. The distinction is the whole basis on which a caller may safely
// fall back to another transport: a command that was never sent cannot have run
// at the home, so sending it again over the relay runs it exactly once. A
// failure after the send is NOT this error, and is never retried on another
// transport.
var ErrNotSent = errors.New("the command never left this machine")

// State is one pair's link state, which is a property of the pair and not of
// either socket.
type State struct {
	Peer    string    `json:"peer"`
	Name    string    `json:"name,omitempty"`
	Status  string    `json:"status"`
	Since   time.Time `json:"since"`
	Address string    `json:"address,omitempty"`
	Reason  string    `json:"reason,omitempty"`
}

// keyHolder is the sliver of the fleet store enrolment needs. Taking an
// interface keeps a mesh testable without a real key file and keeps the secret
// out of everything that merely wants to route.
type keyHolder interface {
	Keys() []fleet.Key
	Has() bool
}

// Options configures a mesh.
type Options struct {
	SelfMachineID string
	SelfName      string
	// SelfPort is the port THIS daemon listens on, stated to a peer during
	// enrolment so the receiving side can learn a route back.
	//
	// Without it a pair was duplex on one socket and one-directional on the
	// record: the dialler kept a peer entry, and the receiver — which only ever
	// learns an address from an inbound connection — kept none, so it could not
	// dial back, could not heartbeat the pair itself, and `workass fleet links`
	// there truthfully reported "this machine has dialled no peer" for a
	// machine it was linked to. The peer never names the HOST (the receiver
	// uses the socket's own ip, internal/wire/peer_links.go), so a peer cannot
	// point this at anybody else.
	SelfPort      int
	Store         *Store
	Keys          keyHolder
	Logf          func(string, ...any)
	Now           func() time.Time
	Heartbeat     time.Duration
	DialTimeout   time.Duration
	InvokeTimeout time.Duration
	// OnEvent receives every event a peer publishes on a link. P3's
	// subscriptions and monitors arrive here; P2 leaves it unset in production,
	// so events are read off the socket and dropped rather than queued.
	OnEvent func(peerID, channel string, payload any)
	// OnState publishes link state locally, which is how everything above the
	// mesh reads connecting | up | down without owning a timer of its own.
	OnState func(State)
	// OnBeat is called at the end of EVERY supervisor tick with the pair's
	// current state, where OnState fires only when the state changes.
	//
	// P3's `home_down` needs this and OnState cannot give it: the threshold is a
	// duration (§16.3a, ten minutes), so it has to be RE-READ as time passes,
	// and a pair that has been down for an hour publishes no state change at
	// all. Rather than let the watcher own a second timer — the heartbeat is the
	// only timer in the runtime (§4) — the one timer that exists reports every
	// beat and the watcher decides.
	OnBeat func(State)
	// InboundUp reports whether this daemon already holds a LIVE socket that the
	// peer itself dialled in on.
	//
	// §4 gives each pair ONE duplex link and lets either side dial it. Without
	// this predicate the outbound half cannot see the inbound socket, so a pair
	// the peer dialled had two problems at once: this side published `down` for
	// a link that was demonstrably carrying commands — and `down` past the
	// threshold is what synthesizes home_down, so a reachable machine would be
	// reported unknown — and the supervisor dialled back anyway, giving the pair
	// two sockets where the design has one. The lower-machine-id interval only
	// de-races two diallers; it does not stop both of them succeeding.
	//
	// It is injectable for the same reason Dial and Probe are, and production
	// backs it with the same test Hub.SendToPeer routes pushes on, so "where a
	// push would go" and "is this pair up" can never disagree.
	InboundUp func(peerID string) bool
	// Probe reads an address's identity card. Injectable so a test can serve a
	// card without a TLS listener; production uses machinebook, which is the one
	// reading of that document.
	Probe func(ctx context.Context, address string) (machinebook.Card, error)
	// Dial opens the socket. Injectable for the same reason.
	Dial func(ctx context.Context, opts DialRequest) (Invoker, error)
}

// DialRequest is what a transport needs to open one link.
type DialRequest struct {
	Peer        string
	Address     string
	DeviceName  string
	DeviceToken string
	Fingerprint string
	OnEvent     func(peerID, channel string, payload any)
	Logf        func(string, ...any)
}

// Invoker is one open link, as the mesh uses it.
type Invoker interface {
	Invoke(ctx context.Context, channel string, args ...any) (any, error)
	Close(cause error)
	Done() <-chan struct{}
	Address() string
	// AccessState is the peer's own verdict on this socket, as it stated it on
	// `lan:access-state`: the state and, when refused, the reason. It is what
	// tells a stale credential from a revoked link without guessing.
	AccessState() (string, string)
}

// Mesh is this daemon's set of peer links.
type Mesh struct {
	selfID   string
	selfName string
	selfPort int
	store    *Store
	keys     keyHolder
	logf     func(string, ...any)
	now      func() time.Time

	heartbeat     time.Duration
	dialTimeout   time.Duration
	invokeTimeout time.Duration
	onEvent       func(peerID, channel string, payload any)
	onState       func(State)
	onBeat        func(State)
	inboundUp     func(peerID string) bool
	probe         func(ctx context.Context, address string) (machinebook.Card, error)
	dial          func(ctx context.Context, opts DialRequest) (Invoker, error)

	mu     sync.Mutex
	links  map[string]Invoker
	states map[string]State
	// downSince is when each pair last stopped being up. It is kept apart from
	// states because `down(since)` must mean "silent since then", not "since the
	// most recent failed retry" — otherwise every retry resets the clock §16's
	// ten-minute threshold is read from, and a machine that has been dark for a
	// day reports as newly down.
	downSince map[string]time.Time
	dialing   map[string]*dialAttempt
	supervise map[string]struct{}
	closed    bool
	// lifetime is the context new supervisors inherit, set once by Start. A peer
	// dialled on demand has to keep its state true afterwards, and the goroutine
	// that does so must die with the daemon rather than with the tool call that
	// happened to open the link.
	lifetime context.Context
	// stopped is closed by Close. Without it, shutdown would wait for every
	// supervisor's NEXT tick to notice, which is up to a whole heartbeat — a
	// minute of a daemon refusing to exit, for nothing.
	stopped chan struct{}

	run sync.WaitGroup
}

// dialAttempt is the single-flight record that makes concurrent demand for one
// peer converge on one link instead of a socket per caller. Two tools asking
// for the same machine at the same instant is the ordinary case, not the
// exception.
type dialAttempt struct {
	done chan struct{}
	link Invoker
	err  error
}

// New builds a mesh. It opens no socket: a link exists because something asked
// for one or because the supervisor's heartbeat found the peer down.
func New(opts Options) *Mesh {
	mesh := &Mesh{
		selfID:        strings.TrimSpace(opts.SelfMachineID),
		selfName:      strings.TrimSpace(opts.SelfName),
		selfPort:      opts.SelfPort,
		store:         opts.Store,
		keys:          opts.Keys,
		logf:          opts.Logf,
		now:           opts.Now,
		heartbeat:     opts.Heartbeat,
		dialTimeout:   opts.DialTimeout,
		invokeTimeout: opts.InvokeTimeout,
		onEvent:       opts.OnEvent,
		onState:       opts.OnState,
		onBeat:        opts.OnBeat,
		inboundUp:     opts.InboundUp,
		probe:         opts.Probe,
		dial:          opts.Dial,
		links:         make(map[string]Invoker),
		states:        make(map[string]State),
		dialing:       make(map[string]*dialAttempt),
		supervise:     make(map[string]struct{}),
		downSince:     make(map[string]time.Time),
		stopped:       make(chan struct{}),
	}
	if mesh.logf == nil {
		mesh.logf = func(string, ...any) {}
	}
	if mesh.now == nil {
		mesh.now = time.Now
	}
	if mesh.heartbeat <= 0 {
		mesh.heartbeat = HeartbeatInterval
	}
	if mesh.dialTimeout <= 0 {
		mesh.dialTimeout = DialTimeout
	}
	if mesh.invokeTimeout <= 0 {
		mesh.invokeTimeout = InvokeTimeout
	}
	if mesh.probe == nil {
		client := &http.Client{Timeout: machinebook.DefaultProbeTimeout, Transport: machinebook.NewProbeTransport()}
		mesh.probe = func(ctx context.Context, address string) (machinebook.Card, error) {
			return machinebook.ProbeAddress(ctx, address, client)
		}
	}
	if mesh.dial == nil {
		mesh.dial = defaultDial
	}
	return mesh
}

// SelfMachineID is the machine this mesh dials out from.
func (m *Mesh) SelfMachineID() string { return m.selfID }

// Linked reports whether this peer has a pairing record with a credential.
func (m *Mesh) Linked(peerID string) bool {
	if m == nil || m.store == nil {
		return false
	}
	peer, ok := m.store.Peer(strings.TrimSpace(peerID))
	return ok && peer.Linked() && !peer.RevokedByPeer
}

// Known reports whether this peer is in the peer file at all, linked or not.
func (m *Mesh) Known(peerID string) bool {
	if m == nil || m.store == nil {
		return false
	}
	_, ok := m.store.Peer(strings.TrimSpace(peerID))
	return ok
}

// States is every pair's current link state.
func (m *Mesh) States() []State {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	for _, state := range m.states {
		out = append(out, state)
	}
	return out
}

// State is one pair's link state. A pair nothing has dialled yet is `down`
// since the zero time, which is the honest answer: not connected, not tried.
func (m *Mesh) State(peerID string) State {
	peerID = strings.TrimSpace(peerID)
	m.mu.Lock()
	defer m.mu.Unlock()
	if state, ok := m.states[peerID]; ok {
		return state
	}
	return State{Peer: peerID, Status: StatusDown}
}

// Invoke forwards one channel call to a peer over its link, opening the link on
// demand. This is the whole of §6's routing for a non-self home: the local API
// and the fleet API differ in one `if`, and this is the other branch.
func (m *Mesh) Invoke(ctx context.Context, peerID, channel string, args ...any) (any, error) {
	if m == nil {
		return nil, errors.New("this daemon has no fleet mesh")
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return nil, errors.New("a forwarded command needs the home machine id")
	}
	if peerID == m.selfID {
		return nil, errors.New("a mesh invoke must not address this machine")
	}
	link, err := m.EnsureLink(ctx, peerID)
	if err != nil {
		// Both sentinels stay readable: the caller asks "may I retry elsewhere?"
		// and, separately, "was this a refusal or a dead route?".
		return nil, fmt.Errorf("%w: %w", ErrNotSent, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, m.invokeTimeout)
	defer cancel()
	result, err := link.Invoke(callCtx, channel, args...)
	if err == nil {
		return result, nil
	}
	if code := errorCode(err); code == "lan:access-rejected" || code == "fleet:peer-revoked" {
		m.handleRejection(peerID, code, err)
	}
	return nil, err
}

// EnsureLink returns a live link to one peer, dialling if there is none.
func (m *Mesh) EnsureLink(ctx context.Context, peerID string) (Invoker, error) {
	peerID = strings.TrimSpace(peerID)
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("this daemon's fleet mesh is shutting down")
		}
		if link := m.links[peerID]; link != nil {
			select {
			case <-link.Done():
				delete(m.links, peerID)
			default:
				m.mu.Unlock()
				return link, nil
			}
		}
		attempt := m.dialing[peerID]
		if attempt == nil {
			attempt = &dialAttempt{done: make(chan struct{})}
			m.dialing[peerID] = attempt
			m.mu.Unlock()
			m.runDial(ctx, peerID, attempt)
			return attempt.link, attempt.err
		}
		m.mu.Unlock()
		// Another caller is already dialling this peer. Waiting is what makes a
		// dial race converge on one link rather than on one socket per caller.
		select {
		case <-attempt.done:
			if attempt.err != nil {
				return nil, attempt.err
			}
			if attempt.link != nil {
				select {
				case <-attempt.link.Done():
					continue
				default:
					return attempt.link, nil
				}
			}
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (m *Mesh) runDial(ctx context.Context, peerID string, attempt *dialAttempt) {
	defer func() {
		m.mu.Lock()
		delete(m.dialing, peerID)
		if attempt.err == nil && attempt.link != nil {
			m.links[peerID] = attempt.link
		}
		m.mu.Unlock()
		close(attempt.done)
	}()
	link, err := m.dialPeer(ctx, peerID)
	attempt.link, attempt.err = link, err
}

// dialPeer walks the route list in order, proving identity before it presents
// anything, and enrolling when it has no credential yet.
func (m *Mesh) dialPeer(ctx context.Context, peerID string) (Invoker, error) {
	if m.store == nil {
		return nil, errors.New("this daemon has no peer store")
	}
	peer, ok := m.store.Peer(peerID)
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a known peer", ErrNoRoute, peerID)
	}
	if peer.RevokedByPeer {
		m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusDown, Since: m.now(), Reason: ErrLinkRevoked.Error()})
		return nil, fmt.Errorf("%w: %s", ErrLinkRevoked, peerID)
	}
	routes := peer.OrderedRoutes(m.now())
	if len(routes) == 0 {
		m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusDown, Since: m.now(), Reason: ErrNoRoute.Error()})
		return nil, fmt.Errorf("%w: %s (every route is cooling down, refused, or absent)", ErrNoRoute, peerID)
	}
	m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusConnecting, Since: m.now()})
	var lastErr error
	for _, route := range routes {
		link, err := m.dialRoute(ctx, peer, route)
		if err == nil {
			m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusUp, Since: m.now(), Address: route.Address})
			m.superviseLink(peerID)
			return link, nil
		}
		lastErr = err
		if errors.Is(err, ErrLinkRevoked) {
			m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusDown, Since: m.now(), Reason: err.Error()})
			return nil, err
		}
		// A refusal is recorded against the address, because the address is the
		// finding: it is the only thing a human can change about it.
		if errors.Is(err, ErrWrongIdentity) {
			m.logf("[fleet] refused %s: %v", route.Address, err)
			_ = m.store.RefuseRoute(peerID, route.Address, err.Error())
			continue
		}
		if errors.Is(err, ErrRefusedCredential) || errors.Is(err, ErrNoKey) {
			// The address is fine; the answer was no. Cooling it down would only
			// delay the next honest attempt.
			continue
		}
		_ = m.store.NoteFailure(peerID, route.Address, err.Error(), RouteCooldown)
		// Re-read: a failure just changed the cooldowns, and the next route must
		// be chosen from the list as it is now.
		if refreshed, ok := m.store.Peer(peerID); ok {
			peer = refreshed
		}
	}
	reason := ErrNoRoute.Error()
	if lastErr != nil {
		reason = lastErr.Error()
	}
	m.publish(State{Peer: peerID, Name: peer.Name, Status: StatusDown, Since: m.now(), Reason: reason})
	if lastErr != nil {
		return nil, fmt.Errorf("link to %s failed on every route: %w", peerID, lastErr)
	}
	return nil, fmt.Errorf("%w: %s", ErrNoRoute, peerID)
}

// dialRoute is the ordering that matters: probe, compare, and only then dial
// with a credential.
func (m *Mesh) dialRoute(ctx context.Context, peer Peer, route Route) (Invoker, error) {
	dialCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
	defer cancel()
	started := m.now()
	card, err := m.probe(dialCtx, route.Address)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(card.MachineID) != peer.MachineID {
		return nil, fmt.Errorf("%w: %s answers as %q, not %q", ErrWrongIdentity, route.Address,
			strings.TrimSpace(card.MachineID), peer.MachineID)
	}
	if pinned := strings.TrimSpace(peer.CertFingerprint); pinned != "" && !strings.EqualFold(pinned, strings.TrimSpace(card.CertFingerprint)) {
		return nil, fmt.Errorf("%w: %s presents certificate %s, not the pinned %s", ErrWrongIdentity,
			route.Address, shortFingerprint(card.CertFingerprint), shortFingerprint(pinned))
	}
	token := strings.TrimSpace(peer.DeviceToken)
	presented := token != ""
	fingerprint := firstNonEmpty(strings.TrimSpace(peer.CertFingerprint), strings.TrimSpace(card.CertFingerprint))
	if token == "" {
		// No credential yet. Enrolment happens on its own socket, so the only
		// thing a parked connection is ever used for is becoming unparked.
		enrolled, err := m.enroll(dialCtx, peer, route, card)
		if err != nil {
			return nil, err
		}
		peer, token = enrolled, enrolled.DeviceToken
		fingerprint = firstNonEmpty(strings.TrimSpace(enrolled.CertFingerprint), fingerprint)
	}
	link, err := m.dial(dialCtx, DialRequest{
		Peer: peer.MachineID, Address: route.Address, DeviceName: m.linkName(),
		DeviceToken: token, Fingerprint: fingerprint, OnEvent: m.onEvent, Logf: m.logf,
	})
	if err != nil {
		return nil, err
	}
	// A token the peer no longer honours is reported as rejected access, not as
	// a closed socket. Clearing it here is what lets the next dial enrol afresh
	// after the peer reinstalled — while a REVOKED link, which answers with its
	// own code, is left alone.
	if _, err := link.Invoke(dialCtx, heartbeatChannel, m.peerBeatPayload()); err != nil {
		state, reason := link.AccessState()
		link.Close(err)
		code := errorCode(err)
		// A peer refuses a credential in two different ways and they must not be
		// confused. "invalid-token" is a stale credential — the peer reinstalled,
		// or its device file was reset — and the honest answer is to enrol again.
		// A revoked link answers with its own code, and the honest answer there
		// is to stop: the key still matches, so a retry would undo a decision.
		// The hub states which on the socket itself (`lan:access-state`), so this
		// is read from the peer's own signal rather than inferred from a timeout.
		if code == "fleet:peer-revoked" {
			m.handleRejection(peer.MachineID, code, err)
			return nil, fmt.Errorf("%w: %s", ErrLinkRevoked, peer.MachineID)
		}
		if code == "lan:access-rejected" || state == "rejected" {
			refusal := fmt.Errorf("%w: %s said %s", ErrRefusedCredential, peer.MachineID, firstNonEmpty(reason, "rejected"))
			m.handleRejection(peer.MachineID, firstNonEmpty(code, "lan:access-rejected"), refusal)
			// A refused credential is the moment to find out WHY, and only
			// enrolment can tell us: a reinstalled peer accepts a fresh
			// enrolment, and a revoked one answers `fleet:peer-revoked`. Guessing
			// between those two is exactly what must not happen, so ask —
			// once, on this same route, with no credential in hand.
			if presented {
				return m.dialRoute(ctx, m.refreshedPeer(peer), route)
			}
			return nil, refusal
		}
		return nil, err
	}
	_ = m.store.NoteSuccess(peer.MachineID, route.Address, m.now().Sub(started))
	return link, nil
}

// heartbeatChannel is the cheapest authenticated round trip there is: it proves
// the socket still carries an approved identity and nothing more.
const heartbeatChannel = "fleet:heartbeat"

// enroll is the key-holder ceremony with no human in it (§4, layer 1).
func (m *Mesh) enroll(ctx context.Context, peer Peer, route Route, card machinebook.Card) (Peer, error) {
	if m.keys == nil || !m.keys.Has() {
		return Peer{}, ErrNoKey
	}
	link, err := m.dial(ctx, DialRequest{
		Peer: peer.MachineID, Address: route.Address, DeviceName: m.linkName(),
		Fingerprint: strings.TrimSpace(card.CertFingerprint), Logf: m.logf,
	})
	if err != nil {
		return Peer{}, err
	}
	defer link.Close(nil)

	raw, err := link.Invoke(ctx, "fleet:challenge")
	if err != nil {
		return Peer{}, fmt.Errorf("ask %s for a fleet challenge: %w", route.Address, err)
	}
	challenge := mapOf(raw)
	if enabled, _ := challenge["enabled"].(bool); !enabled {
		return Peer{}, fmt.Errorf("%s holds no fleet key yet, so it cannot accept a link", peer.MachineID)
	}
	serverNonce := textOf(challenge["serverNonce"])
	challengeMachine := textOf(challenge["machineId"])
	if serverNonce == "" || challengeMachine == "" {
		return Peer{}, fmt.Errorf("%s answered a fleet challenge with no nonce or machine id", route.Address)
	}
	// The socket must be the same daemon the identity card named. Probing one
	// machine and enrolling with another is exactly the substitution the probe
	// exists to prevent, so the check is repeated where the credential is used.
	if challengeMachine != peer.MachineID {
		return Peer{}, fmt.Errorf("%w: %s probed as %q but challenges as %q", ErrWrongIdentity,
			route.Address, peer.MachineID, challengeMachine)
	}
	clientNonce, err := fleet.NewNonce()
	if err != nil {
		return Peer{}, err
	}
	fingerprint := strings.TrimSpace(card.CertFingerprint)
	proofs := mapOf(challenge["certProofs"])
	for _, key := range m.keys.Keys() {
		// The certificate is verified under the fleet key before the proof is
		// sent, so trust-on-first-use never applies to a key-holder: a swapped
		// certificate has a different fingerprint and forging the proof needs
		// the key.
		if fingerprint != "" && len(proofs) > 0 {
			expected := fleet.CertProof(key.Secret, serverNonce, fingerprint)
			if claimed := textOf(proofs[key.KeyID]); claimed != "" && claimed != expected {
				return Peer{}, fmt.Errorf("%w: %s proved certificate %s under key %s incorrectly",
					ErrWrongIdentity, route.Address, shortFingerprint(fingerprint), key.KeyID)
			}
		}
		proof := fleet.Proof(key.Secret, serverNonce, clientNonce, challengeMachine)
		token := fleet.Token(key.Secret, serverNonce, clientNonce, challengeMachine)
		reply, err := link.Invoke(ctx, "fleet:enroll", map[string]any{
			"clientNonce": clientNonce,
			"proof":       proof,
			"name":        m.linkName(),
			// What marks this as a peer link rather than a browser: the receiving
			// side records one revocable pairing record per machine id (§4).
			// The port is this daemon's own, and it is a CLAIM: the receiver
			// joins it to the host this socket actually came from, so a route
			// back is learned without anybody typing an address, and the pair
			// is heartbeat by both ends rather than one.
			"peer": m.peerEnrolmentPayload(),
		})
		if err != nil {
			if errorCode(err) == "fleet:peer-revoked" {
				_ = m.store.MarkRevokedByPeer(peer.MachineID, err.Error())
				return Peer{}, fmt.Errorf("%w: %s", ErrLinkRevoked, peer.MachineID)
			}
			if errorCode(err) == "fleet:rejected" {
				// Wrong fleet for THIS key; another key this daemon holds may
				// still be the right one. One failed proof consumes the
				// challenge by design, so the next attempt needs a fresh nonce
				// from each side.
				retry, challengeErr := link.Invoke(ctx, "fleet:challenge")
				if challengeErr != nil {
					return Peer{}, err
				}
				if refreshed := textOf(mapOf(retry)["serverNonce"]); refreshed != "" {
					serverNonce = refreshed
				}
				if next, nonceErr := fleet.NewNonce(); nonceErr == nil {
					clientNonce = next
				}
				continue
			}
			return Peer{}, err
		}
		answer := mapOf(reply)
		saved, saveErr := m.store.SaveLink(peer.MachineID, textOf(answer["deviceId"]),
			firstNonEmpty(textOf(answer["keyId"]), key.KeyID), token, fingerprint, card.DisplayName())
		if saveErr != nil {
			return Peer{}, saveErr
		}
		m.logf("[fleet] linked to %s (%s) under key %s", peer.MachineID, route.Address, key.KeyID)
		return saved, nil
	}
	return Peer{}, fmt.Errorf("%s refused every fleet key this daemon holds", peer.MachineID)
}

// handleRejection separates the two refusals a peer can make. One says "that
// credential is stale"; the other says "a human cut you off". Treating them
// alike would either strand a reinstalled peer forever or re-pair a revoked one
// on the next tick.
func (m *Mesh) handleRejection(peerID, code string, cause error) {
	if m.store == nil {
		return
	}
	if code == "fleet:peer-revoked" {
		_ = m.store.MarkRevokedByPeer(peerID, cause.Error())
		m.logf("[fleet] %s revoked this link; not retrying", peerID)
	} else {
		_ = m.store.ClearToken(peerID)
		m.logf("[fleet] %s refused this daemon's token; it will enrol again on the next dial", peerID)
	}
	m.mu.Lock()
	link := m.links[peerID]
	delete(m.links, peerID)
	m.mu.Unlock()
	if link != nil {
		link.Close(cause)
	}
	m.publish(State{Peer: peerID, Status: StatusDown, Since: m.now(), Reason: cause.Error()})
}

// LearnRoute records an address a peer connected from, appended below every
// human-entered route until it has worked once (§4).
//
// It reports whether the peer file actually gained something. The caller needs
// that because this is now reached from every heartbeat and not only from an
// enrolment: a log line per beat per peer would bury the one event an operator
// is reading these logs for, which is the beat that taught a NEW address.
func (m *Mesh) LearnRoute(peerID, name, address string) bool {
	if m == nil || m.store == nil || strings.TrimSpace(address) == "" {
		return false
	}
	before := m.store.Saves()
	if _, err := m.store.AddRoute(peerID, name, KindLAN, address, SourceLearned); err != nil {
		m.logf("[fleet] learn route %s for %s: %v", address, peerID, err)
		return false
	}
	return m.store.Saves() != before
}

// AddRoute records an address for a peer. Manual is a human's claim about where
// a machine is and outranks anything observed.
func (m *Mesh) AddRoute(peerID, name, kind, address, source string) error {
	if m == nil || m.store == nil {
		return errors.New("this daemon has no peer store")
	}
	_, err := m.store.AddRoute(peerID, name, kind, address, source)
	return err
}

// Drop closes and forgets one peer link on this side. It is the dialling half
// of revocation: the receiving half is lease.RevokePeerLink.
func (m *Mesh) Drop(peerID string) {
	if m == nil {
		return
	}
	peerID = strings.TrimSpace(peerID)
	m.mu.Lock()
	link := m.links[peerID]
	delete(m.links, peerID)
	delete(m.supervise, peerID)
	m.mu.Unlock()
	if link != nil {
		link.Close(errors.New("link dropped on this machine"))
	}
	if m.store != nil {
		if _, err := m.store.Forget(peerID); err != nil {
			m.logf("[fleet] forget peer %s: %v", peerID, err)
		}
	}
	m.publish(State{Peer: peerID, Status: StatusDown, Since: m.now(), Reason: "dropped here"})
}

// Close ends every link and joins every supervisor.
func (m *Mesh) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		m.run.Wait()
		return
	}
	m.closed = true
	close(m.stopped)
	links := make([]Invoker, 0, len(m.links))
	for peerID, link := range m.links {
		links = append(links, link)
		delete(m.links, peerID)
	}
	m.mu.Unlock()
	for _, link := range links {
		link.Close(errors.New("daemon shutting down"))
	}
	m.run.Wait()
}

// Supervise keeps a peer's link state true for as long as ctx lives: it
// heartbeats a live link and redials a dead one. This is the only timer in the
// runtime, and one peer has exactly one of it.
func (m *Mesh) Supervise(ctx context.Context, peerID string) {
	peerID = strings.TrimSpace(peerID)
	if m == nil || peerID == "" || peerID == m.selfID {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	if _, running := m.supervise[peerID]; running {
		m.mu.Unlock()
		return
	}
	m.supervise[peerID] = struct{}{}
	m.run.Add(1)
	m.mu.Unlock()
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.supervise, peerID)
			m.mu.Unlock()
			m.run.Done()
		}()
		m.superviseLoop(ctx, peerID)
	}()
}

// superviseInterval is the heartbeat, with the asymmetry §4 asks for: the lower
// machine id retries on a shorter interval, so two daemons that both want the
// link do not race each other forever.
func (m *Mesh) superviseInterval(peerID string) time.Duration {
	if m.selfID != "" && peerID != "" && m.selfID > peerID {
		return m.heartbeat * 2
	}
	return m.heartbeat
}

func (m *Mesh) superviseLoop(ctx context.Context, peerID string) {
	ticker := time.NewTicker(m.superviseInterval(peerID))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopped:
			return
		case <-ticker.C:
		}
		m.mu.Lock()
		closed := m.closed
		link := m.links[peerID]
		m.mu.Unlock()
		if closed {
			return
		}
		if link != nil {
			callCtx, cancel := context.WithTimeout(ctx, m.heartbeat)
			_, err := link.Invoke(callCtx, heartbeatChannel, m.peerBeatPayload())
			cancel()
			if err == nil {
				// Coalesced on purpose: a beat answered is worth recording, but
				// not at the price of a disk write per minute per peer.
				if m.store != nil {
					_ = m.store.NoteBeat(peerID, beatRecordInterval)
				}
				m.beat(peerID)
				continue
			}
			// A heartbeat that does not come back is the pair going down. It is
			// not an outcome for anything the peer was running: §7's home_down
			// says unknown, and P3 is what turns this state into that notice.
			link.Close(err)
			m.mu.Lock()
			if m.links[peerID] == link {
				delete(m.links, peerID)
			}
			m.mu.Unlock()
			m.publish(State{Peer: peerID, Status: StatusDown, Since: m.now(), Reason: err.Error()})
			if code := errorCode(err); code == "lan:access-rejected" || code == "fleet:peer-revoked" {
				m.handleRejection(peerID, code, err)
				continue
			}
		}
		if m.inboundLive(peerID) {
			// The peer dialled us and that socket is live, so the pair already
			// has the one duplex link §4 gives it. Dialling back would open a
			// second — the lower-machine-id interval only de-races two diallers,
			// it does not stop them both succeeding — and would then heartbeat a
			// connection the pair does not need. Publishing up is the whole job
			// here: it keeps the pair's state true, which is what home_down is
			// read from, without a second connection. Since is left zero so
			// publish keeps the moment the pair came up rather than re-stamping
			// it once a minute.
			m.publish(State{Peer: peerID, Status: StatusUp})
			m.beat(peerID)
			continue
		}
		if !m.Linked(peerID) && !m.Known(peerID) {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
		if _, err := m.EnsureLink(dialCtx, peerID); err != nil && !errors.Is(err, context.Canceled) {
			m.logf("[fleet] link to %s is down: %v", peerID, err)
		}
		cancel()
		m.beat(peerID)
	}
}

// beat reports the pair's current state at the end of one tick. It is how a
// duration threshold is re-read without a second timer anywhere.
func (m *Mesh) beat(peerID string) {
	m.mu.Lock()
	beat := m.onBeat
	m.mu.Unlock()
	if beat == nil {
		return
	}
	beat(m.State(peerID))
}

// Start begins supervising every peer already in the peer file and fixes the
// lifetime that later supervisors inherit. A daemon with no peers starts
// nothing, which is the point: there is no polling here, only one heartbeat per
// pair that exists.
func (m *Mesh) Start(ctx context.Context) {
	if m == nil || ctx == nil {
		return
	}
	m.mu.Lock()
	m.lifetime = ctx
	m.mu.Unlock()
	if m.store == nil {
		return
	}
	for _, peer := range m.store.Peers() {
		if peer.RevokedByPeer {
			continue
		}
		m.Supervise(ctx, peer.MachineID)
	}
}

// superviseLink starts the pair's supervisor the first time a link comes up, so
// a peer dialled on demand keeps its state true afterwards without the caller
// having to arrange it.
func (m *Mesh) superviseLink(peerID string) {
	m.mu.Lock()
	_, running := m.supervise[peerID]
	closed, lifetime := m.closed, m.lifetime
	m.mu.Unlock()
	if running || closed || lifetime == nil {
		return
	}
	m.Supervise(lifetime, peerID)
}

func (m *Mesh) publish(state State) {
	// Link state is a property of the PAIR, not of this side's socket (§4), and
	// either side may have dialled it. When the peer dialled, this daemon's
	// outbound half holds nothing and would publish `down` for a link that is
	// carrying commands right now — and `down` past the threshold is what
	// synthesizes home_down, so a reachable machine would be announced as
	// unknown and its running work shown as last-known. The inbound socket is
	// the same evidence a push is routed on, so it is read rather than guessed.
	//
	// What this deliberately does NOT claim is that an outbound invoke will
	// work: a pair that is up only inbound still has no route out, and that
	// failure is reported where it happens, as ErrNoRoute, wrapped in
	// ErrNotSent so the caller may safely retry over the renderer relay.
	if state.Status != StatusUp && m.inboundLive(state.Peer) {
		state.Status = StatusUp
		state.Reason = ""
		// The caller's timestamp described the failure it was reporting, which
		// is not when this pair came up. Clearing it hands the question to the
		// branch below, which keeps the moment the pair ACTUALLY came up.
		state.Since = time.Time{}
	}
	m.mu.Lock()
	previous, existed := m.states[state.Peer]
	if state.Status == StatusUp {
		delete(m.downSince, state.Peer)
		if state.Since.IsZero() {
			// An up link that is re-stated keeps the moment it came up. `up
			// since` that moves while nothing changes is both a lie and a state
			// CHANGE, so a pair re-stated every beat would notify every beat.
			if existed && previous.Status == StatusUp && !previous.Since.IsZero() {
				state.Since = previous.Since
			} else {
				state.Since = m.now()
			}
		}
	} else {
		// Connecting is not a recovery: a pair that is dialling is still not up,
		// so a down → connecting → down cycle keeps one timestamp.
		since, ok := m.downSince[state.Peer]
		if !ok {
			since = state.Since
			if since.IsZero() {
				since = m.now()
			}
			m.downSince[state.Peer] = since
		}
		state.Since = since
	}
	if existed && previous.Name != "" && state.Name == "" {
		state.Name = previous.Name
	}
	m.states[state.Peer] = state
	notify := m.onState
	m.mu.Unlock()
	if notify != nil && (!existed || previous != state) {
		notify(state)
	}
}

// peerEnrolmentPayload is how this daemon names itself to a peer it is
// enrolling with. Nothing secret is in it, and nothing in it names a host:
// the address a peer records is the one its own socket reports.
func (m *Mesh) peerEnrolmentPayload() map[string]any {
	payload := map[string]any{"machineId": m.selfID, "name": m.selfName}
	if m.selfPort > 0 {
		payload["port"] = strconv.Itoa(m.selfPort)
	}
	return payload
}

// peerBeatPayload is what this daemon states on every heartbeat: who it is, and
// the port it listens on.
//
// The port is here and not only in the enrolment because enrolment happens ONCE
// in a pair's lifetime — dialRoute enrols only when it holds no token — so a
// pair that linked before this daemon stated a port could never learn the route
// back, and no amount of reconnecting would fix it: the receiver only ever
// learns an address from an inbound connection, and the only inbound thing it
// sees after pairing is this beat. Restating it on every beat is what makes the
// reverse direction self-heal with no re-enrolment and no human, which is the
// only way an already-paired peer gets a route home.
//
// It stays a CLAIM about the port and nothing else: the receiver joins it to
// the host its own socket reports (internal/wire/peer_links.go), so a peer
// cannot point a route at anybody but itself.
func (m *Mesh) peerBeatPayload() map[string]any {
	payload := map[string]any{"machineId": m.selfID}
	if m.selfPort > 0 {
		payload["port"] = strconv.Itoa(m.selfPort)
	}
	return payload
}

// inboundLive asks whether the peer has a live socket of its own here. It is
// read WITHOUT m.mu held: the predicate reaches into the hub, and holding the
// mesh lock across another subsystem's lock is how a deadlock gets written.
func (m *Mesh) inboundLive(peerID string) bool {
	if m == nil || m.inboundUp == nil || strings.TrimSpace(peerID) == "" {
		return false
	}
	return m.inboundUp(peerID)
}

func (m *Mesh) linkName() string {
	name := strings.TrimSpace(m.selfName)
	id := strings.TrimSpace(m.selfID)
	switch {
	case name != "" && id != "":
		return "workass daemon " + name + " (" + id + ")"
	case id != "":
		return "workass daemon " + id
	default:
		return "workass daemon"
	}
}

func defaultDial(ctx context.Context, opts DialRequest) (Invoker, error) {
	return dialLink(ctx, dialOptions{
		address: opts.Address, peerID: opts.Peer, deviceName: opts.DeviceName,
		deviceToken: opts.DeviceToken, tlsConfig: pinnedTLSConfig(opts.Fingerprint),
		onEvent: opts.OnEvent, logf: opts.Logf,
	})
}

// pinnedTLSConfig trusts exactly one certificate: the one the identity card
// named. Workass daemons serve self-signed per-machine certificates, so there
// is no authority to verify against — pinning the fingerprint is the verification.
func pinnedTLSConfig(fingerprint string) *tls.Config {
	fingerprint = strings.ToLower(strings.TrimSpace(fingerprint))
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// #nosec G402 -- a daemon's certificate is self-signed per machine; the
		// pinned SHA-256 below is the identity check, and it is stricter than a
		// public authority would be.
		InsecureSkipVerify: true,
	}
	if fingerprint == "" {
		return config
	}
	config.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		for _, raw := range rawCerts {
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) == fingerprint {
				return nil
			}
		}
		return fmt.Errorf("%w: the certificate presented is not the pinned %s", ErrWrongIdentity, shortFingerprint(fingerprint))
	}
	return config
}

// errorCode reads the structured code a hub refusal carries. Hub errors are
// JSON documents in an error string (`structuredError`, wire.go), which is the
// shape the renderer already reads; a plain error simply has no code.
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	open := strings.Index(text, "{")
	if open < 0 || !strings.HasSuffix(text, "}") {
		return ""
	}
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(text[open:]), &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Code)
}

func mapOf(value any) map[string]any {
	out, _ := value.(map[string]any)
	return out
}

func textOf(value any) string {
	out, _ := value.(string)
	return strings.TrimSpace(out)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func shortFingerprint(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 16 {
		return value
	}
	return value[:16] + "…"
}

// PeerIDs is every peer in the peer file, linked or not. A peer with a record
// and no link yet is still a peer: the next command addressed to it dials.
func (m *Mesh) PeerIDs() []string {
	if m == nil || m.store == nil {
		return nil
	}
	peers := m.store.Peers()
	out := make([]string, 0, len(peers))
	for _, peer := range peers {
		if peer.RevokedByPeer {
			continue
		}
		out = append(out, peer.MachineID)
	}
	return out
}

// refreshedPeer re-reads one peer, so a retry inside a single dial sees the
// credential state the previous attempt just changed.
func (m *Mesh) refreshedPeer(peer Peer) Peer {
	if m.store == nil {
		return peer
	}
	if current, ok := m.store.Peer(peer.MachineID); ok {
		return current
	}
	return peer
}
