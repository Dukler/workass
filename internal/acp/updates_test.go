package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProviderUpdateCheckFakeRegistry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		installed string
		latest    string
		available bool
	}{
		{name: "latest greater", installed: "qwen-code 0.58.1", latest: "0.58.10", available: true},
		{name: "equal", installed: "0.58.10", latest: "0.58.10", available: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoRoot(t)
			pathDir := t.TempDir()
			installFakeAgentWrapperWithEnv(t, pathDir, "qwen", "echo-prompt", map[string]string{"WORKASS_FAKE_CLI_VERSION": tc.installed})
			t.Setenv("PATH", pathDir)
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test-model"}]}`))
			}))
			defer models.Close()
			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"version": tc.latest})
			}))
			defer registry.Close()

			manager := NewManager(Options{
				RootDir:               root,
				InitTimeout:           800 * time.Millisecond,
				RSSSampleInterval:     time.Hour,
				LocalModelEndpoints:   []string{models.URL + "/v1/models"},
				ProviderUpdateSources: map[string]string{"qwen": registry.URL + "/@qwen-code/qwen-code/latest"},
				ProviderUpdateTimeout: 200 * time.Millisecond,
			})
			t.Cleanup(func() { manager.Reset() })

			result := manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
			if result["ok"] != true {
				t.Fatalf("detect qwen = %#v", result)
			}
			qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
			cliVersion, _ := qwen["cliVersion"].(*CLIVersion)
			if cliVersion == nil || cliVersion.Version != parseSemverToken(tc.installed) || cliVersion.Raw != tc.installed {
				t.Fatalf("qwen cliVersion = %#v in %#v", cliVersion, qwen)
			}

			payload := manager.CheckProviderUpdates(context.Background())
			if len(payload.Updates) != 1 {
				t.Fatalf("updates = %#v", payload)
			}
			update := payload.Updates[0]
			if update.ProviderID != "qwen" || update.CLI != "qwen" || update.Installed != parseSemverToken(tc.installed) ||
				update.Latest != tc.latest || update.UpdateAvailable != tc.available || update.Hint != "qwen update" {
				t.Fatalf("qwen update = %#v", update)
			}
			t.Logf("trace providers:updates qwen installed=%s latest=%s updateAvailable=%v hint=%q", update.Installed, update.Latest, update.UpdateAvailable, update.Hint)
		})
	}
}

func TestProviderCLIExecutableUsesExplicitPathVariable(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	providerPath := filepath.Join(pathDir, "qwen-real")
	writeExecutable(t, providerPath, "#!/bin/sh\nprintf 'qwen-code 0.19.11\\n'\n")
	t.Setenv("WORKASS_QWEN", providerPath)

	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "qwen", Name: "Qwen Code", Command: "qwen", Enabled: true,
		}},
		DefaultProviderID: "qwen",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	resolved, err := manager.providerCLIExecutable("qwen")
	if err != nil {
		t.Fatalf("resolve qwen path: %v", err)
	}
	if resolved != providerPath {
		t.Fatalf("resolved qwen path = %q, want %q", resolved, providerPath)
	}
	version := manager.detectInstalledCLIVersion(context.Background(), "qwen")
	if version == nil || version.Version != "0.19.11" {
		t.Fatalf("qwen version through resolved path = %#v", version)
	}
}

func TestProviderCLIExecutableRefreshesValidCacheFromPATH(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	cachedDir := t.TempDir()
	currentPath := filepath.Join(pathDir, "qwen")
	cachedPath := filepath.Join(cachedDir, "qwen")
	writeExecutable(t, currentPath, "#!/bin/sh\nprintf '0.21.12\\n'\n")
	writeExecutable(t, cachedPath, "#!/bin/sh\nprintf '0.19.11\\n'\n")
	t.Setenv("PATH", pathDir)
	t.Setenv("WORKASS_QWEN", "")
	t.Setenv("ASSISTANT_QWEN", "")
	providerFile := filepath.Join(t.TempDir(), "providers.json")

	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "qwen", Name: "Qwen Code", Command: "qwen", ResolvedCommand: cachedPath, Enabled: true,
		}},
		ProviderConfigFile: providerFile,
		DefaultProviderID:  "qwen",
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	resolved, err := manager.providerCLIExecutable("qwen")
	if err != nil {
		t.Fatalf("refresh qwen executable: %v", err)
	}
	if resolved != currentPath {
		t.Fatalf("resolved qwen path = %q, want current PATH entry %q instead of cached %q", resolved, currentPath, cachedPath)
	}
	version := manager.detectInstalledCLIVersion(context.Background(), "qwen")
	if version == nil || version.Version != "0.21.12" {
		t.Fatalf("refreshed qwen version = %#v", version)
	}
	provider, ok := manager.providerSnapshot("qwen")
	if !ok || provider.ResolvedCommand != currentPath {
		t.Fatalf("refreshed qwen provider = %#v", provider)
	}
}

func TestProviderUpdateRunsResolvedProviderExecutable(t *testing.T) {
	for _, providerID := range []string{"qwen", "claude"} {
		t.Run(providerID, func(t *testing.T) {
			root := repoRoot(t)
			pathDir := t.TempDir()
			t.Setenv("PATH", t.TempDir())
			marker := filepath.Join(t.TempDir(), providerID+"-update-ran")
			versionFile := filepath.Join(t.TempDir(), providerID+"-version")
			if err := os.WriteFile(versionFile, []byte("0.19.1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Deliberately give the executable a nonstandard filename. The updater
			// must use the provider's detected absolute path, not a bare PATH name.
			providerPath := filepath.Join(pathDir, providerID+"-user-install")
			writeExecutable(t, providerPath, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then IFS= read -r v < "+shellQuote(versionFile)+"; printf '%s\\n' \"$v\"; exit 0; fi\nif [ \"$1\" = \"update\" ]; then printf '0.19.2\\n' > "+shellQuote(versionFile)+"; printf 'updated\\n' > "+shellQuote(marker)+"; exit 0; fi\nexit 1\n")

			registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.19.2"})
			}))
			t.Cleanup(registry.Close)
			events := newEventCollector()
			manager := NewManager(Options{
				RootDir: root,
				Providers: []ProviderConfig{{
					ID: providerID, Name: providerID, Command: providerID, ResolvedCommand: providerPath, Enabled: true,
				}},
				DefaultProviderID:        providerID,
				Broadcast:                events.Broadcast,
				RSSSampleInterval:        time.Hour,
				ProviderUpdateSources:    map[string]string{providerID: registry.URL + "/latest"},
				ProviderUpdateTimeout:    200 * time.Millisecond,
				ProviderUpdateRunTimeout: time.Second,
			})
			t.Cleanup(func() { manager.Reset() })

			if _, err := manager.StartProviderUpdate(context.Background(), providerID); err != nil {
				t.Fatalf("start %s update: %v", providerID, err)
			}
			progress := waitProviderUpdateProgress(t, events, providerID, func(progress ProviderUpdateProgress) bool {
				return progress.Status == "done" || progress.Status == "failed"
			}, 2*time.Second)
			if progress.Status != "done" || !fileExists(marker) {
				t.Fatalf("%s update progress=%#v marker=%v", providerID, progress, fileExists(marker))
			}
		})
	}
}

func TestClaudeUpdateReresolvesTransientShimAndAtomicInstallSwap(t *testing.T) {
	restore := stubProviderUpdateRecheckTiming(t)
	defer restore()

	home := t.TempDir()
	t.Setenv("HOME", home)
	installDir := filepath.Join(home, ".local", "bin")
	versionsDir := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(versionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(installDir, "claude")
	oldTarget := filepath.Join(versionsDir, "2.1.223")
	newTarget := filepath.Join(versionsDir, "2.1.224")
	marker := filepath.Join(home, "claude-update-ran")
	writeExecutable(t, newTarget, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '2.1.224 (Claude Code)\\n'; exit 0; fi\nexit 1\n")
	writeExecutable(t, oldTarget, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then printf '2.1.223 (Claude Code)\\n'; exit 0; fi\nif [ \"$1\" = \"update\" ]; then ln -s "+shellQuote(newTarget)+" "+shellQuote(linkPath+".next")+" && mv -f "+shellQuote(linkPath+".next")+" "+shellQuote(linkPath)+" && rm -f "+shellQuote(oldTarget)+" && printf 'done\\n' > "+shellQuote(marker)+"; exit 0; fi\nexit 1\n")
	if err := os.Symlink(oldTarget, linkPath); err != nil {
		t.Fatal(err)
	}

	shimDir := filepath.Join(t.TempDir(), "terminal-cli-shims", "session")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	staleShim := filepath.Join(shimDir, "claude")
	writeExecutable(t, staleShim, "#!/bin/sh\nprintf 'transient shim must not run\\n' >&2\nexit 97\n")
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+installDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "2.1.224"})
	}))
	t.Cleanup(registry.Close)
	deadRoot := filepath.Join(t.TempDir(), "removed-app-runtime")
	if err := os.MkdirAll(deadRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	events := newEventCollector()
	providerFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir:  deadRoot,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: "claude", ResolvedCommand: staleShim, Enabled: true,
		}},
		DefaultProviderID:        "claude",
		ProviderConfigFile:       providerFile,
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateSources:    map[string]string{"claude": registry.URL + "/latest"},
		ProviderUpdateTimeout:    time.Second,
		ProviderUpdateRunTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { manager.Reset() })
	if err := os.RemoveAll(deadRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.StartProviderUpdate(context.Background(), "claude"); err != nil {
		t.Fatalf("start Claude update: %v", err)
	}
	progress := waitProviderUpdateProgress(t, events, "claude", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "done" || progress.Status == "failed"
	}, 3*time.Second)
	if progress.Status != "done" || !fileExists(marker) {
		t.Fatalf("Claude update progress=%#v marker=%v", progress, fileExists(marker))
	}
	if fileExists(oldTarget) {
		t.Fatal("old Claude version target still exists after atomic swap")
	}
	version := manager.detectInstalledCLIVersion(context.Background(), "claude")
	if version == nil || version.Version != "2.1.224" {
		t.Fatalf("post-update Claude version = %#v", version)
	}
	provider, ok := manager.providerSnapshot("claude")
	if !ok || provider.ResolvedCommand != linkPath {
		t.Fatalf("refreshed Claude executable = %#v, want %q", provider, linkPath)
	}
}

func TestProviderUpdateSchedulerDefaultCadence(t *testing.T) {
	opts := (Options{}).withDefaults()
	if opts.ProviderUpdateInterval != time.Hour {
		t.Fatalf("provider update interval = %s, want 1h", opts.ProviderUpdateInterval)
	}
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	if len(opts.ProviderUpdateRetryBackoffs) != len(want) {
		t.Fatalf("provider update retry backoffs = %v, want %v", opts.ProviderUpdateRetryBackoffs, want)
	}
	for i := range want {
		if opts.ProviderUpdateRetryBackoffs[i] != want[i] {
			t.Fatalf("provider update retry backoffs = %v, want %v", opts.ProviderUpdateRetryBackoffs, want)
		}
	}
}

func TestProviderUpdateAvailabilityUsesCardWithoutNotify(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	providerPath := filepath.Join(pathDir, "codex")
	writeExecutable(t, providerPath, "#!/bin/sh\nprintf '0.146.1\\n'\n")
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.146.2"})
	}))
	t.Cleanup(registry.Close)
	providerFile := filepath.Join(t.TempDir(), "providers.json")
	configs := []ProviderConfig{{ID: "codex", Name: "Codex", Command: "codex", ResolvedCommand: providerPath, Enabled: true}}
	if err := SaveProviderConfigs(providerFile, configs); err != nil {
		t.Fatalf("seed provider cache: %v", err)
	}

	newManager := func(events *eventCollector) *Manager {
		manager := NewManager(Options{
			RootDir: root, Providers: configs, DefaultProviderID: "codex", ProviderConfigFile: providerFile,
			Broadcast: events.Broadcast, RSSSampleInterval: time.Hour,
			ProviderUpdateSources: map[string]string{"codex": registry.URL + "/latest"}, ProviderUpdateTimeout: time.Second,
		})
		manager.setProviderCLIVersion("codex", &CLIVersion{Version: "0.146.1", Raw: "0.146.1"})
		return manager
	}

	firstEvents := newEventCollector()
	first := newManager(firstEvents)
	first.CheckProviderUpdates(context.Background())
	firstUpdate := firstEvents.waitChannel(t, "providers:updates", time.Second).payload.(ProviderUpdatesPayload)
	if len(firstUpdate.Updates) != 1 || !firstUpdate.Updates[0].UpdateAvailable || firstUpdate.Updates[0].Latest != "0.146.2" {
		t.Fatalf("provider update card payload = %#v", firstUpdate)
	}
	first.CheckProviderUpdates(context.Background())
	for _, event := range firstEvents.snapshot() {
		if event.channel == "notify" {
			t.Fatalf("provider update emitted duplicate notification = %#v", event.payload)
		}
	}
	first.Reset()

	loaded, err := LoadProviderConfigs(providerFile, root)
	if err != nil {
		t.Fatalf("reload provider cache: %v", err)
	}
	secondEvents := newEventCollector()
	second := NewManager(Options{
		RootDir: root, Providers: loaded, DefaultProviderID: "codex", ProviderConfigFile: providerFile,
		Broadcast: secondEvents.Broadcast, RSSSampleInterval: time.Hour,
		ProviderUpdateSources: map[string]string{"codex": registry.URL + "/latest"}, ProviderUpdateTimeout: time.Second,
	})
	t.Cleanup(func() { second.Reset() })
	second.setProviderCLIVersion("codex", &CLIVersion{Version: "0.146.1", Raw: "0.146.1"})
	second.CheckProviderUpdates(context.Background())
	for _, event := range secondEvents.snapshot() {
		if event.channel == "notify" {
			t.Fatalf("restart emitted duplicate update notification = %#v", event.payload)
		}
	}
}

func TestProviderUpdateSchedulerRepeatsWithoutDaemonRestart(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.58.2"})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:                     repoRoot(t),
		StateDir:                    filepath.Join(t.TempDir(), "state"),
		Providers:                   []ProviderConfig{{ID: "qwen", Name: "Qwen", Command: "qwen", Enabled: true}},
		DefaultProviderID:           "qwen",
		Broadcast:                   events.Broadcast,
		RSSSampleInterval:           time.Hour,
		ProviderUpdateInterval:      time.Hour,
		ProviderUpdateRetryBackoffs: []time.Duration{},
		ProviderUpdateSources:       map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:       200 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: "0.58.1", Raw: "0.58.1"})
	ticks := make(chan time.Time)
	loopDone := make(chan struct{})
	go func() {
		manager.runProviderUpdateTicks(ticks)
		close(loopDone)
	}()
	t.Cleanup(func() {
		close(ticks)
		<-loopDone
	})
	ticks <- time.Now()

	first := events.waitFor(t, time.Second, func(ev collectedEvent) bool {
		payload, ok := ev.payload.(ProviderUpdatesPayload)
		return ev.channel == "providers:updates" && ok && len(payload.Updates) == 1
	}).payload.(ProviderUpdatesPayload)
	waitProviderUpdateSchedulerIdle(t, manager)
	ticks <- time.Now()
	second := events.waitFor(t, time.Second, func(ev collectedEvent) bool {
		payload, ok := ev.payload.(ProviderUpdatesPayload)
		return ev.channel == "providers:updates" && ok && len(payload.Updates) == 1 && payload.CheckedAt != first.CheckedAt
	}).payload.(ProviderUpdatesPayload)
	if first.Updates[0].ProviderID != "qwen" || second.Updates[0].ProviderID != "qwen" {
		t.Fatalf("periodic provider updates = first %#v second %#v", first, second)
	}
	t.Logf("trace periodic provider updates first=%s second=%s configuredInterval=%s", first.CheckedAt, second.CheckedAt, manager.opts.ProviderUpdateInterval)
}

func TestProviderUpdateSchedulerRetriesRegistryFailure(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		request := requests
		mu.Unlock()
		if request == 1 {
			http.Error(w, "temporary registry failure", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.58.2"})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:                     repoRoot(t),
		StateDir:                    filepath.Join(t.TempDir(), "state"),
		Providers:                   []ProviderConfig{{ID: "qwen", Name: "Qwen", Command: "qwen", Enabled: true}},
		DefaultProviderID:           "qwen",
		Broadcast:                   events.Broadcast,
		RSSSampleInterval:           time.Hour,
		ProviderUpdateInterval:      time.Hour,
		ProviderUpdateRetryBackoffs: []time.Duration{10 * time.Millisecond},
		ProviderUpdateSources:       map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:       200 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: "0.58.1", Raw: "0.58.1"})
	manager.scheduleProviderUpdateCheck()

	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Latest == "0.58.2" && update.UpdateAvailable
	}, time.Second)
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 2 {
		t.Fatalf("registry requests = %d, want initial + one retry", gotRequests)
	}
	t.Logf("trace provider update retry requests=%d installed=%s latest=%s", gotRequests, update.Installed, update.Latest)
}

func TestProviderUpdateSchedulerRetriesPartialRegistryFailure(t *testing.T) {
	var mu sync.Mutex
	requests := map[string]int{}
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provider := strings.Trim(strings.TrimSpace(r.URL.Path), "/")
		mu.Lock()
		requests[provider]++
		request := requests[provider]
		mu.Unlock()
		if provider == "claude" && request == 1 {
			http.Error(w, "temporary claude registry failure", http.StatusServiceUnavailable)
			return
		}
		versions := map[string]string{"qwen": "0.19.10", "claude": "2.1.208"}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": versions[provider]})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:                     repoRoot(t),
		StateDir:                    filepath.Join(t.TempDir(), "state"),
		Providers:                   []ProviderConfig{{ID: "qwen", Name: "Qwen", Command: "qwen", Enabled: true}, {ID: "claude", Name: "Claude", Command: "claude", Enabled: true}},
		DefaultProviderID:           "qwen",
		Broadcast:                   events.Broadcast,
		RSSSampleInterval:           time.Hour,
		ProviderUpdateInterval:      time.Hour,
		ProviderUpdateRetryBackoffs: []time.Duration{10 * time.Millisecond},
		ProviderUpdateSources:       map[string]string{"qwen": registry.URL + "/qwen", "claude": registry.URL + "/claude"},
		ProviderUpdateTimeout:       200 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: "0.19.9", Raw: "0.19.9"})
	manager.setProviderCLIVersion("claude", &CLIVersion{Version: "2.1.207", Raw: "2.1.207"})
	manager.scheduleProviderUpdateCheck()

	update := waitProviderUpdate(t, events, "claude", func(update ProviderUpdate) bool {
		return update.Latest == "2.1.208" && update.UpdateAvailable
	}, time.Second)
	mu.Lock()
	qwenRequests := requests["qwen"]
	claudeRequests := requests["claude"]
	mu.Unlock()
	if qwenRequests != 2 || claudeRequests != 2 {
		t.Fatalf("registry requests qwen=%d claude=%d, want 2 each after partial-failure retry", qwenRequests, claudeRequests)
	}
	t.Logf("trace partial provider retry qwenRequests=%d claudeRequests=%d claudeLatest=%s", qwenRequests, claudeRequests, update.Latest)
}

func TestProviderUpdateScheduledChecksAreSingleFlight(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var mu sync.Mutex
	requests := 0
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		request := requests
		mu.Unlock()
		if request == 1 {
			started <- struct{}{}
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.58.2"})
	}))
	t.Cleanup(registry.Close)
	manager := NewManager(Options{
		RootDir:                     repoRoot(t),
		StateDir:                    filepath.Join(t.TempDir(), "state"),
		Providers:                   []ProviderConfig{{ID: "qwen", Name: "Qwen", Command: "qwen", Enabled: true}},
		DefaultProviderID:           "qwen",
		RSSSampleInterval:           time.Hour,
		ProviderUpdateInterval:      time.Hour,
		ProviderUpdateRetryBackoffs: []time.Duration{},
		ProviderUpdateSources:       map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:       time.Second,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: "0.58.1", Raw: "0.58.1"})
	manager.scheduleProviderUpdateCheck()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduled update check did not reach registry")
	}
	for i := 0; i < 20; i++ {
		manager.scheduleProviderUpdateCheck()
	}
	mu.Lock()
	gotRequests := requests
	mu.Unlock()
	if gotRequests != 1 {
		t.Fatalf("overlapping scheduled registry requests = %d, want 1", gotRequests)
	}
	releaseOnce.Do(func() { close(release) })
	t.Logf("trace provider update single-flight requestsWhileBlocked=%d", gotRequests)
}

func waitProviderUpdateSchedulerIdle(t *testing.T, manager *Manager) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		manager.updateCheckMu.Lock()
		running := manager.updateCheckRunning
		manager.updateCheckMu.Unlock()
		if !running {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("provider update scheduler did not become idle")
}

func TestProviderUpdateInvokeProgressNoProcRegistryAndReplay(t *testing.T) {
	manager, events, versionFile := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	updateScript := filepath.Join(t.TempDir(), "qwen-update")
	writeExecutable(t, updateScript, "#!/bin/sh\nprintf 'updater start\\n'\n/bin/sleep 0.1\nprintf '0.58.2\\n' > "+shellQuote(versionFile)+"\nprintf 'updater done\\n'\n")
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}

	result, err := manager.StartProviderUpdate(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("providers:update success start: %v", err)
	}
	if result["ok"] != true || result["providerId"] != "qwen" || result["processId"] != nil || result["process"] != nil {
		t.Fatalf("providers:update result = %#v", result)
	}
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("provider updater leaked into proc:list: %#v", processes)
	}

	running := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "running"
	}, 2*time.Second)
	_ = events.waitChannel(t, "providers:list", 2*time.Second)
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Installed == "0.58.2" && update.Latest == "0.58.2" && !update.UpdateAvailable
	}, 2*time.Second)
	done := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "done" && progress.ExitCode != nil && *progress.ExitCode == 0 && strings.Contains(progress.Tail, "updater done")
	}, 2*time.Second)
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("provider updater leaked after exit into proc:list: %#v", processes)
	}
	assertNoCollectedChannel(t, events, "proc:changed")
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusInactive, true)
	version, _ := qwen["cliVersion"].(*CLIVersion)
	if version == nil || version.Version != "0.58.2" {
		t.Fatalf("post-update cliVersion = %#v", version)
	}
	var replayed bool
	manager.PublishProviderSnapshots(func(channel string, payload any) error {
		if channel == "providers:update-progress" {
			progress, ok := payload.(ProviderUpdateProgress)
			if ok && progress.ProviderID == "qwen" && progress.Status == "done" && strings.Contains(progress.Tail, "updater done") {
				replayed = true
			}
		}
		return nil
	})
	if !replayed {
		t.Fatalf("providers:update-progress was not replayed for qwen")
	}
	t.Logf("trace providers:update progress running startedAt=%s done exitCode=%d tail=%q installed=%s latest=%s updateAvailable=%v", running.StartedAt, *done.ExitCode, done.Tail, update.Installed, update.Latest, update.UpdateAvailable)
}

func TestQwenStandaloneUpdateUsesBundledCLI(t *testing.T) {
	root := repoRoot(t)
	prefix := t.TempDir()
	standaloneRoot := filepath.Join(prefix, "lib", "qwen-code")
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(standaloneRoot, "node", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(standaloneRoot, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	versionFile := filepath.Join(t.TempDir(), "qwen-version")
	marker := filepath.Join(t.TempDir(), "bundled-update-ran")
	if err := os.WriteFile(versionFile, []byte("0.58.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"name":"@qwen-code/qwen-code","version":"0.58.1"}`)
	if err := os.WriteFile(filepath.Join(standaloneRoot, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	cliPath := filepath.Join(standaloneRoot, "lib", "cli.js")
	if err := os.WriteFile(cliPath, []byte("// qwen standalone cli fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(binDir, "qwen")
	writeExecutable(t, launcher, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then IFS= read -r v < "+shellQuote(versionFile)+"; printf '%s\\n' \"$v\"; exit 0; fi\nprintf 'public qwen update must not run\\n' >&2\nexit 91\n")
	nodePath := filepath.Join(standaloneRoot, "node", "bin", "node")
	writeExecutable(t, nodePath, "#!/bin/sh\nif [ \"$1\" = "+shellQuote(cliPath)+" ] && [ \"$2\" = \"update\" ]; then printf '0.58.2\\n' > "+shellQuote(versionFile)+"; printf 'bundled standalone updater\\n' > "+shellQuote(marker)+"; exit 0; fi\nexit 92\n")
	t.Setenv("WORKASS_QWEN", launcher)

	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.58.2"})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Providers: []ProviderConfig{{
			ID: "qwen", Name: "Qwen Code ACP", Command: "qwen", Args: []string{"--acp"}, Enabled: true,
		}},
		DefaultProviderID:        "qwen",
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateSources:    map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:    200 * time.Millisecond,
		ProviderUpdateRunTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { manager.Reset() })

	if _, err := manager.StartProviderUpdate(context.Background(), "qwen"); err != nil {
		t.Fatalf("start Qwen standalone update: %v", err)
	}
	progress := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "done" || progress.Status == "failed"
	}, 3*time.Second)
	if progress.Status != "done" || progress.ExitCode == nil || *progress.ExitCode != 0 || !fileExists(marker) {
		t.Fatalf("Qwen standalone progress=%#v marker=%v", progress, fileExists(marker))
	}
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Installed == "0.58.2" && update.Latest == "0.58.2" && !update.UpdateAvailable
	}, 2*time.Second)
	if update.UpdateAvailable {
		t.Fatalf("Qwen standalone update remained pending: %#v", update)
	}
}

func TestProviderUpdateZeroExitWithoutVersionAdvanceFailsVerification(t *testing.T) {
	manager, events, _ := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	updateScript := filepath.Join(t.TempDir(), "qwen-update-no-change")
	writeExecutable(t, updateScript, "#!/bin/sh\nprintf 'Run the following to update:\\n  npm install -g @qwen-code/qwen-code@0.58.2\\n'\nexit 0\n")
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}

	if _, err := manager.StartProviderUpdate(context.Background(), "qwen"); err != nil {
		t.Fatalf("start no-change update: %v", err)
	}
	progress := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "failed"
	}, 2*time.Second)
	if progress.ExitCode == nil || *progress.ExitCode != -1 || !strings.Contains(progress.Error, "sin instalar qwen") {
		t.Fatalf("no-change progress = %#v", progress)
	}
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.UpdateAvailable && update.LastError != ""
	}, 2*time.Second)
	if update.Installed != "0.58.1" || update.Latest != "0.58.2" {
		t.Fatalf("no-change update = %#v", update)
	}
}

func TestProviderUpdateTerminalReceiptDoesNotWaitForRegistryRefresh(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	t.Setenv("PATH", pathDir)
	versionFile := filepath.Join(t.TempDir(), "claude-version")
	if err := os.WriteFile(versionFile, []byte("2.1.223\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(pathDir, "claude")
	writeExecutable(t, claudePath, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then IFS= read -r v < "+shellQuote(versionFile)+"; printf '%s\\n' \"$v\"; exit 0; fi\nif [ \"$1\" = \"update\" ]; then printf '2.1.224\\n' > "+shellQuote(versionFile)+"; exit 0; fi\nexit 1\n")

	var requestsMu sync.Mutex
	requests := 0
	refreshStarted := make(chan struct{})
	refreshRelease := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(refreshRelease) }) })
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsMu.Lock()
		requests++
		n := requests
		requestsMu.Unlock()
		if n > 1 {
			startedOnce.Do(func() { close(refreshStarted) })
			<-refreshRelease
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "2.1.224"})
	}))
	t.Cleanup(registry.Close)

	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code", Command: "claude", ResolvedCommand: claudePath, Enabled: true,
		}},
		DefaultProviderID:        "claude",
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateSources:    map[string]string{"claude": registry.URL + "/latest"},
		ProviderUpdateTimeout:    2 * time.Second,
		ProviderUpdateRunTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { manager.Reset() })

	if _, err := manager.StartProviderUpdate(context.Background(), "claude"); err != nil {
		t.Fatal(err)
	}
	progress := waitProviderUpdateProgress(t, events, "claude", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "done" || progress.Status == "failed"
	}, time.Second)
	if progress.Status != "done" {
		t.Fatalf("Claude terminal progress = %#v", progress)
	}
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("post-update registry refresh did not start")
	}
	releaseOnce.Do(func() { close(refreshRelease) })
}

func TestProviderUpdateInvokeFailureKeepsCardWithRedactedTail(t *testing.T) {
	manager, events, _ := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
	updateScript := filepath.Join(t.TempDir(), "qwen-update-fail")
	writeExecutable(t, updateScript, "#!/bin/sh\ni=0\nwhile [ $i -lt 260 ]; do printf 'line %s api_key=supersecret\\n' \"$i\"; i=$((i+1)); done\nexit 42\n")
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}

	result, err := manager.StartProviderUpdate(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("providers:update failure start: %v", err)
	}
	if result["ok"] != true || result["providerId"] != "qwen" || result["processId"] != nil {
		t.Fatalf("providers:update failure result = %#v", result)
	}
	progress := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "failed" && progress.ExitCode != nil && *progress.ExitCode == 42 && progress.Error != "" && progress.Tail != ""
	}, 2*time.Second)
	if strings.Contains(progress.Tail, "supersecret") || !strings.Contains(progress.Tail, "api_key=[redacted]") {
		t.Fatalf("progress tail was not redacted: %q", progress.Tail)
	}
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.UpdateAvailable && update.ExitCode != nil && *update.ExitCode == 42 && update.LastError != "" && update.Tail != ""
	}, 2*time.Second)
	if strings.Contains(update.Tail, "supersecret") || !strings.Contains(update.Tail, "api_key=[redacted]") {
		t.Fatalf("failure tail was not redacted: %q", update.Tail)
	}
	if len(update.Tail) > providerUpdateTailBytes {
		t.Fatalf("failure tail length = %d, want <= %d", len(update.Tail), providerUpdateTailBytes)
	}
	if len(progress.Tail) > providerUpdateTailBytes {
		t.Fatalf("progress tail length = %d, want <= %d", len(progress.Tail), providerUpdateTailBytes)
	}
	if processes := manager.Processes(); len(processes) != 0 {
		t.Fatalf("failed provider updater leaked into proc:list: %#v", processes)
	}
	assertNoCollectedChannel(t, events, "proc:changed")
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close()
	manager.opts.ProviderUpdateSources = map[string]string{"qwen": down.URL + "/latest"}
	fallback := manager.CheckProviderUpdates(context.Background())
	if len(fallback.Updates) != 1 || fallback.Updates[0].LastError == "" || fallback.Updates[0].Tail == "" || !fallback.Updates[0].UpdateAvailable {
		t.Fatalf("failure fallback update = %#v", fallback)
	}
	t.Logf("trace providers:update failure progress status=%s exitCode=%d error=%q tailBytes=%d updateLastError=%q", progress.Status, *progress.ExitCode, progress.Error, len(progress.Tail), update.LastError)
}

func TestProviderUpdatePostRecheckRetriesUntilVersionLands(t *testing.T) {
	restore := stubProviderUpdateRecheckTiming(t)
	defer restore()
	manager, events, retryLogs, versionFile := newProviderUpdateRecheckTestManager(t, providerUpdateRecheckScript{
		Installed:          "0.58.1",
		Latest:             "0.58.2",
		PostUpdateFailures: 2,
	})
	updateScript := filepath.Join(t.TempDir(), "qwen-update-recheck")
	writeExecutable(t, updateScript, "#!/bin/sh\nprintf '0.58.2\\n' > "+shellQuote(versionFile)+"\nprintf 'updated\\n'\n")
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}

	if _, err := manager.StartProviderUpdate(context.Background(), "qwen"); err != nil {
		t.Fatalf("providers:update recheck retry start: %v", err)
	}
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Installed == "0.58.2" && update.Latest == "0.58.2" && !update.UpdateAvailable && update.RecheckError == ""
	}, 2*time.Second)
	if got := retryLogs(); got != 2 {
		t.Fatalf("post-update recheck retry logs = %d, want 2", got)
	}
	t.Logf("trace providers:update recheck retry installed=%s latest=%s updateAvailable=%v retries=%d", update.Installed, update.Latest, update.UpdateAvailable, retryLogs())
}

func TestProviderUpdatePostRecheckAllFailKeepsEntryWithRecheckError(t *testing.T) {
	restore := stubProviderUpdateRecheckTiming(t)
	defer restore()
	manager, events, retryLogs, versionFile := newProviderUpdateRecheckTestManager(t, providerUpdateRecheckScript{
		Installed:          "0.58.1",
		Latest:             "0.58.2",
		PostUpdateFailures: 99,
	})
	updateScript := filepath.Join(t.TempDir(), "qwen-update-recheck-fail")
	writeExecutable(t, updateScript, "#!/bin/sh\nprintf '0.58.2\\n' > "+shellQuote(versionFile)+"\nprintf 'updated but verify races\\n'\n")
	manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}

	if _, err := manager.StartProviderUpdate(context.Background(), "qwen"); err != nil {
		t.Fatalf("providers:update recheck all-fail start: %v", err)
	}
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.UpdateAvailable && update.Installed == "0.58.1" && update.Latest == "0.58.2" && update.RecheckError != ""
	}, 2*time.Second)
	if update.LastError != "" || update.ExitCode != nil || update.Tail != "" {
		t.Fatalf("recheck failure polluted updater failure fields: %#v", update)
	}
	if got := retryLogs(); got != 3 {
		t.Fatalf("post-update recheck retry logs = %d, want 3", got)
	}
	t.Logf("trace providers:update recheck failed installed=%s latest=%s updateAvailable=%v recheckError=%q retries=%d", update.Installed, update.Latest, update.UpdateAvailable, update.RecheckError, retryLogs())
}

func TestProviderUpdateInvokeRejectsDoubleUnknownAndNoPending(t *testing.T) {
	t.Run("double invoke", func(t *testing.T) {
		manager, events, _ := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
		updateScript := filepath.Join(t.TempDir(), "qwen-update-slow")
		writeExecutable(t, updateScript, "#!/bin/sh\nprintf 'slow update\\n'\n/bin/sleep 0.4\n")
		manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}
		first, err := manager.StartProviderUpdate(context.Background(), "qwen")
		if err != nil {
			t.Fatalf("first update: %v", err)
		}
		_, err = manager.StartProviderUpdate(context.Background(), "qwen")
		if structuredErrorCode(err) != "providers:update-in-progress" || !strings.Contains(err.Error(), "ya hay una actualización en curso") {
			t.Fatalf("second update error = %v", err)
		}
		_ = events.waitChannel(t, "providers:updates", 2*time.Second)
		if first["processId"] != nil || first["process"] != nil {
			t.Fatalf("first update leaked process fields: %#v", first)
		}
		t.Logf("trace providers:update double rejected provider=%s code=%s", first["providerId"], structuredErrorCode(err))
	})

	t.Run("unknown provider", func(t *testing.T) {
		manager, _, _ := newProviderUpdateTestManager(t, "0.58.1", "0.58.2", "")
		_, err := manager.StartProviderUpdate(context.Background(), "missing")
		if structuredErrorCode(err) != "providers:update-unknown-provider" {
			t.Fatalf("unknown provider error = %v", err)
		}
	})

	t.Run("no pending", func(t *testing.T) {
		manager, _, _ := newProviderUpdateTestManager(t, "0.58.2", "0.58.2", "")
		updateScript := filepath.Join(t.TempDir(), "qwen-update-noop")
		writeExecutable(t, updateScript, "#!/bin/sh\nexit 0\n")
		manager.opts.ProviderUpdateCommands = map[string]ProviderUpdateCommand{"qwen": {Command: updateScript}}
		_, err := manager.StartProviderUpdate(context.Background(), "qwen")
		if structuredErrorCode(err) != "providers:update-no-pending" {
			t.Fatalf("no pending error = %v", err)
		}
	})
}

func TestProviderUpdateCheckRegistryFailuresOmitEntries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source func(t *testing.T) string
	}{
		{
			name: "down",
			source: func(t *testing.T) string {
				t.Helper()
				registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
				registry.Close()
				return registry.URL + "/latest"
			},
		},
		{
			name: "timeout",
			source: func(t *testing.T) string {
				t.Helper()
				registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					time.Sleep(200 * time.Millisecond)
					_, _ = w.Write([]byte(`{"version":"0.58.10"}`))
				}))
				t.Cleanup(registry.Close)
				return registry.URL + "/latest"
			},
		},
		{
			name: "garbage",
			source: func(t *testing.T) string {
				t.Helper()
				registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{"version":"garbage"}`))
				}))
				t.Cleanup(registry.Close)
				return registry.URL + "/latest"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := repoRoot(t)
			pathDir := t.TempDir()
			installFakeAgentWrapperWithEnv(t, pathDir, "qwen", "echo-prompt", map[string]string{"WORKASS_FAKE_CLI_VERSION": "0.58.1"})
			t.Setenv("PATH", pathDir)
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test-model"}]}`))
			}))
			defer models.Close()

			manager := NewManager(Options{
				RootDir:               root,
				InitTimeout:           800 * time.Millisecond,
				RSSSampleInterval:     time.Hour,
				LocalModelEndpoints:   []string{models.URL + "/v1/models"},
				ProviderUpdateSources: map[string]string{"qwen": tc.source(t)},
				ProviderUpdateTimeout: 25 * time.Millisecond,
			})
			t.Cleanup(func() { manager.Reset() })

			result := manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
			if result["ok"] != true {
				t.Fatalf("detection should complete despite registry %s: %#v", tc.name, result)
			}
			assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
			payload := manager.CheckProviderUpdates(context.Background())
			if len(payload.Updates) != 0 {
				t.Fatalf("registry %s updates = %#v, want no entries", tc.name, payload)
			}
			t.Logf("trace providers:updates registry=%s checkedAt=%s entries=%d", tc.name, payload.CheckedAt, len(payload.Updates))
		})
	}
}

func TestVersionParsingRealOutputFixtures(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: "2.1.205 (Claude Code)", want: "2.1.205"},
		{raw: "codex-cli 0.144.0", want: "0.144.0"},
		{raw: "0.19.8", want: "0.19.8"},
	} {
		if got := parseSemverToken(tc.raw); got != tc.want {
			t.Fatalf("parseSemverToken(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestLenientSemverCompareEdges(t *testing.T) {
	for _, tc := range []struct {
		installed string
		latest    string
		wantCmp   int
		wantOK    bool
	}{
		{installed: "0.58.1", latest: "0.58.10", wantCmp: 1, wantOK: true},
		{installed: "1.0.0-beta", latest: "1.0.0", wantCmp: 1, wantOK: true},
		{installed: "1.0.0", latest: "1.0.0-beta", wantCmp: -1, wantOK: true},
		{installed: "garbage", latest: "1.0.0", wantOK: false},
		{installed: "1.0.0", latest: "garbage", wantOK: false},
	} {
		got, ok := compareLenientSemver(tc.installed, tc.latest)
		if ok != tc.wantOK || got != tc.wantCmp {
			t.Fatalf("compareLenientSemver(%q, %q) = (%d,%v), want (%d,%v)", tc.installed, tc.latest, got, ok, tc.wantCmp, tc.wantOK)
		}
	}
}

func TestRealProviderUpdateCheck(t *testing.T) {
	if os.Getenv("WORKASS_REAL_UPDATE_CHECK") != "1" {
		t.Skip("set WORKASS_REAL_UPDATE_CHECK=1 to query real vendor registries")
	}
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir:               root,
		RSSSampleInterval:     time.Hour,
		ProviderUpdateTimeout: 10 * time.Second,
	})
	t.Cleanup(func() { manager.Reset() })

	for _, providerID := range []string{"claude", "codex", "qwen"} {
		version := manager.detectInstalledCLIVersion(context.Background(), providerID)
		if version == nil || version.Version == "" {
			t.Fatalf("installed %s CLI version not resolved", providerID)
		}
		manager.mu.Lock()
		if runtime := manager.providers[providerID]; runtime != nil {
			runtime.CLIVersion = version
		}
		manager.mu.Unlock()
		t.Logf("trace real installed provider=%s raw=%q parsed=%s", providerID, version.Raw, version.Version)
	}

	payload := manager.CheckProviderUpdates(context.Background())
	if len(payload.Updates) != 3 {
		data, _ := json.Marshal(payload)
		t.Fatalf("real update payload has %d entries, want 3: %s", len(payload.Updates), data)
	}
	for _, update := range payload.Updates {
		t.Logf("trace real update provider=%s cli=%s installed=%s latest=%s updateAvailable=%v hint=%q", update.ProviderID, update.CLI, update.Installed, update.Latest, update.UpdateAvailable, update.Hint)
	}
}

func TestRealProviderUpdateInvokeQwen(t *testing.T) {
	if os.Getenv("WORKASS_REAL_PROVIDER_UPDATE") != "1" {
		t.Skip("set WORKASS_REAL_PROVIDER_UPDATE=1 to run real providers:update for qwen")
	}
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:                  repoRoot(t),
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateTimeout:    10 * time.Second,
		ProviderUpdateRunTimeout: 10 * time.Minute,
	})
	t.Cleanup(func() { manager.Reset() })

	before, ok := manager.currentProviderUpdate(context.Background(), "qwen")
	if !ok {
		t.Fatalf("qwen update status could not be resolved")
	}
	if !before.UpdateAvailable {
		t.Skipf("qwen has no pending update: installed=%s latest=%s", before.Installed, before.Latest)
	}
	result, err := manager.StartProviderUpdate(context.Background(), "qwen")
	if err != nil {
		t.Fatalf("real qwen providers:update start: %v", err)
	}
	if result["ok"] != true || result["providerId"] != "qwen" || result["processId"] != nil {
		t.Fatalf("real qwen providers:update result = %#v", result)
	}
	progress := waitProviderUpdateProgress(t, events, "qwen", func(progress ProviderUpdateProgress) bool {
		return progress.Status == "done" || progress.Status == "failed"
	}, 11*time.Minute)
	var code any
	if progress.ExitCode != nil {
		code = *progress.ExitCode
	}
	t.Logf("trace real providers:update status=%s code=%v error=%q tail:\n%s", progress.Status, code, progress.Error, progress.Tail)
	update := waitProviderUpdate(t, events, "qwen", func(update ProviderUpdate) bool {
		return update.Installed != before.Installed || update.LastError != "" || !update.UpdateAvailable
	}, 30*time.Second)
	t.Logf("trace real providers:updates qwen installed=%s latest=%s updateAvailable=%v lastError=%q exitCode=%v tail=%q", update.Installed, update.Latest, update.UpdateAvailable, update.LastError, update.ExitCode, update.Tail)
	if update.UpdateAvailable {
		t.Fatalf("qwen update still pending after providers:update: installed=%s latest=%s lastError=%q exitCode=%v", update.Installed, update.Latest, update.LastError, update.ExitCode)
	}
}

func TestProviderUpdatesPayloadSkipsIncomparableInstalledVersion(t *testing.T) {
	manager := NewManager(Options{
		RootDir:           repoRoot(t),
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.providers["qwen"].CLIVersion = &CLIVersion{Version: "garbage", Raw: "garbage"}
	manager.mu.Unlock()
	payload := manager.CheckProviderUpdates(context.Background())
	if len(payload.Updates) != 0 {
		t.Fatalf("garbage installed version produced update entry: %#v", payload)
	}
}

func TestMockAppUpdatePayloadFromEnv(t *testing.T) {
	t.Setenv(mockUpdateEnv, "0.0.2-mock")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:           repoRoot(t),
		Broadcast:         events.Broadcast,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ev := events.waitChannel(t, "app:update", time.Second)
	payload := ev.payload.(map[string]any)
	if payload["version"] != "0.0.2-mock" || payload["mocked"] != true {
		t.Fatalf("app:update payload = %#v", payload)
	}
}

func TestLatestCLIVersionRequiresComparableRegistryVersion(t *testing.T) {
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"version":"not-a-version"}`)
	}))
	defer registry.Close()
	manager := NewManager(Options{
		RootDir:               repoRoot(t),
		RSSSampleInterval:     time.Hour,
		ProviderUpdateTimeout: 100 * time.Millisecond,
	})
	t.Cleanup(func() { manager.Reset() })
	_, err := manager.latestCLIVersion(context.Background(), providerUpdateSpec{ProviderID: "qwen", Source: registry.URL})
	if err == nil {
		t.Fatalf("latestCLIVersion accepted garbage registry version")
	}
}

func newProviderUpdateTestManager(t *testing.T, installed, latest, updateCommand string) (*Manager, *eventCollector, string) {
	t.Helper()
	root := repoRoot(t)
	pathDir := t.TempDir()
	versionFile := filepath.Join(t.TempDir(), "qwen-version")
	if err := os.WriteFile(versionFile, []byte(installed+"\n"), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}
	writeExecutable(t, filepath.Join(pathDir, "qwen"), "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then IFS= read -r v < "+shellQuote(versionFile)+"; printf '%s\\n' \"$v\"; exit 0; fi\nexit 1\n")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": latest})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	commands := map[string]ProviderUpdateCommand{"qwen": {Command: updateCommand}}
	if strings.TrimSpace(updateCommand) == "" {
		commands = map[string]ProviderUpdateCommand{"qwen": {Command: filepath.Join(pathDir, "missing-update-command")}}
	}
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Providers: []ProviderConfig{
			{ID: "qwen", Name: "Qwen Code ACP", Command: "qwen", Args: []string{"--acp"}, Enabled: true, Badge: "agent", CWD: root},
		},
		DefaultProviderID:        "qwen",
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateSources:    map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:    200 * time.Millisecond,
		ProviderUpdateRunTimeout: 2 * time.Second,
		ProviderUpdateCommands:   commands,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: parseSemverToken(installed), Raw: installed})
	return manager, events, versionFile
}

type providerUpdateRecheckScript struct {
	Installed          string
	Latest             string
	PostUpdateFailures int
}

func newProviderUpdateRecheckTestManager(t *testing.T, script providerUpdateRecheckScript) (*Manager, *eventCollector, func() int, string) {
	t.Helper()
	root := repoRoot(t)
	pathDir := t.TempDir()
	stateDir := t.TempDir()
	versionFile := filepath.Join(t.TempDir(), "qwen-version")
	countFile := filepath.Join(t.TempDir(), "qwen-version-count")
	if err := os.WriteFile(versionFile, []byte(script.Installed+"\n"), 0o644); err != nil {
		t.Fatalf("write version file: %v", err)
	}
	qwenScript := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then
  n=0
  if [ -f %[1]s ]; then IFS= read -r n < %[1]s; fi
  n=$((n + 1))
  printf '%%s\n' "$n" > %[1]s
  if [ "$n" -eq 1 ]; then
    printf '%[2]s\n'
    exit 0
  fi
  if [ "$n" -le %[3]d ]; then
    printf 'signal: killed\n' >&2
    exit 137
  fi
  IFS= read -r v < %[4]s
  printf '%%s\n' "$v"
  exit 0
fi
exit 1
`, shellQuote(countFile), script.Installed, script.PostUpdateFailures+1, shellQuote(versionFile))
	writeExecutable(t, filepath.Join(pathDir, "qwen"), qwenScript)
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": script.Latest})
	}))
	t.Cleanup(registry.Close)
	events := newEventCollector()
	var retryMu sync.Mutex
	retryLogs := 0
	manager := NewManager(Options{
		RootDir:  root,
		StateDir: stateDir,
		Providers: []ProviderConfig{
			{ID: "qwen", Name: "Qwen Code ACP", Command: "qwen", Args: []string{"--acp"}, Enabled: true, Badge: "agent", CWD: root},
		},
		DefaultProviderID:        "qwen",
		Broadcast:                events.Broadcast,
		RSSSampleInterval:        time.Hour,
		ProviderUpdateSources:    map[string]string{"qwen": registry.URL + "/latest"},
		ProviderUpdateTimeout:    200 * time.Millisecond,
		ProviderUpdateRunTimeout: 2 * time.Second,
		ProviderUpdateCommands:   map[string]ProviderUpdateCommand{"qwen": {Command: filepath.Join(pathDir, "missing-update-command")}},
		Logf: func(msg string, fields map[string]any) {
			if msg == "provider update version recheck retry" {
				retryMu.Lock()
				retryLogs++
				retryMu.Unlock()
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	manager.setProviderCLIVersion("qwen", &CLIVersion{Version: parseSemverToken(script.Installed), Raw: script.Installed})
	return manager, events, func() int {
		retryMu.Lock()
		defer retryMu.Unlock()
		return retryLogs
	}, versionFile
}

func stubProviderUpdateRecheckTiming(t *testing.T) func() {
	t.Helper()
	oldTimeout := postUpdateCLIVersionTimeout
	oldBackoffs := append([]time.Duration(nil), postUpdateCLIVersionBackoffs...)
	postUpdateCLIVersionTimeout = 200 * time.Millisecond
	postUpdateCLIVersionBackoffs = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return func() {
		postUpdateCLIVersionTimeout = oldTimeout
		postUpdateCLIVersionBackoffs = oldBackoffs
	}
}

func waitProviderUpdateProgress(t *testing.T, events *eventCollector, providerID string, pred func(ProviderUpdateProgress) bool, timeout time.Duration) ProviderUpdateProgress {
	t.Helper()
	ev := events.waitFor(t, timeout, func(ev collectedEvent) bool {
		if ev.channel != "providers:update-progress" {
			return false
		}
		progress, ok := ev.payload.(ProviderUpdateProgress)
		return ok && progress.ProviderID == providerID && pred(progress)
	})
	progress := ev.payload.(ProviderUpdateProgress)
	if progress.ProviderID == providerID && pred(progress) {
		return progress
	}
	t.Fatalf("provider progress disappeared from matched event: %#v", progress)
	return ProviderUpdateProgress{}
}

func assertNoCollectedChannel(t *testing.T, events *eventCollector, channel string) {
	t.Helper()
	for _, ev := range events.snapshot() {
		if ev.channel == channel {
			t.Fatalf("unexpected %s event payload=%#v", channel, ev.payload)
		}
	}
}

func waitProviderUpdate(t *testing.T, events *eventCollector, providerID string, pred func(ProviderUpdate) bool, timeout time.Duration) ProviderUpdate {
	t.Helper()
	ev := events.waitFor(t, timeout, func(ev collectedEvent) bool {
		if ev.channel != "providers:updates" {
			return false
		}
		payload, ok := ev.payload.(ProviderUpdatesPayload)
		if !ok {
			return false
		}
		for _, update := range payload.Updates {
			if update.ProviderID == providerID && pred(update) {
				return true
			}
		}
		return false
	})
	payload := ev.payload.(ProviderUpdatesPayload)
	for _, update := range payload.Updates {
		if update.ProviderID == providerID && pred(update) {
			return update
		}
	}
	t.Fatalf("provider update disappeared from matched event: %#v", payload)
	return ProviderUpdate{}
}

func TestDetectProviderStoresRedactedCLIVersionRaw(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapperWithEnv(t, pathDir, "qwen", "echo-prompt", map[string]string{"WORKASS_FAKE_CLI_VERSION": "token=abc123 0.58.1"})
	t.Setenv("PATH", pathDir)
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test-model"}]}`))
	}))
	defer models.Close()
	manager := NewManager(Options{
		RootDir:             root,
		InitTimeout:         800 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{models.URL + "/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
	version, _ := qwen["cliVersion"].(*CLIVersion)
	if version == nil || version.Raw != "token=[redacted] 0.58.1" || version.Version != "0.58.1" {
		t.Fatalf("redacted cliVersion = %#v", version)
	}
}
