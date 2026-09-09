package acp

import (
	"strings"
	"testing"
)

func TestConfiguredBrowserPromptIsAdjacentToEveryTopLevelTurn(t *testing.T) {
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })

	for _, request := range []string{"inspect the visible page", "continue with the same page"} {
		prompt := manager.buildUserRequestBlock(request, true)
		ruleAt := strings.LastIndex(prompt, "Browser tools: this top-level Workass chat")
		requestAt := strings.LastIndex(prompt, "User request:\n"+request)
		if ruleAt < 0 || requestAt < 0 || ruleAt > requestAt {
			t.Fatalf("per-turn browser rule is missing or misplaced: rule=%d request=%d prompt=%q", ruleAt, requestAt, prompt)
		}
		for _, want := range []string{
			"Browser tools:",
			"Workass CLI catalog",
			"inspect its schema",
			"workass_browser_list",
			"workass_browser_snapshot",
			"then call it",
			"report the exact Workass tool error",
			"do not ask the user to remind you to use Workass",
		} {
			if !strings.Contains(prompt[ruleAt:requestAt], want) {
				t.Fatalf("per-turn browser rule missing %q: %q", want, prompt[ruleAt:requestAt])
			}
		}
	}
}

func TestUnconfiguredBrowserIsNotAdvertisedPerTurn(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	if prompt := manager.buildUserRequestBlock("inspect the page", true); strings.Contains(prompt, "workass_browser_") {
		t.Fatalf("unconfigured browser was advertised: %q", prompt)
	}
}

// A child is told about the browser only if it has one. Getting this wrong is
// not merely wasteful: on claude it costs a fruitless ToolSearch, and on codex
// the model can call a tool that is not attached.
func TestSubagentBriefOmitsTheBrowserItDoesNotHave(t *testing.T) {
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	if brief := manager.buildEnvironmentBrief(true); strings.Contains(brief, "Browser tools:") {
		t.Fatalf("subagent brief advertises a browser server it is not given: %q", brief)
	}
}
