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

	key := crashRecoveryKey(b, targets[0])
	shouldRecover := m.reserveCrashRecovery(key, time.Now())
	var done chan struct{}
	if shouldRecover {
		done = make(chan struct{})
	}
	for _, target := range targets {
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
	go m.recoverCrashedBridge(targets, done)
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
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration(m.opts.InitTimeout*2, 90*time.Second))
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

	seedPrompt := buildReplaySeedPrompt(m.opts.RootDir, target.opts)
	if strings.TrimSpace(seedPrompt) != "" {
		if _, err := bridge.promptSystem(ctx, info.SessionID, seedPrompt); err != nil {
			m.opts.Logf("acp crash recovery replay failed", map[string]any{"oldSessionId": target.oldSessionID, "newSessionId": info.SessionID, "error": err.Error()})
			return
		}
	}
	bridge.markSeeded(info.SessionID)

	m.emit("chat:session-replaced", map[string]any{"chatId": nullableString(target.chatID), "tabId": nullableString(target.tabID), "oldSessionId": target.oldSessionID, "session": info})
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
