package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	providercontract "workass/internal/provider"
)

// nativeSessionForker is the part of a provider fork strategy that copies a
// native session into a NEW resumable session without sampling a turn. It
// backs forked subsessions and forked subagents (user 2026-10-16): the child
// starts as its own native thread that already holds the parent's history.
type nativeSessionForker interface {
	ForkNativeSession(ctx context.Context, launch ProviderConfig, sourceSessionID, dir string, before nativeForkBoundary) (string, error)
}

// nativeForkBoundary cuts a fork right before the user prompt of one Workass
// turn. SessionIDs are the native ids the turn may have been sent under (the
// lane's root and its current head); a zero boundary copies everything.
type nativeForkBoundary struct {
	OperationID string
	SessionIDs  []string
}

// SubsessionForkRequest names the parent's exact native head and the child's
// exact lane options (as the chat actor derives them for the child).
type SubsessionForkRequest struct {
	SourceChatID    string
	SourceSessionID string
	Child           SessionOptions
	// BeforeOperationID cuts the copy right before the user prompt of that
	// Workass turn; empty copies the whole session.
	BeforeOperationID string
}

// NativeSessionForkSupported reports whether a provider can start a child as a
// native fork of a parent session.
func NativeSessionForkSupported(providerID string) bool {
	_, ok := forkStrategy(providerID).(nativeSessionForker)
	return ok
}

// ForkSubsessionSession copies the parent's pinned native head into a new
// native session and returns its id, binding nothing (review
// wa-subagent-1792180415563-2, corrections 1-2): the caller pins the id in the
// child's parent link BEFORE the child exists, so every check that can refuse
// runs first and a retry re-binds the same copy instead of forking the
// parent's later history.
func (m *Manager) ForkSubsessionSession(ctx context.Context, req SubsessionForkRequest) (string, error) {
	child, source, err := m.subsessionForkSource(req)
	if err != nil {
		return "", err
	}
	before := nativeForkBoundary{}
	if strings.TrimSpace(req.BeforeOperationID) != "" {
		before = nativeForkBoundary{OperationID: req.BeforeOperationID, SessionIDs: []string{source.SessionID, strings.TrimSpace(req.SourceSessionID)}}
	}
	// Fork exactly the pinned head, never a later one the parent rotated to.
	forked, err := m.forkNativeSessionCopy(ctx, child.ProviderID, strings.TrimSpace(req.SourceSessionID), source.CWD, before)
	if err != nil {
		return "", err
	}
	m.opts.Logf("acp native subsession fork", map[string]any{
		"chatId": child.ChatID, "parentChatId": strings.TrimSpace(req.SourceChatID), "providerId": child.ProviderID,
	})
	return forked, nil
}

// BindForkedSubsessionLane records an already-forked native session as the
// child's committed lane binding, so lane selection resumes it exactly. No
// provider call: idempotent for the same session, a refusal for any other.
func (m *Manager) BindForkedSubsessionLane(req SubsessionForkRequest, forkedSessionID string) error {
	child, source, err := m.subsessionForkSource(req)
	if err != nil {
		return err
	}
	forkedSessionID = strings.ToLower(strings.TrimSpace(forkedSessionID))
	if forkedSessionID == "" {
		return errors.New("a forked subsession needs its pinned native fork")
	}
	ledger := m.nativeSessions
	unlock := ledger.lock(child)
	defer unlock()
	if existing, exists := ledger.getForOptions(child); exists {
		if existing.ThreadCommitted && existing.SessionID == forkedSessionID {
			return nil
		}
		return errors.New("the subsession lane is already bound to a different provider session")
	}
	return ledger.put(nativeSessionBinding{
		TabID: child.TabID, ChatID: child.ChatID, ProviderID: child.ProviderID,
		MachineID: source.MachineID, AccountScope: source.AccountScope, InstallScope: source.InstallScope,
		RealmVerified: source.RealmVerified, WorkspaceEpoch: string(sessionWorkspaceEpoch(child)),
		SessionID: forkedSessionID, ThreadCommitted: true, CWD: source.CWD,
		ModelID: firstNonEmpty(strings.TrimSpace(child.ModelID), source.ModelID),
		ModeID:  firstNonEmpty(strings.TrimSpace(child.ModeID), source.ModeID),
	})
}

// DiscardForkedSubsessionSession deletes a native copy no child references
// (the child chat could not be created).
func (m *Manager) DiscardForkedSubsessionSession(providerID, sessionID string) {
	m.discardNativeSessionCopy(normalizeProviderID(providerID), strings.TrimSpace(sessionID))
}

// subsessionForkSource validates a fork request and finds the parent's
// committed binding: the pinned head when it is still known, otherwise any
// committed lane of the parent on the same provider (same realm).
func (m *Manager) subsessionForkSource(req SubsessionForkRequest) (SessionOptions, nativeSessionBinding, error) {
	if m == nil || m.nativeSessions == nil || m.nativeSessions.path == "" {
		return SessionOptions{}, nativeSessionBinding{}, errors.New("durable provider lane storage is unavailable")
	}
	child := req.Child
	child.TabID, child.ChatID = strings.TrimSpace(child.TabID), strings.TrimSpace(child.ChatID)
	child.ProviderID = normalizeProviderID(child.ProviderID)
	sourceChatID, sourceSessionID := strings.TrimSpace(req.SourceChatID), strings.TrimSpace(req.SourceSessionID)
	if child.TabID == "" || child.ChatID == "" || sourceChatID == "" || sourceSessionID == "" || sourceChatID == child.ChatID {
		return SessionOptions{}, nativeSessionBinding{}, errors.New("a forked subsession needs the parent's native session and distinct child ids")
	}
	if !NativeSessionForkSupported(child.ProviderID) {
		return SessionOptions{}, nativeSessionBinding{}, unsupportedNativeSessionFork(child.ProviderID)
	}
	var source, fallback nativeSessionBinding
	found, sameProvider := false, false
	for _, binding := range m.nativeSessions.bindingsForChat(sourceChatID) {
		if binding.ProviderID != child.ProviderID || !binding.ThreadCommitted {
			continue
		}
		if bindingCurrentThreadID(binding) == sourceSessionID || binding.SessionID == sourceSessionID {
			source, found = binding, true
			break
		}
		if !sameProvider {
			fallback, sameProvider = binding, true
		}
	}
	if !found && sameProvider {
		source, found = fallback, true
	}
	if !found {
		return SessionOptions{}, nativeSessionBinding{}, errors.New("the parent chat's native session is not a committed lane on this machine")
	}
	if strings.TrimSpace(child.CWD) == "" || !sameFilesystemPath(child.CWD, source.CWD) {
		return SessionOptions{}, nativeSessionBinding{}, errors.New("a forked subsession must keep its parent's workspace")
	}
	return child, source, nil
}

func unsupportedNativeSessionFork(providerID string) error {
	return &providercontract.Error{
		Kind:    providercontract.ErrorUnsupportedCapability,
		Message: fmt.Sprintf("provider %q cannot fork a session natively; Workass never substitutes a transcript replay", providerID),
	}
}

// forkNativeSessionCopy copies sourceSessionID into a new native session with
// the provider's own launch environment and returns the new id.
func (m *Manager) forkNativeSessionCopy(ctx context.Context, providerID, sourceSessionID, dir string, before nativeForkBoundary) (string, error) {
	providerID = normalizeProviderID(providerID)
	forker, ok := forkStrategy(providerID).(nativeSessionForker)
	if !ok {
		return "", unsupportedNativeSessionFork(providerID)
	}
	cfg, err := m.providerConfig(providerID)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	opts, err := m.optionsForProviderLocked(providerID)
	m.mu.Unlock()
	if err != nil {
		return "", err
	}
	launch, err := providerAdapterForID(providerID).launch.Prepare(cfg, opts)
	if err != nil {
		return "", err
	}
	return forker.ForkNativeSession(ctx, launch, sourceSessionID, dir, before)
}

func (m *Manager) discardNativeSessionCopy(providerID, sessionID string) {
	strategy := forkStrategy(providerID)
	if strategy == nil {
		return
	}
	cfg, err := m.providerConfig(providerID)
	if err != nil {
		return
	}
	strategy.DiscardSession(strategy.NativeStore(effectiveProviderEnv(cfg)), sessionID)
}

// openForkedSession attaches a ledger-less (subagent) bridge to an existing
// native fork by exact resume — never session/new.
func (m *Manager) openForkedSession(ctx context.Context, opts SessionOptions, providerID string) (SessionInfo, error) {
	if m.nativeSessions != nil && m.nativeSessions.enabledFor(opts) {
		return SessionInfo{}, errors.New("a forked session opens only on a delegated child's own bridge")
	}
	bridge := m.getBridge(opts)
	if bridge == nil {
		if _, configErr := m.providerConfig(providerID); configErr != nil {
			return SessionInfo{}, configErr
		}
		return SessionInfo{}, fmt.Errorf("ACP provider bridge is unavailable: %s", providerID)
	}
	forked := strings.TrimSpace(opts.ForkedSessionID)
	info, _, err := bridge.RestoreSession(ctx, nativeSessionBinding{SessionID: forked, CWD: opts.CWD}, opts)
	if err != nil {
		return SessionInfo{}, err
	}
	info, err = m.applySessionStartupControls(ctx, opts, info)
	if err != nil {
		bridge.CloseSession(context.Background(), info.SessionID)
		return SessionInfo{}, err
	}
	m.mu.Lock()
	m.bindChatProviderLocked(opts, providerID)
	m.bindAgentOwnerLocked(opts.AgentOwnerKey, opts.ChatID, opts.TabID)
	m.mu.Unlock()
	m.initChatEnvForSession(ctx, opts, info)
	return info, nil
}
