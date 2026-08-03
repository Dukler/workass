package acp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func seedProfileVisibilityCatalog(t *testing.T, profile string) *Manager {
	t.Helper()
	manager := NewManager(Options{
		RootDir: repoRoot(t), RuntimeProfile: profile,
		RSSSampleInterval: time.Hour, LifecycleCheckInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	manager.mu.Lock()
	fixtures := map[string][]Model{
		"mock": {{ModelID: "mock-deterministic", Name: "Mock deterministic", Efforts: []string{"low", "high"}}},
		"qwen": {
			{ModelID: "coder-model(qwen-oauth)", Name: "coder-model"},
			{ModelID: "$runtime|openai|workass-dev(openai)", Name: "workass-dev"},
			{ModelID: "qwen-real", Name: "Qwen Real"},
		},
		"codex": {{ModelID: "gpt-real", Name: "GPT Real", Efforts: []string{"high"}}},
		localLMStudioProviderID: {
			{ModelID: "workass-dev", Name: "workass-dev"},
			{ModelID: "user-model", Name: "User Model"},
		},
	}
	for providerID, models := range fixtures {
		runtime := manager.providers[providerID]
		if runtime == nil {
			t.Fatalf("missing built-in provider %q", providerID)
		}
		runtime.Config.Enabled = true
		runtime.Probed = true
		runtime.Status = providerStatusReady
		runtime.Models = models
		switch providerID {
		case "mock":
			runtime.Modes = []Mode{{ID: "ask", Name: "Ask"}}
		case "codex":
			runtime.Modes = []Mode{{ID: "agent", Name: "Agent"}}
		}
	}
	manager.mu.Unlock()
	return manager
}

func catalogModelPairs(catalog map[string]any) []string {
	groups, _ := catalog["groups"].([]CatalogGroup)
	pairs := []string{}
	for _, group := range groups {
		for _, model := range group.Models {
			pairs = append(pairs, group.ProviderID+"/"+model.ModelID)
		}
	}
	return pairs
}

func TestProductionRuntimeHidesAndRejectsFixtureModels(t *testing.T) {
	manager := seedProfileVisibilityCatalog(t, "prod")
	pairs := strings.Join(catalogModelPairs(manager.Catalog(context.Background())), ",")
	for _, hidden := range []string{"mock-deterministic", "coder-model", "workass-dev"} {
		if strings.Contains(pairs, hidden) {
			t.Fatalf("production catalog leaked %q in %q", hidden, pairs)
		}
	}
	for _, visible := range []string{"qwen/qwen-real", "codex/gpt-real", localLMStudioProviderID + "/user-model"} {
		if !strings.Contains(pairs, visible) {
			t.Fatalf("production catalog lost real model %q in %q", visible, pairs)
		}
	}
	for _, provider := range manager.ProvidersList() {
		if asString(provider["id"]) == "mock" {
			t.Fatalf("production providers:list exposed mock: %#v", provider)
		}
	}
	parent := &Job{ProviderID: "codex", CWD: manager.opts.RootDir, startOpts: JobStartOptions{ModelID: "gpt-real[high]", ModeID: "agent"}}
	catalog := manager.agentCatalogV2(context.Background(), parent)
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatalf("marshal agent catalog: %v", err)
	}
	encoded := strings.ToLower(string(raw))
	for _, hidden := range []string{"mock-deterministic", "coder-model", "workass-dev"} {
		if strings.Contains(encoded, hidden) {
			t.Fatalf("production agent catalog leaked %q: %s", hidden, encoded)
		}
	}
	_, _, _, _, err = manager.resolveSubagentSelection(context.Background(), parent, SubagentSpawnOptions{
		ProviderID: "mock", ModelID: "mock-deterministic", ModeID: "ask",
	})
	if err == nil || !strings.Contains(err.Error(), "unavailable in production") {
		t.Fatalf("production fixture spawn error = %v", err)
	}
	for _, selection := range []struct {
		providerID string
		modelID    string
	}{
		{providerID: "mock", modelID: "mock-deterministic"},
		{providerID: "qwen", modelID: "coder-model(qwen-oauth)"},
		{providerID: "qwen", modelID: "$runtime|openai|workass-dev(openai)"},
		{providerID: localLMStudioProviderID, modelID: "workass-dev"},
	} {
		if err := manager.validateProductionModelSelection(selection.providerID, selection.modelID); err == nil {
			t.Fatalf("production accepted fixture selection %s/%s", selection.providerID, selection.modelID)
		}
	}
	if err := manager.validateProductionModelSelection("codex", "gpt-real[high]"); err != nil {
		t.Fatalf("production rejected real selection: %v", err)
	}

	manager.mu.Lock()
	_, err = manager.resolveSessionProviderLocked(SessionOptions{ProviderID: "mock"})
	manager.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "unavailable in production") {
		t.Fatalf("production mock session error = %v", err)
	}
}

func TestDevelopmentRuntimeRetainsFixtureModels(t *testing.T) {
	manager := seedProfileVisibilityCatalog(t, "dev")
	pairs := strings.Join(catalogModelPairs(manager.Catalog(context.Background())), ",")
	for _, visible := range []string{"mock/mock-deterministic", "qwen/coder-model(qwen-oauth)", localLMStudioProviderID + "/workass-dev"} {
		if !strings.Contains(pairs, visible) {
			t.Fatalf("development catalog lost fixture %q in %q", visible, pairs)
		}
	}
	foundMock := false
	for _, provider := range manager.ProvidersList() {
		foundMock = foundMock || asString(provider["id"]) == "mock"
	}
	if !foundMock {
		t.Fatal("development providers:list lost mock")
	}
	if err := manager.validateProductionModelSelection("mock", "mock-deterministic"); err != nil {
		t.Fatalf("development rejected fixture selection: %v", err)
	}
	manager.mu.Lock()
	providerID, err := manager.resolveSessionProviderLocked(SessionOptions{ProviderID: "mock"})
	manager.mu.Unlock()
	if err != nil || providerID != "mock" {
		t.Fatalf("development mock session = %q, %v", providerID, err)
	}
}
