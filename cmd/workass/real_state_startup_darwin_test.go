//go:build darwin

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

// TestRealCopiedStateStartup is an opt-in migration canary for a disposable
// copy of a real profile. It refuses known live profile roots and never starts
// providers; success means actor cutover and full projection completed from
// the copied bytes.
func TestRealCopiedStateStartup(t *testing.T) {
	if os.Getenv("WORKASS_REAL_STATE_CANARY") != "1" {
		t.Skip("set WORKASS_REAL_STATE_CANARY=1 with a disposable copied state directory")
	}
	stateDir, err := filepath.Abs(strings.TrimSpace(os.Getenv("WORKASS_REAL_STATE_CANARY_DIR")))
	if err != nil || strings.TrimSpace(os.Getenv("WORKASS_REAL_STATE_CANARY_DIR")) == "" {
		t.Fatalf("resolve copied state directory: %v", err)
	}
	liveRoots := []string{
		filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Workass", "state"),
		filepath.Join(repoRoot(t), ".dev", "profiles", "default", "state"),
	}
	for _, live := range liveRoots {
		live, _ = filepath.Abs(live)
		if stateDir == live || strings.HasPrefix(stateDir, live+string(filepath.Separator)) {
			t.Fatalf("real-state canary refuses live profile path %q", stateDir)
		}
	}
	if info, statErr := os.Stat(stateDir); statErr != nil || !info.IsDir() {
		t.Fatalf("copied state directory is unavailable: %v", statErr)
	}

	manager := acp.NewManager(acp.Options{
		StateDir: stateDir, RuntimeProfile: "test", RSSSampleInterval: time.Hour,
		SpawnedWorkReconcileInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newProviderChatRuntimeBeforeProviderStartup(
		manager,
		newSessionStore(filepath.Join(stateDir, sessionStateFilename)),
		stateDir,
	)
	t.Cleanup(func() { _ = runtime.Close(context.Background()) })
	if err := runtime.StartupError(); err != nil {
		t.Fatalf("copied profile did not complete actor startup: %v", err)
	}
	snapshot, err := runtime.ProjectSession()
	if err != nil {
		t.Fatalf("project copied profile after startup: %v", err)
	}
	t.Logf("canary receipt: copied profile projected %d actor chats", len(anySlice(snapshot["chats"])))
}
