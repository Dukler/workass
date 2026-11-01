package acp

import (
	"strings"
	"testing"

	"workass/internal/agenttext"
)

// Nothing Workass injects sets a policy about updates: not the per-turn block, the
// environment brief, the native bootstrap or the tool description.
func TestUpdatePolicyIsNotInjected(t *testing.T) {
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	injected := map[string]string{
		"turn":      manager.buildUserRequestBlock("inspect the failed update", true),
		"agentTurn": manager.buildUserRequestBlock("a scheduled message", false),
		"brief":     manager.buildEnvironmentBrief(false),
		"bootstrap": agenttext.Get("native.bootstrap"),
		"tool":      agenttext.Get("tools.workass_apply_update"),
		"toolShort": agenttext.Get("tools.short.workass_apply_update"),
	}
	for name, text := range injected {
		for _, removed := range []string{
			"Updater tools:", "human request ordering", "human-authored request explicitly",
			"human-authored user request explicitly", "orders an update", "orders installation",
			"never infer authori", "Never infer authori", "Never schedule", "never schedule", "automatically retry",
			"never authorize", "AGENTS.md", "owner's",
		} {
			if strings.Contains(text, removed) {
				t.Errorf("%s still carries the update policy (%q): %q", name, removed, text)
			}
		}
	}
}
