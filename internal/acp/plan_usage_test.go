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

func TestNormalizedPlanUsageCaptureMergesTypedSnapshots(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	used := 78.0
	first, ok := manager.recordNormalizedPlanUsageCapture("", "fixture", planUsageCapture{
		entries: []PlanUsageEntry{{Kind: "rate-limit", ID: "five_hour", UsedPercent: &used}},
		raw:     map[string]any{"window": map[string]any{"source": "typed"}},
	})
	if !ok || len(first.Entries) != 1 {
		t.Fatalf("first normalized snapshot = %#v, ok=%v", first, ok)
	}
	second, ok := manager.recordNormalizedPlanUsageCapture("", "fixture", planUsageCapture{
		entries: []PlanUsageEntry{{Kind: "cost", Amount: 0.25, Currency: "USD"}},
		raw:     map[string]any{"billing": map[string]any{"source": "typed"}},
	})
	if !ok || second.ProviderID != "fixture" || strings.TrimSpace(second.CapturedAt) == "" {
		t.Fatalf("merged normalized snapshot = %#v, ok=%v", second, ok)
	}
	if len(second.Entries) != 2 || second.Entries[0].Kind != "rate-limit" || second.Entries[1].Kind != "cost" {
		t.Fatalf("merged normalized entries = %#v", second.Entries)
	}
	if len(second.Raw) != 2 {
		t.Fatalf("merged normalized raw metadata = %#v", second.Raw)
	}
}

func TestPlanUsageClaudeStructuredRefreshCapturesFiveHourAndWeeklyWindows(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestNormalizedPlanUsageCaptureClearsExplicitResetSnapshot(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	first, ok := manager.recordNormalizedPlanUsageCapture("", "fixture", planUsageCapture{
		resetCredits:    &RateLimitResetCreditsSummary{AvailableCount: 1},
		resetCreditsSet: true,
	})
	if !ok || first.RateLimitResetCredits == nil || first.RateLimitResetCredits.AvailableCount != 1 {
		t.Fatalf("initial earned reset snapshot = %#v, ok=%v", first.RateLimitResetCredits, ok)
	}
	second, ok := manager.recordNormalizedPlanUsageCapture("", "fixture", planUsageCapture{
		resetCreditsSet: true,
	})
	if !ok || second.RateLimitResetCredits != nil {
		t.Fatalf("null earned reset snapshot = %#v, ok=%v; want cleared", second.RateLimitResetCredits, ok)
	}
}

func TestEarnedRateLimitResetRejectsProvidersWithoutNativeCapability(t *testing.T) {
	t.Parallel()
	manager, _ := newPlanUsageFakeManager(t, "claude", "claude-plan-limits")
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "claude-no-earned-reset-tab")
	if _, err := manager.ConsumeRateLimitResetCredit(context.Background(), "claude", session.SessionID, "reset-attempt-1", ""); err == nil {
		t.Fatal("Claude earned-reset consume unexpectedly succeeded")
	}
}

func TestPlanUsageRefreshIsCancelledByReset(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	opts := (Options{}).withDefaults()
	if opts.PlanUsageRefreshInterval != 5*time.Minute {
		t.Fatalf("plan usage refresh interval = %s, want 5m", opts.PlanUsageRefreshInterval)
	}
}

func TestPlanUsagePeriodicTickRefreshesOneLiveSessionPerProviderWithoutPrompt(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestNormalizedPlanUsageCaptureStoresRawWithoutFabricatingEntries(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	snapshot, ok := manager.recordNormalizedPlanUsageCapture("", "custom", planUsageCapture{
		raw: map[string]any{
			"weekly":  map[string]any{"remaining": 42},
			"credits": map[string]any{"used": 7, "size": 100},
		},
	})
	if !ok || len(snapshot.Entries) != 0 {
		t.Fatalf("raw-only normalized capture = %#v, ok=%v", snapshot, ok)
	}
	weekly := mapFromAny(snapshot.Raw["weekly"])
	credits := mapFromAny(snapshot.Raw["credits"])
	if weekly["remaining"] != 42 || credits["used"] != 7 || credits["size"] != 100 {
		t.Fatalf("raw normalized metadata = %#v", snapshot.Raw)
	}
}

func TestPlanUsageRawIsBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	hugeManager := NewManager(Options{})
	t.Cleanup(func() { hugeManager.Reset() })
	hugeSnapshot, ok := hugeManager.recordNormalizedPlanUsageCapture("", "custom", planUsageCapture{
		raw: map[string]any{"blob": strings.Repeat("x", 9000)},
	})
	if !ok {
		t.Fatal("bounded normalized capture was dropped")
	}
	hugeRaw := hugeSnapshot.Raw
	if hugeRaw["_truncated"] != true || hugeRaw["_limitBytes"] != planUsageRawLimitBytes {
		t.Fatalf("huge raw metadata was not capped with note: %#v", hugeRaw)
	}
	if data, err := json.Marshal(hugeRaw); err != nil || len(data) > planUsageRawLimitBytes {
		t.Fatalf("huge raw JSON len=%d err=%v raw=%#v", len(data), err, hugeRaw)
	}

	secretManager := NewManager(Options{})
	t.Cleanup(func() { secretManager.Reset() })
	secretSnapshot, ok := secretManager.recordNormalizedPlanUsageCapture("", "custom", planUsageCapture{
		raw: map[string]any{"api_key": "sk-live-secret", "nested": map[string]any{"safe": "api_key=sk-nested"}},
	})
	if !ok {
		t.Fatal("redacted normalized capture was dropped")
	}
	secretRaw := secretSnapshot.Raw
	if secretRaw["api_key"] != "[redacted]" {
		t.Fatalf("api_key was not redacted: %#v", secretRaw)
	}
	nested := secretRaw["nested"].(map[string]any)
	if nested["safe"] != "api_key=[redacted]" {
		t.Fatalf("nested secret-looking value was not redacted: %#v", nested)
	}
}

func TestPlanUsageReplayToLateClient(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	used := 42.0
	if _, ok := manager.recordNormalizedPlanUsageCapture("", "fixture", planUsageCapture{
		entries: []PlanUsageEntry{{Kind: "rate-limit", ID: "five_hour", UsedPercent: &used}},
	}); !ok {
		t.Fatal("normalized replay fixture was not recorded")
	}

	var replayed []collectedEvent
	manager.PublishProviderSnapshots(func(channel string, payload any) error {
		replayed = append(replayed, collectedEvent{channel: channel, payload: payload})
		return nil
	})
	for _, ev := range replayed {
		if ev.channel != "chat:plan-usage" {
			continue
		}
		snapshot := jsonMap(t, ev.payload)
		if snapshot["providerId"] == "fixture" && len(planEntries(snapshot)) == 1 {
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
