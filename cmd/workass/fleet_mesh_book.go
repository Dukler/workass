package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workass/internal/fleet"
	"workass/internal/fleetclient"
	"workass/internal/machinebook"
	"workass/internal/machineid"
	"workass/internal/wire"
)

// Discovery, for the mesh.
//
// §4 says discovery is "what already exists": the machine book, fed by the
// port-80 probe and by addresses a human typed. Nothing new is built here. This
// file is the two lines that were missing — reading that book into the peer
// store, and starting one heartbeat per peer — so a daemon that holds the fleet
// key links itself to every machine it can see with no human in the loop.
//
// The fleet-id intersection is the whole admission test on this side: the book
// records which fleets a machine advertises (hashes, never the key), so "one of
// mine, link silently" is answerable without asking anybody anything. A machine
// on the same network advertising a different fleet is left alone.

// fleetMeshService binds the mesh to the daemon's lifetime, its machine book
// and its log. Everything that needs to start a supervisor goes through it, so
// a goroutine opened by a tool call still dies with the daemon rather than with
// the call.
type fleetMeshService struct {
	mesh   *fleetclient.Mesh
	book   *machinebook.Book
	keyIDs func() []string
	logger *log.Logger
	ctx    context.Context

	// P3's observers, set after construction: the mesh exists before the chat
	// runtime does, and a mesh with none of them is the P2 daemon exactly.
	observers sync.RWMutex
	onEvent   func(peerID, channel string, payload any)
	onBeat    func(fleetclient.State)
	onUp      func(ctx context.Context, peerID string)
}

// Start begins supervising every peer already paired, then reads the book once.
// A daemon with no peers and an empty book starts nothing at all: there is no
// polling here, only one heartbeat per pair that exists.
func (s *fleetMeshService) Start(ctx context.Context) int {
	if s == nil || s.mesh == nil {
		return 0
	}
	s.ctx = ctx
	s.mesh.Start(ctx)
	return s.SyncFromBook()
}

// SyncFromBook adds a route for every machine in the book that belongs to a
// fleet this daemon holds a key for, and supervises it.
//
// It never removes a peer: a machine that is merely unreachable today is still a
// peer, and forgetting it would throw away the credential and the route history
// that let it come back. Forgetting is `fleet:unlink`, which is a decision.
func (s *fleetMeshService) SyncFromBook() int {
	if s == nil || s.mesh == nil || s.book == nil || s.keyIDs == nil {
		return 0
	}
	keyIDs := s.keyIDs()
	if len(keyIDs) == 0 {
		return 0
	}
	ours := make(map[string]struct{}, len(keyIDs))
	for _, id := range keyIDs {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			ours[trimmed] = struct{}{}
		}
	}
	linked := 0
	for _, entry := range s.book.List() {
		machineID := strings.TrimSpace(entry.MachineID)
		if machineID == "" || machineID == s.mesh.SelfMachineID() {
			continue
		}
		if !sharesFleet(entry.FleetIDs, ours) {
			continue
		}
		source := fleetclient.SourceLearned
		if entry.AddedBy == machinebook.SourceManual {
			// A human typed this address. §4 ranks that above anything observed
			// until the observed one has worked, and this is where the two
			// provenances are told apart.
			source = fleetclient.SourceManual
		}
		added := false
		for _, endpoint := range entry.Endpoints {
			address := strings.TrimSpace(endpoint.Address)
			if address == "" {
				continue
			}
			kind := endpoint.Kind
			if strings.TrimSpace(kind) == "" {
				kind = fleetclient.KindLAN
			}
			if err := s.mesh.AddRoute(machineID, entry.Name, kind, address, source); err != nil {
				s.logger.Printf("[fleet] route %s for %s: %v", address, machineID, err)
				continue
			}
			added = true
		}
		if !added {
			continue
		}
		linked++
		s.supervise(machineID)
	}
	return linked
}

// Observe turns one link's life, as the hub sees it, into the two things the
// dialling side owes it: a learned route when a peer connects in, and a dropped
// link when a human here revokes it.
//
// The second half is why revocation is honest rather than cosmetic. Without it,
// this daemon would keep its own outbound socket to a machine it just cut off —
// the link would be revoked in one direction and live in the other.
func (s *fleetMeshService) Observe(event wire.PeerLinkEvent) {
	if s == nil || s.mesh == nil || strings.TrimSpace(event.Peer) == "" {
		return
	}
	switch event.Kind {
	case "paired":
		learned := false
		if event.Address != "" {
			learned = s.mesh.LearnRoute(event.Peer, event.Name, event.Address)
		}
		s.supervise(event.Peer)
		// Logged on the enrolment and on any beat that teaches a NEW address,
		// and silent otherwise. This arrives once per heartbeat per peer now
		// that the route is restated on the beat, and a line a minute would bury
		// the one line an operator reads these logs for.
		if learned || event.Address == "" {
			s.logger.Printf("[fleet] peer %s linked in from %s", event.Peer, firstNonEmptyAddress(event.Address))
		}
	case "revoked":
		s.mesh.Drop(event.Peer)
		s.logger.Printf("[fleet] dropped this daemon's own link to %s as well", event.Peer)
	}
}

func (s *fleetMeshService) supervise(machineID string) {
	if s.ctx == nil {
		return
	}
	s.mesh.Supervise(s.ctx, machineID)
}

func sharesFleet(advertised []string, ours map[string]struct{}) bool {
	for _, id := range advertised {
		if _, mine := ours[strings.TrimSpace(id)]; mine {
			return true
		}
	}
	return false
}

func firstNonEmptyAddress(address string) string {
	if strings.TrimSpace(address) == "" {
		return "an unstated address"
	}
	return address
}

// openFleetMesh builds this daemon's outbound half and puts it on the hub.
//
// Three things have to be true before a link can exist, and each missing one is
// reported rather than worked around: this daemon needs a provable identity (a
// peer is addressed by machine id), a fleet key (membership is what lets it
// enrol with no human), and a readable peer file (the credential store). A
// daemon missing any of them keeps the renderer relay and says so.
func openFleetMesh(stateDir string, identity machineid.Identity, keys *fleet.Store, book *machinebook.Book, hub *wire.Hub, logger *log.Logger, selfPort int) *fleetMeshService {
	service := &fleetMeshService{book: book, logger: logger}
	if strings.TrimSpace(identity.MachineID) == "" {
		logger.Printf("[workass] fleet mesh unavailable: this daemon has no machine id")
		return service
	}
	if keys == nil {
		logger.Printf("[workass] fleet mesh unavailable: no fleet key store")
		return service
	}
	peers, err := fleetclient.Open(stateDir)
	if err != nil {
		// Deliberately not self-healing, for the same reason the machine book is
		// not: replacing an unreadable peer file would silently drop every link
		// the operator paired, without telling them which ones.
		logger.Printf("[workass] fleet mesh unavailable: %v", err)
		return service
	}
	service.keyIDs = keys.KeyIDs
	service.mesh = fleetclient.New(fleetclient.Options{
		SelfMachineID: identity.MachineID,
		SelfName:      identity.DisplayName,
		SelfPort:      selfPort,
		// One duplex link per pair (§4). The hub already knows which peers hold
		// a socket here, so the outbound half asks it instead of dialling a
		// second connection to a machine that is already connected — and
		// instead of publishing `down`, which past the threshold would announce
		// a reachable machine as home_down.
		InboundUp: hub.HasPeerSocket,
		Store:     peers,
		Keys:      keys,
		Logf:      logger.Printf,
		OnEvent: func(peerID, channel string, payload any) {
			// A peer's log pushes arrive here. Everything else a peer publishes
			// is read off the socket and dropped, as it was before P3.
			if onEvent, _, _ := service.monitorObservers(); onEvent != nil {
				onEvent(peerID, channel, payload)
			}
		},
		OnBeat: func(state fleetclient.State) {
			// Every beat, not only every change: `home_down` is a duration
			// threshold and has to be re-read as time passes (§16.3a).
			if _, onBeat, _ := service.monitorObservers(); onBeat != nil {
				onBeat(state)
			}
		},
		OnState: func(state fleetclient.State) {
			// Link state is a property of the pair, published locally as an
			// event. It is the one vocabulary everything above the mesh reads:
			// connecting | up | down(since), with no timer of its own (§4).
			hub.Broadcast("fleet:link-state", map[string]any{
				"peer": state.Peer, "name": state.Name, "status": state.Status,
				"since": state.Since.UTC().Format(time.RFC3339), "address": state.Address,
				"reason": redactedSessionString(state.Reason),
			})
			_, onBeat, onUp := service.monitorObservers()
			if onBeat != nil {
				onBeat(state)
			}
			if state.Status == fleetclient.StatusUp && onUp != nil && service.ctx != nil {
				// The link came up: state the cursors and take the backlog. This
				// is the only thing that happens on reconnect, and nothing polls
				// in between (§5).
				onUp(service.ctx, state.Peer)
			}
		},
	})
	hub.SetPeerLinks(service.Observe)
	if linked := len(service.mesh.PeerIDs()); linked > 0 {
		logger.Printf("[workass] fleet mesh loaded %d peer link(s) from %s", linked, filepath.Join(stateDir, fleetclient.FileName))
	}
	return service
}

// Close ends every link this daemon dialled.
func (s *fleetMeshService) Close() {
	if s == nil || s.mesh == nil {
		return
	}
	s.mesh.Close()
}
