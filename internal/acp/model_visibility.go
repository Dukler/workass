package acp

import (
	"errors"
	"strings"
)

// isProductionRuntime is deliberately explicit: direct package callers and
// deterministic tests leave RuntimeProfile empty and retain the complete
// fixture registry. The daemon always supplies prod/dev/test.
func (m *Manager) isProductionRuntime() bool {
	return strings.EqualFold(strings.TrimSpace(m.opts.RuntimeProfile), "prod")
}

// isInternalFixtureModel recognizes only fixed Workass-owned test identities.
// Preview/beta vendor models and arbitrary user-owned local models remain real
// catalog entries and must never be hidden by prose heuristics.
func isInternalFixtureModel(providerID, modelID, name string) bool {
	provider := normalizeProviderID(providerID)
	if provider == "mock" {
		return true
	}
	id := strings.ToLower(strings.TrimSpace(modelID))
	label := strings.ToLower(strings.TrimSpace(name))
	if id == "mock-deterministic" || id == "coder-model(qwen-oauth)" {
		return true
	}
	if strings.Contains(id, "workass-dev") {
		return true
	}
	return label == "mock deterministic" || label == "coder-model" || label == "workass-dev"
}

func (m *Manager) hidesFixtureModel(providerID string, model Model) bool {
	return m.isProductionRuntime() && isInternalFixtureModel(providerID, model.ModelID, model.Name)
}

func (m *Manager) validateProductionModelSelection(providerID, modelID string) error {
	if !m.isProductionRuntime() {
		return nil
	}
	providerID = normalizeProviderID(providerID)
	if providerID == "mock" {
		return errors.New("development fixture provider is unavailable in production")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return nil
	}
	if isInternalFixtureModel(providerID, modelID, "") {
		return errors.New("development fixture model is unavailable in production")
	}

	// Exact fixed identities are sufficient before a provider is probed. Once a
	// catalog exists, also reject canonical effort-suffixed selections that map
	// back to a fixture model.
	m.mu.Lock()
	runtime := m.providers[providerID]
	models := []Model(nil)
	if runtime != nil {
		models = append(models, runtime.Models...)
	}
	m.mu.Unlock()
	base, ok := catalogModelSelectionBase(modelID, models)
	if !ok {
		return nil
	}
	for _, model := range models {
		if strings.TrimSpace(model.ModelID) == base && isInternalFixtureModel(providerID, model.ModelID, model.Name) {
			return errors.New("development fixture model is unavailable in production")
		}
	}
	return nil
}

func (m *Manager) userFacingCatalogGroups(groups []CatalogGroup) []CatalogGroup {
	if !m.isProductionRuntime() {
		return groups
	}
	visible := make([]CatalogGroup, 0, len(groups))
	for _, group := range groups {
		if normalizeProviderID(group.ProviderID) == "mock" {
			continue
		}
		models := make([]Model, 0, len(group.Models))
		for _, model := range group.Models {
			if !m.hidesFixtureModel(group.ProviderID, model) {
				models = append(models, model)
			}
		}
		group.Models = models
		visible = append(visible, group)
	}
	return visible
}
