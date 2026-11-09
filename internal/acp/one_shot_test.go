package acp

import (
	"strings"
	"testing"
)

// Claude Code's one-shot prompt reads nothing but the message: no tools, no
// setting sources (hooks, CLAUDE.md), no MCP servers, no slash commands and
// no saved session.
func TestClaudeOneShotIsIsolated(t *testing.T) {
	models := OneShotModels("claude")
	if len(models) == 0 || models[0].ID != "haiku" {
		t.Fatalf("one-shot models = %#v", models)
	}
	argv := claudeOneShotStrategy{}.Args("/bin/claude", "haiku", "name it")
	joined := strings.Join(argv, " ")
	for _, flag := range []string{"/bin/claude -p", "--model haiku", "--no-session-persistence", "--strict-mcp-config", "--disable-slash-commands", "--system-prompt name it"} {
		if !strings.Contains(joined, flag) {
			t.Errorf("argv lacks %q: %v", flag, argv)
		}
	}
	for i, arg := range argv {
		if (arg == "--tools" || arg == "--setting-sources") && (i+1 >= len(argv) || argv[i+1] != "") {
			t.Errorf("%s must be empty: %v", arg, argv)
		}
	}
}

// Only a model the provider offers reaches its command line, so a requested
// model can never smuggle in another flag; a provider without a one-shot
// prompt offers nothing.
func TestOneShotCommandRefusesWhatTheProviderDoesNotOffer(t *testing.T) {
	manager := NewManager(Options{StateDir: t.TempDir(), RuntimeProfile: "dev"})
	if argv, reason := manager.OneShotCommand("claude", "--dangerously-skip-permissions", "x"); argv != nil || reason == "" {
		t.Fatalf("a model that is not offered was accepted: %v %q", argv, reason)
	}
	if argv, reason := manager.OneShotCommand("mock", "mock-deterministic", "x"); argv != nil || reason == "" {
		t.Fatalf("a provider without one-shot prompts was accepted: %v %q", argv, reason)
	}
	if models := OneShotModels("mock"); len(models) != 0 {
		t.Fatalf("mock offers one-shot models: %#v", models)
	}
}
