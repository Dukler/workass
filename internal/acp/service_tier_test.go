package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceTierRefusesUnadvertisedControls(t *testing.T) {
	t.Parallel()
	fixture := newPersistentMockFixture(t, "resume")
	manager, _ := fixture.newManagerTuned(nil)
	t.Cleanup(func() { manager.Reset() })
	info, err := manager.NewSession(context.Background(), SessionOptions{TabID: "speed-tab", ChatID: "speed-chat", ProviderID: "mock", CWD: manager.opts.RootDir})
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.applyServiceTier(context.Background(), info.SessionID, ""); err != nil {
		t.Fatal(err)
	}
	if err = manager.applyServiceTier(context.Background(), info.SessionID, "fast"); err == nil {
		t.Fatal("unsupported Fast was silently accepted")
	}
}

func TestWorkassServiceTierEchoCannotReenterActorRefreshBeforeControlReply(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	manager := NewManager(Options{
		RootDir: root, StateDir: t.TempDir(), RuntimeProfile: "dev", DefaultProviderID: "codex",
		InitTimeout: 2 * time.Second, RSSSampleInterval: time.Hour,
		Provider: ProviderConfig{
			ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")},
			CWD: root, Enabled: true, Env: map[string]string{
				"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args),
			},
		},
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: "tier-echo-tab", ChatID: "tier-echo-chat", ProviderID: "codex", CWD: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	manager.SetSessionRefreshFunc(func(map[string]any) {
		entered <- struct{}{}
		<-release // The actor is still waiting for its own control reply.
	})
	done := make(chan error, 1)
	go func() { done <- manager.applyServiceTier(ctx, session.SessionID, "default") }()
	select {
	case err := <-done:
		close(release)
		if err != nil {
			t.Fatalf("apply service tier: %v", err)
		}
	case <-entered:
		cancel()
		close(release)
		<-done
		t.Fatal("Workass-authored service-tier echo re-entered actor refresh before its RPC reply")
	case <-ctx.Done():
		close(release)
		<-done
		t.Fatal("service-tier RPC did not reach its reply or refresh boundary")
	}
}
