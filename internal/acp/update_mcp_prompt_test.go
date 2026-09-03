package acp

import (
	"strings"
	"testing"
)

func TestUpdaterMCPAuthorityRuleIsAdjacentToEveryConfiguredTurn(t *testing.T) {
	manager := NewManager(Options{WorkassMCPBaseURL: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	for _, request := range []string{"inspect the failed update", "continue"} {
		prompt := manager.buildUserRequestBlock(request, true)
		ruleAt := strings.LastIndex(prompt, "Updater tools:")
		requestAt := strings.LastIndex(prompt, "User request:\n"+request)
		if ruleAt < 0 || requestAt < 0 || ruleAt > requestAt {
			t.Fatalf("per-turn updater rule missing or misplaced: %q", prompt)
		}
		for _, want := range []string{
			"workass_get_update_status", "read-only", "bounded, redacted update-failure evidence",
			"workass_apply_update", "CURRENT human-authored user request", "exact machine",
			"never infer authorization", "Never schedule or automatically retry", "same operation_id",
		} {
			if !strings.Contains(prompt[ruleAt:requestAt], want) {
				t.Fatalf("per-turn updater rule missing %q: %q", want, prompt[ruleAt:requestAt])
			}
		}
	}
}

func TestUpdaterMCPIsNotAdvertisedWithoutConfiguredServer(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	if prompt := manager.buildUserRequestBlock("update", true); strings.Contains(prompt, "workass_apply_update") {
		t.Fatalf("unconfigured updater MCP was advertised: %q", prompt)
	}
}
