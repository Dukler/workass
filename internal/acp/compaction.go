package acp

import (
	"context"
	"time"
)

// providerOwnsContextCompaction identifies runtimes that compact their native
// thread in place. Their adapter reports compaction checkpoints as canonical
// lane events; Workass never manufactures a summary turn or a replacement
// thread.
func providerOwnsContextCompaction(providerID string) bool {
	return providerAdapterForID(providerID).context.Capabilities().NativeCompaction
}

// maybeCompactAfterTurn is deliberately non-mutating. Providers with native
// compaction keep the same thread. Providers without it are allowed to reach a
// visible context limit, but Workass must not sample a summary, create another
// thread, or replay transcript text to disguise that limitation.
func (m *Manager) maybeCompactAfterTurn(_ context.Context, activeBridge *Bridge, job *Job) {
	if !m.opts.CompactionEnabled || job == nil || job.SessionID == "" || job.CrashInterrupted || job.Status != "done" {
		return
	}
	providerID := normalizeProviderID(job.ProviderID)
	if providerID == "" && activeBridge != nil {
		providerID = normalizeProviderID(activeBridge.ProviderID())
	}
	if providerOwnsContextCompaction(providerID) {
		return
	}
	usage, ok := m.usageForSession(job.SessionID)
	if !ok || usage.Size <= 0 || usage.UsedPct < m.opts.CompactionThresholdPct {
		return
	}
	m.opts.Logf("provider lane approaching context limit without native compaction", map[string]any{
		"chatId": job.ChatID, "tabId": job.TabID, "providerId": providerID,
		"sessionId": job.SessionID, "usedPct": usage.UsedPct,
	})
	m.emit("chat:context-limit", map[string]any{
		"chatId": nullableString(job.ChatID), "tabId": nullableString(job.TabID),
		"providerId": nullableString(providerID), "sessionId": job.SessionID,
		"usedPct": usage.UsedPct, "at": time.Now().UTC().Format(time.RFC3339Nano),
	})
}
