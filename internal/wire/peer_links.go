package wire

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// Peer links, on the receiving side.
//
// Trust has two layers and this file is the second one (§4). Fleet membership —
// the key — says *who may ask*. A peer link says *which daemon is actually
// linked to me right now*, and it is one record per peer, visible and revocable
// on its own, so cutting one machine out never means re-keying the fleet.
//
// It reuses the device store rather than inventing a registry, because that
// store already anticipated exactly this: lease.Device carries Owner and KeyID
// so that "retiring a key" can be told from "revoking a device", and revocation
// already fences live sockets. A peer link is a device of kind `peer`, so
// authentication, lastSeen, the controller lease and socket fencing cannot tell
// it apart from an enrolled browser and do not need to.
//
// What is NOT reused is re-enrolment. A browser that loses its token enrols
// again silently, which is the whole point of a fleet key. A peer whose link a
// human revoked must not: the key still matches, so an automatic retry would
// quietly undo the decision a second later. lease.RevokePeerLink therefore
// records the machine id it cut off, and this file refuses it by name until
// `fleet:relink` lifts it.

// peerLinkChannels are the three channels one link's life needs. They sit with
// lan:revoke rather than with the fleet key channels: revoking a link is an
// administrative act on a device, not a reading of a secret, so a controller may
// do it from a phone — and, like lan:revoke, the controller test is explicit
// because acting takes the lease.
var peerLinkChannels = map[string]struct{}{
	"fleet:links":  {},
	"fleet:unlink": {},
	"fleet:relink": {},
}

// PeerLinkChannel is the heartbeat every link rides. It is the cheapest
// authenticated round trip there is — it proves the socket still carries an
// approved identity and says nothing else — and it is the only timer the
// runtime has (§4).
const PeerLinkChannel = "fleet:heartbeat"

// PeerLinkEvent tells the daemon that one link's life changed, so the daemon's
// own outbound mesh can learn a route or drop a link it no longer has.
type PeerLinkEvent struct {
	// Kind is "paired" or "revoked".
	Kind string
	Peer string
	Name string
	// Address is the route this peer connected FROM, as observed here joined to
	// the listening port the peer stated for itself. The host is observed and
	// the port is claimed, which is why §4 ranks a learned route below one a
	// human typed until it has worked.
	Address  string
	DeviceID string
	At       time.Time
}

// SetPeerLinks installs the daemon's peer-link observer. It is a setter for the
// same reason SetFleet is: the hub exists before the daemon has loaded its
// identity or opened its peer store.
func (h *Hub) SetPeerLinks(observe func(PeerLinkEvent)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.peerLinks = observe
}

func (h *Hub) peerLinkObserver() func(PeerLinkEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.peerLinks
}

func (h *Hub) notePeerLink(event PeerLinkEvent) {
	if observe := h.peerLinkObserver(); observe != nil {
		observe(event)
	}
}

// peerEnrolment is what a dialling daemon states about itself on fleet:enroll.
// Absent means a browser, a phone or a renderer, which is every client that
// existed before this.
type peerEnrolment struct {
	MachineID string
	Name      string
	Address   string
}

func readPeerEnrolment(arg map[string]any, ip string) (peerEnrolment, bool) {
	raw, ok := arg["peer"].(map[string]any)
	if !ok {
		return peerEnrolment{}, false
	}
	machineID := stringField(raw, "machineId")
	if machineID == "" {
		return peerEnrolment{}, false
	}
	enrolment := peerEnrolment{MachineID: machineID, Name: stringField(raw, "name")}
	// The port is the peer's claim about where it listens; the host is what this
	// socket actually came from. A peer cannot name someone else's host this way.
	if port := stringField(raw, "port"); port != "" && strings.TrimSpace(ip) != "" {
		enrolment.Address = net.JoinHostPort(strings.TrimSpace(ip), port)
	}
	return enrolment, true
}

// handlePeerLinkChannel serves the three link channels. The caller has already
// proven it may administer devices.
func (h *Hub) handlePeerLinkChannel(c *client, channel string, args []any) (any, error) {
	switch channel {
	case "fleet:links":
		return h.peerLinksPayload(), nil

	case "fleet:unlink":
		peerID := stringField(firstMapArg(args), "machineId")
		if peerID == "" {
			peerID = stringArg(args, 0)
		}
		if peerID == "" {
			return nil, structuredError("fleet:malformed", "machineId is required", nil)
		}
		found, err := h.RevokePeerLink(peerID, c.ip)
		if err != nil {
			return nil, err
		}
		result := h.peerLinksPayload()
		result["ok"] = true
		result["revoked"] = found
		result["peer"] = peerID
		return result, nil

	case "fleet:relink":
		peerID := stringField(firstMapArg(args), "machineId")
		if peerID == "" {
			peerID = stringArg(args, 0)
		}
		if peerID == "" {
			return nil, structuredError("fleet:malformed", "machineId is required", nil)
		}
		lifted, err := h.lease.AllowPeer(peerID)
		if err != nil {
			return nil, err
		}
		if lifted {
			h.logf("[fleet] %s may link again, approved by %s", peerID, c.ip)
		}
		result := h.peerLinksPayload()
		result["ok"] = true
		result["lifted"] = lifted
		result["peer"] = peerID
		return result, nil
	}
	return nil, structuredError("fleet:malformed", "unknown fleet link channel", nil)
}

func (h *Hub) peerLinksPayload() map[string]any {
	_, machineID := h.fleetSnapshot()
	links := h.lease.PeerLinks()
	rows := make([]any, 0, len(links))
	for _, device := range links {
		rows = append(rows, map[string]any{
			"peer": device.Peer, "name": device.Name, "deviceId": device.ID,
			"ip": device.IP, "keyId": device.KeyID, "owner": device.Owner,
			"approvedAt": device.ApprovedAt, "lastSeen": device.LastSeen,
			"grant": firstNonEmpty(device.Grant, "full"),
		})
	}
	blocked := h.lease.BlockedPeers()
	refused := make([]any, 0, len(blocked))
	for _, entry := range blocked {
		refused = append(refused, map[string]any{
			"peer": entry.MachineID, "name": entry.Name, "revokedAt": entry.RevokedAt,
		})
	}
	return map[string]any{"machineId": machineID, "links": rows, "revoked": refused}
}

// handlePeerHeartbeat answers a link's heartbeat. It deliberately reports the
// machine id, so a peer that reconnected to a DIFFERENT daemon at the same
// address finds out on the first beat rather than on the first command.
func (h *Hub) handlePeerHeartbeat(c *client, args []any) (any, error) {
	_, machineID := h.fleetSnapshot()
	device, paired := c.deviceSnapshot()
	payload := map[string]any{
		"ok": true, "machineId": machineID, "at": time.Now().UTC().Format(time.RFC3339),
		"instanceId": h.InstanceID(),
	}
	if paired {
		payload["deviceId"] = device.ID
		if device.IsPeer() {
			payload["peer"] = device.Peer
		}
	}
	if from := stringField(firstMapArg(args), "machineId"); from != "" && paired && device.IsPeer() && device.Peer != from {
		// The record says this socket belongs to one peer and the beat claims
		// another. Nothing good can come of answering it as if both were true.
		return nil, structuredError("fleet:peer-mismatch",
			fmt.Sprintf("this link belongs to %s, not %s", device.Peer, from), nil)
	}
	return payload, nil
}

// Caller is who is on the other end of one invoke.
//
// Almost nothing needs it: an approved device is an approved device, and the
// controller lease already answers "may this one decide". A forwarded fleet
// command needs it for one reason — §6's `limit` is the LESSER of the caller's
// own ceiling and the grant this link was given, and the grant is a property of
// the link the command arrived on.
type Caller struct {
	DeviceID string
	Name     string
	IP       string
	// Peer is the machine id on the other end when this invoke arrived over a
	// daemon link. Empty for a browser, a phone or a renderer.
	Peer string
	// Grant is the permission ceiling recorded for that link. Empty means full,
	// which is what fleet membership has always meant.
	Grant string
	Local bool
}

// IsPeer reports whether this invoke arrived over a peer daemon's link.
func (c Caller) IsPeer() bool { return strings.TrimSpace(c.Peer) != "" }

// CallerHandler is a channel handler that is told who called it.
type CallerHandler func(Caller, []any) (any, error)

// RegisterWithCaller installs a handler that receives the caller's identity
// alongside its args.
//
// The ordinary Handler is registered too, with an empty Caller, so a caller
// that reaches the channel without a socket — the local shell bridge, a test —
// behaves exactly as it did before. An empty Caller is not "no authority": it
// is "no link", which is the local path.
func (h *Hub) RegisterWithCaller(channel string, fn CallerHandler) {
	if fn == nil {
		return
	}
	h.Register(channel, func(args []any) (any, error) { return fn(Caller{}, args) })
	h.mu.Lock()
	if h.callerHandlers == nil {
		h.callerHandlers = make(map[string]CallerHandler)
	}
	h.callerHandlers[channel] = fn
	h.mu.Unlock()
}

func (h *Hub) callerHandler(channel string) CallerHandler {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.callerHandlers == nil {
		return nil
	}
	return h.callerHandlers[channel]
}

func (h *Hub) callerFor(c *client) Caller {
	if c == nil {
		return Caller{}
	}
	caller := Caller{IP: c.ip, Local: isLocalIP(c.ip)}
	if device, paired := c.deviceSnapshot(); paired {
		caller.DeviceID, caller.Name = device.ID, device.Name
		if device.IsPeer() {
			caller.Peer, caller.Grant = device.Peer, device.Grant
		}
	}
	return caller
}

// RevokePeerLink cuts one peer link: the record goes, the machine id is blocked
// from re-enrolling, every live socket for it is fenced in the same lifecycle
// boundary device revocation already uses, and everyone is told.
//
// Revocation is immediate and honest (§4). Entities keep existing at their
// homes — nothing here deletes any work — and every affected watcher is told
// `link_revoked` rather than left to infer silence. It is exported because the
// decision arrives from three places that must all do the same thing: a click,
// the `fleet:unlink` channel, and the daemon itself when a human revokes the
// pair from the other side.
func (h *Hub) RevokePeerLink(peerID, by string) (bool, error) {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return false, structuredError("fleet:malformed", "machineId is required", nil)
	}
	if h.lease == nil {
		return false, structuredError("fleet:unavailable", "this daemon has no device store", nil)
	}
	device, found, err := h.lease.RevokePeerLink(peerID)
	if err != nil {
		return false, err
	}
	if found {
		h.fenceRevokedDevice(device.ID)
	}
	at := time.Now().UTC()
	h.Broadcast("fleet:link-revoked", map[string]any{
		"event": "link_revoked", "peer": peerID, "name": device.Name,
		"deviceId": device.ID, "at": at.Format(time.RFC3339), "linked": found,
	})
	h.notePeerLink(PeerLinkEvent{Kind: "revoked", Peer: peerID, Name: device.Name, DeviceID: device.ID, At: at})
	h.logf("[fleet] link to %s revoked by %s; the fleet key and every other link are untouched",
		peerID, firstNonEmpty(strings.TrimSpace(by), "this machine"))
	return found, nil
}

// SendToPeer admits one event to the sockets of exactly one peer daemon.
//
// It exists because P3's logs and monitors push rather than being polled
// (docs/specs/fleet-runtime.md §5: "Nothing polls"), and a push has an
// addressee: the ONE peer that subscribed. Broadcasting an entity's log to
// every connected client would hand one peer's work to another peer and to
// every browser on the LAN, which §11's blast radius forbids — a monitor
// carries a capability naming the watcher, and nothing else may be read
// through it.
//
// The frozen event envelope is unchanged; this is a narrower `include` over the
// same `broadcastWhere` BroadcastLocalShell already uses. The returned count
// lets the caller tell "the peer is not connected" from "sent", with no
// timeout: an event that cannot be delivered now is simply not delivered, and
// the subscriber asks again from its cursor on link up.
func (h *Hub) SendToPeer(peerID, channel string, payload any) int {
	peerID = strings.TrimSpace(peerID)
	if h == nil || peerID == "" {
		return 0
	}
	h.broadcastMu.Lock()
	defer h.broadcastMu.Unlock()
	started := time.Now()
	delivered := h.broadcastWhere(channel, payload, func(c *client) bool {
		if c == nil {
			return false
		}
		device, paired := c.deviceSnapshot()
		return paired && device.IsPeer() && device.Peer == peerID
	})
	h.recordEventEnqueue(time.Since(started))
	return delivered
}
