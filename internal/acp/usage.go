package acp

import "time"

func (m *Manager) recordUsageUpdate(sessionID, providerID string, update map[string]any) map[string]any {
	meta := mapFromAny(update["_meta"])
	usage := contextUsage{
		Used:             firstPositiveNumber(update["used"], meta["cognition.ai/used"], meta["workass.mock/used"]),
		Size:             firstPositiveNumber(update["size"], meta["cognition.ai/size"], meta["workass.mock/size"]),
		InputTokens:      firstNonNil(meta["cognition.ai/inputTokens"], meta["workass.mock/inputTokens"], meta["inputTokens"], update["inputTokens"]),
		OutputTokens:     firstNonNil(meta["cognition.ai/outputTokens"], meta["workass.mock/outputTokens"], meta["outputTokens"], update["outputTokens"]),
		CachedReadTokens: firstNonNil(meta["cognition.ai/cachedReadTokens"], meta["workass.mock/cachedReadTokens"], meta["cachedReadTokens"], update["cachedReadTokens"]),
		UpdatedAt:        time.Now().UTC(),
	}
	if usage.Size > 0 && usage.Used >= 0 {
		usage.UsedPct = int((int64(usage.Used)*100 + int64(usage.Size)/2) / int64(usage.Size))
	}
	m.recordUsage(sessionID, usage)
	payload := map[string]any{
		"type":             "usage",
		"sessionId":        nullableString(sessionID),
		"providerId":       nullableString(providerID),
		"updatedAt":        usage.UpdatedAt.Format(time.RFC3339Nano),
		"used":             usage.Used,
		"size":             usage.Size,
		"usedPct":          usage.UsedPct,
		"inputTokens":      usage.InputTokens,
		"outputTokens":     usage.OutputTokens,
		"cachedReadTokens": usage.CachedReadTokens,
	}
	if snapshot, ok := m.recordPlanUsageCapture(sessionID, providerID, update); ok {
		payload["planUsage"] = snapshot
	}
	return payload
}

func (m *Manager) recordUsage(sessionID string, usage contextUsage) {
	if sessionID == "" {
		return
	}
	if usage.UpdatedAt.IsZero() {
		usage.UpdatedAt = time.Now().UTC()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usageBySession[sessionID] = usage
}

func (m *Manager) usageForSession(sessionID string) (contextUsage, bool) {
	if sessionID == "" {
		return contextUsage{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	usage, ok := m.usageBySession[sessionID]
	return usage, ok
}

func firstPositiveNumber(values ...any) int {
	for _, value := range values {
		if n := numberOrZero(value); n > 0 {
			return n
		}
	}
	return 0
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
