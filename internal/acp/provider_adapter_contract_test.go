package acp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	providercontract "workass/internal/provider"
)

type fixtureDeliveryOverride struct{}

func (fixtureDeliveryOverride) Capabilities(_ *Bridge) providercontract.DeliveryCapabilities {
	return providercontract.DeliveryCapabilities{LiveSteer: true}
}

func (fixtureDeliveryOverride) Steer(_ *Bridge, _ providerSteerRequest) providerSteerOutcome {
	return providerSteerOutcome{ok: true, strategy: "fixture"}
}

func (fixtureDeliveryOverride) AssistantPhase(map[string]any) string { return "" }

func TestProviderAdapterDefaultsAllowSingleFacetOverride(t *testing.T) {
	adapter := providerAdapterWithDefaults(providerAdapter{delivery: fixtureDeliveryOverride{}})
	if reflect.TypeOf(adapter.delivery) != reflect.TypeOf(fixtureDeliveryOverride{}) {
		t.Fatalf("delivery override was lost: %T", adapter.delivery)
	}
	if adapter.context == nil || adapter.input == nil || adapter.planUsage == nil || adapter.commands == nil || adapter.notifications == nil || adapter.spawnedWork == nil || adapter.catalog == nil || adapter.permission == nil || adapter.launch == nil {
		t.Fatalf("single-facet provider required unrelated implementations: %#v", adapter)
	}
	if got := providerAdapterForID("descriptor-only-dummy"); reflect.TypeOf(got.delivery) != reflect.TypeOf(genericACPProviderAdapter.delivery) {
		t.Fatalf("descriptor-only provider did not inherit generic ACP delivery: %T", got.delivery)
	}
	if got := providerAdapterForID("descriptor-only-dummy"); got.creation.DeferredUntilInput || !got.input.StandardACPActivity() {
		t.Fatalf("descriptor-only provider did not inherit generic ACP creation receipts: %#v", got)
	}
	for _, providerID := range []string{"claude", "codex"} {
		got := providerAdapterForID(providerID)
		if !got.creation.DeferredUntilInput || got.input.StandardACPActivity() {
			t.Fatalf("native provider %q lost its explicit input receipt boundary: %#v", providerID, got)
		}
	}
}

func TestDevinLaunchOwnsACPBackendSanitization(t *testing.T) {
	devin := providerAdapterForID("devin").launch.EnvironmentPolicy()
	for _, key := range []string{"ACP_BACKEND", "acp_backend", " Acp_Backend "} {
		if devin.allows(key) {
			t.Fatalf("Devin launch allowed blocked environment key %q", key)
		}
	}
	if !devin.allows("PATH") {
		t.Fatal("Devin launch blocked unrelated environment")
	}
	if got := devin.startupRecoveryRevision(); got != 1 {
		t.Fatalf("Devin launch sanitation revision = %d, want 1", got)
	}

	for _, providerID := range []string{"mock", "qwen", "claude", "codex", "custom"} {
		policy := providerAdapterForID(providerID).launch.EnvironmentPolicy()
		if !policy.allows("ACP_BACKEND") {
			t.Fatalf("provider %q inherited Devin-only environment policy", providerID)
		}
		if got := policy.startupRecoveryRevision(); got != 0 {
			t.Fatalf("provider %q inherited Devin recovery revision %d", providerID, got)
		}
	}
}

func TestMergedEnvDropsBlockedKeysFromInheritedAndExplicitValues(t *testing.T) {
	policy := providerAdapterForID("devin").launch.EnvironmentPolicy()
	merged := mergedEnv(
		[]string{"ACP_BACKEND=ambient", "Path=C:\\Windows", "KEEP=ambient"},
		map[string]string{"Acp_Backend": "persisted", "KEEP": "launch"},
		policy,
	)
	got := make(map[string]string, len(merged))
	for _, item := range merged {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatalf("invalid merged environment entry %q", item)
		}
		got[key] = value
	}
	for key := range got {
		if strings.EqualFold(strings.TrimSpace(key), "ACP_BACKEND") {
			t.Fatalf("blocked ACP_BACKEND survived merged launch environment as %q", key)
		}
	}
	if got["KEEP"] != "launch" || got["Path"] != `C:\Windows` {
		t.Fatalf("merged environment lost unrelated or explicit values: %#v", got)
	}
}

func TestStandardACPLaunchAddsWorkassMCPTrustWithoutDisablingTLS(t *testing.T) {
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	stateDir := t.TempDir()
	caFile := filepath.Join(stateDir, "workass-ca.pem")
	if err := os.WriteFile(caFile, []byte("workass public certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launch, err := (standardACPLaunchStrategy{}).Prepare(ProviderConfig{
		Command: "fixture-acp", CWD: stateDir, Env: map[string]string{"FIXTURE": "yes"},
	}, Options{StateDir: stateDir, WorkassMCPCACertFile: caFile})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["NODE_EXTRA_CA_CERTS"] != caFile || launch.Env["FIXTURE"] != "yes" {
		t.Fatalf("standard ACP launch env = %#v", launch.Env)
	}
	if _, exists := launch.Env["NODE_TLS_REJECT_UNAUTHORIZED"]; exists {
		t.Fatalf("standard ACP launch disabled TLS verification: %#v", launch.Env)
	}
}

func TestOMLXAwareLaunchInjectsProviderOwnedKeyEphemerally(t *testing.T) {
	t.Setenv("OMLX_API_KEY", "")
	stateDir := t.TempDir()
	settingsFile := filepath.Join(stateDir, "settings.json")
	const apiKey = "omlx-test-secret-value"
	if err := os.WriteFile(settingsFile, []byte(`{"auth":{"api_key":"`+apiKey+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoints := []string{
		"http://127.0.0.1:1234/v1/models",
		"http://127.0.0.1:11434/v1/models",
		"http://127.0.0.1:18000/v1/models",
	}
	config := ProviderConfig{
		ID: "qwen", Command: "qwen", Args: []string{"--acp"}, CWD: stateDir,
		AutoEnv: map[string]string{
			"OPENAI_BASE_URL": "http://127.0.0.1:18000/v1",
			"OPENAI_API_KEY":  "local",
			"OPENAI_MODEL":    "qwen-local",
		},
	}
	options := Options{OMLXSettingsFile: settingsFile, LocalModelEndpoints: endpoints}
	launch, err := (omlxAwareACPLaunchStrategy{}).Prepare(config, options)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["OPENAI_API_KEY"] != apiKey {
		t.Fatalf("oMLX child key was not injected: %#v", redactedStringMap(launch.Env))
	}
	if config.AutoEnv["OPENAI_API_KEY"] != "local" || config.Env != nil {
		t.Fatalf("oMLX launch mutated persisted config: %#v", config)
	}

	explicit := config
	explicit.Env = map[string]string{"OPENAI_API_KEY": "user-owned-subkey"}
	launch, err = (omlxAwareACPLaunchStrategy{}).Prepare(explicit, options)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["OPENAI_API_KEY"] != "user-owned-subkey" {
		t.Fatalf("oMLX launch replaced an explicit user key: %#v", redactedStringMap(launch.Env))
	}

	providersFile := filepath.Join(stateDir, "providers.json")
	if err := SaveProviderConfigs(providersFile, []ProviderConfig{config}); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(providersFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), apiKey) || !strings.Contains(string(persisted), `"OPENAI_API_KEY": "local"`) {
		t.Fatalf("persisted oMLX config crossed the credential boundary: %s", redactSensitiveText(string(persisted)))
	}
}

func TestStandardACPLaunchCombinesExistingAndWorkassNodeCAs(t *testing.T) {
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	stateDir := t.TempDir()
	existing := filepath.Join(stateDir, "existing-ca.pem")
	workass := filepath.Join(stateDir, "workass-ca.pem")
	if err := os.WriteFile(existing, []byte("existing public certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workass, []byte("workass public certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := ProviderConfig{
		Command: "fixture-acp", CWD: stateDir, Env: map[string]string{"NODE_EXTRA_CA_CERTS": existing},
	}
	options := Options{StateDir: stateDir, WorkassMCPCACertFile: workass}
	first, err := (standardACPLaunchStrategy{}).Prepare(config, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (standardACPLaunchStrategy{}).Prepare(config, options)
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := first.Env["NODE_EXTRA_CA_CERTS"]
	if bundlePath == existing || bundlePath == workass || second.Env["NODE_EXTRA_CA_CERTS"] != bundlePath {
		t.Fatalf("combined CA path first=%q second=%q", bundlePath, second.Env["NODE_EXTRA_CA_CERTS"])
	}
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bundle), "existing public certificate") || !strings.Contains(string(bundle), "workass public certificate") {
		t.Fatalf("combined CA bundle = %q", bundle)
	}
}

func TestOfficialNativeHostsExposeExactResumeOnly(t *testing.T) {
	root := repoRoot(t)
	for _, relative := range []string{"scripts/codex-native-host.mjs", "scripts/claude-native-host.mjs"} {
		raw, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		text := string(raw)
		for _, forbidden := range []string{"session/load", "loadSession: true", "noteConversationMissing"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s still contains replacement-session seam %q", relative, forbidden)
			}
		}
		if !strings.Contains(text, "session/resume") {
			t.Fatalf("%s no longer exposes exact resume", relative)
		}
	}
}
