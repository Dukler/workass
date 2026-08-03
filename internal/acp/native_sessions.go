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
)

const (
	nativeSessionLedgerFilename = "native-sessions.json"
	nativeHistoryDigestVersion  = 2
)

// nativeSessionBinding is daemon-owned recovery metadata. It deliberately
// lives outside the renderer mirror: session:save may replace that mirror, but
// a browser must never be able to erase or cross-wire provider-native threads.
type nativeSessionBinding struct {
	TabID      string `json:"tabId"`
	ChatID     string `json:"chatId,omitempty"`
	ProviderID string `json:"providerId"`
	SessionID  string `json:"sessionId"`
	// The id the provider currently speaks under when it diverged from
	// SessionID (Claude's fork family: /clear, forkSession). Alias, never a
	// mutation of SessionID — live-session guards key on SessionID, and the
	// next successful restore folds this back into it.
	ProviderSessionID string `json:"providerSessionId,omitempty"`
	CWD               string `json:"cwd,omitempty"`
	ModelID           string `json:"modelId,omitempty"`
	ModeID            string `json:"modeId,omitempty"`
	SyncedMessages    int    `json:"syncedMessages"`
	HistoryHash       string `json:"historyHash"`
	HistoryVersion    int    `json:"historyVersion,omitempty"`
	Generation        uint64 `json:"generation"`
	ResumeSafe        bool   `json:"resumeSafe"`
	UpdatedAt         string `json:"updatedAt"`
}

type nativeSessionLedgerFile struct {
	Version  int                    `json:"v"`
	Bindings []nativeSessionBinding `json:"bindings"`
}

type nativeSessionLedger struct {
	mu       sync.Mutex
	path     string
	bindings map[string]nativeSessionBinding
	locks    map[string]*sync.Mutex
	loadErr  error
}

func newNativeSessionLedger(stateDir string) *nativeSessionLedger {
	ledger := &nativeSessionLedger{
		bindings: make(map[string]nativeSessionBinding),
		locks:    make(map[string]*sync.Mutex),
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
	if err := dec.Decode(&disk); err != nil {
		ledger.loadErr = err
		return ledger
	}
	ownerBySessionID := make(map[string]string)
	for _, binding := range disk.Bindings {
		if binding.HistoryVersion == 0 && disk.Version <= 1 {
			binding.HistoryVersion = 1
		}
		binding = normalizeNativeBinding(binding)
		if binding.TabID == "" || binding.ChatID == "" || binding.ProviderID == "" || binding.SessionID == "" {
			continue
		}
		key := nativeSessionKey(binding.TabID, binding.ProviderID)
		if prior, exists := ledger.bindings[key]; exists {
			ledger.loadErr = fmt.Errorf("duplicate native binding for tab %q provider %q (sessions %q and %q)", binding.TabID, binding.ProviderID, prior.SessionID, binding.SessionID)
			ledger.bindings = make(map[string]nativeSessionBinding)
			return ledger
		}
		if owner, exists := ownerBySessionID[binding.SessionID]; exists && owner != key {
			ledger.loadErr = fmt.Errorf("provider-native session %q has multiple chat owners", binding.SessionID)
			ledger.bindings = make(map[string]nativeSessionBinding)
			return ledger
		}
		ownerBySessionID[binding.SessionID] = key
		ledger.bindings[key] = binding
	}
	ledger.reconcileAuthoritativeMirror(stateDir)
	return ledger
}

func normalizeNativeBinding(binding nativeSessionBinding) nativeSessionBinding {
	binding.TabID = strings.TrimSpace(binding.TabID)
	binding.ChatID = strings.TrimSpace(binding.ChatID)
	binding.ProviderID = normalizeProviderID(binding.ProviderID)
	binding.SessionID = strings.TrimSpace(binding.SessionID)
	binding.CWD = strings.TrimSpace(binding.CWD)
	binding.ModelID = strings.TrimSpace(binding.ModelID)
	binding.ModeID = strings.TrimSpace(binding.ModeID)
	if binding.SyncedMessages < 0 {
		binding.SyncedMessages = 0
	}
	if binding.HistoryHash == "" && binding.SyncedMessages == 0 {
		binding.HistoryHash = historyDigest(nil)
	}
	if binding.HistoryVersion == 0 {
		binding.HistoryVersion = nativeHistoryDigestVersion
	}
	if binding.Generation == 0 {
		binding.Generation = 1
	}
	return binding
}

func nativeSessionKey(tabID, providerID string) string {
	return strings.TrimSpace(tabID) + "\x00" + normalizeProviderID(providerID)
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
	tabID, chatID, sessionID = strings.TrimSpace(tabID), strings.TrimSpace(chatID), strings.TrimSpace(sessionID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, binding := range l.bindings {
		if binding.TabID == tabID && binding.ChatID == chatID && binding.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (l *nativeSessionLedger) hasChat(tabID, chatID string) bool {
	if l == nil {
		return false
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, binding := range l.bindings {
		if binding.TabID == tabID && binding.ChatID == chatID {
			return true
		}
	}
	return false
}

// lock serializes resume/new decisions for one logical chat+provider. Without
// it, two reconnecting views could both restore/create a native thread and the
// later disk write would silently orphan the first.
func (l *nativeSessionLedger) lock(tabID, providerID string) func() {
	if l == nil {
		return func() {}
	}
	key := nativeSessionKey(tabID, providerID)
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
	binding, ok := l.bindings[nativeSessionKey(tabID, providerID)]
	if !ok || binding.TabID != strings.TrimSpace(tabID) || binding.ChatID != strings.TrimSpace(chatID) {
		return nativeSessionBinding{}, false
	}
	return binding, true
}

func (l *nativeSessionLedger) put(binding nativeSessionBinding) error {
	if l == nil {
		return nil
	}
	binding = normalizeNativeBinding(binding)
	if binding.TabID == "" || binding.ProviderID == "" || binding.SessionID == "" {
		return errors.New("native session binding requires tab, provider, and session ids")
	}
	if binding.ChatID == "" {
		return errors.New("native session binding requires a conversation id")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(binding.TabID, binding.ProviderID)
	for ownerKey, existing := range l.bindings {
		if ownerKey != key && existing.SessionID == binding.SessionID {
			return fmt.Errorf("provider-native session %q is already owned by tab %q conversation %q", binding.SessionID, existing.TabID, existing.ChatID)
		}
	}
	if previous, ok := l.bindings[key]; ok && binding.Generation <= previous.Generation {
		if previous.ChatID != binding.ChatID && previous.SessionID == binding.SessionID {
			return fmt.Errorf("provider reused native session %q for a different conversation owner", binding.SessionID)
		}
		// A mismatched conversation owner is never resumed (get requires an exact
		// match), but once a fresh provider session has been created for the
		// authoritative owner it replaces the stale slot with a new generation.
		binding.Generation = previous.Generation + 1
	}
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	return l.writeLocked()
}

func (l *nativeSessionLedger) updateCursor(tabID, chatID, providerID, sessionID string, generation uint64, history []historyMessage, modelID, modeID string, resumeSafe bool) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(tabID, providerID)
	binding, ok := l.bindings[key]
	if !ok || binding.ChatID != strings.TrimSpace(chatID) || binding.SessionID != strings.TrimSpace(sessionID) || binding.Generation != generation {
		return false
	}
	binding.SyncedMessages = len(history)
	binding.HistoryHash = historyDigest(history)
	binding.HistoryVersion = nativeHistoryDigestVersion
	if strings.TrimSpace(modelID) != "" {
		binding.ModelID = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(modeID) != "" {
		binding.ModeID = strings.TrimSpace(modeID)
	}
	binding.ResumeSafe = resumeSafe
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	return l.writeLocked() == nil
}

// adoptProviderSession records the provider's announced session id (Claude's
// fork family: /clear, forkSession, conversation_reset). Resuming the OLD id
// after a fork silently freezes context at the fork point — turns answered
// under the forked id fall out of the model's memory (hostile-fixture
// finding, 2026-07-28).
func (l *nativeSessionLedger) adoptProviderSession(tabID, chatID, providerID, sessionID, providerSessionID string) bool {
	if l == nil {
		return false
	}
	providerSessionID = strings.TrimSpace(providerSessionID)
	if providerSessionID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(tabID, providerID)
	binding, ok := l.bindings[key]
	if !ok || binding.ChatID != strings.TrimSpace(chatID) || (strings.TrimSpace(sessionID) != "" && binding.SessionID != strings.TrimSpace(sessionID)) {
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
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	_ = l.writeLocked()
	return true
}

func (l *nativeSessionLedger) updateControls(tabID, chatID, providerID, sessionID, modelID, modeID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(tabID, providerID)
	binding, ok := l.bindings[key]
	if !ok || binding.ChatID != strings.TrimSpace(chatID) || (strings.TrimSpace(sessionID) != "" && binding.SessionID != strings.TrimSpace(sessionID)) {
		return
	}
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

func (l *nativeSessionLedger) markInFlight(tabID, chatID, providerID, sessionID string, generation uint64, modelID, modeID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(tabID, providerID)
	binding, ok := l.bindings[key]
	if !ok || binding.ChatID != strings.TrimSpace(chatID) || binding.SessionID != strings.TrimSpace(sessionID) || binding.Generation != generation {
		return false
	}
	if strings.TrimSpace(modelID) != "" {
		binding.ModelID = strings.TrimSpace(modelID)
	}
	if strings.TrimSpace(modeID) != "" {
		binding.ModeID = strings.TrimSpace(modeID)
	}
	binding.ResumeSafe = false
	binding.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.bindings[key] = binding
	return l.writeLocked() == nil
}

func (l *nativeSessionLedger) delete(tabID, chatID, providerID, sessionID string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	key := nativeSessionKey(tabID, providerID)
	binding, ok := l.bindings[key]
	if !ok || binding.ChatID != strings.TrimSpace(chatID) || (strings.TrimSpace(sessionID) != "" && binding.SessionID != strings.TrimSpace(sessionID)) {
		return
	}
	delete(l.bindings, key)
	_ = l.writeLocked()
}

func (l *nativeSessionLedger) deleteChat(tabID, chatID string) {
	if l == nil {
		return
	}
	tabID, chatID = strings.TrimSpace(tabID), strings.TrimSpace(chatID)
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for key, binding := range l.bindings {
		if binding.TabID == tabID && binding.ChatID == chatID {
			delete(l.bindings, key)
			changed = true
		}
	}
	if changed {
		_ = l.writeLocked()
	}
}

func (l *nativeSessionLedger) writeLocked() error {
	if l.path == "" {
		return nil
	}
	keys := make([]string, 0, len(l.bindings))
	for key := range l.bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	disk := nativeSessionLedgerFile{Version: 2, Bindings: make([]nativeSessionBinding, 0, len(keys))}
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
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".native-sessions-*.tmp")
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
	return os.Rename(tmpName, l.path)
}

// reconcileAuthoritativeMirror removes bindings for tabs that no longer exist
// and bindings whose immutable Workass conversation id no longer matches. It is
// intentionally provider-agnostic: one chat may retain separate native threads
// for Codex and Claude, but every one must belong to the same tab+conversation.
// A missing/unreadable mirror is left untouched so native recovery can still be
// used in isolated embeddings and deterministic fixtures.
func (l *nativeSessionLedger) reconcileAuthoritativeMirror(stateDir string) {
	path := filepath.Join(strings.TrimSpace(stateDir), "session-state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snapshot map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&snapshot); err != nil {
		return
	}
	l.reconcileOwners(nativeChatOwners(snapshot))
}

func nativeChatOwners(snapshot map[string]any) map[string]string {
	owners := make(map[string]string)
	for _, raw := range anySlice(snapshot["chats"]) {
		chat := mapFromAny(raw)
		tabID := strings.TrimSpace(asString(chat["id"]))
		chatID := strings.TrimSpace(asString(chat["chatId"]))
		if tabID != "" && chatID != "" {
			owners[tabID] = chatID
		}
	}
	return owners
}

func (l *nativeSessionLedger) reconcileOwners(owners map[string]string) {
	if l == nil || l.path == "" || l.loadErr != nil || owners == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for key, binding := range l.bindings {
		if chatID, exists := owners[binding.TabID]; !exists || chatID != binding.ChatID {
			delete(l.bindings, key)
			changed = true
		}
	}
	if changed {
		_ = l.writeLocked()
	}
}

func historyDigest(history []historyMessage) string {
	hash := sha256.New()
	for _, message := range history {
		hash.Write([]byte(message.ID))
		hash.Write([]byte{0xfe})
		hash.Write([]byte(message.Role))
		hash.Write([]byte{0})
		hash.Write([]byte(message.Content))
		hash.Write([]byte{0xff})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func legacyHistoryDigest(history []historyMessage) string {
	hash := sha256.New()
	for _, message := range history {
		hash.Write([]byte(message.Role))
		hash.Write([]byte{0})
		hash.Write([]byte(message.Content))
		hash.Write([]byte{0xff})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func bindingMatchesHistoryPrefix(binding nativeSessionBinding, history []historyMessage) bool {
	if binding.SyncedMessages < 0 || binding.SyncedMessages > len(history) {
		return false
	}
	digest := historyDigest(history[:binding.SyncedMessages])
	if binding.HistoryVersion <= 1 {
		digest = legacyHistoryDigest(history[:binding.SyncedMessages])
	}
	return binding.HistoryHash == digest
}

func (m *Manager) tryRestoreNativeSession(ctx context.Context, opts SessionOptions) (SessionInfo, bool, nativeRestoreFallbackReason, error) {
	ledger := m.nativeSessions
	if !ledger.enabledFor(opts) || opts.ForceFresh {
		return SessionInfo{}, false, "", nil
	}
	binding, ok := ledger.get(opts.TabID, opts.ChatID, opts.ProviderID)
	if !ok {
		state := nativeRestoreFallbackReadyState()
		state.BindingFound = false
		return SessionInfo{}, false, nativeRestoreFallbackReasonFor(state), nil
	}
	if !binding.ResumeSafe || binding.SessionID == "" {
		state := nativeRestoreFallbackReadyState()
		state.ResumeSafe = false
		return SessionInfo{}, false, nativeRestoreFallbackReasonFor(state), nil
	}
	// The provider-native thread was created in binding.CWD. Adapters commonly
	// accept a cwd on resume while continuing to execute in the original one, so
	// a requested workspace change must never resume this binding. Start fresh;
	// the normal first-turn seed rebuilds context from canonical Workass history.
	if requested := strings.TrimSpace(opts.CWD); requested != "" && strings.TrimSpace(binding.CWD) != "" && !sameFilesystemPath(requested, binding.CWD) {
		state := nativeRestoreFallbackReadyState()
		state.CWDMatches = false
		return SessionInfo{}, false, nativeRestoreFallbackReasonFor(state), nil
	}
	if live, ok := m.LiveSession(binding.SessionID); ok && live.TabID == opts.TabID && live.Info.ProviderID == opts.ProviderID {
		return live.Info, true, "", nil
	}

	bridge := m.getBridge(opts)
	if _, err := bridge.Initialize(ctx); err != nil {
		return SessionInfo{}, true, "", err
	}
	if !bridge.supportsSessionResume() && !bridge.supportsSessionLoad() {
		state := nativeRestoreFallbackReadyState()
		state.RestoreSupported = false
		return SessionInfo{}, true, nativeRestoreFallbackReasonFor(state), errors.New("ACP provider does not support session resume or load")
	}
	info, method, err := bridge.RestoreSession(ctx, binding, opts)
	if err != nil {
		// Whether the resume RPC itself failed or a later attach step did, the
		// chat must degrade to the canonical replay fallback rather than hard-
		// fail the send; the caller closes this bridge, so a provider-side
		// attachment from a successful resume RPC dies with the process.
		state := nativeRestoreFallbackReadyState()
		state.RestoreAttempted = true
		state.RestoreFailed = true
		return SessionInfo{}, true, nativeRestoreFallbackReasonFor(state), err
	}
	history, historyErr := readAllChatHistory(m.opts.StateDir, opts.TabID)
	if historyErr != nil {
		bridge.CloseSession(context.Background(), info.SessionID)
		return SessionInfo{}, true, "", fmt.Errorf("read canonical Workass history: %w", historyErr)
	}
	if !bindingMatchesHistoryPrefix(binding, history) {
		bridge.CloseSession(context.Background(), info.SessionID)
		state := nativeRestoreFallbackReadyState()
		state.HistoryMatches = false
		return SessionInfo{}, true, nativeRestoreFallbackReasonFor(state), errors.New("provider-native history diverged from the Workass archive")
	}
	missing := history[binding.SyncedMessages:]
	if len(missing) > 0 {
		prompt, ok := buildNativeDeltaSeed(missing, historyCharBudget(JobStartOptions{}))
		if !ok {
			bridge.CloseSession(context.Background(), info.SessionID)
			state := nativeRestoreFallbackReadyState()
			state.HistoryMatches = false
			return SessionInfo{}, true, nativeRestoreFallbackReasonFor(state), errors.New("provider-native history delta exceeds the safe context budget")
		}
		if strings.TrimSpace(prompt) != "" {
			if _, err := bridge.promptSystem(ctx, info.SessionID, prompt); err != nil {
				bridge.CloseSession(context.Background(), info.SessionID)
				return SessionInfo{}, true, "", fmt.Errorf("provider-native delta sync failed: %w", err)
			}
		}
	}
	bridge.markSeeded(info.SessionID)
	binding.ChatID = firstNonEmpty(opts.ChatID, binding.ChatID)
	binding.SessionID = info.SessionID
	// The restore ran against the freshest id, so the alias is folded.
	binding.ProviderSessionID = ""
	binding.CWD = info.CWD
	binding.ModelID = firstNonEmpty(strings.TrimSpace(opts.ModelID), binding.ModelID, stringPointer(info.CurrentModelID))
	binding.ModeID = firstNonEmpty(strings.TrimSpace(opts.ModeID), binding.ModeID, stringPointer(info.CurrentModeID))
	binding.SyncedMessages = len(history)
	binding.HistoryHash = historyDigest(history)
	binding.HistoryVersion = nativeHistoryDigestVersion
	binding.ResumeSafe = true
	if err := ledger.put(binding); err != nil {
		bridge.CloseSession(context.Background(), info.SessionID)
		return SessionInfo{}, true, "", fmt.Errorf("persist restored native session binding: %w", err)
	}
	m.opts.Logf("acp native session restored", map[string]any{
		"tabId": opts.TabID, "providerId": opts.ProviderID, "method": method,
		"syncedMessages": len(history), "deltaMessages": len(missing),
	})
	return info, true, "", nil
}

func (m *Manager) rememberNewNativeSession(opts SessionOptions, info SessionInfo) error {
	ledger := m.nativeSessions
	if !ledger.enabledFor(opts) {
		return nil
	}
	binding := nativeSessionBinding{
		TabID: opts.TabID, ChatID: opts.ChatID, ProviderID: info.ProviderID,
		SessionID: info.SessionID, CWD: info.CWD,
		ModelID:     firstNonEmpty(stringPointer(info.CurrentModelID)),
		ModeID:      firstNonEmpty(stringPointer(info.CurrentModeID)),
		HistoryHash: historyDigest(nil), HistoryVersion: nativeHistoryDigestVersion, ResumeSafe: true,
	}
	return ledger.put(binding)
}

// ReconcileNativeSessionOwners is called after the daemon accepts a renderer
// session snapshot. It removes durable provider threads for deleted chats and
// refuses to carry a binding across an immutable conversation-id change.
func (m *Manager) ReconcileNativeSessionOwners(snapshot any) {
	if m == nil || m.nativeSessions == nil {
		return
	}
	m.nativeSessions.reconcileOwners(nativeChatOwners(mapFromAny(snapshot)))
}

func stringPointer(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func (m *Manager) prepareNativeTurn(ctx context.Context, bridge *Bridge, job *Job, opts JobStartOptions) error {
	if m.nativeSessions == nil || m.nativeSessions.path == "" || bridge == nil || job == nil || job.TabID == "" || job.ProviderID == "" {
		return nil
	}
	binding, ok := m.nativeSessions.get(job.TabID, job.ChatID, job.ProviderID)
	if !ok || binding.SessionID != job.SessionID {
		return nil
	}
	history := promptHistoryForTab(m.opts.StateDir, opts)
	job.nativeGeneration = binding.Generation
	job.nativeHistoryBefore = append([]historyMessage(nil), history...)
	if bridge.needsNativeSync(job.SessionID) {
		if !bindingMatchesHistoryPrefix(binding, history) {
			return errors.New("provider-native history cursor diverged from Workass")
		}
		missing := history[binding.SyncedMessages:]
		if len(missing) > 0 {
			prompt, fits := buildNativeDeltaSeed(missing, historyCharBudget(opts))
			if !fits {
				return errors.New("provider-native history delta exceeds the safe context budget")
			}
			if strings.TrimSpace(prompt) != "" {
				if _, err := bridge.promptSystem(ctx, job.SessionID, prompt); err != nil {
					return fmt.Errorf("provider-native delta sync failed: %w", err)
				}
			}
			if !m.nativeSessions.updateCursor(job.TabID, job.ChatID, job.ProviderID, job.SessionID, binding.Generation, history, opts.ModelID, opts.ModeID, true) {
				return errors.New("provider-native session generation changed during delta sync")
			}
		}
		bridge.finishNativeSync(job.SessionID)
	}
	// Write-ahead dirty bit: if the daemon dies after session/prompt reaches the
	// provider but before the terminal cursor commit, the next daemon must not
	// resume a provider thread that may be ahead of Workass's durable transcript.
	if !m.nativeSessions.markInFlight(job.TabID, job.ChatID, job.ProviderID, job.SessionID, job.nativeGeneration, opts.ModelID, opts.ModeID) {
		return errors.New("could not persist provider-native in-flight guard")
	}
	return nil
}

func (m *Manager) finishNativeTurn(job *Job) {
	if m.nativeSessions == nil || job == nil || job.nativeGeneration == 0 || job.TabID == "" || job.ProviderID == "" {
		return
	}
	history := append([]historyMessage(nil), job.nativeHistoryBefore...)
	resumeSafe := job.Status == "done" && strings.TrimSpace(job.Result) != ""
	if resumeSafe {
		prompt := strings.TrimSpace(firstNonEmpty(job.startOpts.Prompt, job.startOpts.Message))
		if prompt != "" {
			history = append(history, historyMessage{ID: job.startOpts.UserMessageID, Role: "user", Content: prompt, At: job.StartedAt})
		}
		history = append(history, historyMessage{ID: job.startOpts.AssistantMessageID, Role: "assistant", Content: strings.TrimSpace(job.Result), At: job.FinishedAt})
	}
	m.nativeSessions.updateCursor(
		job.TabID, job.ChatID, job.ProviderID, job.SessionID, job.nativeGeneration, history,
		job.startOpts.ModelID, job.startOpts.ModeID, resumeSafe,
	)
}

func buildNativeDeltaSeed(history []historyMessage, budget int) (string, bool) {
	if len(history) == 0 {
		return "", true
	}
	used := 0
	var lines []string
	for _, message := range history {
		content := sanitizeReplayContent(message.Content)
		if content == "" {
			continue
		}
		who := "User"
		if message.Role == "assistant" {
			who = "Assistant"
		}
		line := who + ": " + content
		used += len(line) + 2
		if used > budget {
			return "", false
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return "", true
	}
	return "WORKASS HISTORY DELTA SYNC v1\n" +
		"The following finalized messages exist in Workass after the last turn this native session saw. " +
		"Absorb them as conversation context. Do not continue the task and do not address the user; reply only with OK.\n\n" +
		"<workass_history_delta>\n" + strings.Join(lines, "\n\n") + "\n</workass_history_delta>", true
}

func readAllChatHistory(stateDir, tabID string) ([]historyMessage, error) {
	file := filepath.Join(strings.TrimSpace(stateDir), "chat-archive", safeArchiveName(tabID)+".jsonl")
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []historyMessage
	seen := make(map[string]struct{})
	err = visitJSONLRecords(f, func(line []byte) {
		var item map[string]any
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		if err := dec.Decode(&item); err != nil {
			return
		}
		message, ok := historyMessageFromMap(item)
		if !ok {
			return
		}
		key := historyMsgKey(message)
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		out = append(out, message)
	})
	return out, err
}
