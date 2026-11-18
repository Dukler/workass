package acp

import (
	"errors"
	"fmt"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

func TestUsageLimitFromPromptErrorReadsTheHostsStructuredLimit(t *testing.T) {
	m := &Manager{planUsageByProvider: map[string]PlanUsageSnapshot{}}
	b := &Bridge{accountScope: "account-abc", providerName: "Claude"}
	m.planUsageByProvider["claude"] = PlanUsageSnapshot{Account: &PlanUsageAccount{Plan: "Max"}}
	err := fmt.Errorf("prompt: %w", &acpError{Code: -32603, Msg: "You've hit your limit", Data: map[string]any{
		"workassUsageLimit": map[string]any{"resetsAt": "2026-11-19T00:00:00.000Z", "kind": "five_hour"},
	}})
	limit := m.usageLimitFromPromptError(b, "claude", err)
	if limit == nil {
		t.Fatal("no limit")
	}
	if limit.ResetsAt != "2026-11-19T00:00:00Z" || limit.Kind != "five_hour" || limit.Label != "Claude Max" || limit.AccountScope != "account-abc" {
		t.Fatalf("limit = %+v", *limit)
	}
	if m.usageLimitFromPromptError(b, "claude", errors.New("plain failure")) != nil {
		t.Fatal("an ordinary failure is not a usage limit")
	}
	if m.usageLimitFromPromptError(b, "claude", &acpError{Msg: "x", Data: map[string]any{"other": 1}}) != nil {
		t.Fatal("data without workassUsageLimit is not a usage limit")
	}
}

func TestUsageLimitWithoutAProviderTimeTakesTheLatestExhaustedWindow(t *testing.T) {
	now := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	full, half := 100.0, 40.0
	m := &Manager{planUsageByProvider: map[string]PlanUsageSnapshot{"codex": {
		Account: &PlanUsageAccount{Plan: "ChatGPT Pro"},
		Entries: []PlanUsageEntry{
			{ResetsAt: "2026-11-18T20:00:00Z", UsedPercent: &full},
			{ResetsAt: "2026-11-20T09:00:00Z", UsedPercent: &half},
			{ResetsAt: "2026-11-18T19:30:00Z", UsedPercent: &full},
			{ResetsAt: "2026-11-18T17:00:00Z", UsedPercent: &full}, // already past
		},
	}}}
	if got := m.UsageLimitReset("codex", now); got != "2026-11-18T20:00:00Z" {
		t.Fatalf("reset = %q", got)
	}
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	m.planUsageByProvider["codex"] = PlanUsageSnapshot{Account: &PlanUsageAccount{Plan: "ChatGPT Pro"}, Entries: []PlanUsageEntry{{ResetsAt: future.Format(time.RFC3339), UsedPercent: &full}}}
	limit := m.usageLimitFromPromptError(&Bridge{providerName: "Codex"}, "codex", &acpError{Data: map[string]any{
		"workassUsageLimit": map[string]any{"resetsAt": "", "kind": "usageLimitExceeded"},
	}})
	if limit == nil || limit.Label != "ChatGPT Pro" || limit.ResetsAt != future.Format(time.RFC3339) {
		t.Fatalf("limit = %+v", limit)
	}
}

func TestUsageLimitLabelNamesTheSubscription(t *testing.T) {
	for _, tc := range []struct{ provider, plan, want string }{
		{"Claude", "Max", "Claude Max"},
		{"Claude", "", "Claude"},
		{"Codex", "ChatGPT Plus", "ChatGPT Plus"},
		{"Claude", "Claude Pro", "Claude Pro"},
		{"", "Max", "Max"},
	} {
		if got := usageLimitLabel(tc.provider, tc.plan); got != tc.want {
			t.Fatalf("usageLimitLabel(%q, %q) = %q, want %q", tc.provider, tc.plan, got, tc.want)
		}
	}
}

func TestUsageLimitSurvivesTheJobMapToTheTerminal(t *testing.T) {
	job := &Job{ID: "j"}
	if _, ok := job.Public()["usageLimit"]; ok {
		t.Fatal("no limit, no field")
	}
	job.UsageLimit = &providercontract.UsageLimit{ResetsAt: "2026-11-19T00:00:00Z", Kind: "five_hour", Label: "Claude Max", AccountScope: "account-abc"}
	got := usageLimitFromWire(mapFromAny(job.Public()["usageLimit"]))
	if got == nil || *got != *job.UsageLimit {
		t.Fatalf("round trip = %+v", got)
	}
}
