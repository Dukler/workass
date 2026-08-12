package acp

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeferredProviderStartupKeepsSpareSessionsStoppedUntilRelease(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Provider: ProviderConfig{
			ID: "mock", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "echo-prompt"},
		},
		SpareSessions:        1,
		DeferProviderStartup: true,
		InitTimeout:          2 * time.Second,
		RSSSampleInterval:    time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.mu.Lock()
	readyBeforeRelease := manager.providerStartupReady
	warmingBeforeRelease := len(manager.spareWarming)
	sparesBeforeRelease := len(manager.spareSessions)
	manager.mu.Unlock()
	if readyBeforeRelease {
		t.Fatal("provider startup was released during manager construction")
	}
	if warmingBeforeRelease != 0 || sparesBeforeRelease != 0 {
		t.Fatalf("provider startup warmed spares before release: warming=%d spares=%d", warmingBeforeRelease, sparesBeforeRelease)
	}

	manager.StartProviderStartup()
	manager.mu.Lock()
	readyAfterRelease := manager.providerStartupReady
	manager.mu.Unlock()
	if !readyAfterRelease {
		t.Fatal("provider startup release did not open the provider gate")
	}
}
