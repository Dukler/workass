package acp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"workass/internal/durablefs"
	providercontract "workass/internal/provider"
)

const (
	nativeSessionLedgerFilename   = "provider-lanes.json"
	currentNativeLaneStoreVersion = 8
)

// nativeSessionBinding is daemon-owned exact-session metadata. It deliberately
// lives outside the renderer mirror: session:save may replace that mirror, but
// a browser must never be able to erase or cross-wire provider-native threads.
type nativeSessionBinding struct {
	TabID      string `json:"tabId"`
	ChatID     string `json:"chatId,omitempty"`
	ProviderID string `json:"providerId"`
	LaneID     string `json:"laneId"`
	MachineID  string `json:"machineId"`
	// AccountScope and InstallScope are opaque, non-secret realm identifiers.
	// Unverified records remain scoped to the provider installation and are never
	// guessed across accounts.
	AccountScope   string `json:"accountScope,omitempty"`
	InstallScope   string `json:"installScope,omitempty"`
	RealmVerified  bool   `json:"realmVerified,omitempty"`
	WorkspaceEpoch string `json:"workspaceEpoch"`
	SessionID      string `json:"sessionId"`
	// ThreadCommitted is the provider-durability receipt. False means SessionID
	// is only the exact candidate returned by a deferred-creation provider; it
	// must never be exposed to the chat actor as an established ThreadRef.
	ThreadCommitted bool   `json:"threadCommitted"`
	ThreadLineage   uint64 `json:"threadLineage"`
	LineageProof    string `json:"lineageProof,omitempty"`
	// The id the provider currently speaks under when it diverged from
	// SessionID (Claude's fork family: /clear, forkSession). Alias, never a
	// mutation of SessionID — live-session guards key on SessionID, and the
	// next successful restore folds this back into it.
	ProviderSessionID string `json:"providerSessionId,omitempty"`
	CWD               string `json:"cwd,omitempty"`
	ModelID           string `json:"modelId,omitempty"`
	ModeID            string `json:"modeId,omitempty"`
	// These v8 fields are accepted only so old profiles can be migrated. They
	// are cleared on load and never consulted: the provider harness owns turn
	// lifecycle, and stale Workass delivery state must not gate future prompts.
	LegacyPendingOperation json.RawMessage `json:"pendingOperation,omitempty"`
	LegacyLastOperation    json.RawMessage `json:"lastOperation,omitempty"`
	Generation             uint64          `json:"generation"`
	UpdatedAt              string          `json:"updatedAt"`
}

type nativeSessionLedgerFile struct {
	Version  int                    `json:"v"`
	Bindings []nativeSessionBinding `json:"bindings"`
}

type nativeSessionLedger struct {
	mu        sync.Mutex
	path      string
	bindings  map[string]nativeSessionBinding
	locks     map[string]*sync.Mutex
	machineID string
	loadErr   error
}

func newNativeSessionLedger(stateDir string, machineIDs ...string) *nativeSessionLedger {
	machineID := ""
	if len(machineIDs) > 0 {
		machineID = strings.TrimSpace(machineIDs[0])
	}
	if machineID == "" {
		machineID = fallbackNativeMachineScope(stateDir)
	}
	ledger := &nativeSessionLedger{
		bindings:  make(map[string]nativeSessionBinding),
		locks:     make(map[string]*sync.Mutex),
		machineID: machineID,
	}
	stateDir = strings.TrimSpace(stateDir)
	if stateDir == "" {
		return ledger
	}
	ledger.path = filepath.Join(stateDir, nativeSessionLedgerFilename)
	raw, err := os.ReadFile(ledger.path)
	if err != nil {
		if !os.IsNotExist(err) {
			ledger.loadErr = err
		}
		return ledger
	}
	var disk nativeSessionLedgerFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&disk); err != nil {
		ledger.loadErr = err
		return ledger
	}
	if disk.Version != currentNativeLaneStoreVersion {
		ledger.loadErr = fmt.Errorf("provider lane store schema v%d is unsupported", disk.Version)
		return ledger
	}
	ownerBySessionID := make(map[string]string)
	legacyTurnState := false
	for index, binding := range disk.Bindings {
		legacyTurnState = legacyTurnState || len(binding.LegacyPendingOperation) > 0 || len(binding.LegacyLastOperation) > 0
		binding, err = ledger.normalizeBinding(binding)
		if err != nil {
			ledger.loadErr = fmt.Errorf("provider lane store binding %d is invalid: %w", index, err)
			return ledger
		}
		key := nativeLaneStorageKey(binding.LaneID)
		if _, exists := ledger.bindings[key]; exists {
			ledger.loadErr = fmt.Errorf("provider lane store has duplicate immutable lane %q", binding.LaneID)
			return ledger
		}
		for _, providerThreadID := range bindingThreadIDs(binding) {
			providerSessionKey := binding.ProviderID + "\x00" + providerThreadID
			if owner, exists := ownerBySessionID[providerSessionKey]; exists && owner != key {
				ledger.loadErr = fmt.Errorf("provider-native thread %q has multiple immutable lane owners", providerThreadID)
				return ledger
			}
			ownerBySessionID[providerSessionKey] = key
		}
		ledger.bindings[key] = binding
	}
	if legacyTurnState {
		if err := ledger.writeLocked(); err != nil {
			ledger.loadErr = fmt.Errorf("retire legacy provider turn state: %w", err)
		}
	}
	return ledger
}

func (l *nativeSessionLedger) normalizeBinding(binding nativeSessionBinding) (nativeSessionBinding, error) {
	binding.TabID = strings.TrimSpace(binding.TabID)
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.ProviderID = normalizeProviderID(binding.ProviderID)
	binding.MachineID = strings.TrimSpace(firstNonEmpty(binding.MachineID, l.machineID))
	binding.AccountScope = strings.TrimSpace(binding.AccountScope)
	binding.InstallScope = strings.TrimSpace(binding.InstallScope)
	binding.WorkspaceEpoch = strings.TrimSpace(binding.WorkspaceEpoch)
	binding.SessionID = strings.TrimSpace(binding.SessionID)
	binding.LineageProof = strings.TrimSpace(binding.LineageProof)
	binding.ProviderSessionID = strings.TrimSpace(binding.ProviderSessionID)
	binding.CWD = strings.TrimSpace(binding.CWD)
	binding.ModelID = strings.TrimSpace(binding.ModelID)
	binding.ModeID = strings.TrimSpace(binding.ModeID)
	// Old builds persisted Workass-owned provider turn state here. It cannot be
	// authoritative after the transport exits, so discard it without changing
	// the exact session binding.
	binding.LegacyPendingOperation = nil
	binding.LegacyLastOperation = nil
	if binding.Generation == 0 {
		binding.Generation = 1
	}
	if binding.ThreadLineage == 0 {
		binding.ThreadLineage = 1
	}
	if !binding.ThreadCommitted && (binding.ProviderSessionID != "" || binding.ThreadLineage != 1 || binding.LineageProof != "") {
		return nativeSessionBinding{}, errors.New("provisional provider candidate cannot carry established lineage")
	}
	if binding.WorkspaceEpoch == "" && binding.CWD != "" {
		binding.WorkspaceEpoch = string(nativeWorkspaceEpoch(binding.CWD))
	}
	if binding.AccountScope == "" {
		binding.AccountScope = "unverified-account"
	}
	if binding.InstallScope == "" {
		binding.InstallScope = "registered-" + binding.ProviderID
	}
	if l.machineID != "" && binding.MachineID != "" && binding.MachineID != l.machineID {
		return nativeSessionBinding{}, errors.New("provider-native thread belongs to another machine")
	}
	identity := bindingLaneIdentity(binding)
	if err := identity.Validate(); err != nil {
		return nativeSessionBinding{}, fmt.Errorf("incomplete immutable lane identity: %w", err)
	} else if binding.LaneID == "" {
		binding.LaneID = string(identity.ID)
	} else if binding.LaneID != string(identity.ID) {
		return nativeSessionBinding{}, errors.New("stored lane id does not match immutable lane identity")
	}
	if binding.TabID == "" || binding.ChatID == "" || binding.ProviderID == "" || binding.SessionID == "" {
		return nativeSessionBinding{}, errors.New("provider lane requires tab, chat, provider, and native thread ids")
	}
	return binding, nil
}

func fallbackNativeMachineScope(stateDir string) string {
	abs, err := filepath.Abs(strings.TrimSpace(stateDir))
	if err != nil || strings.TrimSpace(abs) == "" {
		abs = "ephemeral"
	}
	digest := sha256.Sum256([]byte(filepath.Clean(abs)))
	return "state-" + hex.EncodeToString(digest[:8])
}

func canonicalNativeWorkspace(cwd string) string {
	clean := filepath.Clean(strings.TrimSpace(cwd))
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	return clean
}

func nativeWorkspaceEpoch(cwd string) providercontract.WorkspaceEpoch {
	clean := canonicalNativeWorkspace(cwd)
	digest := sha256.Sum256([]byte(clean))
	return providercontract.WorkspaceEpoch("ws-" + hex.EncodeToString(digest[:16]))
}

// WorkspaceEpochForRevision turns the actor's monotonic workspace revision
// into the immutable provider-neutral epoch required by a lane. Revision zero
// preserves the original path-derived identity so chats that have never moved
// continue to address their exact native thread. Every committed move gets a
// distinct epoch even when the chat later returns to the same directory.
func WorkspaceEpochForRevision(chatID, cwd string, revision uint64) providercontract.WorkspaceEpoch {
	if revision == 0 {
		return nativeWorkspaceEpoch(cwd)
	}
	parts := []string{
		"workass-workspace-epoch-v1",
		strings.TrimSpace(chatID),
		fmt.Sprintf("%d", revision),
		canonicalNativeWorkspace(cwd),
	}
	hash := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(hash, "%d:", len(part))
		_, _ = hash.Write([]byte(part))
	}
	return providercontract.WorkspaceEpoch("ws-" + hex.EncodeToString(hash.Sum(nil)[:16]))
}

func sessionWorkspaceEpoch(opts SessionOptions) providercontract.WorkspaceEpoch {
	if epoch := providercontract.WorkspaceEpoch(strings.TrimSpace(string(opts.WorkspaceEpoch))); epoch != "" {
		return epoch
	}
	return nativeWorkspaceEpoch(opts.CWD)
}

func bindingLaneIdentity(binding nativeSessionBinding) providercontract.LaneIdentity {
	return providercontract.LaneIdentity{
		ID:     providercontract.LaneID(strings.TrimSpace(binding.LaneID)),
		ChatID: strings.TrimSpace(binding.ChatID),
		Realm: providercontract.Realm{
			ProviderID: providercontract.ID(binding.ProviderID), MachineID: binding.MachineID,
			AccountScope: binding.AccountScope, InstallScope: binding.InstallScope, Verified: binding.RealmVerified,
		},
		WorkspaceEpoch: providercontract.WorkspaceEpoch(binding.WorkspaceEpoch),
	}.Normalize()
}

// nativeLaneStorageKey deliberately excludes TabID. A tab is a disposable
// renderer/transport attachment; the immutable lane id already binds the
// conversation, provider realm, machine, and workspace epoch.
func nativeLaneStorageKey(laneID string) string {
	return strings.TrimSpace(laneID)
}

func bindingCurrentThreadID(binding nativeSessionBinding) string {
	return firstNonEmpty(strings.TrimSpace(binding.ProviderSessionID), strings.TrimSpace(binding.SessionID))
}

func bindingThreadIDs(binding nativeSessionBinding) []string {
	root := strings.TrimSpace(binding.SessionID)
	head := bindingCurrentThreadID(binding)
	if root == "" {
		return nil
	}
	if head == "" || head == root {
		return []string{root}
	}
	return []string{root, head}
}

func (l *nativeSessionLedger) matchingBindingsLocked(chatID, providerID, sessionID string) []struct {
	key     string
	binding nativeSessionBinding
} {
	chatID = strings.TrimSpace(chatID)
	providerID = normalizeProviderID(providerID)
	sessionID = strings.TrimSpace(sessionID)
	var matches []struct {
		key     string
		binding nativeSessionBinding
	}
	for key, binding := range l.bindings {
		if binding.ChatID != chatID || binding.ProviderID != providerID {
			continue
		}
		if sessionID != "" && binding.SessionID != sessionID && bindingCurrentThreadID(binding) != sessionID {
			continue
		}
		matches = append(matches, struct {
			key     string
			binding nativeSessionBinding
		}{key: key, binding: binding})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].key < matches[j].key })
	return matches
}

func (l *nativeSessionLedger) enabledFor(opts SessionOptions) bool {
	return l != nil && l.path != "" && l.loadErr == nil &&
		strings.TrimSpace(opts.TabID) != "" && strings.TrimSpace(opts.ChatID) != "" &&
		strings.TrimSpace(opts.BridgeKey) == "" && !opts.Spare && !opts.Ephemeral
}

func (l *nativeSessionLedger) ownsSession(tabID, chatID, sessionID string) bool {
	if l == nil {
		return false
	}
	_, chatID, sessionID = strings.TrimSpace(tabID), strings.TrimSpace(chatID), strings.TrimSpace(sessionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, binding := range l.bindings {
		if binding.ChatID == chatID && (binding.SessionID == sessionID || bindingCurrentThreadID(binding) == sessionID) {
			return true
		}
	}
	return false
}

func (l *nativeSessionLedger) hasChat(tabID, chatID string) bool {
	if l == nil {
		return false
	}
	_, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, binding := range l.bindings {
		if binding.ChatID == chatID {
			return true
		}
	}
	return false
}

// lock serializes resume/new decisions for one logical chat+provider. Without
// it, two reconnecting views could both restore/create a native thread and the
// later disk write would silently orphan the first.
func (l *nativeSessionLedger) lock(opts SessionOptions) func() {
	if l == nil {
		return func() {}
	}
	identity := providercontract.LaneIdentity{
		ChatID: strings.TrimSpace(opts.ChatID),
		Realm: providercontract.Realm{
			ProviderID: providercontract.ID(normalizeProviderID(opts.ProviderID)), MachineID: l.machineID,
			AccountScope: "unverified-account", InstallScope: "registered-" + normalizeProviderID(opts.ProviderID),
		},
		WorkspaceEpoch: sessionWorkspaceEpoch(opts),
	}.Normalize()
	key := nativeLaneStorageKey(string(identity.ID))
	l.mu.Lock()
	lock := l.locks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		l.locks[key] = lock
	}
	l.mu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (l *nativeSessionLedger) get(tabID, chatID, providerID string) (nativeSessionBinding, bool) {
	if l == nil {
		return nativeSessionBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, "")
	if len(matches) == 0 {
		return nativeSessionBinding{}, false
	}
	if len(matches) != 1 {
		return nativeSessionBinding{}, false
	}
	return matches[0].binding, true
}

// getForWorkspace resolves one immutable lane epoch. A chat may retain several
// historical lanes for the same provider, so chat+provider alone is
// intentionally ambiguous once the workspace changes. Callers that are about
// to create or resume always have a canonical cwd and must use this lookup.
func (l *nativeSessionLedger) getForWorkspace(tabID, chatID, providerID, cwd string) (nativeSessionBinding, bool) {
	if l == nil {
		return nativeSessionBinding{}, false
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return l.get(tabID, chatID, providerID)
	}
	wanted := string(nativeWorkspaceEpoch(cwd))
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, "")
	filtered := matches[:0]
	for _, match := range matches {
		if match.binding.WorkspaceEpoch == wanted {
			filtered = append(filtered, match)
		}
	}
	if len(filtered) == 0 {
		return nativeSessionBinding{}, false
	}
	if len(filtered) != 1 {
		return nativeSessionBinding{}, false
	}
	return filtered[0].binding, true
}

func (l *nativeSessionLedger) getForWorkspaceEpoch(tabID, chatID, providerID string, epoch providercontract.WorkspaceEpoch) (nativeSessionBinding, bool) {
	if l == nil || strings.TrimSpace(string(epoch)) == "" {
		return nativeSessionBinding{}, false
	}
	wanted := strings.TrimSpace(string(epoch))
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, "")
	var found nativeSessionBinding
	count := 0
	for _, match := range matches {
		if match.binding.WorkspaceEpoch == wanted {
			found = match.binding
			count++
		}
	}
	return found, count == 1
}

func (l *nativeSessionLedger) getForSession(tabID, chatID, providerID, sessionID string) (nativeSessionBinding, bool) {
	if l == nil || strings.TrimSpace(sessionID) == "" {
		return nativeSessionBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return nativeSessionBinding{}, false
	}
	return matches[0].binding, true
}

func (l *nativeSessionLedger) getForOptions(opts SessionOptions) (nativeSessionBinding, bool) {
	if strings.TrimSpace(string(opts.WorkspaceEpoch)) != "" {
		return l.getForWorkspaceEpoch(opts.TabID, opts.ChatID, opts.ProviderID, opts.WorkspaceEpoch)
	}
	if strings.TrimSpace(opts.SessionID) != "" {
		if binding, ok := l.getForSession(opts.TabID, opts.ChatID, opts.ProviderID, opts.SessionID); ok {
			return binding, true
		}
	}
	return l.getForWorkspace(opts.TabID, opts.ChatID, opts.ProviderID, opts.CWD)
}

func (l *nativeSessionLedger) getForLane(identity providercontract.LaneIdentity) (nativeSessionBinding, bool) {
	if l == nil {
		return nativeSessionBinding{}, false
	}
	identity = identity.Normalize()
	if identity.ID == "" {
		return nativeSessionBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	binding, ok := l.bindings[nativeLaneStorageKey(string(identity.ID))]
	if !ok || bindingLaneIdentity(binding) != identity {
		return nativeSessionBinding{}, false
	}
	return binding, true
}

func (l *nativeSessionLedger) bindingsForChat(chatID string) []nativeSessionBinding {
	if l == nil || strings.TrimSpace(chatID) == "" {
		return nil
	}
	chatID = strings.TrimSpace(chatID)
	l.mu.Lock()
	bindings := make([]nativeSessionBinding, 0)
	for _, binding := range l.bindings {
		if binding.ChatID == chatID {
			bindings = append(bindings, binding)
		}
	}
	l.mu.Unlock()
	sort.Slice(bindings, func(i, j int) bool {
		left, right := nativeLaneStorageKey(bindings[i].LaneID), nativeLaneStorageKey(bindings[j].LaneID)
		if left != right {
			return left < right
		}
		return bindingCurrentThreadID(bindings[i]) < bindingCurrentThreadID(bindings[j])
	})
	return bindings
}

func (l *nativeSessionLedger) put(binding nativeSessionBinding) error {
	if l == nil {
		return nil
	}
	var err error
	binding, err = l.normalizeBinding(binding)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeLaneStorageKey(binding.LaneID)
	for ownerKey, existing := range l.bindings {
		if ownerKey == key || existing.ProviderID != binding.ProviderID {
			continue
		}
		for _, candidateID := range bindingThreadIDs(binding) {
			for _, existingID := range bindingThreadIDs(existing) {
				if candidateID == existingID {
					return fmt.Errorf("provider-native session %q is already owned by conversation %q", candidateID, existing.ChatID)
				}
			}
		}
	}
	if previous, ok := l.bindings[key]; ok && binding.Generation <= previous.Generation {
		binding.Generation = previous.Generation + 1
	}
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	return l.writeLocked()
}

// adoptProviderSession records the provider's announced session id (Claude's
// fork family: /clear, forkSession, conversation_reset). Resuming the OLD id
// after a fork silently freezes context at the fork point — turns answered
// under the forked id fall out of the model's memory (hostile-fixture
// finding, 2026-07-28).
func (l *nativeSessionLedger) adoptProviderSession(
	tabID, chatID, providerID, sessionID, previousProviderSessionID, providerSessionID string,
	lineageGeneration uint64,
	lineageProof string,
) bool {
	if l == nil {
		return false
	}
	providerSessionID = strings.TrimSpace(providerSessionID)
	previousProviderSessionID = strings.TrimSpace(previousProviderSessionID)
	lineageProof = strings.TrimSpace(lineageProof)
	if providerSessionID == "" || previousProviderSessionID == "" || lineageGeneration == 0 || lineageProof == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return false
	}
	key, binding := matches[0].key, matches[0].binding
	current := firstNonEmpty(binding.ProviderSessionID, binding.SessionID)
	if previousProviderSessionID != current || lineageGeneration != binding.ThreadLineage+1 || providerSessionID == current {
		return false
	}
	next := providerSessionID
	if next == binding.SessionID {
		next = ""
	}
	if binding.ProviderSessionID == next {
		return false
	}
	binding.ProviderSessionID = next
	binding.ThreadLineage = lineageGeneration
	binding.LineageProof = lineageProof
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	_ = l.writeLocked()
	return true
}

// materializeActorLineage updates the adapter lookup only after the chat actor
// has durably accepted the exact lineage edge. It is deliberately not an
// independent authority: the lane identity and both thread endpoints must
// match the existing binding byte-for-byte.
func (l *nativeSessionLedger) materializeActorLineage(identity providercontract.LaneIdentity, from, to providercontract.ThreadRef) error {
	if l == nil {
		return errors.New("native session ledger is unavailable")
	}
	identity, from, to = identity.Normalize(), from.Normalize(), to.Normalize()
	if err := identity.Validate(); err != nil {
		return err
	}
	if !from.CanAdvanceTo(to) {
		return nativeLaneError(providercontract.ErrorNativeIdentityConflict, "actor lineage materialization is not monotonic", nil)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeLaneStorageKey(string(identity.ID))
	binding, ok := l.bindings[key]
	if !ok || bindingLaneIdentity(binding) != identity {
		return nativeLaneError(providercontract.ErrorNativeIdentityConflict, "actor lineage materialization does not match the native lane", nil)
	}
	current := bindingThreadRef(binding)
	if current.Equal(to) {
		return nil
	}
	if !current.Equal(from) {
		return nativeLaneError(providercontract.ErrorNativeIdentityConflict, "native lineage materialization starts at another head", nil)
	}
	binding.ProviderSessionID = to.HeadID
	if to.HeadID == binding.SessionID {
		binding.ProviderSessionID = ""
	}
	binding.ThreadLineage = to.Lineage
	binding.LineageProof = to.Proof
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		return fmt.Errorf("persist actor lineage materialization: %w", err)
	}
	return nil
}

func (l *nativeSessionLedger) updateControls(tabID, chatID, providerID, sessionID, modelID, modeID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return
	}
	key, binding := matches[0].key, matches[0].binding
	if strings.TrimSpace(modelID) != "" {
		binding.ModelID = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(modeID) != "" {
		binding.ModeID = strings.TrimSpace(modeID)
	}
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	_ = l.writeLocked()
}

// updateAttachment changes only disposable routing metadata for an immutable
// lane. It deliberately does not advance Generation: moving the same ChatID to
// another renderer tab is not a provider operation and must not invalidate a
// pending delivery receipt.
func (l *nativeSessionLedger) updateAttachment(tabID, chatID, providerID, sessionID string) bool {
	if l == nil || strings.TrimSpace(tabID) == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return false
	}
	key, binding := matches[0].key, matches[0].binding
	if binding.TabID == strings.TrimSpace(tabID) {
		return true
	}
	previous := binding
	binding.TabID = strings.TrimSpace(tabID)
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = previous
		return false
	}
	return true
}

// commitThread promotes a deferred provider candidate after the harness emits
// its ordinary input-consumed signal. No Workass turn record is created: the
// durable binding contains only the exact provider session identity.
func (l *nativeSessionLedger) commitThread(tabID, chatID, providerID, sessionID string) (nativeSessionBinding, bool) {
	if l == nil {
		return nativeSessionBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return nativeSessionBinding{}, false
	}
	key, binding := matches[0].key, matches[0].binding
	if binding.ThreadCommitted {
		return binding, true
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	binding.ThreadCommitted = true
	binding.UpdatedAt = now
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = matches[0].binding
		return nativeSessionBinding{}, false
	}
	return binding, true
}

func (l *nativeSessionLedger) delete(tabID, chatID, providerID, sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(chatID, providerID, sessionID)
	if len(matches) != 1 {
		return
	}
	delete(l.bindings, matches[0].key)
	_ = l.writeLocked()
}

// removeAbsentCandidate removes exactly one deferred-creation receipt
// after the provider has authoritatively reported that no native thread exists.
// It cannot remove an established binding or a candidate whose input was
// consumed, and restores the prior map entry if the disk commit fails.
func (l *nativeSessionLedger) removeAbsentCandidate(identity providercontract.LaneIdentity, sessionID string) bool {
	if l == nil {
		return false
	}
	identity = identity.Normalize()
	sessionID = strings.TrimSpace(sessionID)
	if identity.ID == "" || sessionID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeLaneStorageKey(string(identity.ID))
	binding, ok := l.bindings[key]
	if !ok || bindingLaneIdentity(binding) != identity || binding.ThreadCommitted ||
		bindingCurrentThreadID(binding) != sessionID {
		return false
	}
	delete(l.bindings, key)
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = binding
		return false
	}
	return true
}

func (l *nativeSessionLedger) deleteChat(tabID, chatID string) {
	if l == nil {
		return
	}
	_, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for key, binding := range l.bindings {
		if binding.ChatID == chatID {
			delete(l.bindings, key)
			changed = true
		}
	}
	if changed {
		_ = l.writeLocked()
	}
}

func (l *nativeSessionLedger) writeLocked() error {
	return l.writeVersionLocked(currentNativeLaneStoreVersion)
}

func (l *nativeSessionLedger) writeVersionLocked(version int) error {
	if l.path == "" {
		return nil
	}
	keys := make([]string, 0, len(l.bindings))
	for key := range l.bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	disk := nativeSessionLedgerFile{Version: version, Bindings: make([]nativeSessionBinding, 0, len(keys))}
	for _, key := range keys {
		disk.Bindings = append(disk.Bindings, l.bindings[key])
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".provider-lanes-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return err
	}
	return durablefs.SyncDirectory(filepath.Dir(l.path))
}

func nativeLaneError(kind providercontract.ErrorKind, message string, cause error) error {
	return &providercontract.Error{
		Kind:      kind,
		Operation: providercontract.OperationID("resume-native-thread"),
		Message:   message,
		Cause:     cause,
	}
}

func nativeResumeError(err error) error {
	kind := providercontract.ErrorTransientTransport
	var rpcErr *acpError
	if errors.As(err, &rpcErr) && rpcErr.Code == -32044 {
		kind = providercontract.ErrorNativeThreadMissing
	}
	return nativeLaneError(kind, "could not resume the chat's exact provider-native thread", err)
}

func (m *Manager) tryRestoreNativeSession(ctx context.Context, opts SessionOptions) (SessionInfo, bool, error) {
	ledger := m.nativeSessions
	if !ledger.enabledFor(opts) {
		return SessionInfo{}, false, nil
	}
	binding, ok := ledger.getForOptions(opts)
	if !ok {
		return SessionInfo{}, false, nil
	}
	if binding.SessionID == "" {
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorNativeThreadMissing,
			"the chat's provider-native thread reference is empty",
			nil,
		)
	}
	// The provider-native thread was created in binding.CWD. Adapters commonly
	// accept a cwd on resume while continuing to execute in the original one, so
	// a requested workspace change must never be disguised as a resume. Workspace
	// moves create a new lane transaction; this established lane stays immutable.
	if requested := strings.TrimSpace(opts.CWD); requested != "" && strings.TrimSpace(binding.CWD) != "" && !sameFilesystemPath(requested, binding.CWD) {
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorNativeIdentityConflict,
			"the requested workspace does not match the provider lane's workspace epoch",
			nil,
		)
	}
	currentThreadID := bindingCurrentThreadID(binding)
	if live, ok := m.LiveSession(currentThreadID); ok {
		if live.TabID == opts.TabID && live.ChatID == opts.ChatID && live.Info.ProviderID == opts.ProviderID {
			return live.Info, true, nil
		}
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorNativeIdentityConflict,
			"the provider-native thread is already attached to a different chat lane",
			nil,
		)
	}

	bridge := m.getBridge(opts)
	if bridge == nil {
		if _, configErr := m.providerConfig(opts.ProviderID); configErr != nil {
			return SessionInfo{}, true, configErr
		}
		return SessionInfo{}, true, &providercontract.Error{
			Kind: providercontract.ErrorProviderUnavailable, Message: "provider bridge is unavailable for exact resume",
		}
	}
	if _, err := bridge.Initialize(ctx); err != nil {
		return SessionInfo{}, true, nativeLaneError(providercontract.ErrorTransientTransport, "could not start the provider host for exact attachment", err)
	}
	if !bridge.supportsExactSessionAttachment() {
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorUnsupportedCapability,
			"the provider does not support exact native-thread resume or same-id load",
			nil,
		)
	}
	info, method, err := bridge.RestoreSession(ctx, binding, opts)
	if err != nil {
		return SessionInfo{}, true, exactSessionAttachmentError(err)
	}
	if info.SessionID != currentThreadID {
		bridge.Close(false, errors.New("provider exact attachment returned a different native head"))
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorNativeIdentityConflict,
			"the provider exact-attachment reply changed the lane's native head without an attested lineage event",
			fmt.Errorf("expected %q, got %q", currentThreadID, info.SessionID),
		)
	}
	if binding.RealmVerified && info.ProviderRealmVerified &&
		(binding.AccountScope != strings.TrimSpace(info.ProviderAccountScope) || binding.InstallScope != strings.TrimSpace(info.ProviderInstallScope)) {
		bridge.Close(false, errors.New("provider realm changed during exact attachment"))
		return SessionInfo{}, true, nativeLaneError(
			providercontract.ErrorNativeIdentityConflict,
			"the provider account or installation no longer matches this chat lane",
			nil,
		)
	}
	bridge.markSeeded(info.SessionID)
	binding.TabID = opts.TabID
	binding.ChatID = firstNonEmpty(opts.ChatID, binding.ChatID)
	binding.CWD = info.CWD
	binding.ModelID = firstNonEmpty(strings.TrimSpace(opts.ModelID), binding.ModelID, stringPointer(info.CurrentModelID))
	binding.ModeID = firstNonEmpty(strings.TrimSpace(opts.ModeID), binding.ModeID, stringPointer(info.CurrentModeID))
	binding.ThreadCommitted = true
	if err := ledger.put(binding); err != nil {
		bridge.Close(false, err)
		return SessionInfo{}, true, nativeLaneError(providercontract.ErrorProtocolViolation, "could not persist the exact attached thread binding", err)
	}
	m.opts.Logf("acp native session restored", map[string]any{
		"tabId": opts.TabID, "providerId": opts.ProviderID, "method": method,
	})
	return info, true, nil
}

func (m *Manager) rememberNewNativeSession(opts SessionOptions, info SessionInfo) error {
	ledger := m.nativeSessions
	if !ledger.enabledFor(opts) {
		return nil
	}
	bridge := m.bridgeForSession(info.SessionID, opts)
	creation := providerAdapterForID(info.ProviderID).negotiatedCreationCapabilities(bridge)
	binding := nativeSessionBinding{
		TabID: opts.TabID, ChatID: opts.ChatID, ProviderID: info.ProviderID,
		SessionID: info.SessionID, CWD: info.CWD, WorkspaceEpoch: string(sessionWorkspaceEpoch(opts)),
		MachineID:       m.nativeSessions.machineID,
		AccountScope:    firstNonEmpty(strings.TrimSpace(info.ProviderAccountScope), "unverified-account"),
		InstallScope:    firstNonEmpty(strings.TrimSpace(info.ProviderInstallScope), "registered-"+normalizeProviderID(info.ProviderID)),
		RealmVerified:   info.ProviderRealmVerified,
		ThreadCommitted: !creation.DeferredUntilInput,
		ModelID:         firstNonEmpty(stringPointer(info.CurrentModelID)),
		ModeID:          firstNonEmpty(stringPointer(info.CurrentModeID)),
	}
	return ledger.put(binding)
}

func stringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
