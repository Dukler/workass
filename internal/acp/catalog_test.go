package acp

import (
	"reflect"
	"testing"
)

func TestNormalizeCatalogModelsCollapsesVocabularyVariants(t *testing.T) {
	models := normalizeCatalogModels([]Model{
		{ModelID: "m[medium]", Name: "M (medium)"},
		{ModelID: "m[low]", Name: "M (low)"},
		{ModelID: "m[high]", Name: "M (high)"},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one collapsed model", models)
	}
	if models[0].ModelID != "m" || models[0].Name != "M" || !reflect.DeepEqual(models[0].Efforts, []string{"low", "medium", "high"}) {
		t.Fatalf("collapsed model = %#v", models[0])
	}
}

func TestNormalizeCatalogModelsKeepsBracketVariantOutsideEffortVocabulary(t *testing.T) {
	models := normalizeCatalogModels([]Model{
		{ModelID: "claude-fable-5[1m]", Name: "Claude Fable 5 (1M context)"},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v, want single passthrough model", models)
	}
	if models[0].ModelID != "claude-fable-5[1m]" || models[0].Name != "Claude Fable 5 (1M context)" || len(models[0].Efforts) != 0 {
		t.Fatalf("passthrough model = %#v", models[0])
	}
}

func TestNormalizeCatalogModelsSplitsMixedEffortAndContextVariants(t *testing.T) {
	models := normalizeCatalogModels([]Model{
		{ModelID: "x[high]", Name: "X (high)"},
		{ModelID: "x[1m]", Name: "X 1M"},
		{ModelID: "x[low]", Name: "X (low)"},
	})
	if len(models) != 2 {
		t.Fatalf("models = %#v, want collapsed effort model plus 1m passthrough", models)
	}
	collapsed := findCatalogModel(models, "x")
	if collapsed == nil || collapsed.Name != "X" || !reflect.DeepEqual(collapsed.Efforts, []string{"low", "high"}) {
		t.Fatalf("collapsed effort subset = %#v in %#v", collapsed, models)
	}
	context := findCatalogModel(models, "x[1m]")
	if context == nil || context.Name != "X 1M" || len(context.Efforts) != 0 {
		t.Fatalf("context variant = %#v in %#v", context, models)
	}
}

func TestNormalizeCatalogModelsSortsEffortsInCanonicalOrder(t *testing.T) {
	models := normalizeCatalogModels([]Model{
		{ModelID: "x[ultra]", Name: "X (ultra)"},
		{ModelID: "x[HIGH]", Name: "X (HIGH)"},
		{ModelID: "x[none]", Name: "X (none)"},
		{ModelID: "x[minimal]", Name: "X (minimal)"},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v, want one collapsed model", models)
	}
	want := []string{"none", "minimal", "HIGH", "ultra"}
	if models[0].ModelID != "x" || !reflect.DeepEqual(models[0].Efforts, want) {
		t.Fatalf("efforts = %#v, want %#v in %#v", models[0].Efforts, want, models[0])
	}
}

func TestNormalizeCatalogModelsKeepsSingleEffortFamilyUncollapsed(t *testing.T) {
	models := normalizeCatalogModels([]Model{
		{ModelID: "x[low]", Name: "X (low)"},
	})
	if len(models) != 1 {
		t.Fatalf("models = %#v, want single uncollapsed model", models)
	}
	if models[0].ModelID != "x[low]" || models[0].Name != "X (low)" || len(models[0].Efforts) != 0 {
		t.Fatalf("single effort model = %#v", models[0])
	}
}

func TestCatalogModelSelectionBaseRepairsOnlyCanonicalSuffixesOnExistingModels(t *testing.T) {
	models := []Model{{ModelID: "opus[1m]"}, {ModelID: "haiku"}}
	cases := []struct {
		selection string
		wantBase  string
		wantOK    bool
	}{
		{selection: "opus[1m]", wantBase: "opus[1m]", wantOK: true},
		{selection: "haiku[low]", wantBase: "haiku", wantOK: true},
		{selection: "haiku[long-context]", wantOK: false},
		{selection: "missing[high]", wantOK: false},
	}
	for _, tc := range cases {
		gotBase, gotOK := catalogModelSelectionBase(tc.selection, models)
		if gotBase != tc.wantBase || gotOK != tc.wantOK {
			t.Fatalf("catalogModelSelectionBase(%q) = (%q,%v), want (%q,%v)", tc.selection, gotBase, gotOK, tc.wantBase, tc.wantOK)
		}
	}
}

func TestCompatibleModeIDTranslatesProviderPermissionIntent(t *testing.T) {
	codexModes := []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}}
	claudeModes := []Mode{{ID: "default"}, {ID: "plan"}, {ID: "bypassPermissions"}}
	if got := compatibleModeID("bypassPermissions", codexModes, "agent"); got != "agent-full-access" {
		t.Fatalf("Claude full access -> Codex = %q", got)
	}
	if got := compatibleModeID("agent-full-access", claudeModes, "default"); got != "bypassPermissions" {
		t.Fatalf("Codex full access -> Claude = %q", got)
	}
	if got := compatibleModeID("plan", codexModes, "agent"); got != "read-only" {
		t.Fatalf("Claude plan -> Codex = %q", got)
	}
	if got := compatibleModeID("stale-provider-mode", codexModes, "agent"); got != "agent" {
		t.Fatalf("unknown stale mode fallback = %q", got)
	}
}

func TestNormalizeClaudeCatalogAddsVersionsAndOrdersByPower(t *testing.T) {
	models := []Model{
		providerCatalogModel("claude", Model{ModelID: "default", Name: "Default (recommended)"}, "Use the default model (currently Opus 4.8 (1M context))"),
		providerCatalogModel("claude", Model{ModelID: "opus[1m]", Name: "Opus"}, "Opus 4.8 with 1M context"),
		providerCatalogModel("claude", Model{ModelID: "claude-fable-5[1m]", Name: "Fable"}, "Fable 5 · Most capable for your hardest tasks"),
		providerCatalogModel("claude", Model{ModelID: "sonnet", Name: "Sonnet"}, "Sonnet 5 · Efficient for routine tasks"),
		providerCatalogModel("claude", Model{ModelID: "haiku", Name: "Haiku"}, "Haiku 4.5 · Fastest for quick answers"),
	}

	models = normalizeProviderCatalogModels("claude", models)
	wantIDs := []string{"claude-fable-5[1m]", "opus[1m]", "sonnet", "haiku"}
	wantNames := []string{"Fable 5", "Opus 4.8", "Sonnet 5", "Haiku 4.5"}
	if len(models) != len(wantIDs) {
		t.Fatalf("claude models = %#v, want %d", models, len(wantIDs))
	}
	for i := range wantIDs {
		if models[i].ModelID != wantIDs[i] || models[i].Name != wantNames[i] {
			t.Fatalf("claude model[%d] = %#v, want id=%q name=%q; all=%#v", i, models[i], wantIDs[i], wantNames[i], models)
		}
	}
}

func TestResolveClaudeSyntheticDefaultAliasRequiresUniqueMetadataMatch(t *testing.T) {
	tests := []struct {
		name   string
		models []Model
		want   string
		wantOK bool
	}{
		{
			name: "one identical normalized explicit model",
			models: []Model{
				{ModelID: "default", Name: " Opus   4.8 "},
				{ModelID: "opus[1m]", Name: "opus 4.8"},
				{ModelID: "sonnet", Name: "Sonnet 5"},
			},
			want:   "opus[1m]",
			wantOK: true,
		},
		{
			name: "metadata mismatch",
			models: []Model{
				{ModelID: "default", Name: "Opus 4.8"},
				{ModelID: "opus[1m]", Name: "Opus 4.7"},
			},
		},
		{
			name: "ambiguous identical metadata",
			models: []Model{
				{ModelID: "default", Name: "Opus 4.8"},
				{ModelID: "opus[1m]", Name: "Opus 4.8"},
				{ModelID: "opus-preview", Name: "Opus 4.8"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gotOK := resolveClaudeSyntheticDefaultAlias(tc.models)
			if got != tc.want || gotOK != tc.wantOK {
				t.Fatalf("resolveClaudeSyntheticDefaultAlias(%#v) = (%q,%v), want (%q,%v)", tc.models, got, gotOK, tc.want, tc.wantOK)
			}
		})
	}
}

func TestPreserveUnknownModelEffortsKeepsOnlyUninspectedCapabilities(t *testing.T) {
	previous := []Model{
		{ModelID: "opus[1m]", Efforts: []string{"low", "high"}},
		{ModelID: "haiku", Efforts: []string{"low"}},
	}
	current := []Model{{ModelID: "opus[1m]"}, {ModelID: "haiku"}}
	got := preserveUnknownModelEfforts(previous, current, map[string]bool{"haiku": true})
	if !stringSlicesEqual(got[0].Efforts, []string{"low", "high"}) {
		t.Fatalf("uninspected Opus efforts = %#v", got[0].Efforts)
	}
	if len(got[1].Efforts) != 0 {
		t.Fatalf("authoritatively unsupported Haiku retained efforts: %#v", got[1].Efforts)
	}
}

func TestReconcileClaudeLiveCatalogPreservesProbedContextVariant(t *testing.T) {
	previous := []Model{
		{ModelID: "claude-fable-5[1m]", Name: "Fable 5", Efforts: []string{"low", "high"}},
		{ModelID: "opus[1m]", Name: "Opus 4.8", Efforts: []string{"low", "high"}},
		{ModelID: "sonnet", Name: "Sonnet 5", Efforts: []string{"low", "high"}},
		{ModelID: "haiku", Name: "Haiku 4.5"},
	}
	live := []Model{
		{ModelID: "claude-fable-5", Name: "Fable 5", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
		{ModelID: "sonnet", Name: "Sonnet 5", Efforts: []string{"low", "medium", "high"}},
		{ModelID: "haiku", Name: "Haiku 4.5"},
	}
	known := map[string]bool{
		"claude-fable-5": true,
		"sonnet":         true,
		"haiku":          true,
	}

	got, gotKnown := reconcileClaudeLiveCatalog(previous, live, known)
	wantIDs := []string{"claude-fable-5[1m]", "opus[1m]", "sonnet", "haiku"}
	if len(got) != len(wantIDs) {
		t.Fatalf("reconciled Claude models = %#v, want %d", got, len(wantIDs))
	}
	for i, wantID := range wantIDs {
		if got[i].ModelID != wantID {
			t.Fatalf("reconciled Claude model[%d] = %#v, want %q; all=%#v", i, got[i], wantID, got)
		}
	}
	if !stringSlicesEqual(got[0].Efforts, live[0].Efforts) {
		t.Fatalf("Fable live effort axis = %#v, want %#v", got[0].Efforts, live[0].Efforts)
	}
	if !gotKnown["claude-fable-5[1m]"] || gotKnown["claude-fable-5"] {
		t.Fatalf("reconciled known model ids = %#v", gotKnown)
	}
}
