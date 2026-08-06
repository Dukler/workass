// Package machinebook is the list of machines this daemon knows about: what
// they are called, where they can be reached, and whether they answered the
// last time anyone asked.
//
// Two shapes here are deliberate and are the reason the package exists rather
// than a map of addresses:
//
//   - A machine has *endpoints*, plural. Reachability belongs to the endpoint,
//     not to the machine, so the same machine reached over a second network
//     later is a second endpoint on an entry that already exists — not a
//     duplicate, and not a new mechanism.
//   - Every entry carries an owner. Today it is one constant nobody reads. The
//     day a second person exists, that changes what fills the field rather than
//     the schema and every query above it.
package machinebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FileName is the per-state-dir file holding the book.
const FileName = "machines.json"

// LocalOwner is the single-owner placeholder. Every entry is written with it
// until there is a second person to distinguish.
const LocalOwner = "local"

// Endpoint kinds. Only KindLAN is produced today; the others exist so that
// adding a transport is a new value here rather than a new field everywhere.
const (
	KindLAN   = "lan"
	KindVPN   = "vpn"
	KindRelay = "relay"
)

// Entry statuses, as of the last probe.
const (
	StatusOK          = "ok"
	StatusUnreachable = "unreachable"
	StatusNeedsUpdate = "needs-update"
)

// How an entry got into the book. Shown to the user, because "I typed this one"
// and "this one announced itself" are different answers to why a machine is in
// a list they did not write.
const (
	SourceManual = "manual"
	SourceBeacon = "beacon"
)

// DefaultPort is assumed when an address names a host and no port.
const DefaultPort = "8788"

// Endpoint is one way to reach a machine.
type Endpoint struct {
	Kind    string `json:"kind"`
	Address string `json:"address"`
}

// Entry is one machine, as this daemon last understood it.
type Entry struct {
	MachineID   string     `json:"machineId"`
	Name        string     `json:"name"`
	Owner       string     `json:"owner"`
	AddedBy     string     `json:"addedBy,omitempty"`
	Endpoints   []Endpoint `json:"endpoints"`
	Version     string     `json:"version,omitempty"`
	WireVersion int        `json:"wireVersion,omitempty"`
	Secure      bool       `json:"secure"`
	// CertFingerprint is the public SHA-256 identity of this daemon's TLS
	// certificate. It is never a credential; it lets clients surface a changed
	// peer identity rather than silently downgrading transport.
	CertFingerprint string `json:"certFingerprint,omitempty"`
	// FleetIDs names which fleets that machine will accept an enrolment for. It
	// is how a client tells "one of mine, enrol silently" from "someone else's
	// machine on this network"; the ids are hashes, never the key.
	FleetIDs   []string `json:"fleetIds,omitempty"`
	Status     string   `json:"status"`
	Reason     string   `json:"reason,omitempty"`
	AddedAt    string   `json:"addedAt"`
	LastSeenAt string   `json:"lastSeenAt,omitempty"`
}

// ErrSelf reports an attempt to add the machine doing the adding.
var ErrSelf = errors.New("that address is this machine")

// Options configures a book. SelfID and WireVersion come from the daemon's own
// identity so the book can refuse itself and name a version gap.
type Options struct {
	StateDir    string
	SelfID      string
	WireVersion int
	Owner       string
	HTTPClient  *http.Client
	Now         func() time.Time
}

// Book is a daemon's machine list, persisted to its state dir.
type Book struct {
	path        string
	selfID      string
	wireVersion int
	owner       string
	client      *http.Client
	now         func() time.Time

	mu      sync.Mutex
	entries map[string]Entry
}

// Open loads the book for a state dir, creating an empty one on first use.
func Open(opts Options) (*Book, error) {
	owner := strings.TrimSpace(opts.Owner)
	if owner == "" {
		owner = LocalOwner
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Second}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	book := &Book{
		path:        filepath.Join(opts.StateDir, FileName),
		selfID:      strings.TrimSpace(opts.SelfID),
		wireVersion: opts.WireVersion,
		owner:       owner,
		client:      client,
		now:         now,
		entries:     map[string]Entry{},
	}
	data, err := os.ReadFile(book.path)
	switch {
	case err == nil:
		var stored []Entry
		if jsonErr := json.Unmarshal(data, &stored); jsonErr != nil {
			return nil, fmt.Errorf("machine book %s: %w", book.path, jsonErr)
		}
		for _, entry := range stored {
			if strings.TrimSpace(entry.MachineID) == "" {
				continue
			}
			if strings.TrimSpace(entry.Owner) == "" {
				entry.Owner = owner
			}
			book.entries[entry.MachineID] = entry
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	return book, nil
}

// List returns the book ordered by display name, so two callers rendering it
// agree without sorting.
func (b *Book) List() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Entry, 0, len(b.entries))
	for _, entry := range b.entries {
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].MachineID < out[j].MachineID
	})
	return out
}

// Add probes an address and records what answered. Probing a machine already
// in the book adds the endpoint to that entry rather than creating a second
// one — the same machine on a second network is one machine.
//
// An address that never answers is an error rather than an unreachable entry:
// the book is keyed by machine id, and a machine that has not spoken has not
// told us its id. There is nothing truthful to file it under.
func (b *Book) Add(ctx context.Context, address string) (Entry, error) {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return Entry{}, err
	}
	card, probeErr := b.probe(ctx, normalized)
	if probeErr != nil {
		return Entry{}, probeErr
	}
	if b.selfID != "" && card.MachineID == b.selfID {
		return Entry{}, ErrSelf
	}
	return b.record(card, normalized, SourceManual)
}

// Sighted records a beacon announcement from machineID at address.
//
// It reaches for the network only when the announcement says something the book
// does not already know — a machine never seen, one that moved, or one we had
// given up on. A machine that is simply still there costs one UDP packet and no
// connection at all, which is what keeps an idle network idle.
//
// Reports whether anything changed, so a caller can broadcast on change instead
// of on every packet.
func (b *Book) Sighted(ctx context.Context, machineID, address string) (Entry, bool, error) {
	machineID = strings.TrimSpace(machineID)
	if machineID == "" {
		return Entry{}, false, errors.New("announcement carries no machine id")
	}
	if b.selfID != "" && machineID == b.selfID {
		return Entry{}, false, ErrSelf
	}
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return Entry{}, false, err
	}

	b.mu.Lock()
	entry, known := b.entries[machineID]
	unchanged := known && entry.Status == StatusOK &&
		hasEndpoint(entry.Endpoints, Endpoint{Kind: KindLAN, Address: normalized})
	if unchanged {
		// Liveness only. Not persisted: a disk write per announcement per
		// machine is a lot of writing to record that nothing happened.
		entry.LastSeenAt = b.now().UTC().Format(time.RFC3339)
		b.entries[machineID] = entry
		b.mu.Unlock()
		return entry, false, nil
	}
	b.mu.Unlock()

	// The id in the packet decided whether to look; the id on the card decides
	// what gets written. One arrived unauthenticated over UDP, the other came
	// back from a connection we opened ourselves to the address we probed.
	card, probeErr := b.probe(ctx, normalized)
	if probeErr != nil {
		return Entry{}, false, probeErr
	}
	if b.selfID != "" && card.MachineID == b.selfID {
		return Entry{}, false, ErrSelf
	}
	recorded, err := b.record(card, normalized, SourceBeacon)
	if err != nil {
		return Entry{}, false, err
	}
	return recorded, true, nil
}

// record merges a probed card into the book and persists it.
func (b *Book) record(card Card, address, source string) (Entry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	stamp := b.now().UTC().Format(time.RFC3339)
	entry, existing := b.entries[card.MachineID]
	if !existing {
		entry = Entry{MachineID: card.MachineID, Owner: b.owner, AddedBy: source, AddedAt: stamp}
	}
	entry.Name = card.DisplayName()
	entry.Version = card.Version
	entry.WireVersion = card.WireVersion
	entry.Secure = card.Secure
	entry.CertFingerprint = card.CertFingerprint
	entry.FleetIDs = card.FleetIDs
	entry.LastSeenAt = stamp
	entry.Endpoints = mergeEndpoint(entry.Endpoints, Endpoint{Kind: KindLAN, Address: address})
	entry.Status, entry.Reason = b.assess(card)
	b.entries[card.MachineID] = entry
	if err := b.save(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// Forget drops a machine. Reports whether it was there.
func (b *Book) Forget(machineID string) (bool, error) {
	machineID = strings.TrimSpace(machineID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[machineID]; !ok {
		return false, nil
	}
	delete(b.entries, machineID)
	if err := b.save(); err != nil {
		return false, err
	}
	return true, nil
}

// refreshConcurrency bounds simultaneous probes. Machines are refreshed in
// parallel because one unreachable machine waiting out its timeout must not
// delay the verdict on every other machine behind it in the list.
const refreshConcurrency = 8

// Refresh re-probes every entry and updates liveness, reporting whether
// anything actually moved so a caller can stay quiet when nothing did.
//
// An entry that stops answering keeps its endpoints and its last-seen time — a
// machine that is off is still a machine you know about, and when it last
// worked is the useful fact about it.
func (b *Book) Refresh(ctx context.Context) ([]Entry, bool) {
	b.mu.Lock()
	targets := make([]Entry, 0, len(b.entries))
	for _, entry := range b.entries {
		targets = append(targets, entry)
	}
	b.mu.Unlock()

	var changed atomic.Bool
	var group sync.WaitGroup
	slots := make(chan struct{}, refreshConcurrency)
	for _, target := range targets {
		group.Add(1)
		go func(target Entry) {
			defer group.Done()
			slots <- struct{}{}
			card, err := b.probeAny(ctx, target.Endpoints)
			<-slots

			b.mu.Lock()
			defer b.mu.Unlock()
			entry, ok := b.entries[target.MachineID]
			if !ok {
				return
			}
			before := entry
			if err != nil {
				entry.Status = StatusUnreachable
				entry.Reason = err.Error()
			} else {
				entry.Name = card.DisplayName()
				entry.Version = card.Version
				entry.WireVersion = card.WireVersion
				entry.Secure = card.Secure
				entry.CertFingerprint = card.CertFingerprint
				entry.FleetIDs = card.FleetIDs
				entry.LastSeenAt = b.now().UTC().Format(time.RFC3339)
				entry.Status, entry.Reason = b.assess(card)
			}
			b.entries[target.MachineID] = entry
			if entry.Status != before.Status || entry.Reason != before.Reason ||
				entry.Name != before.Name || entry.Version != before.Version || entry.Secure != before.Secure ||
				entry.CertFingerprint != before.CertFingerprint {
				changed.Store(true)
			}
		}(target)
	}
	group.Wait()

	if changed.Load() {
		// Written only when something moved: a book saved every interval is a
		// disk write per machine per interval to record that nothing happened.
		b.mu.Lock()
		_ = b.save()
		b.mu.Unlock()
	}
	return b.List(), changed.Load()
}

// assess turns a card into a status. Reachable but unspeakable is its own
// answer: the machine is there and the client still cannot drive it, and the
// message says which side is behind rather than just refusing.
func (b *Book) assess(card Card) (string, string) {
	if b.wireVersion > 0 && card.WireVersion > 0 && card.WireVersion != b.wireVersion {
		behind := "that machine"
		if card.WireVersion > b.wireVersion {
			behind = "this machine"
		}
		return StatusNeedsUpdate, fmt.Sprintf("wire version %d here, %d there — %s needs an update", b.wireVersion, card.WireVersion, behind)
	}
	return StatusOK, ""
}

func hasEndpoint(endpoints []Endpoint, want Endpoint) bool {
	for _, existing := range endpoints {
		if existing.Kind == want.Kind && existing.Address == want.Address {
			return true
		}
	}
	return false
}

func mergeEndpoint(endpoints []Endpoint, add Endpoint) []Endpoint {
	if hasEndpoint(endpoints, add) {
		return endpoints
	}
	return append(endpoints, add)
}

// probeAny tries every known way to reach a machine and gives up only when all
// of them fail. The failure names how many were tried: with several endpoints,
// quoting one address makes it look like the others might have worked.
func (b *Book) probeAny(ctx context.Context, endpoints []Endpoint) (Card, error) {
	var lastErr error
	tried := 0
	for _, endpoint := range endpoints {
		card, err := b.probe(ctx, endpoint.Address)
		if err == nil {
			return card, nil
		}
		tried++
		lastErr = err
	}
	switch {
	case tried == 0:
		return Card{}, errors.New("no known address for this machine")
	case tried == 1:
		return Card{}, lastErr
	default:
		return Card{}, fmt.Errorf("none of its %d addresses answered — last tried %w", tried, lastErr)
	}
}

// save writes atomically; a torn book would lose every machine at once.
func (b *Book) save() error {
	entries := make([]Entry, 0, len(b.entries))
	for _, entry := range b.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].MachineID < entries[j].MachineID })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(b.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, FileName+".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, b.path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// NormalizeAddress accepts what a human types — a host, a host:port, or a URL —
// and returns host:port.
func NormalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return "", errors.New("address is empty")
	}
	address = strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://")
	address = strings.TrimSuffix(address, "/")
	if idx := strings.IndexAny(address, "/?#"); idx >= 0 {
		address = address[:idx]
	}
	if address == "" {
		return "", errors.New("address is empty")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		// Bare IPv6 needs brackets before a port can be joined onto it.
		if strings.Count(address, ":") > 1 && !strings.HasPrefix(address, "[") {
			address = "[" + address + "]"
		}
		address = net.JoinHostPort(strings.Trim(address, "[]"), DefaultPort)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("address %q is not host:port", address)
	}
	if strings.TrimSpace(host) == "" {
		return "", errors.New("address has no host")
	}
	return net.JoinHostPort(host, port), nil
}
