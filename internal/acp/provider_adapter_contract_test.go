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
	if adapter.context == nil || adapter.planUsage == nil || adapter.commands == nil || adapter.catalog == nil || adapter.permission == nil || adapter.launch == nil {
		t.Fatalf("single-facet provider required unrelated implementations: %#v", adapter)
	}
	if got := providerAdapterForID("descriptor-only-dummy"); reflect.TypeOf(got.delivery) != reflect.TypeOf(genericACPProviderAdapter.delivery) {
		t.Fatalf("descriptor-only provider did not inherit generic ACP delivery: %T", got.delivery)
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
