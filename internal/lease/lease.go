package lease

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	logf         func(format string, args ...any)
	now          func() time.Time
}

type stateFile struct {
	Version int      `json:"version"`
	Devices []Device `json:"devices"`
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

// PairDevice is a compatibility alias for older callers; it now records an approved device.
func (m *Manager) PairDevice(name string) (Device, string, error) {
	return m.ApproveDevice(name, "")
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
	return changed
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
	m.devices = append([]Device(nil), root.Devices...)
	return nil
}

func (m *Manager) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(stateFile{Version: stateVersion, Devices: m.devices}, "", "  ")
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
