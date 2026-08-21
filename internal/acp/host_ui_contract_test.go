package acp

import (
	"strings"
	"testing"
)

const expectedPerTurnHostUIRule = "Host UI rule: never use OS accessibility or GUI automation"

func TestWorkassHostUIRuleIsAdjacentToEveryTurn(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	seeded := false

	assertTurn := func(request string) {
		t.Helper()
		result := buildUserRequestBlock(request, true)
		if !seeded {
			result = manager.buildAppChatPrompt(JobStartOptions{HumanAuthored: true}, request)
			seeded = true
		}
		ruleAt := strings.LastIndex(result, expectedPerTurnHostUIRule)
		requestAt := strings.LastIndex(result, "User request:\n"+request)
		if ruleAt < 0 || requestAt < 0 || ruleAt > requestAt {
			t.Fatalf("per-turn host UI rule is missing or misplaced: rule=%d request=%d prompt=%q", ruleAt, requestAt, result)
		}
		for _, want := range []string{
			"osascript", "System Events", "synthetic keyboard or mouse input",
			"Workass control, browser, and shell diagnostic surfaces",
			"report the limitation instead of requesting Accessibility access",
		} {
			if !strings.Contains(result[ruleAt:requestAt], want) {
				t.Fatalf("per-turn host UI rule missing %q: %q", want, result[ruleAt:requestAt])
			}
		}
	}

	assertTurn("inspect the current Workass state")
	// The environment brief is seeded only once. The host UI rule must still be
	// repeated after the session has already been seeded or provider-resumed.
	assertTurn("continue inspecting the same Workass state")
}

func TestEnvironmentBriefForbidsAccessibilityAutomationForWorkass(t *testing.T) {
	t.Parallel()
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	brief := manager.buildEnvironmentBrief(false)
	for _, want := range []string{
		expectedPerTurnHostUIRule,
		"osascript", "System Events",
		"report the limitation instead of requesting Accessibility access",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("environment brief missing host UI law %q:\n%s", want, brief)
		}
	}
}
