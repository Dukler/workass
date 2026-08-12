package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ID is the stable Workass identifier for a provider implementation. It is
// configuration identity, never a capability probe or a vendor-family guess.
type ID string

type LaneID string
type OperationID string
type WorkspaceEpoch string

func NormalizeID(raw string) ID {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var out strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			out.WriteRune(r)
		case r == ' ', r == '.', r == '/':
			out.WriteByte('-')
		}
	}
	return ID(strings.Trim(out.String(), "-_"))
}

func normalizeIdentityPart(raw string) string {
	return strings.TrimSpace(raw)
}

// Realm identifies the provider account/install boundary without carrying any
// credential. AccountScope and InstallScope must be stable, non-secret labels
// or hashes supplied by the provider adapter.
type Realm struct {
	ProviderID   ID
	MachineID    string
	AccountScope string
	InstallScope string
	// Verified means the registered adapter attested the non-secret account and
	// installation scopes. It is ownership evidence, not a credential. The bit
	// is deliberately compared as part of LaneIdentity even though the opaque
	// scopes themselves remain the stable lane-id material.
	Verified bool
}

func (r Realm) Normalize() Realm {
	r.ProviderID = NormalizeID(string(r.ProviderID))
	r.MachineID = normalizeIdentityPart(r.MachineID)
	r.AccountScope = normalizeIdentityPart(r.AccountScope)
	r.InstallScope = normalizeIdentityPart(r.InstallScope)
	return r
}

func (r Realm) Validate() error {
	r = r.Normalize()
	if r.ProviderID == "" {
		return errors.New("provider realm requires provider id")
	}
	if r.MachineID == "" {
		return errors.New("provider realm requires machine id")
	}
	if r.Verified && (r.AccountScope == "" || r.AccountScope == "unverified-account" || r.InstallScope == "") {
		return errors.New("verified provider realm requires attested account and installation scopes")
	}
	return nil
}

// LaneIdentity is the immutable Workass ownership boundary for one native
// provider lineage.
type LaneIdentity struct {
	ID             LaneID
	ChatID         string
	Realm          Realm
	WorkspaceEpoch WorkspaceEpoch
}

func (l LaneIdentity) Normalize() LaneIdentity {
	l.ChatID = normalizeIdentityPart(l.ChatID)
	l.Realm = l.Realm.Normalize()
	l.WorkspaceEpoch = WorkspaceEpoch(normalizeIdentityPart(string(l.WorkspaceEpoch)))
	if l.ID == "" && l.ChatID != "" && l.WorkspaceEpoch != "" && l.Realm.Validate() == nil {
		l.ID = DeriveLaneID(l.ChatID, l.Realm, l.WorkspaceEpoch)
	}
	return l
}

func (l LaneIdentity) Validate() error {
	l = l.Normalize()
	if l.ChatID == "" {
		return errors.New("lane requires chat id")
	}
	if err := l.Realm.Validate(); err != nil {
		return err
	}
	if l.WorkspaceEpoch == "" {
		return errors.New("lane requires workspace epoch")
	}
	if l.ID == "" {
		return errors.New("lane requires lane id")
	}
	if expected := DeriveLaneID(l.ChatID, l.Realm, l.WorkspaceEpoch); l.ID != expected {
		return fmt.Errorf("lane id %q does not match immutable lane identity", l.ID)
	}
	return nil
}

func DeriveLaneID(chatID string, realm Realm, epoch WorkspaceEpoch) LaneID {
	realm = realm.Normalize()
	parts := []string{
		normalizeIdentityPart(chatID),
		string(realm.ProviderID),
		realm.MachineID,
		realm.AccountScope,
		realm.InstallScope,
		normalizeIdentityPart(string(epoch)),
	}
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(fmt.Sprintf("%d:", len(part))))
		_, _ = hash.Write([]byte(part))
	}
	return LaneID("lane-" + hex.EncodeToString(hash.Sum(nil)[:16]))
}

// ThreadRef names one provider-native lineage. RootID never changes. HeadID may
// advance only through an adapter-attested lineage event; a host restart is not
// such an event and must resume the exact current head.
type ThreadRef struct {
	ProviderID ID
	RootID     string
	HeadID     string
	Lineage    uint64
	Proof      string
}

func (r ThreadRef) Normalize() ThreadRef {
	r.ProviderID = NormalizeID(string(r.ProviderID))
	r.RootID = strings.TrimSpace(r.RootID)
	r.HeadID = strings.TrimSpace(r.HeadID)
	r.Proof = strings.TrimSpace(r.Proof)
	return r
}

func (r ThreadRef) Validate(expected ID) error {
	r = r.Normalize()
	if r.ProviderID == "" || r.RootID == "" || r.HeadID == "" {
		return errors.New("native thread reference is incomplete")
	}
	if expected = NormalizeID(string(expected)); expected != "" && r.ProviderID != expected {
		return fmt.Errorf("native thread provider %q does not match lane provider %q", r.ProviderID, expected)
	}
	if r.Lineage == 0 {
		return errors.New("native thread reference requires a positive lineage")
	}
	return nil
}

func (r ThreadRef) Equal(other ThreadRef) bool {
	r, other = r.Normalize(), other.Normalize()
	return r.ProviderID == other.ProviderID && r.RootID == other.RootID && r.HeadID == other.HeadID && r.Lineage == other.Lineage
}

func (r ThreadRef) SameLineage(other ThreadRef) bool {
	r, other = r.Normalize(), other.Normalize()
	return r.ProviderID != "" && r.ProviderID == other.ProviderID && r.RootID != "" && r.RootID == other.RootID
}

// CanAdvanceTo rejects silent native-id replacement. The trusted provider
// adapter must attest the new head and monotonic lineage generation.
func (r ThreadRef) CanAdvanceTo(next ThreadRef) bool {
	r, next = r.Normalize(), next.Normalize()
	return r.SameLineage(next) && next.HeadID != r.HeadID && next.Lineage > r.Lineage && next.Proof != ""
}

func (r ThreadRef) IsZero() bool {
	return strings.TrimSpace(r.RootID) == "" && strings.TrimSpace(r.HeadID) == ""
}

func NormalizeOperationID(raw string) OperationID {
	return OperationID(strings.TrimSpace(raw))
}
