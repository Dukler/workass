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

type nativeOperationState string

const (
	nativeOperationDispatched nativeOperationState = "dispatched"
	nativeOperationConsumed   nativeOperationState = "consumed"
	nativeOperationTerminal   nativeOperationState = "terminal"
	nativeOperationAbsent     nativeOperationState = "absent"
)

type nativeOperationRecord struct {
	OperationID  string               `json:"operationId"`
	PromptDigest string               `json:"promptDigest"`
	State        nativeOperationState `json:"state"`
	NativeTurnID string               `json:"nativeTurnId,omitempty"`
	Status       string               `json:"status,omitempty"`
	ResultDigest string               `json:"resultDigest,omitempty"`
	UpdatedAt    string               `json:"updatedAt"`
}

// nativeSessionBinding is daemon-owned recovery metadata. It deliberately
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
	ProviderSessionID string                 `json:"providerSessionId,omitempty"`
	CWD               string                 `json:"cwd,omitempty"`
	ModelID           string                 `json:"modelId,omitempty"`
	ModeID            string                 `json:"modeId,omitempty"`
	PendingOperation  *nativeOperationRecord `json:"pendingOperation,omitempty"`
	LastOperation     *nativeOperationRecord `json:"lastOperation,omitempty"`
	Generation        uint64                 `json:"generation"`
	UpdatedAt         string                 `json:"updatedAt"`
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
	if err := upgradeProviderLaneStoreV8(ledger.path, ledger.machineID); err != nil {
		ledger.loadErr = err
		return ledger
	}
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
	for index, binding := range disk.Bindings {
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
	normalizeOperation := func(operation *nativeOperationRecord) (*nativeOperationRecord, error) {
		if operation == nil {
			return nil, nil
		}
		copy := *operation
		copy.OperationID = strings.TrimSpace(copy.OperationID)
		copy.PromptDigest = strings.TrimSpace(copy.PromptDigest)
		copy.NativeTurnID = strings.TrimSpace(copy.NativeTurnID)
		copy.Status = strings.TrimSpace(copy.Status)
		copy.ResultDigest = strings.TrimSpace(copy.ResultDigest)
		copy.UpdatedAt = strings.TrimSpace(copy.UpdatedAt)
		if copy.OperationID == "" || copy.PromptDigest == "" {
			return nil, errors.New("provider lane contains an incomplete delivery operation")
		}
		switch copy.State {
		case nativeOperationDispatched, nativeOperationConsumed, nativeOperationTerminal, nativeOperationAbsent:
		default:
			return nil, errors.New("provider lane contains an unknown delivery state")
		}
		return &copy, nil
	}
	var err error
	if binding.PendingOperation, err = normalizeOperation(binding.PendingOperation); err != nil {
		return nativeSessionBinding{}, err
	}
	if binding.LastOperation, err = normalizeOperation(binding.LastOperation); err != nil {
		return nativeSessionBinding{}, err
	}
	if binding.PendingOperation != nil && binding.PendingOperation.State != nativeOperationDispatched && binding.PendingOperation.State != nativeOperationConsumed {
		return nativeSessionBinding{}, errors.New("provider lane pending slot contains a terminal delivery operation")
	}
	if binding.LastOperation != nil && binding.LastOperation.State != nativeOperationTerminal && binding.LastOperation.State != nativeOperationAbsent {
		return nativeSessionBinding{}, errors.New("provider lane settled slot contains an unfinished delivery operation")
	}
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

func nativeWorkspaceEpoch(cwd string) providercontract.WorkspaceEpoch {
	clean := filepath.Clean(strings.TrimSpace(cwd))
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		clean = resolved
	}
	digest := sha256.Sum256([]byte(clean))
	return providercontract.WorkspaceEpoch("ws-" + hex.EncodeToString(digest[:16]))
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

func (l *nativeSessionLedger) matchingBindingsLocked(tabID, chatID, providerID, sessionID string) []struct {
	key     string
	binding nativeSessionBinding
} {
	_ = strings.TrimSpace(tabID) // compatibility parameter; never lane identity.
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
func (l *nativeSessionLedger) lock(tabID, chatID, providerID, cwd string) func() {
	if l == nil {
		return func() {}
	}
	identity := providercontract.LaneIdentity{
		ChatID: strings.TrimSpace(chatID),
		Realm: providercontract.Realm{
			ProviderID: providercontract.ID(normalizeProviderID(providerID)), MachineID: l.machineID,
			AccountScope: "unverified-account", InstallScope: "registered-" + normalizeProviderID(providerID),
		},
		WorkspaceEpoch: nativeWorkspaceEpoch(cwd),
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
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, "")
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
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, "")
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
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
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
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
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
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
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

func (l *nativeSessionLedger) markInFlight(
	tabID, chatID, providerID, sessionID string,
	generation uint64,
	modelID, modeID, operationID, promptDigest string,
) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
	if len(matches) != 1 || matches[0].binding.Generation != generation {
		return false
	}
	key, binding := matches[0].key, matches[0].binding
	operationID = strings.TrimSpace(operationID)
	promptDigest = strings.TrimSpace(promptDigest)
	if operationID == "" || promptDigest == "" || binding.PendingOperation != nil {
		return false
	}
	if strings.TrimSpace(modelID) != "" {
		binding.ModelID = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(modeID) != "" {
		binding.ModeID = strings.TrimSpace(modeID)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	binding.PendingOperation = &nativeOperationRecord{
		OperationID: operationID, PromptDigest: promptDigest, State: nativeOperationDispatched, UpdatedAt: now,
	}
	binding.UpdatedAt = now
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = matches[0].binding
		return false
	}
	return true
}

func (l *nativeSessionLedger) markOperationConsumed(tabID, chatID, providerID, sessionID, operationID, nativeTurnID string) bool {
	_, committed := l.markOperationConsumedAndCommit(tabID, chatID, providerID, sessionID, operationID, nativeTurnID)
	return committed
}

// markOperationConsumedAndCommit is the one atomic boundary that promotes a
// deferred provider candidate into a durable native thread. The operation
// receipt and ThreadCommitted flag reach disk together before the actor can
// observe either fact.
func (l *nativeSessionLedger) markOperationConsumedAndCommit(tabID, chatID, providerID, sessionID, operationID, nativeTurnID string) (nativeSessionBinding, bool) {
	if l == nil {
		return nativeSessionBinding{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
	if len(matches) != 1 {
		return nativeSessionBinding{}, false
	}
	key, binding := matches[0].key, matches[0].binding
	if binding.PendingOperation == nil || binding.PendingOperation.OperationID != strings.TrimSpace(operationID) {
		return nativeSessionBinding{}, false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := *binding.PendingOperation
	operation.State = nativeOperationConsumed
	operation.NativeTurnID = strings.TrimSpace(firstNonEmpty(nativeTurnID, operation.NativeTurnID))
	operation.UpdatedAt = now
	binding.PendingOperation = &operation
	binding.ThreadCommitted = true
	binding.UpdatedAt = now
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = matches[0].binding
		return nativeSessionBinding{}, false
	}
	return binding, true
}

func (l *nativeSessionLedger) settleOperation(
	tabID, chatID, providerID, sessionID, operationID, status, resultDigest, modelID, modeID string,
) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
	if len(matches) != 1 {
		return false
	}
	key, binding := matches[0].key, matches[0].binding
	if binding.PendingOperation == nil || binding.PendingOperation.OperationID != strings.TrimSpace(operationID) {
		return false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := *binding.PendingOperation
	operation.State = nativeOperationTerminal
	operation.Status = strings.TrimSpace(status)
	operation.ResultDigest = strings.TrimSpace(resultDigest)
	operation.UpdatedAt = now
	binding.PendingOperation = nil
	binding.LastOperation = &operation
	if strings.TrimSpace(modelID) != "" {
		binding.ModelID = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(modeID) != "" {
		binding.ModeID = strings.TrimSpace(modeID)
	}
	binding.UpdatedAt = now
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = matches[0].binding
		return false
	}
	return true
}

func (l *nativeSessionLedger) recordOperationReadback(
	tabID, chatID, providerID, sessionID, operationID, nativeTurnID, status string,
	found, consumed, terminal bool,
) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
	if len(matches) != 1 {
		return false
	}
	key, binding := matches[0].key, matches[0].binding
	if binding.PendingOperation == nil || binding.PendingOperation.OperationID != strings.TrimSpace(operationID) {
		return false
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	operation := *binding.PendingOperation
	if (!found && (consumed || terminal)) || (terminal && !consumed) {
		return false
	}
	if operation.State == nativeOperationConsumed && (!found || !consumed) {
		return false
	}
	if operation.NativeTurnID != "" && strings.TrimSpace(nativeTurnID) != "" && operation.NativeTurnID != strings.TrimSpace(nativeTurnID) {
		return false
	}
	operation.NativeTurnID = strings.TrimSpace(firstNonEmpty(nativeTurnID, operation.NativeTurnID))
	operation.Status = strings.TrimSpace(status)
	operation.UpdatedAt = now
	switch {
	case !found:
		operation.State = nativeOperationAbsent
		binding.PendingOperation = nil
		binding.LastOperation = &operation
	case terminal:
		operation.State = nativeOperationTerminal
		binding.PendingOperation = nil
		binding.LastOperation = &operation
	case consumed:
		operation.State = nativeOperationConsumed
		binding.PendingOperation = &operation
	default:
		binding.PendingOperation = &operation
	}
	binding.UpdatedAt = now
	l.bindings[key] = binding
	if err := l.writeLocked(); err != nil {
		l.bindings[key] = matches[0].binding
		return false
	}
	return true
}

func (l *nativeSessionLedger) delete(tabID, chatID, providerID, sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	matches := l.matchingBindingsLocked(tabID, chatID, providerID, sessionID)
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
		bindingCurrentThreadID(binding) != sessionID ||
		(binding.PendingOperation != nil && binding.PendingOperation.State == nativeOperationConsumed) {
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

type nativeOperationReadback struct {
	Found        bool
	Consumed     bool
	Terminal     bool
	NativeTurnID string
	Status       string
}

// supportsOperationReadback requires both halves of the exactly-once
// contract. A provider that can look up turns but did not durably bind the
// Workass operation id to its native input cannot reconcile the right input.
func (b *Bridge) supportsOperationReadback() bool {
	return b != nil && b.hasProviderCapability(
		"workassStableTurnInputV1",
	) && b.hasProviderCapability(
		"workassOperationReadbackV1",
	) && b.hasProviderCapability(
		"workassTurnReconcileRequest",
	)
}

func (b *Bridge) readbackOperation(ctx context.Context, sessionID, operationID string) (nativeOperationReadback, error) {
	if !b.supportsOperationReadback() {
		return nativeOperationReadback{}, providercontract.Unsupported(
			providercontract.OperationID(operationID),
			"the provider does not expose authoritative operation-specific readback",
		)
	}
	timeout := b.opts.PromptReconcileTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := b.request(readCtx, "_workass/turn/reconcile", map[string]any{
		"sessionId": sessionID, "clientUserMessageId": operationID,
	}, timeout)
	if err != nil {
		return nativeOperationReadback{}, err
	}
	found, foundOK := result["found"].(bool)
	consumed, consumedOK := result["consumed"].(bool)
	terminal, terminalOK := result["terminal"].(bool)
	if !foundOK || !consumedOK || !terminalOK {
		return nativeOperationReadback{}, errors.New("provider operation readback omitted required boolean receipts")
	}
	if (!found && (consumed || terminal)) || (terminal && !consumed) {
		return nativeOperationReadback{}, errors.New("provider operation readback returned contradictory receipts")
	}
	readback := nativeOperationReadback{
		Found: found, Consumed: consumed, Terminal: terminal,
		NativeTurnID: strings.TrimSpace(asString(result["turnId"])),
		Status:       strings.TrimSpace(asString(result["status"])),
	}
	if found && readback.NativeTurnID == "" {
		return nativeOperationReadback{}, errors.New("provider operation readback found input without a native turn id")
	}
	return readback, nil
}

func (m *Manager) reconcilePendingNativeOperation(
	ctx context.Context,
	bridge *Bridge,
	binding nativeSessionBinding,
) (bool, error) {
	pending := binding.PendingOperation
	if pending == nil {
		return true, nil
	}
	currentThreadID := bindingCurrentThreadID(binding)
	readback, err := bridge.readbackOperation(ctx, currentThreadID, pending.OperationID)
	if err != nil {
		return false, err
	}
	if pending.State == nativeOperationConsumed && (!readback.Found || !readback.Consumed) {
		return false, errors.New("provider readback contradicted a durable input-consumed receipt")
	}
	if pending.NativeTurnID != "" && readback.NativeTurnID != "" && pending.NativeTurnID != readback.NativeTurnID {
		return false, errors.New("provider readback changed the native turn owner for one operation")
	}
	if !m.nativeSessions.recordOperationReadback(
		binding.TabID, binding.ChatID, binding.ProviderID, currentThreadID,
		pending.OperationID, readback.NativeTurnID, readback.Status,
		readback.Found, readback.Consumed, readback.Terminal,
	) {
		return false, errors.New("could not durably commit provider operation readback")
	}
	return !readback.Found || readback.Terminal, nil
}

func (m *Manager) tryRestoreNativeSession(ctx context.Context, opts SessionOptions) (SessionInfo, bool, error) {
	ledger := m.nativeSessions
	if !ledger.enabledFor(opts) {
		return SessionInfo{}, false, nil
	}
	binding, ok := ledger.getForWorkspace(opts.TabID, opts.ChatID, opts.ProviderID, opts.CWD)
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
	operationState := "clear"
	if binding.PendingOperation != nil {
		operationState = string(binding.PendingOperation.State)
		cleared, readbackErr := m.reconcilePendingNativeOperation(ctx, bridge, binding)
		if readbackErr != nil {
			m.opts.Logf("provider lane operation readback unavailable", map[string]any{
				"tabId": opts.TabID, "chatId": opts.ChatID, "providerId": opts.ProviderID,
				"operationId": binding.PendingOperation.OperationID,
				"error":       redactSensitiveText(readbackErr.Error()),
			})
		} else if cleared {
			operationState = "reconciled"
		} else {
			operationState = "provider-active"
		}
	}
	m.opts.Logf("acp native session restored", map[string]any{
		"tabId": opts.TabID, "providerId": opts.ProviderID, "method": method,
		"operationState": operationState,
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
		SessionID: info.SessionID, CWD: info.CWD,
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

type nativeTurnDispatch struct {
	TabID        string
	ChatID       string
	ProviderID   string
	SessionID    string
	OperationID  string
	PromptDigest string
}

func digestProviderPrompt(prompt []any) (string, error) {
	raw, err := json.Marshal(prompt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// prepareNativeTurn is the write-ahead admission boundary. It runs after the
// provider payload has been validated but before session/prompt is written to
// the host. Workass history is deliberately absent from this decision.
func (m *Manager) prepareNativeTurn(job *Job, operationID string, prompt []any) (*nativeTurnDispatch, error) {
	if m.nativeSessions == nil || m.nativeSessions.path == "" || job == nil || job.internal || job.TabID == "" || job.ProviderID == "" {
		return nil, nil
	}
	binding, ok := m.nativeSessions.getForWorkspace(job.TabID, job.ChatID, job.ProviderID, job.CWD)
	if !ok || bindingCurrentThreadID(binding) != job.SessionID {
		return nil, nil
	}
	if binding.PendingOperation != nil {
		return nil, &providercontract.Error{
			Kind:      providercontract.ErrorAcceptanceAmbiguous,
			Operation: providercontract.OperationID(binding.PendingOperation.OperationID),
			Message:   "the exact provider thread has an unresolved delivery operation; Workass will not resend or admit another prompt",
		}
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return nil, errors.New("provider-native turn requires a stable Workass operation id")
	}
	promptDigest, err := digestProviderPrompt(prompt)
	if err != nil {
		return nil, fmt.Errorf("digest provider-native prompt: %w", err)
	}
	if !m.nativeSessions.markInFlight(
		job.TabID, job.ChatID, job.ProviderID, job.SessionID, binding.Generation,
		job.startOpts.ModelID, job.startOpts.ModeID, operationID, promptDigest,
	) {
		return nil, errors.New("could not durably persist provider-native operation before dispatch")
	}
	return &nativeTurnDispatch{
		TabID: job.TabID, ChatID: job.ChatID, ProviderID: job.ProviderID,
		SessionID: job.SessionID, OperationID: operationID, PromptDigest: promptDigest,
	}, nil
}

func (m *Manager) finishNativeTurn(job *Job, dispatch *nativeTurnDispatch, result map[string]any) error {
	if m.nativeSessions == nil || dispatch == nil {
		return nil
	}
	resultReceipt := map[string]any{
		"stopReason": strings.TrimSpace(asString(result["stopReason"])),
		"output":     strings.TrimSpace(m.outputForJob(job)),
	}
	raw, err := json.Marshal(resultReceipt)
	if err != nil {
		return fmt.Errorf("digest provider-native terminal receipt: %w", err)
	}
	digest := sha256.Sum256(raw)
	status := firstNonEmpty(asString(result["stopReason"]), "terminal")
	modelID, modeID := "", ""
	if job != nil {
		modelID, modeID = job.startOpts.ModelID, job.startOpts.ModeID
	}
	if !m.nativeSessions.settleOperation(
		dispatch.TabID, dispatch.ChatID, dispatch.ProviderID, dispatch.SessionID,
		dispatch.OperationID, status, hex.EncodeToString(digest[:]), modelID, modeID,
	) {
		return nativeLaneError(
			providercontract.ErrorProtocolViolation,
			"the provider completed the turn, but Workass could not durably settle its delivery operation",
			nil,
		)
	}
	return nil
}
