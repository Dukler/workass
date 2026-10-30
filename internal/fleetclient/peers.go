package fleetclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName is the per-state-dir file holding this daemon's peer links.
//
// It is written owner-only and atomically, like the fleet file beside it, and
// for the same reason: it holds derived device tokens. A token is not the fleet
// key — it opens one peer, not the fleet — but it is still a credential, so it
// lives at 0600 in the daemon's own state and never in argv, a log line or an
// event payload (§9, §11).
const FileName = "peers.json"

// Route kinds, matching the machine book's vocabulary so one address means the
// same thing in both files.
const (
	KindLAN   = "lan"
	KindVPN   = "vpn"
	KindRelay = "relay"
)

// How a route got here. "I typed this" and "this one connected to me" are
// different claims, and §4 ranks them differently: a learned route is appended
// and never outranks a human-entered one until it has a success of its own.
const (
	SourceManual  = "manual"
	SourceLearned = "learned"
)

// RouteCooldown is how long a failed route is left alone. It exists so a dead
// address is not retried in a tight loop while a live one waits behind it
// (§14 — "a dead route retried in a loop").
const RouteCooldown = 2 * time.Minute

// Route is one way to reach a peer, with what happened last time.
type Route struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Source  string `json:"source"`
	// LastSuccessAt is RFC3339; empty means this route has never worked.
	LastSuccessAt string `json:"lastSuccessAt,omitempty"`
	LatencyMS     int    `json:"latencyMs,omitempty"`
	// CooldownUntil is RFC3339; while it is in the future this route is skipped.
	CooldownUntil string `json:"cooldownUntil,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	// Refused marks an address that answered as the WRONG daemon. That is not a
	// retryable failure and therefore not a cooldown: it is a refusal that
	// stands until a human changes the address, because retrying it could only
	// ever mean presenting a credential to an impostor again (§4).
	Refused       bool   `json:"refused,omitempty"`
	RefusedReason string `json:"refusedReason,omitempty"`
}

// Peer is one link, as the dialling side knows it.
type Peer struct {
	MachineID string  `json:"machineId"`
	Name      string  `json:"name,omitempty"`
	Routes    []Route `json:"routes,omitempty"`
	// DeviceToken is derived, never received, and never sent anywhere but the
	// socket's query string on the peer it belongs to.
	DeviceToken string `json:"deviceToken,omitempty"`
	DeviceID    string `json:"deviceId,omitempty"`
	KeyID       string `json:"keyId,omitempty"`
	// CertFingerprint is the peer's TLS identity, pinned at pairing and checked
	// on every dial before any credential is presented.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	PairedAt        string `json:"pairedAt,omitempty"`
	LastUseAt       string `json:"lastUseAt,omitempty"`
	// Grant is the permission ceiling this daemon asks of the peer, in the
	// read/edit/full vocabulary. Empty means full.
	Grant string `json:"grant,omitempty"`
	// RevokedByPeer records that the peer answered "that link was revoked".
	// A revoked link must not be re-enrolled on a timer: the key still matches,
	// so a retry would quietly undo a human's decision (§4).
	RevokedByPeer bool   `json:"revokedByPeer,omitempty"`
	RevokedAt     string `json:"revokedAt,omitempty"`
	// Paired says a credential EXISTS for this peer without carrying it. It is
	// set only by Redacted, and it is why Linked can still answer after the
	// token has been removed: `workass fleet links` read a redacted copy and
	// then asked Linked(), which is DeviceToken != "", so every paired peer
	// printed "known, not linked" while the peer itself showed the link up.
	// The display was the only thing wrong, and this is the field that makes
	// a redacted peer answer honestly.
	Paired bool `json:"-"`
}

// Linked reports whether this peer has a credential to dial with. A redacted
// copy answers from Paired, so a reader that must not see the token still
// reads the same truth.
func (p Peer) Linked() bool { return strings.TrimSpace(p.DeviceToken) != "" || p.Paired }

// OrderedRoutes is the preference order a dial tries. A refused address is left
// out entirely; a cooling one is left out only while something else is worth
// trying first.
//
// That distinction is the difference between a cooldown and a ban. A cooldown
// exists so a dead address is not hammered while a live one waits behind it
// (§14) — so while any route is hot, the cooling ones are skipped. When every
// route is cooling, they are tried anyway, in the same rank order: a cooldown
// that could make a peer permanently unreachable would turn "this address did
// not answer a minute ago" into "this machine is gone", and the thing that
// bounds the loop then is the supervisor's own interval, which is the one timer
// the runtime has. A refusal is different and is a ban: that address proved it
// is a different daemon.
//
// The rank order is the one §4 asks for, in four steps:
//
//  1. a learned route that has never succeeded never precedes a human-entered
//     one — that is the whole "never promoted without a success" rule;
//  2. then the most recent success first, so the address that worked a minute
//     ago is tried before one that worked last week;
//  3. then the lower latency;
//  4. then the order the routes were added, which for manual routes is the
//     order a human typed them in.
func (p Peer) OrderedRoutes(now time.Time) []Route {
	type ranked struct {
		route Route
		index int
	}
	hot := make([]ranked, 0, len(p.Routes))
	cold := make([]ranked, 0, len(p.Routes))
	for i, route := range p.Routes {
		if route.Refused {
			continue
		}
		if route.cooling(now) {
			cold = append(cold, ranked{route: route, index: i})
			continue
		}
		hot = append(hot, ranked{route: route, index: i})
	}
	chosen := hot
	if len(chosen) == 0 {
		chosen = cold
	}
	sort.SliceStable(chosen, func(a, b int) bool {
		left, right := chosen[a], chosen[b]
		if lp, rp := left.route.proven(), right.route.proven(); lp != rp {
			return lp
		}
		leftAt, rightAt := left.route.successAt(), right.route.successAt()
		if !leftAt.Equal(rightAt) {
			return leftAt.After(rightAt)
		}
		if left.route.LatencyMS != right.route.LatencyMS && left.route.LatencyMS > 0 && right.route.LatencyMS > 0 {
			return left.route.LatencyMS < right.route.LatencyMS
		}
		return left.index < right.index
	})
	out := make([]Route, 0, len(chosen))
	for _, entry := range chosen {
		out = append(out, entry.route)
	}
	return out
}

// proven is the rank bucket: a human-entered route is trusted on sight, and a
// learned one earns the same standing by working once.
func (r Route) proven() bool {
	return r.Source != SourceLearned || strings.TrimSpace(r.LastSuccessAt) != ""
}

func (r Route) successAt() time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(r.LastSuccessAt))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func (r Route) cooling(now time.Time) bool {
	until, err := time.Parse(time.RFC3339, strings.TrimSpace(r.CooldownUntil))
	if err != nil {
		return false
	}
	return now.Before(until)
}

// Store is this daemon's set of peer links, persisted to its state dir.
type Store struct {
	path string
	now  func() time.Time

	mu    sync.Mutex
	peers []Peer
	// saves counts completed writes. It exists so a test can assert that a
	// no-op AddRoute wrote NOTHING, which is a claim about the disk that no
	// amount of reading the resulting file back can prove: an identical save
	// and no save leave identical bytes.
	saves int
}

type storeFile struct {
	Version int    `json:"version"`
	Peers   []Peer `json:"peers"`
}

const storeVersion = 1

// Open loads the peer file, creating nothing. A daemon with no peers has no
// file, which is the honest state of a machine nobody has linked yet.
func Open(stateDir string) (*Store, error) {
	store := &Store{path: filepath.Join(stateDir, FileName), now: time.Now}
	data, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		var stored storeFile
		if jsonErr := json.Unmarshal(data, &stored); jsonErr != nil {
			return nil, fmt.Errorf("peer file %s: %w", store.path, jsonErr)
		}
		if stored.Version != storeVersion {
			return nil, fmt.Errorf("peer file %s: unsupported version %d", store.path, stored.Version)
		}
		for _, peer := range stored.Peers {
			if strings.TrimSpace(peer.MachineID) == "" {
				continue
			}
			store.peers = append(store.peers, peer)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	return store, nil
}

// Saves is how many times this store has written the peer file since it was
// opened.
//
// It is the one honest answer to "did that call change anything?". A writer
// that saves an identical record leaves bytes indistinguishable from a writer
// that saved nothing, so reading the file back cannot tell them apart — and
// both the no-op guard in AddRoute and the log line in Mesh.LearnRoute depend
// on telling them apart, now that a route is restated on every heartbeat.
func (s *Store) Saves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// Peers returns every link, credentials included. Callers that publish a list
// to a client use Redacted instead.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Peer(nil), s.peers...)
}

// Peer returns one link by machine id.
func (s *Store) Peer(machineID string) (Peer, bool) {
	machineID = strings.TrimSpace(machineID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, peer := range s.peers {
		if peer.MachineID == machineID {
			return peer, true
		}
	}
	return Peer{}, false
}

// Redacted is the list without credentials, for an event or a CLI listing.
func (s *Store) Redacted() []Peer {
	peers := s.Peers()
	for i := range peers {
		peers[i].Paired = strings.TrimSpace(peers[i].DeviceToken) != ""
		peers[i].DeviceToken = ""
	}
	return peers
}

// AddRoute records a way to reach a peer, creating the peer if it is new.
//
// A route this daemon learned from an inbound connection is appended; it never
// displaces a human-entered one, and re-adding an address that is already there
// only refreshes how it got there when that is an upgrade (learned → manual).
func (s *Store) AddRoute(machineID, name, kind, address, source string) (Peer, error) {
	machineID, address = strings.TrimSpace(machineID), strings.TrimSpace(address)
	if machineID == "" {
		return Peer{}, errors.New("a peer route needs the peer's machine id")
	}
	if address == "" {
		return Peer{}, errors.New("a peer route needs an address")
	}
	if source != SourceManual && source != SourceLearned {
		source = SourceLearned
	}
	if strings.TrimSpace(kind) == "" {
		kind = KindLAN
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(machineID)
	// changed is tracked rather than assumed because this route is now restated
	// on EVERY heartbeat (internal/wire/peer_links.go learns it from the beat),
	// and the overwhelmingly common call is one that changes nothing. Writing
	// an identical file anyway would turn the one timer in the runtime into a
	// disk write per minute per peer — the exact cost beatRecordInterval exists
	// to avoid one layer up.
	changed := false
	if index < 0 {
		s.peers = append(s.peers, Peer{MachineID: machineID, Name: strings.TrimSpace(name)})
		index = len(s.peers) - 1
		changed = true
	} else if trimmed := strings.TrimSpace(name); trimmed != "" && s.peers[index].Name != trimmed {
		s.peers[index].Name = trimmed
		changed = true
	}
	peer := &s.peers[index]
	for i := range peer.Routes {
		if peer.Routes[i].Address != address {
			continue
		}
		if source == SourceManual && peer.Routes[i].Source != SourceManual {
			// A human typing an address this daemon had merely observed is an
			// upgrade in standing, and clears a refusal: they may have fixed
			// exactly the thing that was wrong with it.
			peer.Routes[i].Source = SourceManual
			peer.Routes[i].Refused, peer.Routes[i].RefusedReason = false, ""
			peer.Routes[i].CooldownUntil = ""
			changed = true
		}
		if !changed {
			// This exact route, from this exact source, is already on file. The
			// caller still gets the peer back, so a beat that teaches nothing new
			// is indistinguishable from one that does — except on disk.
			return *peer, nil
		}
		if err := s.saveLocked(); err != nil {
			return Peer{}, err
		}
		return *peer, nil
	}
	peer.Routes = append(peer.Routes, Route{Kind: kind, Address: address, Source: source})
	if err := s.saveLocked(); err != nil {
		return Peer{}, err
	}
	return *peer, nil
}

// SaveLink persists what an enrolment produced. It is called only with a token
// this daemon derived itself and the peer accepted.
func (s *Store) SaveLink(machineID, deviceID, keyID, token, fingerprint, name string) (Peer, error) {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" {
		return Peer{}, errors.New("a peer link needs the peer's machine id")
	}
	if strings.TrimSpace(token) == "" {
		return Peer{}, errors.New("a peer link needs a derived device token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(machineID)
	if index < 0 {
		s.peers = append(s.peers, Peer{MachineID: machineID})
		index = len(s.peers) - 1
	}
	peer := &s.peers[index]
	peer.DeviceToken = strings.TrimSpace(token)
	peer.DeviceID = strings.TrimSpace(deviceID)
	peer.KeyID = strings.TrimSpace(keyID)
	if trimmed := strings.TrimSpace(fingerprint); trimmed != "" {
		peer.CertFingerprint = trimmed
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		peer.Name = trimmed
	}
	peer.PairedAt = s.now().UTC().Format(time.RFC3339)
	peer.RevokedByPeer, peer.RevokedAt = false, ""
	if err := s.saveLocked(); err != nil {
		return Peer{}, err
	}
	return *peer, nil
}

// NoteSuccess records that a route worked, which is what gives a learned route
// its standing and what clears a cooldown.
func (s *Store) NoteSuccess(machineID, address string, latency time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	peer := &s.peers[index]
	now := s.now().UTC().Format(time.RFC3339)
	peer.LastUseAt = now
	for i := range peer.Routes {
		if peer.Routes[i].Address != strings.TrimSpace(address) {
			continue
		}
		peer.Routes[i].LastSuccessAt = now
		peer.Routes[i].CooldownUntil = ""
		peer.Routes[i].LastError = ""
		if latency > 0 {
			peer.Routes[i].LatencyMS = int(latency / time.Millisecond)
		}
	}
	return s.saveLocked()
}

// NoteBeat records that this pair's link answered a heartbeat, at most once per
// `every`. The write is coalesced because the heartbeat is the only timer in
// the runtime and a disk write per minute per peer would make it an expensive
// one; what it buys is that `lastUseAt` means "this link was alive then"
// instead of "this link was paired then", which is what made a live link read
// as a dead one in `workass fleet links`.
func (s *Store) NoteBeat(machineID string, every time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	peer := &s.peers[index]
	now := s.now().UTC()
	if every > 0 && strings.TrimSpace(peer.LastUseAt) != "" {
		if last, err := time.Parse(time.RFC3339, peer.LastUseAt); err == nil && now.Sub(last) < every {
			return nil
		}
	}
	peer.LastUseAt = now.Format(time.RFC3339)
	return s.saveLocked()
}

// NoteFailure cools a route down instead of letting it be retried immediately.
func (s *Store) NoteFailure(machineID, address, reason string, cooldown time.Duration) error {
	if cooldown <= 0 {
		cooldown = RouteCooldown
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	peer := &s.peers[index]
	for i := range peer.Routes {
		if peer.Routes[i].Address != strings.TrimSpace(address) {
			continue
		}
		peer.Routes[i].CooldownUntil = s.now().UTC().Add(cooldown).Format(time.RFC3339)
		peer.Routes[i].LastError = reason
	}
	return s.saveLocked()
}

// RefuseRoute marks an address that answered as a different daemon. The reason
// is kept with the address, because the address IS the finding: a route list is
// the only place that record could usefully be read later.
func (s *Store) RefuseRoute(machineID, address, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	peer := &s.peers[index]
	for i := range peer.Routes {
		if peer.Routes[i].Address != strings.TrimSpace(address) {
			continue
		}
		peer.Routes[i].Refused = true
		peer.Routes[i].RefusedReason = reason
	}
	return s.saveLocked()
}

// MarkRevokedByPeer records the peer's own refusal, so nothing here re-enrols
// on a timer and quietly undoes a human's decision.
func (s *Store) MarkRevokedByPeer(machineID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	peer := &s.peers[index]
	peer.RevokedByPeer = true
	peer.RevokedAt = s.now().UTC().Format(time.RFC3339)
	peer.DeviceToken, peer.DeviceID = "", ""
	return s.saveLocked()
}

// ClearToken drops a credential the peer no longer accepts, so the next dial
// enrols afresh rather than presenting a token that cannot work.
func (s *Store) ClearToken(machineID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(strings.TrimSpace(machineID))
	if index < 0 {
		return nil
	}
	s.peers[index].DeviceToken, s.peers[index].DeviceID = "", ""
	return s.saveLocked()
}

// Forget drops a peer entirely, credential included.
func (s *Store) Forget(machineID string) (bool, error) {
	machineID = strings.TrimSpace(machineID)
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.indexLocked(machineID)
	if index < 0 {
		return false, nil
	}
	s.peers = append(s.peers[:index], s.peers[index+1:]...)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) indexLocked(machineID string) int {
	for i := range s.peers {
		if s.peers[i].MachineID == machineID {
			return i
		}
	}
	return -1
}

// saveLocked writes atomically with owner-only permissions. The file holds
// device tokens, so a world-readable one hands every linked machine to any
// other account on this box.
func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(storeFile{Version: storeVersion, Peers: s.peers}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		return err
	}
	s.saves++
	return nil
}
