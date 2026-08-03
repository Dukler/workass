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
				update.Latest != tc.latest || update.UpdateAvailable != tc.available || update.Hint != "npm i -g @qwen-code/qwen-code" {
				t.Fatalf("qwen update = %#v", update)
			}
			t.Logf("trace providers:updates qwen installed=%s latest=%s updateAvailable=%v hint=%q", update.Installed, update.Latest, update.UpdateAvailable, update.Hint)
		})
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
	manager.ReplayProviderEvents(func(channel string, payload any) error {
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
