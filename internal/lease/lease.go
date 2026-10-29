package lease

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	stateVersion  = 1
	tokenBytes    = 32
	deviceIDBytes = 16
)

// Device is the persisted public identity for an approved browser/device.
//
// Owner and KeyID are additive and empty for every device approved the old way.
// Owner is the constant "local" today (D8); KeyID records which fleet key let a
// device in, so retiring a key can be told from revoking a device.
type Device struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	IP         string `json:"ip"`
	TokenHash  string `json:"tokenHash"`
	ApprovedAt string `json:"approvedAt"`
	LastSeen   string `json:"lastSeen"`
	Owner      string `json:"owner,omitempty"`
	KeyID      string `json:"keyId,omitempty"`
	// Kind tells one shape of approved identity from another. It is empty for
	// every device approved or enrolled before peer links existed, which is
	// exactly what a browser, a phone and a renderer still are; KindPeer marks
	// another daemon's link (docs/specs/fleet-runtime.md §4).
	Kind string `json:"kind,omitempty"`
	// Peer is the machine id on the other end of a peer link. It is the name a
	// human revokes by: one link, one machine, revocable without re-keying the
	// fleet.
	Peer string `json:"peer,omitempty"`
	// Grant is the permission ceiling this link was given, in the read/edit/full
	// vocabulary the provider lanes already use. Empty means full, which is what
	// fleet membership has always meant; a forwarded command's effective
	// permission is the lesser of this and the caller's own limit (§6).
	Grant string `json:"grant,omitempty"`
}

// KindPeer marks a device record that is another daemon's link rather than a
// person's browser.
const KindPeer = "peer"

// IsPeer reports whether this record is a peer link.
func (d Device) IsPeer() bool { return d.Kind == KindPeer }

// BlockedPeer records a peer link a human revoked.
//
// Revocation has to outlive the connection, and this is the record that makes
// it mean something: a key-holding peer could otherwise re-enrol itself
// silently one second later under the very fleet key that is meant to stay
// untouched (§4 — "revoking a link stops its subscriptions and monitors at
// once" and "cutting one machine out never means re-keying the fleet"). So the
// receiving side remembers the machine id it cut off and refuses its enrolments
// until a human re-approves that one peer.
type BlockedPeer struct {
	MachineID string `json:"machineId"`
	RevokedAt string `json:"revokedAt"`
	Name      string `json:"name,omitempty"`
}

// Options configures the device lease store.
type Options struct {
	StateDir string
	Logf     func(format string, args ...any)
	Now      func() time.Time
}

// Manager owns paired devices, token validation, and the runtime controller.
type Manager struct {
	mu           sync.Mutex
	path         string
	devices      []Device
	controllerID string
	// controllerTaken records that the current lease was taken (explicitly or
	// by acting) rather than granted while free. Device admission and
	// revocation must not be reachable through a lease a device took for
	// itself (PORT-SPEC "Controller lease").
	controllerTaken bool
	logf            func(format string, args ...any)
	now             func() time.Time
	blockedPeers    []BlockedPeer
}

type stateFile struct {
	Version int      `json:"version"`
	Devices []Device `json:"devices"`
	// BlockedPeers is additive and absent from every file written before peer
	// links existed, so an older daemon reading a newer file simply has no
	// blocks and a newer daemon reading an older file starts with none.
	BlockedPeers []BlockedPeer `json:"blockedPeers,omitempty"`
}

// NewManager loads state/devices.json from the supplied state directory.
func NewManager(opts Options) (*Manager, error) {
	if opts.StateDir == "" {
		opts.StateDir = "state"
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	m := &Manager{
		path: filepath.Join(opts.StateDir, "devices.json"),
		logf: opts.Logf,
		now:  opts.Now,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// ApproveDevice persists a newly approved device and returns its one-time plaintext token.
func (m *Manager) ApproveDevice(name, ip string) (Device, string, error) {
	name = cleanDeviceName(name)
	ip = strings.TrimSpace(ip)
	id, err := randomHex(deviceIDBytes)
	if err != nil {
		return Device{}, "", err
	}
	token, err := randomHex(tokenBytes)
	if err != nil {
		return Device{}, "", err
	}
	now := m.now().Format(time.RFC3339)
	device := Device{
		ID:         id,
		Name:       name,
		IP:         ip,
		TokenHash:  tokenHash(token),
		ApprovedAt: now,
		LastSeen:   now,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices = append(m.devices, device)
	if err := m.saveLocked(); err != nil {
		return Device{}, "", err
	}
	return device, token, nil
}

// EnrollDevice records a device whose token was *derived* rather than minted
// here, which is what a fleet enrolment produces.
//
// The token is not returned, because it is never sent: the client computed the
// same value from the same inputs, and the whole point is that it never crosses
// the wire. Only the hash is stored, exactly as for an approved device — so
// everything downstream, from authentication to revocation, cannot tell the two
// kinds of device apart and does not need to.
func (m *Manager) EnrollDevice(name, ip, token, owner, keyID string) (Device, error) {
	return m.enroll(name, ip, token, owner, keyID, "", "")
}

// EnrollPeerDevice records another daemon's link under the same derivation and
// the same storage as a browser's, marked as a peer and named by the machine on
// the other end.
//
// It is one mechanism, not two: everything downstream — authentication,
// lastSeen, the controller lease, revocation, socket fencing — cannot tell a
// peer link from an enrolled browser and does not need to. What the kind buys
// is the thing §4 asks for: a link a human can SEE and revoke on its own,
// without touching the fleet key or any other peer.
func (m *Manager) EnrollPeerDevice(name, ip, token, owner, keyID, peerMachineID, grant string) (Device, error) {
	peerMachineID = strings.TrimSpace(peerMachineID)
	if peerMachineID == "" {
		return Device{}, errors.New("a peer link needs the peer's machine id")
	}
	if m.PeerBlocked(peerMachineID) {
		return Device{}, fmt.Errorf("%w: %s", ErrPeerRevoked, peerMachineID)
	}
	return m.enroll(name, ip, token, owner, keyID, peerMachineID, grant)
}

// ErrPeerRevoked reports an enrolment from a peer a human cut off. It is its
// own error because it is not a credential failure: the key was right, and the
// answer is still no until that one link is approved again.
var ErrPeerRevoked = errors.New("that peer link was revoked on this machine")

func (m *Manager) enroll(name, ip, token, owner, keyID, peerMachineID, grant string) (Device, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Device{}, errors.New("enrol needs a derived token")
	}
	name = cleanDeviceName(name)
	ip = strings.TrimSpace(ip)
	id, err := randomHex(deviceIDBytes)
	if err != nil {
		return Device{}, err
	}
	now := m.now().Format(time.RFC3339)
	device := Device{
		ID:         id,
		Name:       name,
		IP:         ip,
		TokenHash:  tokenHash(token),
		ApprovedAt: now,
		LastSeen:   now,
		Owner:      strings.TrimSpace(owner),
		KeyID:      strings.TrimSpace(keyID),
		Peer:       strings.TrimSpace(peerMachineID),
		Grant:      strings.TrimSpace(grant),
	}
	if device.Peer != "" {
		device.Kind = KindPeer
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-enrolling the same device — a reinstall, a cleared browser store, the
	// same key pasted twice — must not pile up duplicate records for one thing.
	for i := range m.devices {
		if subtle.ConstantTimeCompare([]byte(m.devices[i].TokenHash), []byte(device.TokenHash)) == 1 {
			m.devices[i].LastSeen = now
			if ip != "" {
				m.devices[i].IP = ip
			}
			// A peer that re-derives the same token is the same link. Keep its
			// kind and peer id current rather than leaving a record that once
			// enrolled as a browser describing a daemon.
			if device.Peer != "" {
				m.devices[i].Kind, m.devices[i].Peer = device.Kind, device.Peer
				if device.Grant != "" {
					m.devices[i].Grant = device.Grant
				}
			}
			if err := m.saveLocked(); err != nil {
				return Device{}, err
			}
			return m.devices[i], nil
		}
	}
	m.devices = append(m.devices, device)
	if err := m.saveLocked(); err != nil {
		return Device{}, err
	}
	return device, nil
}

// AuthenticateToken returns the approved device for a presented plaintext token and stamps lastSeen.
func (m *Manager) AuthenticateToken(token, ip string) (Device, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Device{}, false
	}
	hash := tokenHash(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, device := range m.devices {
		if subtle.ConstantTimeCompare([]byte(device.TokenHash), []byte(hash)) == 1 {
			now := m.now().Format(time.RFC3339)
			m.devices[i].LastSeen = now
			if strings.TrimSpace(ip) != "" {
				m.devices[i].IP = strings.TrimSpace(ip)
			}
			if err := m.saveLocked(); err != nil {
				m.logf("[lan] device lastSeen save failed: %v", err)
			}
			return m.devices[i], true
		}
	}
	return Device{}, false
}

// TouchDevice updates lastSeen and, when available, the current connection IP
// for an already-approved device.
func (m *Manager) TouchDevice(deviceID, ip string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	ip = strings.TrimSpace(ip)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.devices {
		if m.devices[i].ID != deviceID {
			continue
		}
		m.devices[i].LastSeen = m.now().Format(time.RFC3339)
		if ip != "" {
			m.devices[i].IP = ip
		}
		if err := m.saveLocked(); err != nil {
			m.logf("[lan] device lastSeen save failed: %v", err)
		}
		return true
	}
	return false
}

// Devices returns approved devices without plaintext tokens.
func (m *Manager) Devices() []Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Device(nil), m.devices...)
}

// Device returns one approved device by id. Unlike AuthenticateToken it never
// stamps lastSeen or writes devices.json, so a hot read path (every chat image
// a renderer fetches) can ask "is this device still approved?" for free.
func (m *Manager) Device(deviceID string) (Device, bool) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return Device{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, device := range m.devices {
		if device.ID == deviceID {
			return device, true
		}
	}
	return Device{}, false
}

// RevokeDevice removes an approved device token from the allow list.
func (m *Manager) RevokeDevice(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.devices[:0]
	removed := false
	for _, device := range m.devices {
		if device.ID == deviceID {
			removed = true
			continue
		}
		out = append(out, device)
	}
	if !removed {
		return false
	}
	m.devices = out
	if m.controllerID == deviceID {
		m.controllerID = ""
	}
	if err := m.saveLocked(); err != nil {
		m.logf("[lan] revoke save failed: %v", err)
	}
	return true
}

// EnsureController grants the runtime controller lease when it is empty.
func (m *Manager) EnsureController(device Device) bool {
	if device.ID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controllerID != "" {
		return false
	}
	m.controllerID = device.ID
	m.controllerTaken = false
	return true
}

// TakeControl moves the runtime controller lease to the supplied device.
func (m *Manager) TakeControl(device Device) bool {
	if device.ID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.controllerID != device.ID
	m.controllerID = device.ID
	if changed {
		m.controllerTaken = true
	}
	return changed
}

// ControllerGranted reports that the device holds the controller lease and
// received it while the lease was free, rather than taking it from another
// device.
func (m *Manager) ControllerGranted(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controllerID == deviceID && !m.controllerTaken
}

// ReleaseController clears the runtime lease only when it still belongs to
// the supplied device. The wire hub calls this after the last live connection
// for that device disappears, allowing the next authenticated client to become
// controller without disturbing a newer explicit takeover.
func (m *Manager) ReleaseController(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.controllerID != deviceID {
		return false
	}
	m.controllerID = ""
	m.controllerTaken = false
	return true
}

// IsController reports whether a paired device owns the runtime controller.
func (m *Manager) IsController(deviceID string) bool {
	if deviceID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controllerID == deviceID
}

// Controller returns the runtime controller device, if any.
func (m *Manager) Controller() (Device, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.controllerLocked()
}

func (m *Manager) controllerLocked() (Device, bool) {
	if m.controllerID == "" {
		return Device{}, false
	}
	for _, device := range m.devices {
		if device.ID == m.controllerID {
			return device, true
		}
	}
	return Device{ID: m.controllerID, Name: ""}, true
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var root stateFile
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&root); err != nil {
		return err
	}
	if root.Version != stateVersion {
		return fmt.Errorf("unsupported device state version %d", root.Version)
	}
	m.devices = append([]Device(nil), root.Devices...)
	m.blockedPeers = append([]BlockedPeer(nil), root.BlockedPeers...)
	return nil
}

func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Devices: m.devices, BlockedPeers: m.blockedPeers}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func cleanDeviceName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Unknown device"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// PeerLinks returns the peer links this machine has accepted, newest last.
//
// This is the list §4 calls "visible and revocable individually": one row per
// linked daemon, naming which fleet key let it in and when it was last used.
func (m *Manager) PeerLinks() []Device {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Device, 0, len(m.devices))
	for _, device := range m.devices {
		if device.IsPeer() {
			out = append(out, device)
		}
	}
	return out
}

// PeerLink returns the accepted link for one peer machine id.
func (m *Manager) PeerLink(peerMachineID string) (Device, bool) {
	peerMachineID = strings.TrimSpace(peerMachineID)
	if peerMachineID == "" {
		return Device{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, device := range m.devices {
		if device.IsPeer() && device.Peer == peerMachineID {
			return device, true
		}
	}
	return Device{}, false
}

// RevokePeerLink cuts exactly one peer: its device record is removed, its
// machine id is blocked from re-enrolling, and nothing else is touched — not
// the fleet key, not another peer, not a browser.
//
// The returned device is the record that was removed, so the caller can fence
// its live sockets and name it in the `link_revoked` it owes its watchers.
func (m *Manager) RevokePeerLink(peerMachineID string) (Device, bool, error) {
	peerMachineID = strings.TrimSpace(peerMachineID)
	if peerMachineID == "" {
		return Device{}, false, errors.New("revoking a link needs the peer's machine id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := Device{}
	kept := make([]Device, 0, len(m.devices))
	for _, device := range m.devices {
		if device.IsPeer() && device.Peer == peerMachineID {
			removed = device
			continue
		}
		kept = append(kept, device)
	}
	now := m.now().Format(time.RFC3339)
	blocked := false
	for i := range m.blockedPeers {
		if m.blockedPeers[i].MachineID == peerMachineID {
			m.blockedPeers[i].RevokedAt = now
			blocked = true
		}
	}
	if !blocked {
		m.blockedPeers = append(m.blockedPeers, BlockedPeer{MachineID: peerMachineID, RevokedAt: now, Name: removed.Name})
	}
	found := removed.ID != ""
	if found {
		m.devices = kept
		if m.controllerID == removed.ID {
			m.controllerID = ""
			m.controllerTaken = false
		}
	}
	if err := m.saveLocked(); err != nil {
		return removed, found, err
	}
	return removed, found, nil
}

// AllowPeer lifts a revocation so that peer may link again. Re-approving one
// link is as narrow as cutting it was.
func (m *Manager) AllowPeer(peerMachineID string) (bool, error) {
	peerMachineID = strings.TrimSpace(peerMachineID)
	if peerMachineID == "" {
		return false, errors.New("allowing a link needs the peer's machine id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]BlockedPeer, 0, len(m.blockedPeers))
	lifted := false
	for _, blocked := range m.blockedPeers {
		if blocked.MachineID == peerMachineID {
			lifted = true
			continue
		}
		kept = append(kept, blocked)
	}
	if !lifted {
		return false, nil
	}
	m.blockedPeers = kept
	if err := m.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// PeerBlocked reports whether a peer machine id was revoked here.
func (m *Manager) PeerBlocked(peerMachineID string) bool {
	peerMachineID = strings.TrimSpace(peerMachineID)
	if peerMachineID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, blocked := range m.blockedPeers {
		if blocked.MachineID == peerMachineID {
			return true
		}
	}
	return false
}

// BlockedPeers lists the revoked links, so a human can see what they cut off
// and lift it.
func (m *Manager) BlockedPeers() []BlockedPeer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BlockedPeer(nil), m.blockedPeers...)
}
