package acp

import (
	"context"
	"strings"
	"time"
)

const engineRecoveredChunk = "[engine reiniciado — contexto restaurado]"

type crashRecoveryTarget struct {
	job          *Job
	oldSessionID string
	chatID       string
	tabID        string
	cwd          string
	providerID   string
	opts         JobStartOptions
}

func (m *Manager) handleUnexpectedBridgeExit(b *Bridge, cause error) {
	if b == nil {
		return
	}
	hint, policyErr := m.markProviderNeedsLogin(context.Background(), b.ProviderID(), cause)
	if hint != "" || policyErr != nil {
		m.abandonAdoptedHarnessTurns(b)
		closeCause := cause
		reason := providerStatusNeedsLogin
		if policyErr != nil {
			closeCause = policyErr
			reason = "authentication-policy-invalid"
		}
		b.Close(false, closeCause)
		m.opts.Logf("acp crash recovery suppressed", map[string]any{
			"provider": b.ProviderID(), "reason": reason,
		})
		return
	}
	// A job adopted for a harness-born turn holds no prompt to re-drive, so it
	// is ended here rather than recovered. Without this the engine's death would
	// leave it running forever and every later human prompt would queue behind
	// it — an adopted turn must never outlive the engine that started it.
	m.abandonAdoptedHarnessTurns(b)
	targets := b.crashRecoveryTargets()
	if len(targets) == 0 {
		b.Close(false, cause)
		return
	}
	legacyTargets := make([]crashRecoveryTarget, 0, len(targets))
	for _, target := range targets {
		if target.opts.ProviderLaneManaged {
			target.job.CrashInterrupted = true
			target.job.actorRecoveryPending.Store(true)
			continue
		}
		legacyTargets = append(legacyTargets, target)
	}
	// Bridge.Close emits the typed LaneDetached event for every actor-managed
	// attachment. Its durable reducer transition is the only code allowed to
	// schedule exact resume and turn readback.
	if len(legacyTargets) == 0 {
		b.Close(false, cause)
		return
	}

	key := crashRecoveryKey(b, legacyTargets[0])
	shouldRecover := m.reserveCrashRecovery(key, time.Now())
	var done chan struct{}
	if shouldRecover {
		done = make(chan struct{})
	}
	for _, target := range legacyTargets {
		target.job.CrashInterrupted = true
		target.job.StopReason = "engine-crash"
		if done != nil {
			target.job.crashRecoveryDone = done
		}
	}
	b.Close(false, cause)
	if !shouldRecover {
		m.opts.Logf("acp crash recovery suppressed", map[string]any{"key": key, "reason": "recent-crash"})
		return
	}
	go m.recoverCrashedBridge(legacyTargets, done)
}

func (b *Bridge) crashRecoveryTargets() []crashRecoveryTarget {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []crashRecoveryTarget
	for sessionID, job := range b.jobsBySession {
		if job == nil || job.internal || job.Status != "running" {
			continue
		}
		target := crashRecoveryTarget{
			job:          job,
			oldSessionID: sessionID,
			chatID:       firstNonEmpty(job.ChatID, b.chatID),
			tabID:        firstNonEmpty(job.TabID, b.tabID),
			cwd:          firstNonEmpty(job.CWD, b.cwd),
			providerID:   firstNonEmpty(job.ProviderID, b.providerID),
			opts:         job.startOpts,
		}
		target.opts.SessionID = sessionID
		target.opts.ChatID = target.chatID
		target.opts.TabID = target.tabID
		target.opts.CWD = target.cwd
		target.opts.ProviderID = target.providerID
		out = append(out, target)
	}
	return out
}

func crashRecoveryKey(b *Bridge, target crashRecoveryTarget) string {
	for _, raw := range []string{target.tabID, target.chatID, b.chatID, b.tabID, b.key} {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			return trimmed
		}
	}
	return "default"
}

func (m *Manager) reserveCrashRecovery(key string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.crashRecoveries[key]; ok && now.Sub(last) < m.opts.CrashRecoveryWindow {
		return false
	}
	m.crashRecoveries[key] = now
	return true
}

func (m *Manager) recoverCrashedBridge(targets []crashRecoveryTarget, done chan struct{}) {
	defer close(done)
	if len(targets) == 0 {
		return
	}
	time.Sleep(m.opts.CrashRecoveryBackoff)

	target := targets[0]
	timeout := m.opts.InitTimeout * 2
	if timeout < 90*time.Second {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	info, err := m.resurrectSession(ctx, target.opts)
	if err != nil {
		m.opts.Logf("acp crash recovery failed", map[string]any{"oldSessionId": target.oldSessionID, "error": err.Error()})
		return
	}
	bridge := m.bridgeForSession(info.SessionID, SessionOptions{TabID: target.tabID, ChatID: target.chatID, SessionID: info.SessionID, ProviderID: info.ProviderID})
	if bridge == nil {
		m.opts.Logf("acp crash recovery missing bridge", map[string]any{"oldSessionId": target.oldSessionID, "newSessionId": info.SessionID})
		return
	}

	// The host process changed; the provider-native thread did not. Resume is
	// the entire recovery operation—never replay Workass transcript text and
	// never manufacture a replacement session.
	bridge.markSeeded(info.SessionID)
	for _, item := range targets {
		if item.job != nil && item.job.ID != "" {
			m.emit("job:event", map[string]any{"type": "data", "id": item.job.ID, "stream": "system", "chunk": engineRecoveredChunk})
		}
	}
	m.emit("chat:engine-recovered", map[string]any{
		"chatId":       nullableString(target.chatID),
		"tabId":        nullableString(target.tabID),
		"oldSessionId": target.oldSessionID,
		"sessionId":    info.SessionID,
		"at":           time.Now().UTC().Format(time.RFC3339Nano),
	})
}
