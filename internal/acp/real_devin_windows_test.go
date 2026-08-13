//go:build windows

package acp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealDevinAuthenticationLifecycle is an opt-in protocol canary for the
// vendor-owned Windows CLI. Deterministic fixtures remain the correctness
// oracle; this test proves that Devin's current real auth and ACP shapes still
// traverse the same provider contract. It never judges model output quality.
func TestRealDevinAuthenticationLifecycle(t *testing.T) {
	if os.Getenv("WORKASS_REAL_DEVIN") != "1" {
		t.Skip("set WORKASS_REAL_DEVIN=1 on a Windows Devin installation")
	}
	devin := strings.TrimSpace(os.Getenv("WORKASS_REAL_DEVIN_BIN"))
	if devin == "" {
		t.Fatal("WORKASS_REAL_DEVIN_BIN is required")
	}
	if info, err := os.Stat(devin); err != nil || info.IsDir() {
		t.Fatalf("real Devin executable is unavailable: %v", err)
	}

	t.Run("authenticated-session-and-turn", func(t *testing.T) {
		root := t.TempDir()
		stateDir := filepath.Join(root, "state")
		events := newEventCollector()
		newAuthenticatedManager := func(events *eventCollector) *Manager {
			return NewManager(Options{
				RootDir: root, StateDir: stateDir,
				Providers: []ProviderConfig{{
					ID: "devin", Name: "Devin ACP", Command: devin, Args: []string{"acp"}, Enabled: true,
				}},
				DefaultProviderID: "devin", Broadcast: events.Broadcast,
				InitTimeout: 120 * time.Second, RSSSampleInterval: time.Hour,
			})
		}
		manager := newAuthenticatedManager(events)
		t.Cleanup(func() { manager.Reset() })

		manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
		assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
		session, err := manager.NewSession(context.Background(), SessionOptions{
			TabID: "real-devin-tab", ChatID: "real-devin-chat", ProviderID: "devin",
		})
		if err != nil {
			t.Fatalf("real authenticated Devin session failed")
		}
		job, err := manager.StartJob(context.Background(), JobStartOptions{
			Kind: "app-chat", SessionID: session.SessionID, TabID: "real-devin-tab", ChatID: "real-devin-chat",
			ProviderID: "devin", Prompt: "Reply with a short acknowledgement.",
		})
		if err != nil {
			t.Fatalf("real authenticated Devin turn was not admitted")
		}
		end := events.waitJobEnd(t, jobID(job), 180*time.Second)
		endJob := jobFromEnd(end)
		if got := endJob["status"]; got != "done" {
			t.Fatalf("real authenticated Devin turn status=%v stopReason=%v code=%v error=%q, want done",
				got, endJob["stopReason"], endJob["code"], redactSensitiveText(fmt.Sprint(endJob["error"])))
		}
		firstSessionID := session.SessionID
		manager.Reset()

		restartedEvents := newEventCollector()
		restarted := newAuthenticatedManager(restartedEvents)
		t.Cleanup(func() { restarted.Reset() })
		loaded, err := restarted.NewSession(context.Background(), SessionOptions{
			TabID: "real-devin-tab", ChatID: "real-devin-chat", ProviderID: "devin",
		})
		if err != nil {
			t.Fatalf("real Devin exact session attachment failed")
		}
		if loaded.SessionID != firstSessionID {
			t.Fatalf("real Devin exact attachment changed the saved session identity")
		}
		second, err := restarted.StartJob(context.Background(), JobStartOptions{
			Kind: "app-chat", SessionID: loaded.SessionID, TabID: "real-devin-tab", ChatID: "real-devin-chat",
			ProviderID: "devin", Prompt: "Reply with another short acknowledgement.",
		})
		if err != nil {
			t.Fatalf("real resumed Devin turn was not admitted")
		}
		secondEnd := restartedEvents.waitJobEnd(t, jobID(second), 180*time.Second)
		if got := jobFromEnd(secondEnd)["status"]; got != "done" {
			t.Fatalf("real resumed Devin turn status=%v, want done", got)
		}
		t.Log("canary receipt: session/new, first prompt, process restart, exact same-id load, and second prompt all completed")
	})

	t.Run("isolated-logged-out-state-fails-closed", func(t *testing.T) {
		root := t.TempDir()
		isolatedProfile := filepath.Join(root, "profile")
		isolatedAppData := filepath.Join(isolatedProfile, "AppData", "Roaming")
		isolatedLocalAppData := filepath.Join(isolatedProfile, "AppData", "Local")
		isolatedXDGData := filepath.Join(root, "empty-xdg")
		for _, dir := range []string{
			isolatedAppData,
			isolatedLocalAppData,
			isolatedXDGData,
		} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("create isolated Devin profile directory: %v", err)
			}
		}
		providersFile := filepath.Join(root, "providers.json")
		provider := ProviderConfig{
			ID: "devin", Name: "Devin ACP", Command: devin, Args: []string{"acp"}, Enabled: true,
			Env: map[string]string{
				"APPDATA":          isolatedAppData,
				"XDG_DATA_HOME":    isolatedXDGData,
				"HOME":             isolatedProfile,
				"USERPROFILE":      isolatedProfile,
				"LOCALAPPDATA":     isolatedLocalAppData,
				"HOMEDRIVE":        filepath.VolumeName(root),
				"HOMEPATH":         strings.TrimPrefix(isolatedProfile, filepath.VolumeName(isolatedProfile)),
				"WINDSURF_API_KEY": "",
			},
		}
		manager := NewManager(Options{
			RootDir: root, Providers: []ProviderConfig{provider}, DefaultProviderID: "devin",
			ProviderConfigFile: providersFile,
			InitTimeout:        120 * time.Second, RSSSampleInterval: time.Hour,
		})
		t.Cleanup(func() { manager.Reset() })

		_, sessionErr := manager.NewSession(context.Background(), SessionOptions{
			TabID: "real-devin-logged-out-tab", ChatID: "real-devin-logged-out-chat", ProviderID: "devin",
		})
		if sessionErr == nil {
			t.Fatal("isolated logged-out Devin unexpectedly created a session")
		}
		strategy, strategyErr := manager.providerAuthenticationStrategy("devin")
		if strategyErr != nil || !strategy.IsAuthenticationFailure(sessionErr) {
			t.Fatalf("real logged-out Devin failure was not authentication-shaped: %q",
				redactSensitiveText(sessionErr.Error()))
		}
		devinState := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
		if devinState["fixHint"] != "Ejecuta `devin auth login`" {
			t.Fatalf("real logged-out Devin fix hint = %v", devinState["fixHint"])
		}
		t.Log("canary receipt: isolated first session/new failed authentication; no prompt path was entered")
		manager.Reset()

		persisted, err := LoadProviderConfigs(providersFile, root)
		if err != nil {
			t.Fatalf("reload real Devin provider state: %v", err)
		}
		restarted := NewManager(Options{
			RootDir: root, Providers: persisted, DefaultProviderID: "devin",
			ProviderConfigFile: providersFile, InitTimeout: 120 * time.Second,
			ProviderDetectionRetryBackoffs: []time.Duration{25 * time.Millisecond},
			SpareSessions:                  1, RSSSampleInterval: time.Hour,
		})
		t.Cleanup(func() { restarted.Reset() })
		restarted.StartProviderDetection(context.Background())
		time.Sleep(300 * time.Millisecond)
		if bridges := restarted.allBridges(); len(bridges) != 0 {
			t.Fatalf("persisted needs-login startup created %d Devin bridges", len(bridges))
		}
		t.Log("canary receipt: persisted restart startup detection and spare warming created zero bridges")
		if _, err := restarted.NewSession(context.Background(), SessionOptions{
			TabID: "real-devin-restart-tab", ChatID: "real-devin-restart-chat", ProviderID: "devin",
		}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "login") {
			t.Fatalf("persisted needs-login Devin did not fail closed: %v", err)
		}
		if bridges := restarted.allBridges(); len(bridges) != 0 {
			t.Fatalf("blocked Devin session created %d bridges", len(bridges))
		}
		t.Log("canary receipt: persisted needs-login session request failed before bridge creation")
	})
}
