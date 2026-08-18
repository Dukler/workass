package acp

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fixtureDeliveryOverride struct{}

func (fixtureDeliveryOverride) Steer(_ *Bridge, _ providerSteerRequest) providerSteerOutcome {
	return providerSteerOutcome{ok: true, strategy: "fixture"}
}

func (fixtureDeliveryOverride) AssistantPhase(map[string]any) string { return "" }

func TestProviderAdapterDefaultsAllowSingleFacetOverride(t *testing.T) {
	adapter := providerAdapterWithDefaults(providerAdapter{delivery: fixtureDeliveryOverride{}})
	if reflect.TypeOf(adapter.delivery) != reflect.TypeOf(fixtureDeliveryOverride{}) {
		t.Fatalf("delivery override was lost: %T", adapter.delivery)
	}
	if adapter.context == nil || adapter.input == nil || adapter.planUsage == nil || adapter.commands == nil || adapter.catalog == nil || adapter.permission == nil || adapter.launch == nil {
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

func TestDevinLaunchOwnsInheritedACPBackendSanitization(t *testing.T) {
	devin := providerAdapterForID("devin").launch.EnvironmentPolicy()
	for _, key := range []string{"ACP_BACKEND", "acp_backend", " Acp_Backend "} {
		if devin.inherits(key) {
			t.Fatalf("Devin launch inherited blocked environment key %q", key)
		}
	}
	if !devin.inherits("PATH") {
		t.Fatal("Devin launch blocked unrelated inherited environment")
	}

	for _, providerID := range []string{"mock", "qwen", "claude", "codex", "custom"} {
		if !providerAdapterForID(providerID).launch.EnvironmentPolicy().inherits("ACP_BACKEND") {
			t.Fatalf("provider %q inherited Devin-only environment policy", providerID)
		}
	}
}

func TestMergedEnvDropsBlockedInheritedKeysButKeepsExplicitLaunchValues(t *testing.T) {
	policy := providerEnvironmentPolicy{blockedInheritedKeys: []string{"ACP_BACKEND"}}
	merged := mergedEnv(
		[]string{"ACP_BACKEND=ambient", "Path=C:\\Windows", "KEEP=ambient"},
		map[string]string{"ACP_BACKEND": "explicit", "KEEP": "launch"},
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
	if got["ACP_BACKEND"] != "explicit" {
		t.Fatalf("explicit launch ACP_BACKEND = %q, want explicit", got["ACP_BACKEND"])
	}
	if got["KEEP"] != "launch" || got["Path"] != `C:\Windows` {
		t.Fatalf("merged environment lost unrelated or explicit values: %#v", got)
	}

	withoutExplicit := mergedEnv([]string{"acp_backend=ambient", "KEEP=ambient"}, nil, policy)
	for _, item := range withoutExplicit {
		if strings.HasPrefix(strings.ToUpper(item), "ACP_BACKEND=") {
			t.Fatalf("blocked inherited environment survived: %q", item)
		}
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

func TestDaemonSourcesContainNoProviderNameBranchesOutsideRegistrationsAndAdapters(t *testing.T) {
	root := repoRoot(t)
	approved := map[string]bool{
		"provider_registration.go":     true,
		"provider_delivery.go":         true,
		"provider_plan_usage.go":       true,
		"provider_catalog_strategy.go": true,
		"provider_permission.go":       true,
		"native_claude.go":             true,
		"native_codex.go":              true,
	}
	entries, err := os.ReadDir(filepath.Join(root, "internal", "acp"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || approved[name] {
			continue
		}
		path := filepath.Join(root, "internal", "acp", name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, literal := range []string{"\"codex\"", "\"claude\"", "\"devin\"", "\"qwen\""} {
			if strings.Contains(string(raw), literal) {
				t.Fatalf("%s contains provider-name branch literal %s; register a provider facet instead", name, literal)
			}
		}
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
