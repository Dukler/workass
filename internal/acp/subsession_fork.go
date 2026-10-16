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

// ForkSubsessionLane forks the parent's native session and records the new
// session as the child's committed lane binding, so the child's first turn is
// an exact resume of its own fork. It is idempotent per child lane: a retry
// after a lost reply finds the committed binding and forks nothing.
func (m *Manager) ForkSubsessionLane(ctx context.Context, req SubsessionForkRequest) error {
	if m == nil || m.nativeSessions == nil || m.nativeSessions.path == "" {
		return errors.New("durable provider lane storage is unavailable")
	}
	child := req.Child
	child.TabID, child.ChatID = strings.TrimSpace(child.TabID), strings.TrimSpace(child.ChatID)
	providerID := normalizeProviderID(child.ProviderID)
	child.ProviderID = providerID
	sourceChatID, sourceSessionID := strings.TrimSpace(req.SourceChatID), strings.TrimSpace(req.SourceSessionID)
	if child.TabID == "" || child.ChatID == "" || sourceChatID == "" || sourceSessionID == "" || sourceChatID == child.ChatID {
		return errors.New("a forked subsession needs the parent's native session and distinct child ids")
	}
	if !NativeSessionForkSupported(providerID) {
		return unsupportedNativeSessionFork(providerID)
	}
	ledger := m.nativeSessions
	if existing, exists := ledger.getForOptions(child); exists {
		if existing.ThreadCommitted {
			return nil
		}
		return errors.New("the subsession lane already holds an uncommitted provider candidate")
	}
	var source nativeSessionBinding
	found := false
	for _, binding := range ledger.bindingsForChat(sourceChatID) {
		if binding.ProviderID == providerID && binding.ThreadCommitted &&
			(bindingCurrentThreadID(binding) == sourceSessionID || binding.SessionID == sourceSessionID) {
			source, found = binding, true
			break
		}
	}
	if !found {
		return errors.New("the parent chat's native session is not a committed lane on this machine")
	}
	if strings.TrimSpace(child.CWD) == "" || !sameFilesystemPath(child.CWD, source.CWD) {
		return errors.New("a forked subsession must keep its parent's workspace")
	}
	unlock := ledger.lock(child)
	defer unlock()
	if existing, exists := ledger.getForOptions(child); exists && existing.ThreadCommitted {
		return nil
	}
	before := nativeForkBoundary{}
	if strings.TrimSpace(req.BeforeOperationID) != "" {
		before = nativeForkBoundary{OperationID: req.BeforeOperationID, SessionIDs: []string{source.SessionID, sourceSessionID}}
	}
	// Fork exactly the pinned head, never a later one the parent rotated to.
	forked, err := m.forkNativeSessionCopy(ctx, providerID, sourceSessionID, source.CWD, before)
	if err != nil {
		return err
	}
	binding := nativeSessionBinding{
		TabID: child.TabID, ChatID: child.ChatID, ProviderID: providerID,
		MachineID: source.MachineID, AccountScope: source.AccountScope, InstallScope: source.InstallScope,
		RealmVerified: source.RealmVerified, WorkspaceEpoch: string(sessionWorkspaceEpoch(child)),
		SessionID: forked, ThreadCommitted: true, CWD: source.CWD,
		ModelID: firstNonEmpty(strings.TrimSpace(child.ModelID), source.ModelID),
		ModeID:  firstNonEmpty(strings.TrimSpace(child.ModeID), source.ModeID),
	}
	if err := ledger.put(binding); err != nil {
		m.discardNativeSessionCopy(providerID, forked)
		return err
	}
	m.opts.Logf("acp native subsession fork", map[string]any{
		"chatId": child.ChatID, "parentChatId": sourceChatID, "providerId": providerID,
	})
	return nil
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
