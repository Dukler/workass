package acp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeModelScoresClampsAndDropsUnknownData(t *testing.T) {
	raw := map[string]any{
		" Codex ": map[string]any{
			" gpt-test ": map[string]any{
				"intelligence": json.Number("14"), "taste": float64(-2), "cost": float64(8),
				"unknown": 99, "note": strings.Repeat("useful ", 80),
			},
			"empty": map[string]any{"unknown": true},
		},
	}
	scores := NormalizeModelScores(raw)
	score, ok := scores["codex"]["gpt-test"]
	if !ok || score.Intelligence == nil || *score.Intelligence != 10 || score.Taste == nil || *score.Taste != 1 || score.Cost == nil || *score.Cost != 8 {
		t.Fatalf("normalized score = %#v", scores)
	}
	if len([]rune(score.Note)) > 500 {
		t.Fatalf("note was not bounded: %d characters", len([]rune(score.Note)))
	}
	if _, ok := scores["codex"]["empty"]; ok {
		t.Fatalf("empty score survived normalization: %#v", scores)
	}
}

func TestNormalizeModelScoresPreservesTypedSettingsReload(t *testing.T) {
	intelligence, taste, cost := 8, 3, 8
	raw := ModelScores{"codex": {"gpt-5.6-sol": {
		Intelligence: &intelligence, Taste: &taste, Cost: &cost,
		Note: "really smart coding; use xhigh",
	}}}
	normalized := NormalizeModelScores(raw)
	score, ok := normalized["codex"]["gpt-5.6-sol"]
	if !ok || score.Intelligence == nil || *score.Intelligence != 8 || score.Taste == nil || *score.Taste != 3 || score.Cost == nil || *score.Cost != 8 {
		t.Fatalf("typed settings reload = %#v", normalized)
	}
}

func TestRecommendationTreatsHigherCostAsLessDesirable(t *testing.T) {
	cheap, expensive := 2, 9
	intelligence, taste := 8, 7
	weights := map[string]int{"intelligence": 20, "taste": 20, "cost": 60}
	cheapScore, _ := scoreRecommendation(ModelScore{Intelligence: &intelligence, Taste: &taste, Cost: &cheap}, weights)
	expensiveScore, _ := scoreRecommendation(ModelScore{Intelligence: &intelligence, Taste: &taste, Cost: &expensive}, weights)
	if cheapScore <= expensiveScore {
		t.Fatalf("cheap score %.2f must exceed expensive score %.2f", cheapScore, expensiveScore)
	}
}

func TestPermissionIntentModesUsesProviderNativeIds(t *testing.T) {
	codex := permissionIntentModes("codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}})
	if codex["read"] != "read-only" || codex["edit"] != "agent" || codex["full"] != "agent-full-access" {
		t.Fatalf("codex intents = %#v", codex)
	}
	claude := permissionIntentModes("claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}})
	if claude["read"] != "plan" || claude["edit"] != "acceptEdits" || claude["full"] != "bypassPermissions" {
		t.Fatalf("claude intents = %#v", claude)
	}
	mock := permissionIntentModes("mock", []Mode{{ID: "ask"}, {ID: "bypass"}})
	if mock["full"] != "bypass" {
		t.Fatalf("mock full intent = %#v", mock)
	}
}

func TestPermissionIntentInheritanceTranslatesAcrossProviders(t *testing.T) {
	tests := []struct {
		providerID string
		modeID     string
		want       string
	}{
		{providerID: "codex", modeID: "agent-full-access", want: "full"},
		{providerID: "claude", modeID: "bypassPermissions", want: "full"},
		{providerID: "mock", modeID: "bypass", want: "full"},
		{providerID: "codex", modeID: "agent", want: "edit"},
		{providerID: "claude", modeID: "acceptEdits", want: "edit"},
		{providerID: "codex", modeID: "read-only", want: "read"},
		{providerID: "claude", modeID: "plan", want: "read"},
	}
	for _, tt := range tests {
		if got := permissionIntentForMode(tt.providerID, tt.modeID); got != tt.want {
			t.Fatalf("%s/%s intent = %q, want %q", tt.providerID, tt.modeID, got, tt.want)
		}
	}
	claude := permissionIntentModes("claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}})
	if got := claude[permissionIntentForMode("codex", "agent-full-access")]; got != "bypassPermissions" {
		t.Fatalf("Codex full inheritance selected Claude mode %q", got)
	}
	if got := inheritedPermissionMode("codex", "agent-full-access", "claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}}); got != "bypassPermissions" {
		t.Fatalf("resolved Codex→Claude inherited mode = %q", got)
	}
	codex := permissionIntentModes("codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}})
	if got := codex[permissionIntentForMode("claude", "bypassPermissions")]; got != "agent-full-access" {
		t.Fatalf("Claude full inheritance selected Codex mode %q", got)
	}
	if got := inheritedPermissionMode("claude", "bypassPermissions", "codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}}); got != "agent-full-access" {
		t.Fatalf("resolved Claude→Codex inherited mode = %q", got)
	}
}
