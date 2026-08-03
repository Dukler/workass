package acp

import (
	"strings"
	"testing"
	"time"
)

const expectedPerTurnHostUIRule = "Host UI rule: never use OS accessibility or GUI automation"

func TestWorkassHostUIRuleIsAdjacentToEveryTurn(t *testing.T) {
	manager, events := newFakeManager(t, "echo-prompt", Options{})
	t.Cleanup(func() { manager.Reset() })
	session := newFakeSession(t, manager, "host-ui-every-turn")

	assertTurn := func(request string) {
		t.Helper()
		job := startAppChatJob(t, manager, session.SessionID, "host-ui-every-turn", request)
		result := jobFromEnd(events.waitJobEnd(t, jobID(job), 2*time.Second))["result"].(string)
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
