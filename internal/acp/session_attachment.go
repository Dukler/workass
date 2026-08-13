package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	providercontract "workass/internal/provider"
)

// exactSessionAttachmentMethod is selected once from the ACP initialize
// handshake. It is deliberately not a retry list: one attachment call is sent,
// and a failure is returned without trying another method or creating a thread.
type exactSessionAttachmentMethod string

const (
	exactSessionResume exactSessionAttachmentMethod = "session/resume"
	exactSessionLoad   exactSessionAttachmentMethod = "session/load"
)

type exactSessionAttachment struct {
	method         exactSessionAttachmentMethod
	replaysHistory bool
}

func (b *Bridge) exactSessionAttachment() (exactSessionAttachment, bool) {
	if b == nil {
		return exactSessionAttachment{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	sessionCapabilities := mapFromAny(b.agentCaps["sessionCapabilities"])
	if raw, exists := sessionCapabilities["resume"]; exists && raw != nil {
		if _, valid := raw.(map[string]any); valid {
			return exactSessionAttachment{method: exactSessionResume}, true
		}
	}
	if load, valid := b.agentCaps["loadSession"].(bool); valid && load {
		return exactSessionAttachment{method: exactSessionLoad, replaysHistory: true}, true
	}
	return exactSessionAttachment{}, false
}

func (b *Bridge) supportsExactSessionAttachment() bool {
	_, ok := b.exactSessionAttachment()
	return ok
}

// supportsSessionResume remains a narrow capability query for native-host
// conformance. Generic ACP lane code uses supportsExactSessionAttachment.
func (b *Bridge) supportsSessionResume() bool {
	attachment, ok := b.exactSessionAttachment()
	return ok && attachment.method == exactSessionResume
}

func (b *Bridge) beginSessionLoad(sessionID string) {
	if b == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	b.mu.Lock()
	if b.loadingSessions == nil {
		b.loadingSessions = make(map[string]uint64)
	}
	b.loadingSessions[sessionID]++
	b.mu.Unlock()
}

func (b *Bridge) endSessionLoad(sessionID string) {
	if b == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	b.mu.Lock()
	if count := b.loadingSessions[sessionID]; count > 1 {
		b.loadingSessions[sessionID] = count - 1
	} else {
		delete(b.loadingSessions, sessionID)
	}
	b.mu.Unlock()
}

func (b *Bridge) sessionLoadInProgress(sessionID string) bool {
	if b == nil || strings.TrimSpace(sessionID) == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loadingSessions[sessionID] > 0
}

// RestoreSession attaches this disposable ACP process to the exact saved
// provider thread. Capability negotiation selects either stable ACP v1 resume
// or same-id load. There is no method retry and no session/new path here.
func (b *Bridge) RestoreSession(ctx context.Context, binding nativeSessionBinding, opts SessionOptions) (SessionInfo, string, error) {
	if _, err := b.Initialize(ctx); err != nil {
		return SessionInfo{}, "", err
	}
	attachment, ok := b.exactSessionAttachment()
	if !ok {
		return SessionInfo{}, "", providercontract.Unsupported("attach-native-thread", "ACP provider does not expose exact session attachment")
	}
	cwd := b.sessionCWD(firstNonEmpty(opts.CWD, binding.CWD))
	// A verified fork advances ProviderSessionID. That exact head, rather than
	// the immutable root id, is the session whose provider context is attached.
	sessionID := firstNonEmpty(binding.ProviderSessionID, binding.SessionID)
	mcpServers, err := b.sessionMCPServers(opts)
	if err != nil {
		return SessionInfo{}, string(attachment.method), err
	}
	params := map[string]any{
		"sessionId":  sessionID,
		"cwd":        cwd,
		"mcpServers": mcpServers,
	}
	releaseOwner := b.manager.provisionAgentOwner(opts)
	if attachment.replaysHistory {
		b.beginSessionLoad(sessionID)
		defer b.endSessionLoad(sessionID)
	}
	res, err := b.request(ctx, string(attachment.method), params, b.opts.InitTimeout)
	if err != nil {
		releaseOwner()
		return SessionInfo{}, string(attachment.method), b.withStderrTail(err)
	}
	if returnedID := strings.TrimSpace(asString(res["sessionId"])); returnedID != "" && returnedID != sessionID {
		releaseOwner()
		return SessionInfo{}, string(attachment.method), &providercontract.Error{
			Kind:    providercontract.ErrorNativeIdentityConflict,
			Message: "ACP exact attachment returned a different provider thread",
			Cause:   fmt.Errorf("requested %q, received %q", sessionID, returnedID),
		}
	}
	info, attachErr := b.attachSession(sessionID, cwd, opts, res, string(attachment.method))
	if attachErr != nil {
		releaseOwner()
		return SessionInfo{}, string(attachment.method), attachErr
	}
	return info, string(attachment.method), nil
}

func exactSessionAttachmentError(err error) error {
	if err == nil {
		return nil
	}
	var providerErr *providercontract.Error
	if errors.As(err, &providerErr) {
		return err
	}
	return nativeResumeError(err)
}
