// Package fleet is the one secret that decides which machines are yours.
//
// A daemon holds a fleet key; a client that can prove it holds the same key is
// allowed in. That single fact replaces per-pair pairing entirely (D3): there is
// one ceremony per daemon and one per client, never one per pair, and a client
// already holding the key joins a newly-discovered machine with no human in the
// loop at all.
//
// Two properties are the reason this is not simply a password:
//
//   - The key never crosses the wire. Enrolment is a challenge-response over
//     nonces both sides contribute, so a listener learns nothing reusable.
//   - Neither does the resulting device token. Both ends derive it from the same
//     inputs, so it is never transmitted even once — which is what makes this
//     safe to run before TLS exists (E5).
//
// The known weakness is written down rather than hidden: HMAC is symmetric, so
// every daemon holds the key in the clear and compromising any one machine
// compromises the fleet. E5 replaces the shared secret with a public key the
// daemons merely verify against; the ceremony a human performs does not change.
package fleet

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
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

// FileName is the per-state-dir file holding fleet keys.
const FileName = "fleet.json"

// KeyPrefix marks a fleet key on sight. Keys get pasted into terminals, chat
// windows and phone keyboards; a prefix makes one recognisable as a secret so it
// is less likely to be treated as an id and pasted somewhere public.
const KeyPrefix = "wf-"

// LocalOwner is the single-owner placeholder, matching the machine book. Keys
// are a list carrying an owner from day one so that "someone else joins" is a
// second key rather than a schema change (D8).
const LocalOwner = "local"

// secretBytes is 128 bits of entropy — enough that guessing is not a threat
// model, short enough to type on a phone at 26 base32 characters.
const secretBytes = 16

// Domain separators. A proof must never be replayable as a token, so the two
// derivations never see the same input even though they share a key.
const (
	proofContext = "workass-fleet-proof-v1"
	certContext  = "workass-fleet-cert-v1"
	tokenContext = "workass-fleet-token-v1"
	keyIDContext = "workass-fleet-keyid-v1"
)

// NonceBytes is how much randomness each side contributes to an enrolment.
const NonceBytes = 16

var secretEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Key is one fleet credential.
type Key struct {
	KeyID     string `json:"keyId"`
	Secret    string `json:"secret"`
	Owner     string `json:"owner"`
	Label     string `json:"label,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// ErrMalformedKey reports something that is not a fleet key at all, as opposed
// to a well-formed key that simply is not ours. The two need different words: a
// typo and a wrong fleet are different problems.
var ErrMalformedKey = errors.New("that is not a fleet key")

// Store is a daemon's set of fleet keys, persisted to its state dir.
type Store struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	keys []Key
}

// Open loads the fleet file. It does not mint: a daemon that is joining an
// existing fleet must not invent a second one behind the user's back, so
// minting is an explicit call.
func Open(stateDir string) (*Store, error) {
	store := &Store{path: filepath.Join(stateDir, FileName), now: time.Now}
	data, err := os.ReadFile(store.path)
	switch {
	case err == nil:
		var stored struct {
			Keys []Key `json:"keys"`
		}
		if jsonErr := json.Unmarshal(data, &stored); jsonErr != nil {
			return nil, fmt.Errorf("fleet file %s: %w", store.path, jsonErr)
		}
		for _, key := range stored.Keys {
			if strings.TrimSpace(key.Secret) == "" {
				continue
			}
			if strings.TrimSpace(key.Owner) == "" {
				key.Owner = LocalOwner
			}
			store.keys = append(store.keys, key)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	return store, nil
}

// EnsureKey mints a key if this daemon has none, reporting whether it minted.
// The first daemon anyone starts becomes the origin of a fleet; every other
// machine is told the key rather than inventing one.
func (s *Store) EnsureKey() (Key, bool, error) {
	s.mu.Lock()
	if len(s.keys) > 0 {
		key := s.keys[0]
		s.mu.Unlock()
		return key, false, nil
	}
	s.mu.Unlock()

	secret, err := NewSecret()
	if err != nil {
		return Key{}, false, err
	}
	key, err := s.Join(secret, LocalOwner, "minted here")
	if err != nil {
		return Key{}, false, err
	}
	return key, true, nil
}

// Join adds a key a human supplied. Joining a fleet this daemon is already in is
// not an error and does not duplicate the entry — the ceremony is idempotent, so
// pasting twice is harmless rather than confusing.
func (s *Store) Join(secret, owner, label string) (Key, error) {
	normalized, err := NormalizeSecret(secret)
	if err != nil {
		return Key{}, err
	}
	if strings.TrimSpace(owner) == "" {
		owner = LocalOwner
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	keyID := KeyIDOf(normalized)
	for _, existing := range s.keys {
		if existing.KeyID == keyID {
			return existing, nil
		}
	}
	key := Key{
		KeyID:     keyID,
		Secret:    normalized,
		Owner:     owner,
		Label:     strings.TrimSpace(label),
		CreatedAt: s.now().UTC().Format(time.RFC3339),
	}
	s.keys = append(s.keys, key)
	if err := s.saveLocked(); err != nil {
		return Key{}, err
	}
	return key, nil
}

// Forget drops a key. Devices that already enrolled under it keep working —
// their tokens are independent — so this gates future enrolments only.
func (s *Store) Forget(keyID string) (bool, error) {
	keyID = strings.TrimSpace(keyID)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, key := range s.keys {
		if key.KeyID != keyID {
			continue
		}
		s.keys = append(s.keys[:i], s.keys[i+1:]...)
		if err := s.saveLocked(); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// Keys returns a copy, secrets included. Callers outside this package should
// prefer KeyIDs; this exists for the CLI that has to show a human their key.
func (s *Store) Keys() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, len(s.keys))
	copy(out, s.keys)
	return out
}

// KeyIDs is the non-secret identity of each fleet this daemon belongs to. It is
// safe to advertise: it is a one-way hash of a 128-bit secret, and publishing it
// is what lets a client recognise a machine as one of its own and enrol without
// asking anybody anything.
func (s *Store) KeyIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.keys))
	for _, key := range s.keys {
		out = append(out, key.KeyID)
	}
	return out
}

// Has reports whether this daemon holds any key at all.
func (s *Store) Has() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys) > 0
}

// Verify checks an enrolment proof against every key this daemon holds and
// returns the key that matched plus the device token both sides derive.
//
// The comparison is constant-time and the loop deliberately does not stop early
// on a match, so the time taken says nothing about which key was right or how
// many this daemon holds.
func (s *Store) Verify(serverNonce, clientNonce, machineID, proof string) (Key, string, bool) {
	s.mu.Lock()
	keys := make([]Key, len(s.keys))
	copy(keys, s.keys)
	s.mu.Unlock()

	var matched Key
	found := false
	for _, key := range keys {
		expected := Proof(key.Secret, serverNonce, clientNonce, machineID)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(proof)) == 1 && !found {
			matched = key
			found = true
		}
	}
	if !found {
		return Key{}, "", false
	}
	return matched, Token(matched.Secret, serverNonce, clientNonce, machineID), true
}

// Proof is what a client sends to show it holds the key without revealing it.
func Proof(secret, serverNonce, clientNonce, machineID string) string {
	return derive(secret, proofContext, serverNonce, clientNonce, machineID)
}

// Token is the device token both ends compute independently. It is never sent,
// which is the property that makes enrolment safe on a plaintext network.
func Token(secret, serverNonce, clientNonce, machineID string) string {
	return derive(secret, tokenContext, serverNonce, clientNonce, machineID)
}

func derive(secret, context, serverNonce, clientNonce, machineID string) string {
	raw, err := decodeSecret(secret)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, raw)
	for _, part := range []string{context, serverNonce, clientNonce, machineID} {
		mac.Write([]byte(part))
		// A separator, so that moving a character across a boundary cannot
		// produce the same input from different fields.
		mac.Write([]byte{0})
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// CertProof lets a client holding the fleet key check that the certificate it is
// actually talking to belongs to this fleet, rather than trusting the first one
// it ever saw (E5).
//
// Trust-on-first-use is only as good as the first use: an attacker present for
// that connection is believed forever. Binding the fingerprint to the server
// nonce under the fleet key removes that window entirely — a swapped
// certificate has a different fingerprint, and forging the proof needs the key.
func CertProof(secret, serverNonce, fingerprint string) string {
	if strings.TrimSpace(fingerprint) == "" {
		return ""
	}
	return derive(secret, certContext, serverNonce, fingerprint, "")
}

// KeyIDOf is the public name of a key: a one-way hash under its own domain
// separator, so it can be advertised without weakening the secret.
func KeyIDOf(secret string) string {
	normalized, err := NormalizeSecret(secret)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(keyIDContext + "\x00" + normalized))
	return hex.EncodeToString(sum[:])[:16]
}

// NewSecret mints a fleet key.
func NewSecret() (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("mint fleet key: %w", err)
	}
	return KeyPrefix + strings.ToLower(secretEncoding.EncodeToString(raw)), nil
}

// NormalizeSecret accepts what a human actually types — any case, with spaces or
// dashes anywhere, with or without the prefix — and returns the canonical form.
// A key gets read off one screen and typed into another; being strict about
// whitespace would only ever punish the person doing the typing.
func NormalizeSecret(secret string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' || r == '\n' || r == '\r' || r == '_' {
			return -1
		}
		return r
	}, strings.TrimSpace(secret))
	cleaned = strings.ToLower(cleaned)
	// Order is load-bearing and the natural way to write it is wrong: separators
	// are gone by now, so the prefix to trim is "wf", two characters, not "wf-".
	// Trim "wf-" from the raw string first and "wf <body>" — a space typed where
	// the dash belongs — keeps its "wf" and becomes a different key.
	cleaned = strings.TrimPrefix(cleaned, strings.TrimSuffix(KeyPrefix, "-"))
	if cleaned == "" {
		return "", fmt.Errorf("%w: it is empty", ErrMalformedKey)
	}
	raw, err := secretEncoding.DecodeString(strings.ToUpper(cleaned))
	if err != nil {
		return "", fmt.Errorf("%w: unreadable characters", ErrMalformedKey)
	}
	if len(raw) != secretBytes {
		return "", fmt.Errorf("%w: wrong length", ErrMalformedKey)
	}
	return KeyPrefix + cleaned, nil
}

func decodeSecret(secret string) ([]byte, error) {
	normalized, err := NormalizeSecret(secret)
	if err != nil {
		return nil, err
	}
	return secretEncoding.DecodeString(strings.ToUpper(strings.TrimPrefix(normalized, KeyPrefix)))
}

// NewNonce mints one side's contribution to an enrolment.
func NewNonce() (string, error) {
	raw := make([]byte, NonceBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// saveLocked writes atomically with owner-only permissions. This file is the
// fleet: a torn one locks every machine out at once, and a world-readable one
// hands the fleet to any other account on the box.
func (s *Store) saveLocked() error {
	payload := struct {
		Version int   `json:"version"`
		Keys    []Key `json:"keys"`
	}{Version: 1, Keys: s.keys}
	data, err := json.MarshalIndent(payload, "", "  ")
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
	return nil
}
