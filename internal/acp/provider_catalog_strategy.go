package acp

// providerCatalogStrategy owns provider-specific model metadata semantics.
// Catalog callers never infer them from a provider id or display name.
type providerCatalogStrategy interface {
	Normalize([]Model) []Model
	Reconcile(previous, current []Model, known map[string]bool) ([]Model, map[string]bool)
	Present(Model, string) Model
	ResolveSyntheticDefault([]Model) (string, bool)
}

type genericProviderCatalogStrategy struct{}

func (genericProviderCatalogStrategy) Normalize(models []Model) []Model {
	return normalizeCatalogModels(models)
}

func (genericProviderCatalogStrategy) Reconcile(previous, current []Model, known map[string]bool) ([]Model, map[string]bool) {
	return preserveUnknownModelEfforts(previous, current, known), known
}

func (genericProviderCatalogStrategy) Present(model Model, _ string) Model { return model }

func (genericProviderCatalogStrategy) ResolveSyntheticDefault([]Model) (string, bool) {
	return "", false
}

type claudeProviderCatalogStrategy struct{}

func (claudeProviderCatalogStrategy) Normalize(models []Model) []Model {
	return normalizeClaudeCatalogModels(models)
}

func (claudeProviderCatalogStrategy) Reconcile(previous, current []Model, known map[string]bool) ([]Model, map[string]bool) {
	return reconcileClaudeLiveCatalog(previous, current, known)
}

func (claudeProviderCatalogStrategy) Present(model Model, description string) Model {
	return presentClaudeCatalogModel(model, description)
}

func (claudeProviderCatalogStrategy) ResolveSyntheticDefault(models []Model) (string, bool) {
	return resolveClaudeSyntheticDefaultAlias(models)
}
