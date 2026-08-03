// Package machineid gives a daemon a stable name it can prove.
//
// Every remote feature keys off this id: presence announcements, the pairing
// token store, and the machine book a client keeps. So the id is minted once
// per state dir from random bytes and is never derived from the hostname, the
// IP, or anything else the OS or the network can change underneath it. A
// machine that renames itself is still the same machine.
package machineid

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileName is the per-state-dir file that holds the minted identity.
const FileName = "machine-id.json"

// idPrefix keeps a machine id recognisable on sight in logs and wire payloads,
// where it sits beside chat, tab, and session ids that look much the same.
const idPrefix = "m-"

// Identity is what a daemon answers with when asked who it is. DisplayName is
// the human label and may be changed freely; MachineID never changes once
// minted.
type Identity struct {
	MachineID   string `json:"machineId"`
	DisplayName string `json:"displayName"`
	CreatedAt   string `json:"createdAt"`
}

// ErrCorrupt reports an identity file that exists but cannot be read. It is
// deliberately not self-healing: silently minting a replacement would change
// this machine's identity behind the user's back and strand every device that
// had paired with the old one.
var ErrCorrupt = errors.New("machine identity file is unreadable")

// Load returns the identity for a state dir, minting and persisting one the
// first time. It is safe to call on every start: an existing id is returned
// untouched.
func Load(stateDir string) (Identity, error) {
	path := filepath.Join(stateDir, FileName)
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		var existing Identity
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			return Identity{}, fmt.Errorf("%w: %s: %v", ErrCorrupt, path, jsonErr)
		}
		existing.MachineID = strings.TrimSpace(existing.MachineID)
		if existing.MachineID == "" {
			return Identity{}, fmt.Errorf("%w: %s: no machineId", ErrCorrupt, path)
		}
		// An older file, or one hand-edited to drop the label, still has a
		// usable id — fill the label in without disturbing the id.
		if strings.TrimSpace(existing.DisplayName) == "" {
			existing.DisplayName = defaultDisplayName()
			if writeErr := write(path, existing); writeErr != nil {
				return existing, writeErr
			}
		}
		return existing, nil
	case errors.Is(err, os.ErrNotExist):
		minted, mintErr := mint()
		if mintErr != nil {
			return Identity{}, mintErr
		}
		if writeErr := write(path, minted); writeErr != nil {
			return Identity{}, writeErr
		}
		return minted, nil
	default:
		return Identity{}, err
	}
}

// SetDisplayName renames a machine without reissuing its id, so devices that
// already paired with it stay paired.
func SetDisplayName(stateDir, name string) (Identity, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Identity{}, errors.New("display name is empty")
	}
	current, err := Load(stateDir)
	if err != nil {
		return Identity{}, err
	}
	if current.DisplayName == name {
		return current, nil
	}
	current.DisplayName = name
	if writeErr := write(filepath.Join(stateDir, FileName), current); writeErr != nil {
		return Identity{}, writeErr
	}
	return current, nil
}

func mint() (Identity, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Identity{}, fmt.Errorf("mint machine id: %w", err)
	}
	return Identity{
		MachineID:   idPrefix + hex.EncodeToString(raw),
		DisplayName: defaultDisplayName(),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// defaultDisplayName seeds the label from the hostname once, at mint time. The
// label is persisted from then on, so a later hostname change leaves both the
// id and the name the user already recognises alone.
func defaultDisplayName() string {
	if host, err := os.Hostname(); err == nil {
		if host = strings.TrimSpace(host); host != "" {
			return host
		}
	}
	return "workass"
}

// write replaces the identity file atomically: a torn file on this path reads
// as ErrCorrupt and blocks the daemon's identity until a human looks at it.
func write(path string, identity Identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), FileName+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(tempName)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(tempName)
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		os.Remove(tempName)
		return err
	}
	return nil
}
