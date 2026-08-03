package acp

import (
	"context"
	"strings"
	"time"
)

const compactionPromptPreamble = "WORKASS AUTO-COMPACTION v2"

func compactionPrompt() string {
	return compactionPromptPreamble + `

Summarize the complete conversation so a new ACP session can retain its task context.
This is internal memory, not a user-facing reply. Its wording must not determine the language of a later reply; that always comes from the later human-authored user request.
Return concise, stable sections without greetings or decorative Markdown:

1. Current user objective.
2. Technical state and decisions already made.
3. Relevant files, commands, tests, and outcomes.
4. Risks, constraints, and next steps.
5. Facts that must remain verbatim when necessary.

Do not invent information or continue the task. Return only the summary.`
}

// providerOwnsContextCompaction identifies frontier runtimes that already
// compact their provider-native session in place. Workass must not race those
// runtimes with its fallback summary + fresh-session reseed: doing so discards
// provider-owned hidden context and can publish a fabricated zero-usage reset.
func providerOwnsContextCompaction(providerID string) bool {
	switch normalizeProviderID(providerID) {
	case "claude", "codex":
		return true
	default:
		return false
	}
}

func (m *Manager) maybeCompactAfterTurn(ctx context.Context, activeBridge *Bridge, job *Job) {
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
	if activeBridge == nil || !activeBridge.hasLiveSession(job.SessionID) {
		return
	}

	oldSessionID := job.SessionID
	summary, err := m.runCompactionTurn(ctx, activeBridge, oldSessionID)
	if err != nil {
		m.opts.Logf("acp compaction failed", map[string]any{"sessionId": oldSessionID, "error": err.Error()})
		return
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		m.opts.Logf("acp compaction skipped empty summary", map[string]any{"sessionId": oldSessionID})
		return
	}

	conversation := m.completedConversationForJob(job)
	kept := lastTurnMessages(conversation, m.opts.CompactionKeepLastTurns)
	keptTurns := countUserTurns(kept)
	seed := compactedSeed{Summary: summary, Messages: kept, KeepTurns: keptTurns}

	activeBridge.CloseSession(ctx, oldSessionID)
	info, err := m.resurrectSession(ctx, JobStartOptions{
		Kind:       "app-chat",
		ChatID:     job.ChatID,
		TabID:      job.TabID,
		SessionID:  oldSessionID,
		CWD:        job.CWD,
		ModelID:    job.startOpts.ModelID,
		ModeID:     job.startOpts.ModeID,
		ProviderID: job.ProviderID,
		ForceFresh: true,
	})
	if err != nil {
		m.opts.Logf("acp compaction reseed session failed", map[string]any{"oldSessionId": oldSessionID, "error": err.Error()})
		return
	}
	newBridge := m.bridgeForSession(info.SessionID, SessionOptions{TabID: job.TabID, ChatID: job.ChatID, SessionID: info.SessionID, ProviderID: info.ProviderID})
	if newBridge == nil {
		m.opts.Logf("acp compaction missing replacement bridge", map[string]any{"oldSessionId": oldSessionID, "newSessionId": info.SessionID})
		return
	}

	m.emit("chat:session-replaced", map[string]any{"chatId": nullableString(job.ChatID), "tabId": nullableString(job.TabID), "oldSessionId": oldSessionID, "session": info})
	if err := m.seedCompactedSession(ctx, newBridge, info.SessionID, seed); err != nil {
		m.storeCompactedSeed(info.SessionID, seed)
		m.opts.Logf("acp compaction seed prompt failed", map[string]any{"sessionId": info.SessionID, "error": err.Error()})
		return
	}
	if m.nativeSessions != nil {
		if binding, ok := m.nativeSessions.get(job.TabID, job.ChatID, job.ProviderID); ok && binding.SessionID == info.SessionID {
			m.nativeSessions.updateCursor(
				job.TabID, job.ChatID, job.ProviderID, info.SessionID, binding.Generation, conversation,
				job.startOpts.ModelID, job.startOpts.ModeID, true,
			)
		}
	}
	m.recordUsage(info.SessionID, contextUsage{Used: 0, Size: usage.Size, UsedPct: 0, UpdatedAt: time.Now().UTC()})
	m.emit("job:event", map[string]any{
		"type":             "usage",
		"sessionId":        nullableString(info.SessionID),
		"tabId":            nullableString(job.TabID),
		"chatId":           nullableString(job.ChatID),
		"providerId":       nullableString(job.ProviderID),
		"updatedAt":        time.Now().UTC().Format(time.RFC3339Nano),
		"used":             0,
		"size":             usage.Size,
		"usedPct":          0,
		"inputTokens":      nil,
		"outputTokens":     nil,
		"cachedReadTokens": nil,
	})
	m.emit("chat:compacted", map[string]any{
		"chatId":       nullableString(job.ChatID),
		"tabId":        nullableString(job.TabID),
		"sessionId":    info.SessionID,
		"usedPct":      usage.UsedPct,
		"summaryChars": len(summary),
		"keptTurns":    keptTurns,
		"at":           time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (m *Manager) runCompactionTurn(ctx context.Context, bridge *Bridge, sessionID string) (string, error) {
	promptCtx, cancel := context.WithTimeout(ctx, maxDuration(m.opts.InitTimeout, 60*time.Second))
	defer cancel()
	res, err := bridge.promptSystem(promptCtx, sessionID, compactionPrompt())
	if err != nil {
		return "", err
	}
	return cleanDraft(res.Output), nil
}

func (m *Manager) seedCompactedSession(ctx context.Context, bridge *Bridge, sessionID string, seed compactedSeed) error {
	prompt := buildCompactedSeedBlock(seed)
	if strings.TrimSpace(prompt) == "" {
		bridge.markSeeded(sessionID)
		return nil
	}
	promptCtx, cancel := context.WithTimeout(ctx, maxDuration(m.opts.InitTimeout, 60*time.Second))
	defer cancel()
	if _, err := bridge.promptSystem(promptCtx, sessionID, prompt); err != nil {
		return err
	}
	bridge.markSeeded(sessionID)
	return nil
}

func (m *Manager) storeCompactedSeed(sessionID string, seed compactedSeed) {
	if sessionID == "" || strings.TrimSpace(seed.Summary) == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compactedSeeds[sessionID] = seed
}

func (m *Manager) completedConversationForJob(job *Job) []historyMessage {
	if job == nil {
		return nil
	}
	opts := job.startOpts
	history := promptHistoryForTab(m.opts.RootDir, opts)
	if userText := strings.TrimSpace(firstNonEmpty(opts.Prompt, opts.Message)); userText != "" {
		history = append(history, historyMessage{Role: "user", Content: userText})
	}
	if result := strings.TrimSpace(job.Result); result != "" {
		history = append(history, historyMessage{Role: "assistant", Content: result})
	}
	return dedupeHistoryMessages(history)
}

func dedupeHistoryMessages(history []historyMessage) []historyMessage {
	seen := map[string]struct{}{}
	out := make([]historyMessage, 0, len(history))
	for _, msg := range history {
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		key := historyMsgKey(msg)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, msg)
	}
	return out
}

func lastTurnMessages(history []historyMessage, keepTurns int) []historyMessage {
	if len(history) == 0 || keepTurns <= 0 {
		return nil
	}
	start := 0
	seenTurns := 0
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			seenTurns++
			if seenTurns == keepTurns {
				start = i
				break
			}
		}
	}
	if seenTurns < keepTurns {
		start = 0
	}
	return append([]historyMessage(nil), history[start:]...)
}

func countUserTurns(history []historyMessage) int {
	turns := 0
	for _, msg := range history {
		if msg.Role == "user" {
			turns++
		}
	}
	if turns == 0 && len(history) > 0 {
		return 1
	}
	return turns
}

func buildCompactedSeedBlock(seed compactedSeed) string {
	summary := strings.TrimSpace(seed.Summary)
	if summary == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("Workass auto-compaction context. Use it only as prior task memory and continue without greeting or restarting the conversation. Its language must not determine the language of a later reply.\n\n")
	b.WriteString("<compacted_summary>\n")
	b.WriteString(summary)
	b.WriteString("\n</compacted_summary>\n")
	if len(seed.Messages) > 0 {
		b.WriteString("\n<recent_turns_verbatim>\n")
		for _, msg := range seed.Messages {
			content := strings.TrimSpace(msg.Content)
			if content == "" {
				continue
			}
			who := "Assistant"
			if msg.Role == "user" {
				who = "User"
			}
			b.WriteString(who)
			b.WriteString(": ")
			b.WriteString(content)
			b.WriteString("\n\n")
		}
		b.WriteString("</recent_turns_verbatim>\n")
	}
	b.WriteString("\nDo not reply to this internal context message. Retain it for the next real user turn.")
	return b.String()
}

func buildReplaySeedPrompt(rootDir string, opts JobStartOptions) string {
	history := promptHistoryForTab(rootDir, opts)
	if len(history) == 0 {
		return ""
	}
	block := buildHistoryBlock(history, historyCharBudget(opts))
	if block == "" {
		return ""
	}
	return block + "Internal Workass instruction: this is a replay after the ACP engine restarted. Do not reply to the user; retain the context for the next real user turn. The replay language is not a user language preference."
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
