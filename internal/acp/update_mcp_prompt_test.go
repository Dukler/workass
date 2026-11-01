package acp

import (
	"strings"
	"testing"

	"workass/internal/agenttext"
)

// The update permission rule lives in AGENTS.md (owner law 2026-10-31). Nothing
// Workass injects may state it: not the per-turn block, the environment brief,
// the native bootstrap or the tool description, which only points at the file.
func TestUpdatePermissionRuleIsNotInjected(t *testing.T) {
	manager := NewManager(Options{WorkassToolsOrigin: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	injected := map[string]string{
		"turn":      manager.buildUserRequestBlock("inspect the failed update", true),
		"agentTurn": manager.buildUserRequestBlock("a scheduled message", false),
		"brief":     manager.buildEnvironmentBrief(false),
		"bootstrap": agenttext.Get("native.bootstrap"),
		"tool":      agenttext.Get("tools.workass_apply_update"),
	}
	for name, text := range injected {
		for _, removed := range []string{
			"Updater tools:", "human request ordering", "human-authored request explicitly",
			"human-authored user request explicitly", "orders an update", "orders installation",
			"never infer authori", "Never infer authori", "Never schedule", "never schedule", "automatically retry",
			"never authorize",
		} {
			if strings.Contains(text, removed) {
				t.Errorf("%s still carries the update rule (%q): %q", name, removed, text)
			}
		}
	}
	if !strings.Contains(injected["tool"], "AGENTS.md") {
		t.Errorf("tool description does not say where the rule lives: %q", injected["tool"])
	}
}
