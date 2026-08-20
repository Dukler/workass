package acp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var (
	_ providerPlanUsageStrategy = unsupportedPlanUsageStrategy{}
	_ providerPlanUsageStrategy = codexPlanUsageStrategy{}
	_ providerPlanUsageStrategy = claudePlanUsageStrategy{}
)

func TestPlanUsageCarrierDispatchUsesRegisteredStrategy(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	payload := manager.recordUsageUpdate("", "claude", fakeClaudeRateLimitUsageUpdate())
	snapshot, ok := payload["planUsage"].(PlanUsageSnapshot)
	if !ok || snapshot.ProviderID != "claude" || len(snapshot.Entries) != 1 {
		t.Fatalf("registered plan-usage dispatch = %#v", payload["planUsage"])
	}
	entry := snapshot.Entries[0]
	if entry.Kind != "rate-limit" || entry.ID != "five_hour" || entry.UsedPercent == nil || *entry.UsedPercent != 78 {
		t.Fatalf("registered plan-usage capture = %#v", entry)
	}
}

func TestProviderPlanUsageStrategiesNormalizeOnlyTheirRegisteredProtocol(t *testing.T) {
	claudeMeta := mapFromAny(fakeClaudeRateLimitUsageUpdate()["_meta"])
	codexMeta := mapFromAny(fakeCodexPromptResult()["_meta"])
	carrier := map[string]any{
		"_meta": map[string]any{
			"_claude/rateLimit":        claudeMeta["_claude/rateLimit"],
			"workass.codex/rateLimits": fakeCodexRateLimits(),
			"quota":                    codexMeta["quota"],
		},
		"cost": map[string]any{"amount": 0.25, "currency": "USD"},
	}

	codex := (codexPlanUsageStrategy{}).Normalize(carrier)
	assertPlanUsageCaptureKinds(t, codex, []string{"rate-limit", "rate-limit", "quota", "cost"})
	if codex.entries[0].ID != "five_hour" || codex.entries[1].ID != "seven_day" {
		t.Fatalf("Codex rate-limit normalization = %#v", codex.entries)
	}
	if codex.resetCredits == nil || !codex.resetCreditsSet || codex.resetCredits.AvailableCount != 1 {
		t.Fatalf("Codex reset-credit normalization = %#v set=%v", codex.resetCredits, codex.resetCreditsSet)
	}

	claude := (claudePlanUsageStrategy{}).Normalize(carrier)
	assertPlanUsageCaptureKinds(t, claude, []string{"rate-limit", "cost"})
	if claude.entries[0].ID != "five_hour" || claude.entries[0].UsedPercent == nil || *claude.entries[0].UsedPercent != 78 {
		t.Fatalf("Claude rate-limit normalization = %#v", claude.entries[0])
	}
	if claude.resetCreditsSet {
		t.Fatalf("Claude adapter consumed Codex reset metadata: %#v", claude.resetCredits)
	}

	generic := (unsupportedPlanUsageStrategy{}).Normalize(carrier)
	assertPlanUsageCaptureKinds(t, generic, []string{"cost"})
	if generic.resetCreditsSet {
		t.Fatalf("generic ACP adapter consumed vendor reset metadata: %#v", generic.resetCredits)
	}
	for name, capture := range map[string]planUsageCapture{"codex": codex, "claude": claude, "generic": generic} {
		if len(capture.raw) != 3 {
			t.Fatalf("%s raw diagnostic metadata = %#v, want all three source fields", name, capture.raw)
		}
	}
}

func TestProviderPlanUsageStrategiesNormalizeStructuredWindows(t *testing.T) {
	claude := (claudePlanUsageStrategy{}).Normalize(map[string]any{
		"_meta": map[string]any{"workass.claude/usage": fakeClaudeStructuredUsage()},
	})
	assertPlanUsageCaptureRateLimit(t, claude, "five_hour", 37.5, "2026-07-13T20:00:00Z")
	assertPlanUsageCaptureRateLimit(t, claude, "seven_day", 78, "2026-07-15T18:00:00Z")
	assertPlanUsageCaptureRateLimit(t, claude, "seven_day_model:fable", 4, "2026-07-15T18:00:00Z")

	codex := (codexPlanUsageStrategy{}).Normalize(map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": fakeCodexRateLimits()},
	})
	fiveHour := assertPlanUsageCaptureRateLimit(t, codex, "five_hour", 42, "2026-07-13T20:00:00Z")
	if fiveHour.WindowMinutes == nil || *fiveHour.WindowMinutes != 300 || fiveHour.LimitName != "Codex" {
		t.Fatalf("Codex five-hour window = %#v", fiveHour)
	}
	weekly := assertPlanUsageCaptureRateLimit(t, codex, "seven_day", 67, "2026-07-15T20:00:00Z")
	if weekly.WindowMinutes == nil || *weekly.WindowMinutes != 10080 || weekly.LimitName != "Codex" {
		t.Fatalf("Codex weekly window = %#v", weekly)
	}
}

func TestProviderPlanUsageStrategiesNormalizeQuotaAndResetClearing(t *testing.T) {
	codex := codexPlanUsageStrategy{}
	quota := codex.Normalize(fakeCodexPromptResult())
	assertPlanUsageCaptureKinds(t, quota, []string{"quota"})
	if len(quota.entries[0].PerModel) != 1 || quota.entries[0].PerModel[0]["model"] != "gpt-5.6-sol" {
		t.Fatalf("Codex quota normalization = %#v", quota.entries[0])
	}

	cleared := codex.Normalize(map[string]any{
		"_meta": map[string]any{"workass.codex/rateLimits": map[string]any{
			"rateLimits":            fakeCodexRateLimitsWithCredits(0)["rateLimits"],
			"rateLimitResetCredits": nil,
		}},
	})
	if !cleared.resetCreditsSet || cleared.resetCredits != nil {
		t.Fatalf("explicit null reset-credit snapshot = %#v set=%v, want typed clear", cleared.resetCredits, cleared.resetCreditsSet)
	}
}

func TestGenericPlanUsageSourceDoesNotParseProviderProtocol(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "acp", "plan_usage.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{
		"workass.codex/rateLimits",
		"workass.claude/usage",
		"_claude/rateLimit",
		"rateLimitsByLimitId",
		"model_usage",
		"rate_limits",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("generic plan-usage source parses provider protocol field %q", forbidden)
		}
	}
	if !strings.Contains(text, "recordNormalizedPlanUsageCapture") || !strings.Contains(text, ".planUsage.Normalize(carrier)") {
		t.Fatal("generic plan-usage source lost its typed adapter boundary")
	}
}

func assertPlanUsageCaptureKinds(t *testing.T, capture planUsageCapture, want []string) {
	t.Helper()
	if len(capture.entries) != len(want) {
		t.Fatalf("capture kinds count = %d, want %d; entries=%#v", len(capture.entries), len(want), capture.entries)
	}
	for i, kind := range want {
		if capture.entries[i].Kind != kind {
			t.Fatalf("capture kind[%d] = %q, want %q; entries=%#v", i, capture.entries[i].Kind, kind, capture.entries)
		}
	}
}

func assertPlanUsageCaptureRateLimit(t *testing.T, capture planUsageCapture, id string, percent float64, resetsAt string) PlanUsageEntry {
	t.Helper()
	for _, entry := range capture.entries {
		if entry.Kind != "rate-limit" || entry.ID != id {
			continue
		}
		if entry.UsedPercent == nil || *entry.UsedPercent != percent || entry.ResetsAt != resetsAt {
			t.Fatalf("rate limit %s = %#v, want usedPercent=%v resetsAt=%s", id, entry, percent, resetsAt)
		}
		return entry
	}
	t.Fatalf("rate limit %s missing from %#v", id, capture.entries)
	return PlanUsageEntry{}
}
