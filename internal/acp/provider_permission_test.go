package acp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Levels across every provider's own words for a mode. A mode that asks before
// acting ranks with edit. An unknown mode has no level, and that is never read as
// a high one.
func TestPermissionLevelsRankEveryProvidersVocabulary(t *testing.T) {
	for _, row := range []struct{ provider, mode, level string }{
		{"claude", "plan", "read"}, {"claude", "default", "edit"}, {"claude", "acceptEdits", "edit"}, {"claude", "bypassPermissions", "full"},
		{"codex", "read-only", "read"}, {"codex", "agent", "edit"}, {"codex", "agent-full-access", "full"},
		{"mock", "ask", "edit"}, {"mock", "bypass", "full"},
		{"devin", "ask", "edit"}, {"devin", "guardian", "edit"}, {"devin", "dontAsk", "full"},
		{"qwen", "plan", "read"}, {"qwen", "default", "edit"}, {"qwen", "auto-edit", "edit"}, {"qwen", "yolo", "full"},
		{"omp", "plan", "read"}, {"omp", "default", "edit"}, {"omp", "always-ask", "edit"}, {"omp", "write", "edit"}, {"omp", "yolo", "full"},
		{"pi", "native", "full"},
		{"mock", "made-up", ""}, {"claude", "", ""},
	} {
		if got := PermissionLevel(row.provider, row.mode); got != row.level {
			t.Errorf("%s/%s = %q, want %q", row.provider, row.mode, got, row.level)
		}
	}
	if PermissionLevelRank("read") >= PermissionLevelRank("edit") || PermissionLevelRank("edit") >= PermissionLevelRank("full") || PermissionLevelRank("") != 0 {
		t.Fatal("levels do not rank read < edit < full with unknown apart")
	}
}

// The comment here promised this and nothing did it: an explicit mode id walked
// straight past the ceiling a hosted spawn is meant to respect.
func TestAHostedSpawnNamingAModeAboveItsLimitIsRefused(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root, StateDir: t.TempDir(), RuntimeProfile: "test",
		Provider: ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	_, err := manager.SpawnSubagent(context.Background(), SubagentSpawnOptions{
		HostedForOrigin: true, OriginMachine: "m-away", OriginChatID: "asking-chat", OriginTabID: "asking-tab",
		LimitIntent: "read", Prompt: "go", ProviderID: "mock", ModeID: "bypass", CWD: root,
	})
	if err == nil || !strings.Contains(err.Error(), "bypass") || !strings.Contains(err.Error(), "read") {
		t.Fatalf("a read-limited hosted spawn naming bypass = %v", err)
	}
}
