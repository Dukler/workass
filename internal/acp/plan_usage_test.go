package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlanUsageClaudeUsageUpdatesNormalizeAndMerge(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "claude", "claude-plan-usage")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "claude-plan-tab")
	if session.ProviderID != "claude" {
		t.Fatalf("session provider = %q, want claude", session.ProviderID)
	}

	job := startAppChatJob(t, manager, session.SessionID, "claude-plan-tab", "capture claude plan usage")
	rateUsage := events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		payload, _ := ev.payload.(map[string]any)
		return ev.channel == "job:event" && payload["type"] == "usage" && payload["sessionId"] == session.SessionID && fmt.Sprint(payload["size"]) == "200000"
	}).payload.(map[string]any)
	if rateUsage["tabId"] != "claude-plan-tab" || rateUsage["chatId"] != "chat-claude-plan-tab" || rateUsage["providerId"] != "claude" {
		t.Fatalf("usage identity = tab=%#v chat=%#v provider=%#v", rateUsage["tabId"], rateUsage["chatId"], rateUsage["providerId"])
	}
	if strings.TrimSpace(fmt.Sprint(rateUsage["updatedAt"])) == "" {
		t.Fatalf("usage update omitted durable timestamp: %#v", rateUsage)
	}
	rateSnapshot := jsonMap(t, rateUsage["planUsage"])
	assertPlanUsageBase(t, rateSnapshot, "claude")
	rateEntry := onlyPlanEntry(t, rateSnapshot, "rate-limit")
	wantRate := map[string]any{
		"kind":           "rate-limit",
		"id":             "five_hour",
		"status":         "allowed",
		"resetsAt":       "2026-07-10T20:00:00Z",
		"usedPercent":    json.Number("78"),
		"overageStatus":  "allowed",
		"isUsingOverage": false,
	}
	assertJSONMapEqual(t, rateEntry, wantRate)

	finalEvent := events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:plan-usage" {
			return false
		}
		snapshot := jsonMap(t, ev.payload)
		return snapshot["providerId"] == "claude" && len(planEntries(snapshot)) == 2
	})
	finalSnapshot := jsonMap(t, finalEvent.payload)
	costEntry := onlyPlanEntry(t, finalSnapshot, "cost")
	if got := numberString(costEntry["amount"]); got != "0.0581616" {
		t.Fatalf("cost amount = %s, want 0.0581616; entry=%#v", got, costEntry)
	}
	if costEntry["currency"] != "USD" {
		t.Fatalf("cost currency = %#v, want USD", costEntry["currency"])
	}
	raw := rawPlanUsage(t, finalSnapshot)
	if _, ok := raw["_claude/rateLimit"].(map[string]any); !ok {
		t.Fatalf("raw metadata did not retain captured rate limit: %#v", raw)
	}

	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestPlanUsageClaudeStructuredRefreshCapturesFiveHourAndWeeklyWindows(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "claude", "claude-plan-limits")
	t.Cleanup(func() { manager.Reset() })
	_ = newFakeSession(t, manager, "claude-limits-tab")

	snapshot := jsonMap(t, events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:plan-usage" {
			return false
		}
		return jsonMap(t, ev.payload)["providerId"] == "claude"
	}).payload)
	assertPlanUsageBase(t, snapshot, "claude")
	assertRateLimitEntry(t, snapshot, "five_hour", "37.5", "2026-07-13T20:00:00Z", "")
	assertRateLimitEntry(t, snapshot, "seven_day", "78", "2026-07-15T18:00:00Z", "")
	assertRateLimitEntry(t, snapshot, "seven_day_opus", "12.25", "2026-07-15T18:00:00Z", "")
	assertRateLimitEntry(t, snapshot, "seven_day_model:fable", "4", "2026-07-15T18:00:00Z", "Fable")
}

func TestPlanUsageCodexStructuredRefreshCapturesFiveHourAndWeeklyWindows(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "codex", "codex-plan-limits")
	t.Cleanup(func() { manager.Reset() })
	_ = newFakeSession(t, manager, "codex-limits-tab")

	snapshot := jsonMap(t, events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:plan-usage" {
			return false
		}
		return jsonMap(t, ev.payload)["providerId"] == "codex"
	}).payload)
	assertPlanUsageBase(t, snapshot, "codex")
	fiveHour := assertRateLimitEntry(t, snapshot, "five_hour", "42", "2026-07-13T20:00:00Z", "Codex")
	if numberString(fiveHour["windowMinutes"]) != "300" {
		t.Fatalf("five-hour windowMinutes = %#v, want 300", fiveHour["windowMinutes"])
	}
	weekly := assertRateLimitEntry(t, snapshot, "seven_day", "67", "2026-07-15T20:00:00Z", "Codex")
	if numberString(weekly["windowMinutes"]) != "10080" {
		t.Fatalf("weekly windowMinutes = %#v, want 10080", weekly["windowMinutes"])
	}
	resetCredits, _ := snapshot["rateLimitResetCredits"].(map[string]any)
	if numberString(resetCredits["availableCount"]) != "1" {
		t.Fatalf("earned reset availableCount = %#v, want 1", resetCredits["availableCount"])
	}
	credits, _ := resetCredits["credits"].([]any)
	if len(credits) != 1 {
		t.Fatalf("earned reset credits = %#v, want one detail row", resetCredits["credits"])
	}
	credit := credits[0].(map[string]any)
	if credit["id"] != "RateLimitResetCredit_test" || credit["status"] != "available" || credit["title"] != "Full reset (Weekly + 5 hr)" {
		t.Fatalf("earned reset detail = %#v", credit)
	}
}

func TestCodexEarnedRateLimitResetIsIdempotentAndRefreshesPlanUsage(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "codex", "codex-plan-limits")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-earned-reset-tab")
	_ = events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		if ev.channel != "chat:plan-usage" {
			return false
		}
		summary, _ := jsonMap(t, ev.payload)["rateLimitResetCredits"].(map[string]any)
		return numberString(summary["availableCount"]) == "1"
	})

	nothing, err := manager.ConsumeRateLimitResetCredit(context.Background(), "codex", "", "reset-no-window", "nothing-to-reset")
	if err != nil || nothing["outcome"] != "nothingToReset" {
		t.Fatalf("nothing-to-reset outcome = %#v, err=%v", nothing, err)
	}
	nothingPlan := jsonMap(t, nothing["planUsage"])
	nothingSummary := nothingPlan["rateLimitResetCredits"].(map[string]any)
	if numberString(nothingSummary["availableCount"]) != "1" {
		t.Fatalf("nothing-to-reset spent credit: %#v", nothingSummary)
	}

	result, err := manager.ConsumeRateLimitResetCredit(context.Background(), "codex", session.SessionID, "reset-attempt-1", "RateLimitResetCredit_test")
	if err != nil {
		t.Fatalf("consume earned reset: %v", err)
	}
	if result["outcome"] != "reset" {
		t.Fatalf("first consume outcome = %#v, want reset", result["outcome"])
	}
	plan := jsonMap(t, result["planUsage"])
	summary, _ := plan["rateLimitResetCredits"].(map[string]any)
	if numberString(summary["availableCount"]) != "0" {
		t.Fatalf("refreshed earned reset count = %#v, want 0", summary["availableCount"])
	}
	credits, ok := summary["credits"].([]any)
	if !ok || len(credits) != 0 {
		t.Fatalf("refreshed earned reset details = %#v, want authoritative empty list", summary["credits"])
	}

	retry, err := manager.ConsumeRateLimitResetCredit(context.Background(), "codex", session.SessionID, "reset-attempt-1", "RateLimitResetCredit_test")
	if err != nil {
		t.Fatalf("retry earned reset: %v", err)
	}
	if retry["outcome"] != "alreadyRedeemed" {
		t.Fatalf("retry outcome = %#v, want alreadyRedeemed", retry["outcome"])
	}
	noCredit, err := manager.ConsumeRateLimitResetCredit(context.Background(), "codex", session.SessionID, "reset-attempt-2", "")
	if err != nil || noCredit["outcome"] != "noCredit" {
		t.Fatalf("empty credit balance outcome = %#v, err=%v", noCredit, err)
	}
}

func TestCodexEarnedRateLimitResetNullSnapshotClearsPreviousCredit(t *testing.T) {
	manager := NewManager(Options{})
	first, ok := manager.recordPlanUsageCapture("", "codex", map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": fakeCodexRateLimitsWithCredits(1)},
	})
	if !ok || first.RateLimitResetCredits == nil || first.RateLimitResetCredits.AvailableCount != 1 {
		t.Fatalf("initial earned reset snapshot = %#v, ok=%v", first.RateLimitResetCredits, ok)
	}
	second, ok := manager.recordPlanUsageCapture("", "codex", map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": map[string]any{
			"rateLimits":            fakeCodexRateLimitsWithCredits(0)["rateLimits"],
			"rateLimitResetCredits": nil,
		}},
	})
	if !ok || second.RateLimitResetCredits != nil {
		t.Fatalf("null earned reset snapshot = %#v, ok=%v; want cleared", second.RateLimitResetCredits, ok)
	}
}

func TestEarnedRateLimitResetRejectsProvidersWithoutNativeCapability(t *testing.T) {
	manager, _ := newPlanUsageFakeManager(t, "claude", "claude-plan-limits")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "claude-no-earned-reset-tab")
	if _, err := manager.ConsumeRateLimitResetCredit(context.Background(), "claude", session.SessionID, "reset-attempt-1", ""); err == nil {
		t.Fatal("Claude earned-reset consume unexpectedly succeeded")
	}
}

func TestPlanUsageRefreshIsCancelledByReset(t *testing.T) {
	manager, _ := newPlanUsageFakeManager(t, "claude", "claude-plan-limits-hang")
	_ = newFakeSession(t, manager, "claude-limits-hang-tab")
	started := time.Now()
	manager.Reset()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Reset waited %s for a cancelled plan-usage refresh", elapsed)
	}
}

func TestPlanUsageRefreshesOnSessionAttachAndTerminalTurn(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "fake-methods.log")
	manager, events := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits"},
	)
	t.Cleanup(func() { manager.Reset() })

	tabID := "refresh-boundaries-tab"
	session := newFakeSessionForProvider(t, manager, tabID, "chat-"+tabID, "claude")
	attachMethods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	if countMethod(attachMethods, "session/new") != 1 || countMethod(attachMethods, "session/prompt") != 0 {
		t.Fatalf("session attach methods = %v, want one session/new, one usage refresh, no prompt", attachMethods)
	}
	waitPlanUsageRefreshesIdle(t, manager, 2*time.Second)
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("clear method trace: %v", err)
	}

	job := startAppChatJob(t, manager, session.SessionID, tabID, "refresh after terminal turn")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	terminalMethods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	if countMethod(terminalMethods, "session/new") != 0 || countMethod(terminalMethods, "session/prompt") != 1 {
		t.Fatalf("terminal-turn methods = %v, want one prompt and one usage refresh without a new session", terminalMethods)
	}
}

func TestPlanUsageTerminalRefreshQueuesBehindSlowAttachRefresh(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "fake-methods.log")
	manager, events := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits-delay"},
	)
	t.Cleanup(func() { manager.Reset() })

	tabID := "refresh-overlap-tab"
	session := newFakeSessionForProvider(t, manager, tabID, "chat-"+tabID, "claude")
	waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})

	// The fake keeps the attach-time usage request in flight while the prompt
	// reaches end_turn. The terminal boundary must queue one trailing refresh,
	// not get dropped by provider-level single-flight suppression.
	job := startAppChatJob(t, manager, session.SessionID, tabID, "finish while attach usage is slow")
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
	methods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 2
	})
	if countMethod(methods, "session/new") != 1 || countMethod(methods, "session/prompt") != 1 {
		t.Fatalf("overlapped refresh methods = %v, want one real session and one prompt", methods)
	}
}

func TestPlanUsageRefreshDefaultsToFiveMinutes(t *testing.T) {
	opts := (Options{}).withDefaults()
	if opts.PlanUsageRefreshInterval != 5*time.Minute {
		t.Fatalf("plan usage refresh interval = %s, want 5m", opts.PlanUsageRefreshInterval)
	}
}

func TestPlanUsagePeriodicTickRefreshesOneLiveSessionPerProviderWithoutPrompt(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "periodic-plan-usage.log")
	manager, _ := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits"},
	)
	t.Cleanup(func() { manager.Reset() })
	newFakeSessionForProvider(t, manager, "periodic-a", "chat-periodic-a", "claude")
	newFakeSessionForProvider(t, manager, "periodic-b", "chat-periodic-b", "claude")
	waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 2
	})
	waitPlanUsageRefreshesIdle(t, manager, 2*time.Second)
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("clear method trace: %v", err)
	}

	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() {
		manager.runPlanUsageTicks(ticks)
		close(done)
	}()
	ticks <- time.Now()
	methods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	close(ticks)
	<-done
	if countMethod(methods, "session/new") != 0 || countMethod(methods, "session/prompt") != 0 {
		t.Fatalf("periodic plan refresh touched conversation methods: %v", methods)
	}
}

func TestRefreshPlanUsageSessionReusesLiveChatWithoutPrompt(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "active-plan-usage.log")
	manager, _ := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits"},
	)
	t.Cleanup(func() { manager.Reset() })
	first := newFakeSessionForProvider(t, manager, "active-plan-tab", "active-plan-chat", "claude")
	waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	waitPlanUsageRefreshesIdle(t, manager, 2*time.Second)
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("clear method trace: %v", err)
	}

	refreshed, err := manager.RefreshPlanUsageSession(context.Background(), SessionOptions{
		TabID: "active-plan-tab", ChatID: "active-plan-chat", ProviderID: "claude", SessionID: first.SessionID,
	})
	if err != nil || refreshed.SessionID != first.SessionID {
		t.Fatalf("refresh active plan session = %#v, err=%v", refreshed, err)
	}
	methods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	if countMethod(methods, "session/new") != 0 || countMethod(methods, "session/prompt") != 0 {
		t.Fatalf("active plan refresh touched conversation methods: %v", methods)
	}
}

func TestRefreshProviderPlanUsageReusesAnyLiveProviderSessionWithoutPrompt(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "provider-plan-usage.log")
	manager, events := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits"},
	)
	t.Cleanup(func() { manager.Reset() })
	_ = newFakeSessionForProvider(t, manager, "other-plan-tab", "other-plan-chat", "claude")
	waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	waitPlanUsageRefreshesIdle(t, manager, 2*time.Second)
	if err := os.WriteFile(tracePath, nil, 0o600); err != nil {
		t.Fatalf("clear method trace: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.RefreshProviderPlanUsage(ctx, "claude"); err != nil {
		t.Fatalf("refresh provider plan usage: %v", err)
	}
	methods := waitForMethodLog(t, tracePath, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "_workass/claude/usage") == 1
	})
	if countMethod(methods, "session/new") != 0 || countMethod(methods, "session/prompt") != 0 {
		t.Fatalf("provider refresh touched conversation methods: %v", methods)
	}
	snapshot := jsonMap(t, events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		return ev.channel == "chat:plan-usage" && jsonMap(t, ev.payload)["providerId"] == "claude"
	}).payload)
	assertRateLimitEntry(t, snapshot, "seven_day", "78", "2026-07-15T18:00:00Z", "")
}

func TestRefreshProviderPlanUsageColdStartIsEphemeralAndPromptFree(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "cold-provider-plan-usage.log")
	manager, events := newPlanUsageFakeManagerWithProviders(t, tracePath,
		planUsageFakeProvider{id: "claude", mode: "claude-plan-limits"},
	)
	t.Cleanup(func() { manager.Reset() })

	// Hold an unrelated chat lifecycle boundary to model the screenshot case:
	// another provider turn owns the visible chat while Claude account metadata
	// must remain independently readable.
	unlock := manager.lockChatLifecycle("busy-codex-tab", "busy-codex-chat")
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.RefreshProviderPlanUsage(ctx, "claude"); err != nil {
		t.Fatalf("cold provider plan usage refresh: %v", err)
	}
	snapshot := jsonMap(t, events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		return ev.channel == "chat:plan-usage" && jsonMap(t, ev.payload)["providerId"] == "claude"
	}).payload)
	assertRateLimitEntry(t, snapshot, "five_hour", "37.5", "2026-07-13T20:00:00Z", "")
	assertRateLimitEntry(t, snapshot, "seven_day", "78", "2026-07-15T18:00:00Z", "")
	methods := readMethodLog(t, tracePath)
	if countMethod(methods, "session/new") != 1 || countMethod(methods, "_workass/claude/usage") != 1 || countMethod(methods, "session/prompt") != 0 {
		t.Fatalf("cold provider refresh methods = %v, want one metadata session + usage and no prompt", methods)
	}
	if live := manager.LiveSessions(); len(live) != 0 {
		t.Fatalf("metadata-only refresh leaked a live chat session: %#v", live)
	}
}

func TestPlanUsageCodexPromptResultNormalizesQuota(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "codex", "codex-plan-usage")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "codex-plan-tab")

	job := startAppChatJob(t, manager, session.SessionID, "codex-plan-tab", "capture codex quota")
	ev := events.waitChannel(t, "chat:plan-usage", 2*time.Second)
	snapshot := jsonMap(t, ev.payload)
	assertPlanUsageBase(t, snapshot, "codex")
	quota := onlyPlanEntry(t, snapshot, "quota")
	perModel, _ := quota["perModel"].([]any)
	if len(perModel) != 1 {
		t.Fatalf("quota perModel = %#v, want one model", quota["perModel"])
	}
	got := perModel[0].(map[string]any)
	want := map[string]any{
		"model":                 "gpt-5.6-sol",
		"totalTokens":           json.Number("14609"),
		"inputTokens":           json.Number("4616"),
		"cachedInputTokens":     json.Number("9984"),
		"outputTokens":          json.Number("9"),
		"reasoningOutputTokens": json.Number("0"),
	}
	assertJSONMapEqual(t, got, want)
	raw := rawPlanUsage(t, snapshot)
	if _, ok := raw["quota"].(map[string]any); !ok {
		t.Fatalf("raw quota missing: %#v", raw)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestPlanUsageUnknownMetaLandsInRaw(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "custom", "unknown-plan-usage")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "unknown-plan-tab")

	job := startAppChatJob(t, manager, session.SessionID, "unknown-plan-tab", "capture unknown metadata")
	ev := events.waitChannel(t, "chat:plan-usage", 2*time.Second)
	snapshot := jsonMap(t, ev.payload)
	if entries := planEntries(snapshot); len(entries) != 0 {
		t.Fatalf("unknown metadata produced typed entries: %#v", entries)
	}
	raw := rawPlanUsage(t, snapshot)
	weekly := raw["weekly"].(map[string]any)
	credits := raw["credits"].(map[string]any)
	if weekly["remaining"] != json.Number("42") || credits["used"] != json.Number("7") || credits["size"] != json.Number("100") {
		t.Fatalf("raw unknown metadata = %#v", raw)
	}
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")
}

func TestPlanUsageRawIsBoundedAndRedacted(t *testing.T) {
	hugeManager, hugeEvents := newPlanUsageFakeManager(t, "custom", "huge-plan-usage")
	t.Cleanup(func() { hugeManager.Reset() })
	hugeSession := newFakeSession(t, hugeManager, "huge-plan-tab")
	hugeJob := startAppChatJob(t, hugeManager, hugeSession.SessionID, "huge-plan-tab", "capture huge metadata")
	hugeSnapshot := jsonMap(t, hugeEvents.waitChannel(t, "chat:plan-usage", 2*time.Second).payload)
	hugeRaw := rawPlanUsage(t, hugeSnapshot)
	if hugeRaw["_truncated"] != true || hugeRaw["_limitBytes"] != json.Number("8192") {
		t.Fatalf("huge raw metadata was not capped with note: %#v", hugeRaw)
	}
	if data, err := json.Marshal(hugeRaw); err != nil || len(data) > planUsageRawLimitBytes {
		t.Fatalf("huge raw JSON len=%d err=%v raw=%#v", len(data), err, hugeRaw)
	}
	assertJobStatus(t, hugeEvents.waitJobEnd(t, jobID(hugeJob), 2*time.Second), "done", 0, "end_turn")

	secretManager, secretEvents := newPlanUsageFakeManager(t, "custom", "secret-plan-usage")
	t.Cleanup(func() { secretManager.Reset() })
	secretSession := newFakeSession(t, secretManager, "secret-plan-tab")
	secretJob := startAppChatJob(t, secretManager, secretSession.SessionID, "secret-plan-tab", "capture secret metadata")
	secretSnapshot := jsonMap(t, secretEvents.waitChannel(t, "chat:plan-usage", 2*time.Second).payload)
	secretRaw := rawPlanUsage(t, secretSnapshot)
	if secretRaw["api_key"] != "[redacted]" {
		t.Fatalf("api_key was not redacted: %#v", secretRaw)
	}
	nested := secretRaw["nested"].(map[string]any)
	if nested["safe"] != "api_key=[redacted]" {
		t.Fatalf("nested secret-looking value was not redacted: %#v", nested)
	}
	assertJobStatus(t, secretEvents.waitJobEnd(t, jobID(secretJob), 2*time.Second), "done", 0, "end_turn")
}

func TestPlanUsageReplayToLateClient(t *testing.T) {
	manager, events := newPlanUsageFakeManager(t, "claude", "claude-plan-usage")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "replay-plan-tab")
	job := startAppChatJob(t, manager, session.SessionID, "replay-plan-tab", "capture replay metadata")
	_ = events.waitFor(t, 2*time.Second, func(ev collectedEvent) bool {
		return ev.channel == "chat:plan-usage" && len(planEntries(jsonMap(t, ev.payload))) == 2
	})
	assertJobStatus(t, events.waitJobEnd(t, jobID(job), 2*time.Second), "done", 0, "end_turn")

	var replayed []collectedEvent
	manager.ReplayProviderEvents(func(channel string, payload any) error {
		replayed = append(replayed, collectedEvent{channel: channel, payload: payload})
		return nil
	})
	for _, ev := range replayed {
		if ev.channel != "chat:plan-usage" {
			continue
		}
		snapshot := jsonMap(t, ev.payload)
		if snapshot["providerId"] == "claude" && len(planEntries(snapshot)) == 2 {
			return
		}
	}
	t.Fatalf("late client replay missing plan usage: %#v", replayed)
}

func newPlanUsageFakeManager(t *testing.T, providerID, mode string) (*Manager, *eventCollector) {
	t.Helper()
	events := newEventCollector()
	root := repoRoot(t)
	providerID = normalizeProviderID(providerID)
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Providers: []ProviderConfig{{
			ID:      providerID,
			Name:    "Plan Usage Fake",
			Command: os.Args[0],
			Args:    []string{"-test.run=TestFakeACPHelper", "--"},
			CWD:     root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": mode},
			Enabled: true,
		}},
		DefaultProviderID:    providerID,
		Broadcast:            events.Broadcast,
		InitTimeout:          2 * time.Second,
		PermissionTimeout:    2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	return manager, events
}

type planUsageFakeProvider struct {
	id   string
	mode string
}

func newPlanUsageFakeManagerWithProviders(t *testing.T, methodLog string, fakes ...planUsageFakeProvider) (*Manager, *eventCollector) {
	t.Helper()
	events := newEventCollector()
	root := repoRoot(t)
	providers := make([]ProviderConfig, 0, len(fakes))
	for _, fake := range fakes {
		providerID := normalizeProviderID(fake.id)
		providers = append(providers, ProviderConfig{
			ID:      providerID,
			Name:    "Plan Usage Fake " + providerID,
			Command: os.Args[0],
			Args:    []string{"-test.run=TestFakeACPHelper", "--"},
			CWD:     root,
			Env: map[string]string{
				"WORKASS_FAKE_ACP":            "1",
				"WORKASS_FAKE_ACP_MODE":       fake.mode,
				"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
			},
			Enabled: true,
		})
	}
	manager := NewManager(Options{
		RootDir:              root,
		StateDir:             filepath.Join(t.TempDir(), "state"),
		Providers:            providers,
		DefaultProviderID:    providers[0].ID,
		Broadcast:            events.Broadcast,
		InitTimeout:          2 * time.Second,
		PermissionTimeout:    2 * time.Second,
		StdoutFlushInterval:  10 * time.Millisecond,
		ThoughtFlushInterval: 10 * time.Millisecond,
		RSSSampleInterval:    time.Hour,
	})
	return manager, events
}

func newFakeSessionForProvider(t *testing.T, manager *Manager, tabID, chatID, providerID string) SessionInfo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: providerID})
	if err != nil {
		t.Fatalf("new %s session: %v", providerID, err)
	}
	return session
}

func waitPlanUsageRefreshesIdle(t *testing.T, manager *Manager, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		manager.planUsageRefreshMu.Lock()
		active := len(manager.planUsageRefreshes)
		manager.planUsageRefreshMu.Unlock()
		if active == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for plan-usage refreshes to finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForMethodLog(t *testing.T, path string, timeout time.Duration, pred func([]string) bool) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		methods := readMethodLog(t, path)
		if pred(methods) {
			return methods
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for method trace; methods=%v", methods)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readMethodLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read method trace: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

func countMethod(methods []string, method string) int {
	count := 0
	for _, got := range methods {
		if got == method {
			count++
		}
	}
	return count
}

func assertPlanUsageBase(t *testing.T, snapshot map[string]any, providerID string) {
	t.Helper()
	if snapshot["providerId"] != providerID {
		t.Fatalf("providerId = %#v, want %s; snapshot=%#v", snapshot["providerId"], providerID, snapshot)
	}
	if snapshot["capturedAt"] == "" {
		t.Fatalf("capturedAt missing: %#v", snapshot)
	}
}

func onlyPlanEntry(t *testing.T, snapshot map[string]any, kind string) map[string]any {
	t.Helper()
	var matches []map[string]any
	for _, raw := range planEntries(snapshot) {
		entry := raw.(map[string]any)
		if entry["kind"] == kind {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("entries with kind %q = %#v in snapshot %#v", kind, matches, snapshot)
	}
	return matches[0]
}

func assertRateLimitEntry(t *testing.T, snapshot map[string]any, id, percent, resetsAt, limitName string) map[string]any {
	t.Helper()
	for _, raw := range planEntries(snapshot) {
		entry := raw.(map[string]any)
		if entry["kind"] != "rate-limit" || entry["id"] != id {
			continue
		}
		if numberString(entry["usedPercent"]) != percent || entry["resetsAt"] != resetsAt {
			t.Fatalf("rate limit %s = %#v, want usedPercent=%s resetsAt=%s", id, entry, percent, resetsAt)
		}
		if limitName != "" && entry["limitName"] != limitName {
			t.Fatalf("rate limit %s limitName = %#v, want %q", id, entry["limitName"], limitName)
		}
		return entry
	}
	t.Fatalf("rate limit %s missing from %#v", id, snapshot)
	return nil
}

func planEntries(snapshot map[string]any) []any {
	entries, _ := snapshot["entries"].([]any)
	return entries
}

func rawPlanUsage(t *testing.T, snapshot map[string]any) map[string]any {
	t.Helper()
	raw, _ := snapshot["raw"].(map[string]any)
	if len(raw) == 0 {
		t.Fatalf("raw plan usage metadata missing: %#v", snapshot)
	}
	return raw
}

func jsonMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json map: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode json map: %v\n%s", err, data)
	}
	return out
}

func assertJSONMapEqual(t *testing.T, got, want map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("map len got=%d want=%d got=%#v want=%#v", len(got), len(want), got, want)
	}
	for key, wantValue := range want {
		gotValue, ok := got[key]
		if !ok {
			t.Fatalf("missing key %q in %#v", key, got)
		}
		if numberString(gotValue) != numberString(wantValue) {
			t.Fatalf("field %s got=%#v want=%#v in map %#v", key, gotValue, wantValue, got)
		}
	}
}

func numberString(v any) string {
	switch x := v.(type) {
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}
