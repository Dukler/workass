package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	providercontract "workass/internal/provider"
)

type testProviderAuthenticationStrategy struct {
	hint string
}

func (strategy testProviderAuthenticationStrategy) IsAuthenticationFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Authentication required")
}

func (strategy testProviderAuthenticationStrategy) LoginHint() string { return strategy.hint }

func TestProviderDetectionDefaultRetryCadenceMatchesPortContract(t *testing.T) {
	got := (Options{}).withDefaults().ProviderDetectionRetryBackoffs
	want := []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	if !slices.Equal(got, want) {
		t.Fatalf("provider detection retry cadence = %v, want %v", got, want)
	}
}

func TestProviderDetectionAllowsFullInitializeAndSessionBudgets(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	initGate := filepath.Join(pathDir, "initialize-gate")
	sessionGate := filepath.Join(pathDir, "session-gate")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "split-probe-delay", map[string]string{
		"WORKASS_FAKE_ACP_INIT_GATE":    initGate,
		"WORKASS_FAKE_ACP_SESSION_GATE": sessionGate,
	})
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))

	original := providerRegistrations["devin"]
	registration := original
	registration.ProbeTimeout = 5 * time.Second
	providerRegistrations["devin"] = registration
	t.Cleanup(func() { providerRegistrations["devin"] = original })

	manager := NewManager(Options{
		RootDir:           root,
		InitTimeout:       5 * time.Second,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	detected := make(chan struct{})
	go func() {
		manager.DetectProviders(ctx, DetectOptions{ProviderID: "devin"})
		close(detected)
	}()
	waitForFakeACPProbeGate(t, initGate)
	// Each stage is held for less than its five-second request budget, while the
	// pair is longer than one budget. The extra margin keeps package-wide builds
	// from turning scheduler pressure into a false timeout; a single outer
	// timeout would still fail, so the test continues to prove that initialize
	// and session/new each own a complete budget.
	time.Sleep(2600 * time.Millisecond)
	if err := os.WriteFile(initGate+".release", []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release initialize probe gate: %v", err)
	}
	waitForFakeACPProbeGate(t, sessionGate)
	time.Sleep(2600 * time.Millisecond)
	if err := os.WriteFile(sessionGate+".release", []byte("release\n"), 0o600); err != nil {
		t.Fatalf("release session probe gate: %v", err)
	}
	select {
	case <-detected:
	case <-time.After(5 * time.Second):
		t.Fatal("provider detection did not finish after releasing both probe stages")
	}
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
}

func waitForFakeACPProbeGate(t *testing.T, gate string) {
	t.Helper()
	entered := gate + ".entered"
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for fake ACP probe gate %s", entered)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestStartupDetectProvidersAutoEnableEnvCatalogPersistenceAndSession(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installNodeWrapper(t, pathDir)
	installFakeAgentWrapper(t, pathDir, "devin", "echo-prompt")
	installFakeAgentWrapper(t, pathDir, "qwen", "echo-prompt")
	installFakeAgentWrapper(t, pathDir, "omp", "echo-prompt")
	t.Setenv("PATH", pathDir)
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))

	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen-test-model"}]}`))
	}))
	defer models.Close()

	providersFile := filepath.Join(t.TempDir(), "providers.json")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		ProviderConfigFile:  providersFile,
		Broadcast:           events.Broadcast,
		InitTimeout:         800 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{models.URL + "/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	providersEvent := events.waitChannel(t, "providers:list", 2*time.Second).payload.([]map[string]any)
	catalogEvent := events.waitChannel(t, "chat:catalog", 2*time.Second).payload.(map[string]any)
	t.Logf("trace event providers:list %s", providerListSummary(providersEvent))
	t.Logf("trace event chat:catalog %s", catalogSummaryForACP(catalogEvent))

	list := manager.ProvidersList()
	assertProviderListItem(t, list, "mock", providerStatusReady, true)
	devin := assertProviderListItem(t, list, "devin", providerStatusReady, true)
	qwen := assertProviderListItem(t, list, "qwen", providerStatusReady, true)
	omp := assertProviderListItem(t, list, "omp", providerStatusReady, true)
	assertProviderListItem(t, list, "claude", providerStatusNotFound, false)
	assertProviderListItem(t, list, "codex", providerStatusNotFound, false)
	if devin["resolvedCommand"] == "" {
		t.Fatalf("devin missing resolvedCommand: %#v", devin)
	}
	if omp["resolvedCommand"] != filepath.Join(pathDir, "omp") {
		t.Fatalf("OMP resolvedCommand = %#v", omp)
	}
	autoEnv, _ := qwen["autoEnv"].(map[string]string)
	if autoEnv["OPENAI_BASE_URL"] != models.URL+"/v1" || autoEnv["OPENAI_MODEL"] != "qwen-test-model" || autoEnv["OPENAI_API_KEY"] != "[redacted]" {
		t.Fatalf("qwen autoEnv = %#v", autoEnv)
	}
	if _, exists := autoEnv["QWEN_CODE_SAFE_MODE"]; exists {
		t.Fatalf("non-oMLX Qwen unexpectedly entered safe mode: %#v", autoEnv)
	}

	groups, _ := manager.Catalog(context.Background())["groups"].([]CatalogGroup)
	assertCatalogGroup(t, groups, "mock", providerStatusReady, true)
	assertCatalogGroup(t, groups, "devin", providerStatusReady, true)
	assertCatalogGroup(t, groups, "qwen", providerStatusReady, true)
	assertCatalogGroup(t, groups, "omp", providerStatusReady, true)

	saved := readProviderFile(t, providersFile)
	qwenSaved := saved["qwen"]
	if qwenSaved["enabled"] != true || qwenSaved["detected"] != true || qwenSaved["resolvedCommand"] == "" || qwenSaved["detectedAt"] == "" {
		t.Fatalf("saved qwen detection metadata = %#v", qwenSaved)
	}
	savedAutoEnv := qwenSaved["autoEnv"].(map[string]any)
	if savedAutoEnv["OPENAI_BASE_URL"] != models.URL+"/v1" || savedAutoEnv["OPENAI_API_KEY"] != "local" || savedAutoEnv["OPENAI_MODEL"] != "qwen-test-model" {
		t.Fatalf("saved qwen autoEnv = %#v", savedAutoEnv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "detect-qwen-tab", ChatID: "detect-qwen-chat", ProviderID: "qwen"})
	if err != nil {
		t.Fatalf("new qwen session: %v", err)
	}
	if session.ProviderID != "qwen" || len(session.Models) == 0 {
		t.Fatalf("qwen session = %#v", session)
	}
	t.Logf("trace reply app-chat:new-session provider=%s session=%s models=%d", session.ProviderID, session.SessionID, len(session.Models))

	ompCtx, ompCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ompCancel()
	ompSession, err := manager.NewSession(ompCtx, SessionOptions{TabID: "detect-omp-tab", ChatID: "detect-omp-chat", ProviderID: "omp"})
	if err != nil {
		t.Fatalf("new OMP session: %v", err)
	}
	if ompSession.ProviderID != "omp" || len(ompSession.Models) == 0 {
		t.Fatalf("OMP session = %#v", ompSession)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: ompSession.SessionID, TabID: "detect-omp-tab", ChatID: "detect-omp-chat",
		ProviderID: "omp", Prompt: "OMP deterministic turn",
	})
	if err != nil {
		t.Fatalf("start OMP job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if got := jobFromEnd(end)["providerId"]; got != "omp" {
		t.Fatalf("OMP job provider = %v, want omp", got)
	}
}

func TestDetectProvidersQwenInactiveWhenModelServerDown(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "qwen", "echo-prompt")
	t.Setenv("PATH", pathDir)
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	endpoint := models.URL + "/v1/models"
	models.Close()

	manager := NewManager(Options{
		RootDir:             root,
		InitTimeout:         200 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{endpoint},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusInactive, false)
	if !strings.Contains(fmt.Sprint(qwen["message"]), "no local OpenAI-compatible model server responded") {
		t.Fatalf("qwen inactive message = %#v", qwen)
	}
}

func TestDetectProvidersMissingBinaryNotFound(t *testing.T) {
	manager := NewManager(Options{RootDir: repoRoot(t), RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	t.Setenv("PATH", t.TempDir())

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "claude"})
	claude := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNotFound, false)
	if !strings.Contains(fmt.Sprint(claude["message"]), "official claude executable not found") {
		t.Fatalf("claude not-found message = %#v", claude)
	}
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "codex"})
	codex := assertProviderListItem(t, manager.ProvidersList(), "codex", providerStatusNotFound, false)
	if !strings.Contains(fmt.Sprint(codex["message"]), "official codex executable not found") {
		t.Fatalf("codex not-found message = %#v", codex)
	}
}

func TestDetectProvidersPersistsQwenCLIPathWhenModelServerIsInactive(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapperWithEnv(t, pathDir, "qwen", "echo-prompt", map[string]string{"WORKASS_FAKE_CLI_VERSION": "0.19.11"})
	t.Setenv("PATH", pathDir)
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir:             root,
		ProviderConfigFile:  providersFile,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{"http://127.0.0.1:1/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusInactive, false)
	if qwen["resolvedCommand"] != filepath.Join(pathDir, "qwen") {
		t.Fatalf("inactive qwen resolved command = %#v", qwen)
	}
	saved := readProviderFile(t, providersFile)
	if saved["qwen"]["resolvedCommand"] != filepath.Join(pathDir, "qwen") {
		t.Fatalf("saved inactive qwen resolved command = %#v", saved["qwen"])
	}
}

func TestStartupDetectProvidersRetriesOnlyStatusErrors(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installNodeWrapper(t, pathDir)
	flakyState := filepath.Join(t.TempDir(), "flaky-devin-state")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "flaky-init", map[string]string{"WORKASS_FAKE_ACP_FLAKY_STATE": flakyState})
	t.Setenv("PATH", pathDir)
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))
	t.Setenv("WORKASS_PROD", "1")

	providers := BuiltInProviderConfigs(root)
	for i := range providers {
		if providers[i].ID == "qwen" {
			providers[i].DisabledByUser = true
		}
	}
	events := newEventCollector()
	var logMu sync.Mutex
	probeLogs := map[string]int{}
	retryLogs := 0
	manager := NewManager(Options{
		RootDir:                        root,
		Providers:                      providers,
		Broadcast:                      events.Broadcast,
		InitTimeout:                    800 * time.Millisecond,
		RSSSampleInterval:              time.Hour,
		LocalModelEndpoints:            []string{"http://127.0.0.1:1/v1/models", "http://127.0.0.1:1/v1/models"},
		ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond, 25 * time.Millisecond, 40 * time.Millisecond},
		Logf: func(message string, fields map[string]any) {
			logMu.Lock()
			defer logMu.Unlock()
			switch message {
			case "provider detection":
				probeLogs[fmt.Sprint(fields["provider"])]++
			case "provider detection retry":
				retryLogs++
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	initialProviders := waitCollectedChannelCount(t, events, "providers:list", 1, 2*time.Second)[0].payload.([]map[string]any)
	assertProviderListItem(t, initialProviders, "devin", providerStatusError, false)
	qwen := assertProviderListItem(t, initialProviders, "qwen", providerStatusInactive, false)
	if qwen["message"] != "disabled by user" {
		t.Fatalf("qwen disabled item = %#v", qwen)
	}
	assertProviderListItem(t, initialProviders, "claude", providerStatusNotFound, false)
	assertProviderListItem(t, initialProviders, "codex", providerStatusNotFound, false)
	_ = waitCollectedChannelCount(t, events, "chat:catalog", 1, 2*time.Second)

	retryProviders := waitCollectedChannelCount(t, events, "providers:list", 2, 2*time.Second)[1].payload.([]map[string]any)
	assertProviderListItem(t, retryProviders, "devin", providerStatusReady, true)
	retryCatalog := waitCollectedChannelCount(t, events, "chat:catalog", 2, 2*time.Second)[1].payload.(map[string]any)
	groups, _ := retryCatalog["groups"].([]CatalogGroup)
	assertCatalogGroup(t, groups, "devin", providerStatusReady, true)

	time.Sleep(120 * time.Millisecond)
	providerEvents := waitCollectedChannelCount(t, events, "providers:list", 2, time.Millisecond)
	catalogEvents := waitCollectedChannelCount(t, events, "chat:catalog", 2, time.Millisecond)
	if len(providerEvents) != 2 || len(catalogEvents) != 2 {
		t.Fatalf("retry emitted extra provider/catalog events: providers=%d catalog=%d events=%#v", len(providerEvents), len(catalogEvents), events.snapshot())
	}

	logMu.Lock()
	defer logMu.Unlock()
	if retryLogs != 1 {
		t.Fatalf("retry log count = %d, want 1 logs=%#v", retryLogs, probeLogs)
	}
	if probeLogs["devin"] != 2 {
		t.Fatalf("devin probe log count = %d, want 2 logs=%#v", probeLogs["devin"], probeLogs)
	}
	if probeLogs["claude"] != 1 || probeLogs["codex"] != 1 {
		t.Fatalf("not-found providers retried unexpectedly: logs=%#v", probeLogs)
	}
	if probeLogs["qwen"] != 0 {
		t.Fatalf("user-disabled provider probed unexpectedly: logs=%#v", probeLogs)
	}
	t.Logf("trace retry providers:list %s", providerListSummary(retryProviders))
	t.Logf("trace retry chat:catalog %s", catalogSummaryForACP(retryCatalog))
	t.Logf("trace retry logs attempts=%d providerLogs=%#v", retryLogs, probeLogs)
}

func TestDetectFrontierProvidersReadyWithNativeProtocolFixtures(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	claudePath, codexPath := installNativeFrontierFixtures(t, root, pathDir)
	manager := NewManager(Options{
		RootDir:           root,
		InitTimeout:       800 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "claude"})
	claude := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusReady, true)
	if claude["resolvedCommand"] != claudePath {
		t.Fatalf("claude resolved command = %#v want %s", claude, claudePath)
	}
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "codex"})
	codexItem := assertProviderListItem(t, manager.ProvidersList(), "codex", providerStatusReady, true)
	if codexItem["resolvedCommand"] != codexPath {
		t.Fatalf("codex resolved command = %#v want %s", codexItem, codexPath)
	}
	groups := manager.Catalog(context.Background())["groups"].([]CatalogGroup)
	claudeGroup := findCatalogGroup(groups, "claude")
	var fixtureOpus *Model
	if claudeGroup != nil {
		fixtureOpus = findCatalogModel(claudeGroup.Models, "claude-opus-fixture")
	}
	if fixtureOpus == nil || !stringSliceContainsAll(fixtureOpus.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("native Claude per-model efforts = %#v", claudeGroup)
	}
	if haiku := findCatalogModel(claudeGroup.Models, "claude-haiku-fixture"); haiku == nil || len(haiku.Efforts) != 0 {
		t.Fatalf("native Claude Haiku efforts = %#v in %#v", haiku, claudeGroup.Models)
	}
	codexGroup := findCatalogGroup(groups, "codex")
	var fixtureCodex, fixtureCodexMini *Model
	if codexGroup != nil {
		fixtureCodex = findCatalogModel(codexGroup.Models, "gpt-fixture")
		fixtureCodexMini = findCatalogModel(codexGroup.Models, "gpt-fixture-mini")
	}
	if fixtureCodex == nil || fixtureCodexMini == nil || !stringSlicesEqual(fixtureCodex.Efforts, []string{"low", "high"}) ||
		!stringSlicesEqual(fixtureCodexMini.Efforts, []string{"low"}) {
		t.Fatalf("native Codex per-model efforts = %#v", codexGroup)
	}
}

func TestProviderDetectionCollapsesEffortVariantCatalog(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{
			{ID: "effort-agent", Name: "Effort Agent", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root, Env: map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "effort-catalog"}, Enabled: true},
		},
		DefaultProviderID: "effort-agent",
		InitTimeout:       2 * time.Second,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	catalog := manager.Catalog(context.Background())
	groups, _ := catalog["groups"].([]CatalogGroup)
	group := findCatalogGroup(groups, "effort-agent")
	if group == nil || group.Status != providerStatusReady {
		t.Fatalf("effort-agent catalog group = %#v in %#v", group, groups)
	}
	if len(group.Models) != 2 {
		t.Fatalf("effort-agent models = %#v, want collapsed m plus plain", group.Models)
	}
	collapsed := group.Models[0]
	if collapsed.ModelID != "m" || collapsed.Name != "M" || !stringSlicesEqual(collapsed.Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("collapsed effort model = %#v", collapsed)
	}
	plain := group.Models[1]
	if plain.ModelID != "plain" || plain.Name != "Plain" || len(plain.Efforts) != 0 {
		t.Fatalf("plain model = %#v", plain)
	}
	encoded, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal plain model: %v", err)
	}
	if bytes.Contains(encoded, []byte("efforts")) {
		t.Fatalf("plain model JSON includes efforts despite omitempty: %s", encoded)
	}
	t.Logf("trace effort catalog provider=%s models=%#v", group.ProviderID, group.Models)
}

func TestClaudeCatalogProbeDiscoversEffortBehindDefaultAlias(t *testing.T) {
	root := repoRoot(t)
	logPath := filepath.Join(t.TempDir(), "claude-catalog-controls.log")
	manager := NewManager(Options{
		RootDir:           root,
		InitTimeout:       2 * time.Second,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	cfg := ProviderConfig{
		ID:      "claude",
		Name:    "Claude Code ACP",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestFakeACPHelper", "--"},
		CWD:     root,
		Enabled: true,
		Env: map[string]string{
			"WORKASS_FAKE_ACP":            "1",
			"WORKASS_FAKE_ACP_MODE":       "claude-effort-default",
			"WORKASS_FAKE_ACP_CONFIG_LOG": logPath,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, _, _, err := manager.probeProviderCatalogWithInitTimeout(ctx, cfg, 2*time.Second)
	if err != nil {
		t.Fatalf("probe Claude catalog: %v", err)
	}
	wantEfforts := []string{"low", "medium", "high", "xhigh", "max"}
	for _, modelID := range []string{"claude-fable-5[1m]", "opus[1m]", "sonnet"} {
		model := findCatalogModel(models, modelID)
		if model == nil || !stringSlicesEqual(model.Efforts, wantEfforts) {
			t.Fatalf("Claude model %s efforts = %#v in %#v", modelID, model, models)
		}
	}
	if haiku := findCatalogModel(models, "haiku"); haiku == nil || len(haiku.Efforts) != 0 {
		t.Fatalf("Haiku must remain effort-less: %#v in %#v", haiku, models)
	}
	if findCatalogModel(models, "default") != nil {
		t.Fatalf("synthetic default alias leaked into visible catalog: %#v", models)
	}
	calls := readConfigCalls(t, logPath)
	for _, want := range []string{"model=claude-fable-5[1m]", "model=opus[1m]", "model=sonnet", "model=haiku", "model=default"} {
		if !containsString(calls, want) {
			t.Fatalf("Claude catalog probe missing %q in %#v", want, calls)
		}
	}
}

func TestDetectFrontierProvidersNeedsLogin(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	claudePath, codexPath := installNativeFrontierFixtures(t, root, pathDir)
	t.Setenv("WORKASS_CLAUDE_FIXTURE_AUTH_ERROR", "1")
	t.Setenv("WORKASS_CODEX_FIXTURE_AUTH_ERROR", "1")
	manager := NewManager(Options{
		RootDir:           root,
		InitTimeout:       800 * time.Millisecond,
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "claude"})
	claude := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNeedsLogin, false)
	if claude["fixHint"] != "Ejecuta `claude auth login`" || claude["resolvedCommand"] != claudePath {
		t.Fatalf("claude needs-login = %#v", claude)
	}

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "codex"})
	codex := assertProviderListItem(t, manager.ProvidersList(), "codex", providerStatusNeedsLogin, false)
	if codex["fixHint"] != "Ejecuta `codex login`" || codex["resolvedCommand"] != codexPath {
		t.Fatalf("codex needs-login = %#v", codex)
	}
}

func TestDetectClaudeUsesOfficialSDKSessionNotSeparateCLIAuthPreflight(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installNativeFrontierFixtures(t, root, pathDir)
	writeExecutable(t, filepath.Join(pathDir, "claude"), `#!/bin/sh
if [ "$1" = "auth" ]; then
  echo '{"loggedIn":false,"authMethod":"none"}'
  exit 1
fi
echo '2.1.207 (Claude Code)'
`)
	manager := NewManager(Options{RootDir: root, InitTimeout: time.Second, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "claude"})
	claude := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusReady, true)
	if claude["enabled"] != true {
		t.Fatalf("successful Claude SDK session was hidden by CLI auth preflight: %#v", claude)
	}
	if group := findCatalogGroup(manager.Catalog(context.Background())["groups"].([]CatalogGroup), "claude"); group == nil || len(group.Models) == 0 {
		t.Fatalf("ready Claude SDK provider missing from catalog: %#v", group)
	}
}

func TestFrontierTurnAuthFailureMarksProviderNeedsLogin(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "auth-on-prompt"},
			Enabled: true,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: providersFile,
		Broadcast:          events.Broadcast,
		InitTimeout:        time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	session, err := manager.NewSession(context.Background(), SessionOptions{TabID: "claude-auth-tab", ChatID: "claude-auth-chat", ProviderID: "claude"})
	if err != nil {
		t.Fatalf("new claude auth session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "claude-auth-tab", ChatID: "claude-auth-chat",
		ProviderID: "claude", Prompt: "trigger prompt auth",
	})
	if err != nil {
		t.Fatalf("start claude auth job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	endJob := jobFromEnd(end)
	if endJob["status"] != "failed" || !strings.Contains(fmt.Sprint(endJob["result"]), "Authentication required") ||
		!strings.Contains(fmt.Sprint(endJob["result"]), "claude auth login") {
		t.Fatalf("claude auth failure job = %#v", endJob)
	}
	claude := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNeedsLogin, false)
	if claude["enabled"] != false || claude["fixHint"] != "Ejecuta `claude auth login`" {
		t.Fatalf("claude runtime did not transition to needs-login: %#v", claude)
	}
	if group := findCatalogGroup(manager.Catalog(context.Background())["groups"].([]CatalogGroup), "claude"); group != nil {
		t.Fatalf("logged-out claude remained in composer catalog: %#v", group)
	}
	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload needs-login provider cache: %v", err)
	}
	reloadedClaude, ok := providerFromSlice(reloaded, "claude")
	if !ok {
		t.Fatalf("reloaded provider cache missing claude: %#v", reloaded)
	}
	if reloadedClaude.DisabledByUser {
		t.Fatalf("needs-login state became an explicit user disable after reload: %#v", reloadedClaude)
	}
	if !reloadedClaude.NeedsLogin {
		t.Fatalf("needs-login state was not persisted: %#v", reloadedClaude)
	}
	t.Logf("trace turn auth transition provider=%s status=%s hint=%q", claude["id"], claude["status"], claude["fixHint"])
}

func TestAuthShapedErrorDoesNotMatchAuthSubstringInsideOrdinaryWords(t *testing.T) {
	for _, message := range []string{
		"authoritative provider state temporarily unavailable",
		"the authoring session closed unexpectedly",
		"request failed while refreshing the catalog",
	} {
		if isAuthShapedError(message) {
			t.Fatalf("ordinary provider error was classified as authentication failure: %q", message)
		}
	}
	for _, message := range []string{
		"Authentication required",
		"Unauthorized: login required; credential missing",
		"auth token expired",
	} {
		if !isAuthShapedError(message) {
			t.Fatalf("authentication failure was not classified: %q", message)
		}
	}
}

func TestAuthenticatedProvidersShareProviderContractStrategy(t *testing.T) {
	manager := NewManager(Options{RootDir: repoRoot(t), RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	wants := map[string]string{
		"claude": "claude auth login",
		"codex":  "codex login",
		"devin":  "devin auth login",
		"omp":    "omp` y usa `/login",
	}
	for providerID, hintFragment := range wants {
		definition, err := manager.ProviderDefinition(providerID)
		if err != nil {
			t.Fatalf("provider definition %s: %v", providerID, err)
		}
		strategy := definition.Authentication
		if strategy == nil || !strategy.IsAuthenticationFailure(fmt.Errorf("Authentication required")) ||
			strategy.IsAuthenticationFailure(fmt.Errorf("authoritative catalog unavailable")) ||
			!strings.Contains(strategy.LoginHint(), hintFragment) {
			t.Fatalf("provider %s authentication strategy = %#v hint=%q", providerID, strategy, func() string {
				if strategy == nil {
					return ""
				}
				return strategy.LoginHint()
			}())
		}
	}
}

func TestProviderAuthenticationRuntimeUsesDefinitionWithoutRegistrationFallback(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "auth-on-session"},
			Enabled: true,
		}},
		DefaultProviderID: "devin", InitTimeout: time.Second, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	definition, err := manager.ProviderDefinition("devin")
	if err != nil {
		t.Fatal(err)
	}
	const definitionHint = "Run the definition-owned external login"
	definition.Authentication = testProviderAuthenticationStrategy{hint: definitionHint}
	registry := providercontract.NewRegistry()
	if err := registry.Register(definition); err != nil {
		t.Fatal(err)
	}
	manager.providerRegistry = registry
	manager.providerRegistryErr = nil

	_, err = manager.NewSession(context.Background(), SessionOptions{
		TabID: "definition-auth-tab", ChatID: "definition-auth-chat", ProviderID: "devin",
	})
	if err == nil || !strings.Contains(err.Error(), definitionHint) || strings.Contains(err.Error(), "devin auth login") {
		t.Fatalf("runtime authentication did not use the definition strategy: %v", err)
	}
	state := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
	if state["fixHint"] != definitionHint {
		t.Fatalf("definition-owned login hint was not persisted in runtime state: %#v", state)
	}
}

func TestMissingProviderDefinitionFailsClosedBeforeBridgeSpawn(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "echo-prompt"},
			Enabled: true,
		}},
		DefaultProviderID: "devin", InitTimeout: time.Second, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.providerRegistry = providercontract.NewRegistry()
	manager.providerRegistryErr = nil

	_, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "missing-definition-tab", ChatID: "missing-definition-chat", ProviderID: "devin",
	})
	if err == nil || !strings.Contains(err.Error(), "provider definition") {
		t.Fatalf("missing provider definition did not fail closed: %v", err)
	}
	if bridges := manager.allBridges(); len(bridges) != 0 {
		t.Fatalf("missing provider definition spawned %d bridges", len(bridges))
	}
}

func TestAuthenticationDemotionRedactsReturnedAndCatalogErrors(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "auth-on-session-secret"},
			Enabled: true,
		}},
		DefaultProviderID: "devin", InitTimeout: time.Second, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	_, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "redacted-auth-tab", ChatID: "redacted-auth-chat", ProviderID: "devin",
	})
	if err == nil || strings.Contains(err.Error(), "raw-sensitive-value") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("authentication error was not redacted: %v", err)
	}
	state := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
	if message := fmt.Sprint(state["message"]); strings.Contains(message, "raw-sensitive-value") {
		t.Fatalf("provider state leaked authentication diagnostics: %#v", state)
	}
}

func TestNeedsLoginBlocksStaleSessionUntilExplicitReenable(t *testing.T) {
	root := repoRoot(t)
	events := newEventCollector()
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "claude", Name: "Claude Code ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env:     map[string]string{"WORKASS_FAKE_ACP": "1", "WORKASS_FAKE_ACP_MODE": "echo-prompt"},
			Enabled: true,
		}},
		DefaultProviderID:  "claude",
		ProviderConfigFile: providersFile,
		Broadcast:          events.Broadcast,
		InitTimeout:        time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	session, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "claude-recovery-tab", ChatID: "claude-recovery-chat", ProviderID: "claude",
	})
	if err != nil {
		t.Fatalf("new Claude recovery session: %v", err)
	}
	if hint, err := manager.markProviderNeedsLogin(context.Background(), "claude", fmt.Errorf("Authentication required")); err != nil || hint == "" {
		t.Fatal("auth-shaped turn failure did not mark Claude needs-login")
	}
	assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNeedsLogin, false)

	if _, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "claude-recovery-tab", ChatID: "claude-recovery-chat",
		ProviderID: "claude", Prompt: "must not bypass needs-login",
	}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "login") {
		t.Fatalf("stale Claude session bypassed needs-login: %v", err)
	}
	blocked := assertProviderListItem(t, manager.ProvidersList(), "claude", providerStatusNeedsLogin, false)
	if blocked["enabled"] != false {
		t.Fatalf("blocked Claude provider was re-enabled by stale session: %#v", blocked)
	}

	if _, err := manager.ToggleProvider(context.Background(), "claude", true); err != nil {
		t.Fatalf("explicitly re-enable Claude provider: %v", err)
	}
	manager.mu.Lock()
	if got := manager.providers["claude"].Config.NeedsLogin; got {
		manager.mu.Unlock()
		t.Fatal("explicit re-enable did not clear needs-login")
	}
	manager.mu.Unlock()
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", SessionID: session.SessionID, TabID: "claude-recovery-tab", ChatID: "claude-recovery-chat",
		ProviderID: "claude", Prompt: "successful explicit retry",
	})
	if err != nil {
		t.Fatalf("start explicitly re-enabled Claude job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 2*time.Second)
	if got := jobFromEnd(end)["status"]; got != "done" {
		t.Fatalf("explicitly re-enabled retry status = %v, want done", got)
	}

	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload re-enabled provider cache: %v", err)
	}
	reloadedClaude, ok := providerFromSlice(reloaded, "claude")
	if !ok || !reloadedClaude.Enabled || reloadedClaude.DisabledByUser || reloadedClaude.NeedsLogin {
		t.Fatalf("explicitly re-enabled Claude state was not durable: %#v", reloadedClaude)
	}
}

func TestRealNativeFrontierCatalogs(t *testing.T) {
	if os.Getenv("WORKASS_REAL_FRONTIER") != "1" {
		t.Skip("set WORKASS_REAL_FRONTIER=1 to run real Claude SDK and Codex app-server handshakes")
	}
	root := repoRoot(t)
	for _, id := range []string{"claude", "codex"} {
		t.Run(id, func(t *testing.T) {
			manager := NewManager(Options{
				RootDir:           root,
				InitTimeout:       120 * time.Second,
				RSSSampleInterval: time.Hour,
				Logf: func(message string, fields map[string]any) {
					t.Logf("real %s log %s %#v", id, message, fields)
				},
			})
			t.Cleanup(func() { manager.Reset() })
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			result := manager.DetectProviders(ctx, DetectOptions{ProviderID: id})
			t.Logf("trace real %s detect result=%#v", id, result)
			catalogEvent := manager.Catalog(ctx)
			groups, _ := catalogEvent["groups"].([]CatalogGroup)
			group := findCatalogGroup(groups, id)
			if group == nil || group.Status != providerStatusReady {
				t.Fatalf("real %s catalog group = %#v in %#v", id, group, groups)
			}
			if len(group.Models) == 0 {
				t.Fatalf("real %s native catalog is empty: %#v", id, group)
			}
			if id == "claude" {
				opus := findCatalogModel(group.Models, "opus[1m]")
				haiku := findCatalogModel(group.Models, "haiku")
				if opus == nil || !stringSliceContainsAll(opus.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) || haiku == nil || len(haiku.Efforts) != 0 {
					t.Fatalf("real Claude effort support mismatch: opus=%#v haiku=%#v", opus, haiku)
				}
			}
			if id == "codex" {
				terra := findCatalogModel(group.Models, "gpt-5.6-terra")
				if terra == nil || !stringSliceContainsAll(terra.Efforts, []string{"low", "medium", "high", "xhigh", "max", "ultra"}) {
					t.Fatalf("real Codex per-model effort support mismatch: terra=%#v", terra)
				}
			}
			t.Logf("trace real %s catalog %s", id, catalogSummaryForACP(catalogEvent))
		})
	}
}

func TestDetectProvidersExplicitDisableSurvivesRedetection(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "devin", "echo-prompt")
	t.Setenv("PATH", pathDir)
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir:            root,
		ProviderConfigFile: providersFile,
		InitTimeout:        500 * time.Millisecond,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
	if _, err := manager.ToggleProvider(context.Background(), "devin", false); err != nil {
		t.Fatalf("disable devin: %v", err)
	}
	disabledCatalog := manager.Catalog(context.Background())
	disabledGroups, _ := disabledCatalog["groups"].([]CatalogGroup)
	if group := findCatalogGroup(disabledGroups, "devin"); group != nil {
		t.Fatalf("disabled devin remained in model catalog: %#v", group)
	}
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	devin := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusInactive, false)
	if devin["message"] != "disabled by user" {
		t.Fatalf("disabled devin = %#v", devin)
	}
	if devin["disabledByUser"] != true {
		t.Fatalf("disabled devin did not expose explicit user state: %#v", devin)
	}

	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload providers: %v", err)
	}
	manager2 := NewManager(Options{
		RootDir:            root,
		Providers:          reloaded,
		ProviderConfigFile: providersFile,
		InitTimeout:        500 * time.Millisecond,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager2.Reset() })
	manager2.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	reloadedDevin := assertProviderListItem(t, manager2.ProvidersList(), "devin", providerStatusInactive, false)
	if reloadedDevin["disabledByUser"] != true {
		t.Fatalf("reloaded disabled devin did not expose explicit user state: %#v", reloadedDevin)
	}
}

func TestProvidersListHidesOnlyUnconfiguredCustomPlaceholder(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{RootDir: root, RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	for _, provider := range manager.ProvidersList() {
		if provider["id"] == "custom" {
			t.Fatalf("unconfigured custom placeholder leaked into providers:list: %#v", provider)
		}
	}

	configured := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "custom", Name: "My ACP", Command: os.Args[0], Enabled: false,
		}},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { configured.Reset() })
	assertProviderListItem(t, configured.ProvidersList(), "custom", providerStatusInactive, false)
}

func TestSaveProviderConfigsConcurrentWritersUseDistinctTemps(t *testing.T) {
	dir := t.TempDir()
	providersFile := filepath.Join(dir, "providers.json")
	errs := make(chan error, 32)
	var writers sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		i := i
		writers.Add(1)
		go func() {
			defer writers.Done()
			errs <- SaveProviderConfigs(providersFile, []ProviderConfig{{
				ID: "devin", Command: "devin", Args: []string{"acp"},
				Enabled: i%2 == 0, ResolvedCommand: fmt.Sprintf("/fixture/devin-%02d", i),
			}})
		}()
	}
	writers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent provider config write: %v", err)
		}
	}
	raw, err := os.ReadFile(providersFile)
	if err != nil {
		t.Fatalf("read provider config: %v", err)
	}
	providers, err := decodeProviderConfigs(raw)
	if err != nil || len(providers) != 1 || providers[0].ID != "devin" {
		t.Fatalf("final provider config = %#v, err=%v", providers, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read provider config dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("provider config temp was not reclaimed: %s", entry.Name())
		}
	}
}

func TestFailedDetectionDisablesPreviouslyReadyProviderWithoutUserDisable(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "devin", "crash-stderr")
	t.Setenv("PATH", pathDir)
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))
	providers := BuiltInProviderConfigs(root)
	for i := range providers {
		if providers[i].ID == "devin" {
			providers[i].Enabled = true
			providers[i].Detected = true
			providers[i].DetectedAt = "2026-08-01T00:00:00Z"
		}
	}
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root, Providers: providers, ProviderConfigFile: providersFile,
		InitTimeout: 300 * time.Millisecond, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	devin := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusError, false)
	if devin["detected"] != true {
		t.Fatalf("failed provider should retain install detection metadata: %#v", devin)
	}
	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload providers: %v", err)
	}
	cfg, ok := providerFromSlice(reloaded, "devin")
	if !ok || cfg.Enabled || cfg.DisabledByUser {
		t.Fatalf("persisted failed Devin config = %#v", cfg)
	}
}

func TestDevinAuthenticationFailureBecomesNeedsLoginWithoutRetryLoop(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "devin", "auth-stderr")
	t.Setenv("PATH", pathDir)
	t.Setenv("ASSISTANT_DEVIN", filepath.Join(pathDir, "devin"))
	manager := NewManager(Options{
		RootDir: root, InitTimeout: 300 * time.Millisecond, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	devin := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
	if devin["fixHint"] != "Ejecuta `devin auth login`" || devin["detected"] != true {
		t.Fatalf("logged-out Devin state = %#v", devin)
	}
	if retryable := retryableProviderDetectionIDs([]providerDetectionResult{{ProviderID: "devin", Status: providerStatusNeedsLogin}}); len(retryable) != 0 {
		t.Fatalf("needs-login Devin was scheduled for startup retry: %v", retryable)
	}
}

func TestDevinSessionAuthenticationFailureDemotesOnceWithoutReplacement(t *testing.T) {
	root := repoRoot(t)
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env: map[string]string{
				"WORKASS_FAKE_ACP":            "1",
				"WORKASS_FAKE_ACP_MODE":       "auth-on-session",
				"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
			},
			Enabled: true,
		}},
		DefaultProviderID:  "devin",
		ProviderConfigFile: providersFile,
		Broadcast:          events.Broadcast,
		InitTimeout:        time.Second,
		RSSSampleInterval:  time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	manager.providers["devin"].Models = []Model{{ModelID: "stale-model", Name: "Stale"}}
	manager.providers["devin"].Modes = []Mode{{ID: "stale-mode", Name: "Stale"}}
	manager.providers["devin"].AgentName = "Stale Devin"
	manager.mu.Unlock()

	_, err := manager.NewSession(context.Background(), SessionOptions{
		TabID: "devin-auth-tab", ChatID: "devin-auth-chat", ProviderID: "devin",
	})
	if err == nil || !strings.Contains(err.Error(), "Authentication required") || !strings.Contains(err.Error(), "devin auth login") {
		t.Fatalf("Devin session auth error = %v", err)
	}
	if got := countMethod(readMethodLog(t, methodLog), "session/new"); got != 1 {
		t.Fatalf("Devin session/new attempts = %d, want exactly 1", got)
	}
	devin := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
	if devin["enabled"] != false || devin["fixHint"] != "Ejecuta `devin auth login`" {
		t.Fatalf("Devin needs-login provider = %#v", devin)
	}
	manager.mu.Lock()
	runtime := manager.providers["devin"]
	if !runtime.Config.NeedsLogin || len(runtime.Models) != 0 || len(runtime.Modes) != 0 || runtime.AgentName != "" {
		manager.mu.Unlock()
		t.Fatalf("Devin demotion retained usable catalog: %#v", runtime)
	}
	manager.mu.Unlock()
	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload Devin needs-login config: %v", err)
	}
	reloadedDevin, ok := providerFromSlice(reloaded, "devin")
	if !ok || !reloadedDevin.NeedsLogin || reloadedDevin.Enabled || reloadedDevin.DisabledByUser {
		t.Fatalf("persisted Devin needs-login config = %#v", reloadedDevin)
	}
	channels := map[string]bool{}
	for _, event := range events.snapshot() {
		channels[event.channel] = true
	}
	if !channels["providers:list"] || !channels["chat:catalog"] {
		t.Fatalf("Devin demotion events = %#v", channels)
	}

	_, secondErr := manager.NewSession(context.Background(), SessionOptions{
		TabID: "devin-auth-tab-2", ChatID: "devin-auth-chat-2", ProviderID: "devin",
	})
	if secondErr == nil || !strings.Contains(strings.ToLower(secondErr.Error()), "login") {
		t.Fatalf("second Devin session did not fail closed: %v", secondErr)
	}
	if got := countMethod(readMethodLog(t, methodLog), "session/new"); got != 1 {
		t.Fatalf("disabled Devin was spawned again; session/new attempts = %d", got)
	}
}

func TestStartupDetectionDoesNotRetryDevinNeedsLogin(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "auth-stderr", map[string]string{
		"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
	})
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: filepath.Join(pathDir, "devin"), Args: []string{"acp"}, Enabled: true,
		}},
		DefaultProviderID:              "devin",
		ProviderConfigFile:             filepath.Join(t.TempDir(), "providers.json"),
		InitTimeout:                    300 * time.Millisecond,
		ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
		RSSSampleInterval:              time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		var item map[string]any
		for _, candidate := range manager.ProvidersList() {
			if candidate["id"] == "devin" {
				item = candidate
				break
			}
		}
		if item != nil && item["status"] == providerStatusNeedsLogin {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup detection never reached needs-login: %#v", item)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if got := countMethod(readMethodLog(t, methodLog), "initialize"); got != 1 {
		t.Fatalf("startup retried logged-out Devin %d times, want 1", got)
	}
}

func TestStartupDetectionSkipsPersistedNeedsLoginUntilExplicitProbeSucceeds(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "echo-prompt", map[string]string{
		"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
	})
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: filepath.Join(pathDir, "devin"), Args: []string{"acp"},
			Enabled: false, NeedsLogin: true, Detected: true, ResolvedCommand: filepath.Join(pathDir, "devin"),
			LaunchSanitizationRevision: 1,
		}},
		DefaultProviderID:              "devin",
		ProviderConfigFile:             providersFile,
		InitTimeout:                    300 * time.Millisecond,
		ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond},
		RSSSampleInterval:              time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	time.Sleep(100 * time.Millisecond)
	if methods := readMethodLog(t, methodLog); len(methods) != 0 {
		t.Fatalf("startup spawned persisted needs-login Devin: %v", methods)
	}
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "devin"})
	devin := assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusReady, true)
	if devin["enabled"] != true {
		t.Fatalf("successful explicit Devin probe did not enable provider: %#v", devin)
	}
	manager.mu.Lock()
	needsLogin := manager.providers["devin"].Config.NeedsLogin
	manager.mu.Unlock()
	if needsLogin {
		t.Fatal("successful explicit Devin probe did not clear needs-login")
	}
}

func TestStartupDetectionRecoversLegacyDevinNeedsLoginOnceUnderSanitizedLaunch(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "echo-prompt", map[string]string{
		"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
	})
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: filepath.Join(pathDir, "devin"), Args: []string{"acp"},
			Enabled: false, NeedsLogin: true, Detected: true, ResolvedCommand: filepath.Join(pathDir, "devin"),
		}},
		DefaultProviderID:              "devin",
		ProviderConfigFile:             providersFile,
		InitTimeout:                    300 * time.Millisecond,
		ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond},
		RSSSampleInterval:              time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	deadline := time.Now().Add(2 * time.Second)
	for {
		manager.mu.Lock()
		cfg := manager.providers["devin"].Config
		manager.mu.Unlock()
		if cfg.Enabled && !cfg.NeedsLogin && cfg.LaunchSanitizationRevision == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy Devin recovery did not become ready: %#v", cfg)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := countMethod(readMethodLog(t, methodLog), "initialize"); got != 1 {
		t.Fatalf("legacy Devin recovery initialize attempts = %d, want 1", got)
	}
	// Runtime readiness is committed before the daemon-owned provider cache is
	// flushed. Wait for that separate durable boundary instead of racing the
	// writer immediately after observing the in-memory transition.
	persistDeadline := time.Now().Add(5 * time.Second)
	for {
		reloaded, err := LoadProviderConfigs(providersFile, root)
		devin, ok := providerFromSlice(reloaded, "devin")
		if err == nil && ok && !devin.NeedsLogin && devin.Enabled && devin.LaunchSanitizationRevision == 1 {
			break
		}
		if time.Now().After(persistDeadline) {
			if err != nil {
				t.Fatalf("reload recovered Devin config: %v", err)
			}
			t.Fatalf("persisted recovered Devin config = %#v", devin)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLegacyDevinNeedsLoginRecoveryFailureDoesNotLoopAcrossRestart(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "auth-stderr", map[string]string{
		"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
	})
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	provider := ProviderConfig{
		ID: "devin", Name: "Devin ACP", Command: filepath.Join(pathDir, "devin"), Args: []string{"acp"},
		Enabled: false, NeedsLogin: true, Detected: true, ResolvedCommand: filepath.Join(pathDir, "devin"),
	}
	newManager := func(providers []ProviderConfig) *Manager {
		return NewManager(Options{
			RootDir: root, Providers: providers, DefaultProviderID: "devin", ProviderConfigFile: providersFile,
			InitTimeout: 300 * time.Millisecond, ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
			RSSSampleInterval: time.Hour,
		})
	}

	first := newManager([]ProviderConfig{provider})
	first.StartProviderDetection(context.Background())
	waitForMethodLog(t, methodLog, 2*time.Second, func(methods []string) bool {
		return countMethod(methods, "initialize") == 1
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		first.mu.Lock()
		cfg := first.providers["devin"].Config
		first.mu.Unlock()
		if cfg.NeedsLogin && cfg.LaunchSanitizationRevision == 1 {
			break
		}
		if time.Now().After(deadline) {
			first.Reset()
			t.Fatalf("failed legacy recovery was not latched: %#v", cfg)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(80 * time.Millisecond)
	if got := countMethod(readMethodLog(t, methodLog), "initialize"); got != 1 {
		first.Reset()
		t.Fatalf("failed legacy recovery retried in-process %d times, want 1", got)
	}
	first.Reset()

	reloaded, err := LoadProviderConfigs(providersFile, root)
	if err != nil {
		t.Fatalf("reload failed Devin recovery: %v", err)
	}
	devin, ok := providerFromSlice(reloaded, "devin")
	if !ok || !devin.NeedsLogin || devin.LaunchSanitizationRevision != 1 {
		t.Fatalf("persisted failed Devin recovery = %#v", devin)
	}
	second := newManager(reloaded)
	t.Cleanup(func() { second.Reset() })
	second.StartProviderDetection(context.Background())
	time.Sleep(100 * time.Millisecond)
	if got := countMethod(readMethodLog(t, methodLog), "initialize"); got != 1 {
		t.Fatalf("failed legacy recovery retried after restart %d times, want 1", got)
	}
	assertProviderListItem(t, second.ProvidersList(), "devin", providerStatusNeedsLogin, false)
}

func TestLegacyDevinNeedsLoginRecoveryFailsClosedWhenClaimCannotPersist(t *testing.T) {
	root := repoRoot(t)
	pathDir := t.TempDir()
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	installFakeAgentWrapperWithEnv(t, pathDir, "devin", "echo-prompt", map[string]string{
		"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
	})
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: filepath.Join(pathDir, "devin"), Args: []string{"acp"},
			Enabled: false, NeedsLogin: true, Detected: true, ResolvedCommand: filepath.Join(pathDir, "devin"),
		}},
		DefaultProviderID: "devin", ProviderConfigFile: filepath.Join(blockedParent, "providers.json"),
		InitTimeout: 300 * time.Millisecond, ProviderDetectionRetryBackoffs: []time.Duration{10 * time.Millisecond},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.StartProviderDetection(context.Background())
	time.Sleep(100 * time.Millisecond)
	if methods := readMethodLog(t, methodLog); len(methods) != 0 {
		t.Fatalf("legacy recovery launched before its claim persisted: %v", methods)
	}
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
}

func TestNeedsLoginRejectsStaleSessionAndChatProviderBindings(t *testing.T) {
	root := repoRoot(t)
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{
			{ID: "devin", Name: "Devin ACP", Command: "devin", Enabled: false, NeedsLogin: true},
			{ID: "custom", Name: "Fallback", Command: os.Args[0], Args: []string{"-test.run=TestFakeACPHelper", "--"}, Env: map[string]string{"WORKASS_FAKE_ACP": "1"}, Enabled: true},
		},
		DefaultProviderID: "custom", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.mu.Lock()
	manager.sessionProvider["stale-devin-session"] = "devin"
	_, sessionErr := manager.resolveSessionProviderLocked(SessionOptions{SessionID: "stale-devin-session"})
	manager.chatProviders["chat:stale-devin-chat"] = "devin"
	_, chatErr := manager.resolveSessionProviderLocked(SessionOptions{ChatID: "stale-devin-chat"})
	_, configErr := manager.providerConfigLocked("devin")
	manager.mu.Unlock()
	for label, err := range map[string]error{"session": sessionErr, "chat": chatErr, "config": configErr} {
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "login") {
			t.Fatalf("%s binding bypassed needs-login: %v", label, err)
		}
	}
}

func TestSpareWarmingDemotesDevinAuthenticationFailureAndStops(t *testing.T) {
	root := repoRoot(t)
	methodLog := filepath.Join(t.TempDir(), "methods.log")
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	manager := NewManager(Options{
		RootDir: root,
		Providers: []ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: os.Args[0],
			Args: []string{"-test.run=TestFakeACPHelper", "--"}, CWD: root,
			Env: map[string]string{
				"WORKASS_FAKE_ACP":            "1",
				"WORKASS_FAKE_ACP_MODE":       "auth-on-session",
				"WORKASS_FAKE_ACP_METHOD_LOG": methodLog,
			},
			Enabled: true,
		}},
		DefaultProviderID: "devin", ProviderConfigFile: providersFile,
		SpareSessions: 1, InitTimeout: time.Second, RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })

	deadline := time.Now().Add(2 * time.Second)
	for {
		manager.mu.Lock()
		needsLogin := manager.providers["devin"].Config.NeedsLogin
		manager.mu.Unlock()
		if needsLogin {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("spare authentication failure did not demote Devin")
		}
		time.Sleep(10 * time.Millisecond)
	}
	manager.WarmSpareSessions()
	time.Sleep(100 * time.Millisecond)
	if got := countMethod(readMethodLog(t, methodLog), "session/new"); got != 1 {
		t.Fatalf("spare warming retried logged-out Devin %d times, want 1", got)
	}
	assertProviderListItem(t, manager.ProvidersList(), "devin", providerStatusNeedsLogin, false)
}

func TestDetectProvidersLocalServerRegistersNativeProviderAndStreamsThroughAgent(t *testing.T) {
	root := repoRoot(t)
	fake := newFakeLocalOpenAI(t, "local-first", "local-second")
	defer fake.Close()
	agentBin := buildWorkassAgentBinary(t, root)
	t.Setenv(workassAgentBinEnv, agentBin)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "qwen", "echo-prompt")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	providersFile := filepath.Join(t.TempDir(), "providers.json")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		StateDir:            filepath.Join(t.TempDir(), "state"),
		ProviderConfigFile:  providersFile,
		Broadcast:           events.Broadcast,
		InitTimeout:         5 * time.Second,
		StdoutFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{fake.URL() + "/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })

	result := manager.DetectProviders(context.Background(), DetectOptions{ProviderID: localLMStudioProviderID})
	if result["ok"] != true {
		t.Fatalf("detect result = %#v", result)
	}
	local := assertProviderListItem(t, manager.ProvidersList(), localLMStudioProviderID, providerStatusReady, true)
	if local["name"] != "LM Studio (local)" || local["badge"] != "native" || local["resolvedCommand"] != agentBin {
		t.Fatalf("local provider metadata = %#v", local)
	}
	autoEnv, _ := local["autoEnv"].(map[string]string)
	if autoEnv["OPENAI_BASE_URL"] != fake.URL()+"/v1" || autoEnv["OPENAI_MODEL"] != "local-first" || autoEnv["OPENAI_API_KEY"] != "[redacted]" {
		t.Fatalf("local autoEnv = %#v", autoEnv)
	}

	groups, _ := manager.Catalog(context.Background())["groups"].([]CatalogGroup)
	group := findCatalogGroup(groups, localLMStudioProviderID)
	if group == nil || group.ProviderName != "LM Studio (local)" || group.Badge != "native" || strings.Join(modelIDs(group.Models), ",") != "local-first,local-second" {
		t.Fatalf("local catalog group = %#v in %#v", group, groups)
	}
	t.Logf("trace event chat:catalog %s", catalogSummaryForACP(manager.Catalog(context.Background())))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{TabID: "local-native-tab", ChatID: "chat-local-native", ProviderID: localLMStudioProviderID})
	if err != nil {
		t.Fatalf("new local session: %v", err)
	}
	if session.ProviderID != localLMStudioProviderID || strings.Join(modelIDs(session.Models), ",") != "local-first,local-second" {
		t.Fatalf("local session = %#v", session)
	}
	t.Logf("trace reply app-chat:new-session provider=%s session=%s models=%d", session.ProviderID, session.SessionID, len(session.Models))

	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind:      "app-chat",
		ChatID:    "chat-local-native",
		TabID:     "local-native-tab",
		SessionID: session.SessionID,
		Prompt:    "native local turn",
	})
	if err != nil {
		t.Fatalf("start local job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 10*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"].(string); result != "native local ok" {
		t.Fatalf("local job result = %q", result)
	}
	req := fake.LastChatRequest()
	archivePath := filepath.Join(manager.StateDir(), "chat-archive", "<chatId>.jsonl")
	if req.Model != "local-first" || !req.Stream || len(req.Messages) != 1 ||
		!strings.Contains(req.Messages[0].Content, archivePath) ||
		!strings.Contains(req.Messages[0].Content, "User request:\nnative local turn") {
		t.Fatalf("fake backend request = %#v", req)
	}
	t.Logf("trace event job:event type=end provider=%s status=%s stopReason=%s result=%q", jobFromEnd(end)["providerId"], jobFromEnd(end)["status"], jobFromEnd(end)["stopReason"], jobFromEnd(end)["result"])

	saved := readProviderFile(t, providersFile)
	localSaved := saved[localLMStudioProviderID]
	if localSaved["enabled"] != true || localSaved["badge"] != "native" || localSaved["command"] != agentBin || localSaved["resolvedCommand"] != agentBin {
		t.Fatalf("saved local provider = %#v", localSaved)
	}
	savedEnv := localSaved["autoEnv"].(map[string]any)
	if savedEnv["OPENAI_MODEL"] != "local-first" || savedEnv["OPENAI_BASE_URL"] != fake.URL()+"/v1" || savedEnv["OPENAI_API_KEY"] != "local" {
		t.Fatalf("saved local autoEnv = %#v", savedEnv)
	}

	// The real local-agent boundary above also owns the coexistence assertion;
	// rebuilding the same binary in a second test added no provider behavior.
	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
	assertProviderListItem(t, manager.ProvidersList(), localLMStudioProviderID, providerStatusReady, true)
	groups, _ = manager.Catalog(context.Background())["groups"].([]CatalogGroup)
	assertCatalogGroup(t, groups, "qwen", providerStatusReady, true)
	localGroup := findCatalogGroup(groups, localLMStudioProviderID)
	if localGroup == nil || strings.Join(modelIDs(localGroup.Models), ",") != "local-first,local-second" {
		t.Fatalf("local group changed after Qwen detection = %#v groups=%#v", localGroup, groups)
	}
	t.Logf("trace qwen+local coexist catalog %s", catalogSummaryForACP(manager.Catalog(context.Background())))
}

func TestDetectProvidersOMLXAuthenticatesQwenAndNativeProviderWithoutPersistingKey(t *testing.T) {
	root := repoRoot(t)
	const apiKey = "omlx-test-secret-value"
	t.Setenv("OMLX_API_KEY", "")
	settingsFile := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"auth":{"api_key":"`+apiKey+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeLocalOpenAIWithAPIKey(t, apiKey, "omlx-qwen-new", "omlx-qwen-second")
	defer fake.Close()
	agentBin := buildWorkassAgentBinary(t, root)
	t.Setenv(workassAgentBinEnv, agentBin)
	pathDir := t.TempDir()
	installFakeAgentWrapper(t, pathDir, "qwen", "echo-prompt")
	t.Setenv("PATH", pathDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	providersFile := filepath.Join(t.TempDir(), "providers.json")
	events := newEventCollector()
	manager := NewManager(Options{
		RootDir:             root,
		StateDir:            filepath.Join(t.TempDir(), "state"),
		ProviderConfigFile:  providersFile,
		Broadcast:           events.Broadcast,
		InitTimeout:         5 * time.Second,
		StdoutFlushInterval: 5 * time.Millisecond,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{
			"http://127.0.0.1:1/v1/models",
			"http://127.0.0.1:1/v1/models",
			fake.URL() + "/v1/models",
		},
		OMLXSettingsFile: settingsFile,
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: "qwen"})
	qwen := assertProviderListItem(t, manager.ProvidersList(), "qwen", providerStatusReady, true)
	qwenEnv, _ := qwen["autoEnv"].(map[string]string)
	if qwenEnv["OPENAI_BASE_URL"] != fake.URL()+"/v1" || qwenEnv["OPENAI_MODEL"] != "omlx-qwen-new" || qwenEnv["OPENAI_API_KEY"] != "[redacted]" || qwenEnv["QWEN_CODE_SAFE_MODE"] != "true" {
		t.Fatalf("oMLX-backed Qwen autoEnv = %#v", qwenEnv)
	}

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: localOMLXProviderID})
	omlx := assertProviderListItem(t, manager.ProvidersList(), localOMLXProviderID, providerStatusReady, true)
	if omlx["name"] != "oMLX (local)" || omlx["resolvedCommand"] != agentBin {
		t.Fatalf("oMLX provider metadata = %#v", omlx)
	}
	groups, _ := manager.Catalog(context.Background())["groups"].([]CatalogGroup)
	group := findCatalogGroup(groups, localOMLXProviderID)
	if group == nil || strings.Join(modelIDs(group.Models), ",") != "omlx-qwen-new,omlx-qwen-second" {
		t.Fatalf("oMLX catalog group = %#v", group)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := manager.NewSession(ctx, SessionOptions{
		TabID: "omlx-native-tab", ChatID: "chat-omlx-native", ProviderID: localOMLXProviderID,
	})
	if err != nil {
		t.Fatalf("new oMLX session: %v", err)
	}
	job, err := manager.StartJob(context.Background(), JobStartOptions{
		Kind: "app-chat", ChatID: "chat-omlx-native", TabID: "omlx-native-tab",
		SessionID: session.SessionID, Prompt: "authenticated oMLX turn",
	})
	if err != nil {
		t.Fatalf("start oMLX job: %v", err)
	}
	end := events.waitJobEnd(t, jobID(job), 10*time.Second)
	assertJobStatus(t, end, "done", 0, "end_turn")
	if result := jobFromEnd(end)["result"]; result != "native local ok" {
		t.Fatalf("oMLX job result = %q", result)
	}

	authorizationHeaders := fake.AuthorizationHeaders()
	if len(authorizationHeaders) == 0 {
		t.Fatal("oMLX test server received no authenticated requests")
	}
	for i, authorization := range authorizationHeaders {
		if authorization != "Bearer "+apiKey {
			t.Fatalf("oMLX request %d did not carry the provider-owned credential", i)
		}
	}
	persisted, err := os.ReadFile(providersFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, []byte(apiKey)) {
		t.Fatal("oMLX API key was persisted in providers.json")
	}
	saved := readProviderFile(t, providersFile)
	for _, providerID := range []string{"qwen", localOMLXProviderID} {
		autoEnv, _ := saved[providerID]["autoEnv"].(map[string]any)
		if autoEnv["OPENAI_API_KEY"] != "local" {
			t.Fatalf("persisted %s credential placeholder = %#v", providerID, autoEnv["OPENAI_API_KEY"])
		}
	}
	t.Logf("trace oMLX auth qwen=%s native=%s models=%d", qwen["status"], omlx["status"], len(group.Models))
}

func TestDetectProvidersLocalServerInactiveWhenDown(t *testing.T) {
	root := repoRoot(t)
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	endpoint := models.URL + "/v1/models"
	models.Close()
	manager := NewManager(Options{
		RootDir:             root,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{endpoint},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: localLMStudioProviderID})
	local := assertProviderListItem(t, manager.ProvidersList(), localLMStudioProviderID, providerStatusInactive, false)
	if !strings.Contains(fmt.Sprint(local["message"]), "LM Studio (local) did not respond") {
		t.Fatalf("local inactive message = %#v", local)
	}
}

func TestDetectProvidersLocalServerBinaryUnresolvableStatusError(t *testing.T) {
	root := repoRoot(t)
	fake := newFakeLocalOpenAI(t, "local-first")
	defer fake.Close()
	t.Setenv(workassAgentBinEnv, "")
	t.Setenv(workassAgentDevGoRunEnv, "")
	manager := NewManager(Options{
		RootDir:             root,
		RSSSampleInterval:   time.Hour,
		LocalModelEndpoints: []string{fake.URL() + "/v1/models"},
	})
	t.Cleanup(func() { manager.Reset() })

	manager.DetectProviders(context.Background(), DetectOptions{ProviderID: localLMStudioProviderID})
	local := assertProviderListItem(t, manager.ProvidersList(), localLMStudioProviderID, providerStatusError, false)
	message := fmt.Sprint(local["message"])
	if !strings.Contains(message, "workass-agent binary not found") || !strings.Contains(message, workassAgentBinEnv) {
		t.Fatalf("local binary error = %#v", local)
	}
}

func TestResolveWorkassAgentLaunchOrder(t *testing.T) {
	root := repoRoot(t)
	override := filepath.Join(t.TempDir(), "agent-override")
	writeExecutable(t, override, "#!/bin/sh\nexit 0\n")
	t.Setenv(workassAgentBinEnv, override)
	launch, err := resolveWorkassAgentLaunchWithExecutable(root, filepath.Join(t.TempDir(), "workass"))
	if err != nil {
		t.Fatalf("override resolve: %v", err)
	}
	if launch.Command != override || len(launch.Args) != 0 {
		t.Fatalf("override launch = %#v", launch)
	}

	t.Setenv(workassAgentBinEnv, "")
	dir := t.TempDir()
	daemon := filepath.Join(dir, "workass-"+runtime.GOOS+"-"+runtime.GOARCH+executableSuffix())
	expectedSibling := filepath.Join(dir, "workass-agent-"+runtime.GOOS+"-"+runtime.GOARCH+executableSuffix())
	writeExecutable(t, expectedSibling, "#!/bin/sh\nexit 0\n")
	launch, err = resolveWorkassAgentLaunchWithExecutable(root, daemon)
	if err != nil {
		t.Fatalf("sibling resolve: %v", err)
	}
	if launch.Command != expectedSibling {
		t.Fatalf("sibling launch = %#v want %s", launch, expectedSibling)
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go not available on PATH for dev fallback assertion: %v", err)
	}
	t.Setenv(workassAgentDevGoRunEnv, "1")
	launch, err = resolveWorkassAgentLaunchWithExecutable(root, filepath.Join(t.TempDir(), "not-workass"))
	if err != nil {
		t.Fatalf("dev go run resolve: %v", err)
	}
	if strings.TrimSuffix(filepath.Base(launch.Command), ".exe") != "go" || strings.Join(launch.Args, " ") != "run ./cmd/workass-agent" {
		t.Fatalf("dev launch = %#v", launch)
	}
}

func installNodeWrapper(t *testing.T, dir string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for mock ACP detection: %v", err)
	}
	writeExecutable(t, filepath.Join(dir, "node"), fmt.Sprintf("#!/bin/sh\nexec %s \"$@\"\n", shellQuote(node)))
}

func installNativeFrontierFixtures(t *testing.T, root, dir string) (string, string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not available for native frontier fixtures: %v", err)
	}
	claude := filepath.Join(dir, "claude")
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, claude, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'fixture Claude Code'; fi\nexit 0\n")
	writeExecutable(t, codex, fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'fixture Codex'; exit 0; fi\nexec %s \"$@\"\n", shellQuote(node)))
	t.Setenv("PATH", dir)
	t.Setenv("WORKASS_NODE", node)
	t.Setenv("WORKASS_CLAUDE_CODE", "")
	t.Setenv("WORKASS_CODEX", "")
	t.Setenv("WORKASS_CLAUDE_SDK_MODULE", filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"))
	t.Setenv("WORKASS_CODEX_APP_SERVER_ARGS", mustJSONStrings(t, []string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")}))
	return claude, codex
}

func mustJSONStrings(t *testing.T, values []string) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func installFakeAgentWrapper(t *testing.T, dir, name, mode string) {
	t.Helper()
	installFakeAgentWrapperWithEnv(t, dir, name, mode, nil)
}

func installFakeAgentWrapperWithEnv(t *testing.T, dir, name, mode string, env map[string]string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("WORKASS_FAKE_ACP=1\n")
	b.WriteString(fmt.Sprintf("WORKASS_FAKE_ACP_MODE=%s\n", shellQuote(mode)))
	for key, value := range env {
		b.WriteString(fmt.Sprintf("%s=%s\n", key, shellQuote(value)))
	}
	b.WriteString("if [ \"$1\" = \"--version\" ]; then\n")
	b.WriteString("  if [ -n \"$WORKASS_FAKE_CLI_VERSION\" ]; then echo \"$WORKASS_FAKE_CLI_VERSION\"; exit 0; fi\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")
	b.WriteString(fmt.Sprintf("export WORKASS_FAKE_ACP WORKASS_FAKE_ACP_MODE%s\n", shellExportSuffix(env)))
	b.WriteString(fmt.Sprintf("exec %s -test.run=TestFakeACPHelper -- \"$@\"\n", shellQuote(os.Args[0])))
	writeExecutable(t, filepath.Join(dir, name), b.String())
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func shellExportSuffix(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return " " + strings.Join(keys, " ")
}

func waitCollectedChannelCount(t *testing.T, events *eventCollector, channel string, count int, timeout time.Duration) []collectedEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var matches []collectedEvent
		for _, ev := range events.snapshot() {
			if ev.channel == channel {
				matches = append(matches, ev)
			}
		}
		if len(matches) >= count {
			return matches
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d %s events; events=%#v", count, channel, events.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertProviderListItem(t *testing.T, list []map[string]any, id, status string, enabled bool) map[string]any {
	t.Helper()
	for _, item := range list {
		if item["id"] == id {
			if item["status"] != status || item["enabled"] != enabled {
				t.Fatalf("provider %s = %#v, want status=%s enabled=%v in %#v", id, item, status, enabled, list)
			}
			return item
		}
	}
	t.Fatalf("provider %s missing from %#v", id, list)
	return nil
}

func readProviderFile(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read providers file: %v", err)
	}
	var rows []map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&rows); err != nil {
		t.Fatalf("decode providers file: %v\n%s", err, data)
	}
	out := make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		out[fmt.Sprint(row["id"])] = row
	}
	return out
}

func providerListSummary(list []map[string]any) string {
	parts := make([]string, 0, len(list))
	for _, item := range list {
		autoEnv := ""
		if env, ok := item["autoEnv"].(map[string]string); ok && len(env) > 0 {
			autoEnv = fmt.Sprintf(" autoEnv=%v", env)
		}
		parts = append(parts, fmt.Sprintf("%s:%s enabled=%v%s", item["id"], item["status"], item["enabled"], autoEnv))
	}
	return strings.Join(parts, ", ")
}

func catalogSummaryForACP(catalog map[string]any) string {
	groups, _ := catalog["groups"].([]CatalogGroup)
	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		parts = append(parts, fmt.Sprintf("%s:%s models=%d modes=%d", group.ProviderID, group.Status, len(group.Models), len(group.Modes)))
	}
	return strings.Join(parts, ", ")
}

func buildWorkassAgentBinary(t *testing.T, root string) string {
	t.Helper()
	suffix := executableSuffix()
	out := filepath.Join(t.TempDir(), "workass-agent"+suffix)
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/workass-agent")
	cmd.Dir = root
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build workass-agent: %v\n%s", err, data)
	}
	return out
}

func modelIDs(models []Model) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.ModelID)
	}
	return out
}

type fakeLocalOpenAI struct {
	server                *httptest.Server
	mu                    sync.Mutex
	models                []string
	reqs                  []fakeLocalChatRequest
	requiredAuthorization string
	authorizationHeaders  []string
}

type fakeLocalChatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Stream bool `json:"stream"`
}

func newFakeLocalOpenAI(t *testing.T, models ...string) *fakeLocalOpenAI {
	return newFakeLocalOpenAIWithAPIKey(t, "", models...)
}

func newFakeLocalOpenAIWithAPIKey(t *testing.T, apiKey string, models ...string) *fakeLocalOpenAI {
	t.Helper()
	if len(models) == 0 {
		models = []string{"local-first"}
	}
	requiredAuthorization := ""
	if strings.TrimSpace(apiKey) != "" {
		requiredAuthorization = "Bearer " + strings.TrimSpace(apiKey)
	}
	fake := &fakeLocalOpenAI{
		models: append([]string(nil), models...), requiredAuthorization: requiredAuthorization,
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

func (f *fakeLocalOpenAI) URL() string {
	return f.server.URL
}

func (f *fakeLocalOpenAI) Close() {
	f.server.Close()
}

func (f *fakeLocalOpenAI) LastChatRequest() fakeLocalChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		return fakeLocalChatRequest{}
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeLocalOpenAI) AuthorizationHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authorizationHeaders...)
}

func (f *fakeLocalOpenAI) handle(w http.ResponseWriter, r *http.Request) {
	if f.requiredAuthorization != "" {
		authorization := r.Header.Get("Authorization")
		f.mu.Lock()
		f.authorizationHeaders = append(f.authorizationHeaders, authorization)
		f.mu.Unlock()
		if authorization != f.requiredAuthorization {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		items := make([]map[string]string, 0, len(f.models))
		for _, id := range f.models {
			items = append(items, map[string]string{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
	case "/v1/chat/completions":
		var req fakeLocalChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.reqs = append(f.reqs, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"native local "}}]}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}
