package acp

import (
	"regexp"
	"sort"
	"strings"
)

var effortModelIDPattern = regexp.MustCompile(`^(.+)\[([^\[\]]+)\]$`)

var canonicalEffortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

var canonicalEffortIndex = func() map[string]int {
	out := make(map[string]int, len(canonicalEffortOrder))
	for i, effort := range canonicalEffortOrder {
		out[effort] = i
	}
	return out
}()

type catalogModelRecord struct {
	model       Model
	base        string
	effort      string
	effortKey   string
	isBracketed bool
	isEffort    bool
}

type effortVariantGroup struct {
	base     string
	variants []effortVariant
	seen     map[string]bool
}

type effortVariant struct {
	key    string
	effort string
	model  Model
}

func normalizeCatalogModels(models []Model) []Model {
	if len(models) == 0 {
		return models
	}
	records := make([]catalogModelRecord, 0, len(models))
	effortGroups := make(map[string]*effortVariantGroup)
	for _, model := range models {
		model.ModelID = strings.TrimSpace(model.ModelID)
		model.Name = strings.TrimSpace(model.Name)
		if model.ModelID == "" {
			continue
		}
		record := catalogModelRecord{model: model}
		match := effortModelIDPattern.FindStringSubmatch(model.ModelID)
		if match != nil {
			base := strings.TrimSpace(match[1])
			effort := strings.TrimSpace(match[2])
			if base == "" || effort == "" {
				continue
			}
			record.base = base
			record.effort = effort
			record.isBracketed = true
			if key, ok := canonicalEffortKey(effort); ok {
				record.effortKey = key
				record.isEffort = true
				group := effortGroups[base]
				if group == nil {
					group = &effortVariantGroup{base: base, seen: make(map[string]bool)}
					effortGroups[base] = group
				}
				if !group.seen[key] {
					group.variants = append(group.variants, effortVariant{key: key, effort: effort, model: model})
					group.seen[key] = true
				}
			}
		}
		records = append(records, record)
	}

	collapsedByBase := make(map[string]Model)
	for base, group := range effortGroups {
		if len(group.variants) < 2 {
			continue
		}
		sort.SliceStable(group.variants, func(i, j int) bool {
			return canonicalEffortIndex[group.variants[i].key] < canonicalEffortIndex[group.variants[j].key]
		})
		collapsed := Model{
			ModelID: base,
			Name:    effortBaseName(group.variants[0].model, base, group.variants[0].effort),
			Efforts: make([]string, 0, len(group.variants)),
		}
		for _, variant := range group.variants {
			collapsed.Efforts = append(collapsed.Efforts, variant.effort)
		}
		collapsedByBase[base] = collapsed
	}

	out := make([]Model, 0, len(records))
	indexByID := make(map[string]int, len(records))
	emittedCollapsed := make(map[string]bool, len(collapsedByBase))
	for _, record := range records {
		if record.isEffort {
			if collapsed, ok := collapsedByBase[record.base]; ok {
				if !emittedCollapsed[record.base] {
					appendOrMergeCatalogModel(&out, indexByID, collapsed)
					emittedCollapsed[record.base] = true
				}
				continue
			}
		}
		if !record.isBracketed {
			if collapsed, ok := collapsedByBase[record.model.ModelID]; ok {
				if record.model.Name != "" && record.model.Name != record.model.ModelID {
					collapsed.Name = record.model.Name
				}
				if !emittedCollapsed[record.model.ModelID] {
					appendOrMergeCatalogModel(&out, indexByID, collapsed)
					emittedCollapsed[record.model.ModelID] = true
				} else if idx, exists := indexByID[record.model.ModelID]; exists && (out[idx].Name == "" || out[idx].Name == out[idx].ModelID) {
					out[idx].Name = firstNonEmpty(record.model.Name, record.model.ModelID)
				}
				continue
			}
		}
		appendOrMergeCatalogModel(&out, indexByID, record.model)
	}
	return out
}

// normalizeProviderCatalogModels keeps provider-specific presentation rules at
// the daemon boundary so every renderer receives the same deterministic
// catalog. Model IDs remain byte-for-byte unchanged; only display order is
// provider-specific here.
func normalizeProviderCatalogModels(providerID string, models []Model) []Model {
	return providerAdapterForID(providerID).catalog.Normalize(models)
}

// preserveUnknownModelEfforts carries previously discovered model-specific
// effort stops across a partial live-session catalog update. Frontier adapters
// expose only the current model's control surface, so an empty incoming effort
// list is authoritative only when that bridge has actually inspected the model.
// The known map preserves that distinction: present+empty means unsupported;
// absent means unknown to this bridge.
func preserveUnknownModelEfforts(previous, current []Model, known map[string]bool) []Model {
	if len(previous) == 0 || len(current) == 0 {
		return current
	}
	previousByID := make(map[string]Model, len(previous))
	for _, model := range previous {
		previousByID[strings.TrimSpace(model.ModelID)] = model
	}
	out := append([]Model(nil), current...)
	for i := range out {
		id := strings.TrimSpace(out[i].ModelID)
		if id == "" || known[id] || len(out[i].Efforts) != 0 {
			continue
		}
		if prior, ok := previousByID[id]; ok && len(prior.Efforts) != 0 {
			out[i].Efforts = append([]string(nil), prior.Efforts...)
		}
	}
	return out
}

// providerCatalogModel delegates provider-owned presentation to the registered
// catalog facet. The adapter value remains the selection ID.
func providerCatalogModel(providerID string, model Model, description string) Model {
	return providerAdapterForID(providerID).catalog.Present(model, description)
}

func modelsFromAvailableModels(raw any) []Model {
	return modelsFromAvailableModelsForProvider(raw, "")
}

func modelsFromAvailableModelsForProvider(raw any, providerID string) []Model {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	models := make([]Model, 0, len(values))
	for _, rawValue := range values {
		switch value := rawValue.(type) {
		case string:
			if id := strings.TrimSpace(value); id != "" {
				models = append(models, Model{ModelID: id, Name: id})
			}
		default:
			item := mapFromAny(rawValue)
			modelID := firstNonEmpty(asString(item["modelId"]), asString(item["id"]), asString(item["value"]))
			if modelID == "" {
				continue
			}
			models = append(models, providerCatalogModel(providerID, Model{
				ModelID: modelID,
				Name:    firstNonEmpty(asString(item["name"]), asString(item["displayName"]), modelID),
			}, asString(item["description"])))
		}
	}
	return models
}

func catalogModelsContainEffortVariants(models []Model) bool {
	for _, model := range models {
		if effortModelIDPattern.MatchString(strings.TrimSpace(model.ModelID)) {
			return true
		}
	}
	return false
}

func effortBaseName(model Model, base, effort string) string {
	name := strings.TrimSpace(model.Name)
	if name == "" || name == model.ModelID {
		return base
	}
	suffix := "(" + effort + ")"
	if strings.HasSuffix(name, suffix) {
		name = strings.TrimSpace(strings.TrimSuffix(name, suffix))
		if name != "" {
			return name
		}
	}
	bracketSuffix := "[" + effort + "]"
	if strings.HasSuffix(name, bracketSuffix) {
		name = strings.TrimSpace(strings.TrimSuffix(name, bracketSuffix))
		if name != "" {
			return name
		}
	}
	return name
}

func canonicalEffortKey(effort string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(effort))
	_, ok := canonicalEffortIndex[key]
	return key, ok
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// splitEffortSuffix separates a renderer-composed `base[effort]` id back into its
// model base and effort when the trailing bracket names one of the provider's
// separate effort levels. It intentionally leaves ids untouched when the suffix
// is not a known effort — that path covers context variants like `opus[1m]` and
// any adapter that exposes bracketed variants as whole model ids.
func splitEffortSuffix(modelID string, efforts []string) (string, string, bool) {
	if len(efforts) == 0 {
		return modelID, "", false
	}
	match := effortModelIDPattern.FindStringSubmatch(strings.TrimSpace(modelID))
	if match == nil {
		return modelID, "", false
	}
	base := strings.TrimSpace(match[1])
	effort := strings.TrimSpace(match[2])
	if base == "" || effort == "" {
		return modelID, "", false
	}
	for _, known := range efforts {
		if strings.EqualFold(known, effort) {
			return base, known, true
		}
	}
	return modelID, "", false
}

// SplitCanonicalEffortSuffix peels only Workass's canonical trailing effort
// suffix. Literal adapter ids such as claude-fable-5[1m] are left intact.
func SplitCanonicalEffortSuffix(modelID string) (string, string, bool) {
	match := effortModelIDPattern.FindStringSubmatch(strings.TrimSpace(modelID))
	if match == nil {
		return strings.TrimSpace(modelID), "", false
	}
	base := strings.TrimSpace(match[1])
	effort, ok := canonicalEffortKey(match[2])
	if base == "" || !ok {
		return strings.TrimSpace(modelID), "", false
	}
	return base, effort, true
}

// catalogModelSelectionBase validates a persisted/UI model selection against a
// catalog. Exact ids always win (Claude uses literal ids such as opus[1m]). If
// an exact id is gone, a canonical effort suffix may still be peeled from a
// valid base model; this normalizes stale selections such as
// haiku[low] after the adapter reports that Haiku has no effort axis.
func catalogModelSelectionBase(modelID string, models []Model) (string, bool) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", false
	}
	for _, model := range models {
		if strings.TrimSpace(model.ModelID) == modelID {
			return modelID, true
		}
	}
	match := effortModelIDPattern.FindStringSubmatch(modelID)
	if match == nil {
		return "", false
	}
	base := strings.TrimSpace(match[1])
	if base == "" {
		return "", false
	}
	if _, ok := canonicalEffortKey(match[2]); !ok {
		return "", false
	}
	for _, model := range models {
		if strings.TrimSpace(model.ModelID) == base {
			return base, true
		}
	}
	return "", false
}

var compatibleModeFamilies = [][]string{
	{"agent-full-access", "bypassPermissions", "bypass", "dontAsk"},
	{"read-only", "plan"},
	{"agent", "default", "ask", "auto", "acceptEdits", "guardian"},
}

// compatibleModeID keeps exact provider-native ids when possible, then maps a
// stale id only to the same permission intent on the newly selected provider.
// If no semantic peer exists, the fresh session's own default wins. A stale
// renderer snapshot must never make job:start fail before session/prompt.
func compatibleModeID(requested string, modes []Mode, providerDefault string) string {
	find := func(candidate string) string {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return ""
		}
		for _, mode := range modes {
			if mode.ID == candidate {
				return mode.ID
			}
		}
		return ""
	}
	if exact := find(requested); exact != "" {
		return exact
	}
	for _, family := range compatibleModeFamilies {
		inFamily := false
		for _, alias := range family {
			if strings.EqualFold(strings.TrimSpace(requested), alias) {
				inFamily = true
				break
			}
		}
		if !inFamily {
			continue
		}
		for _, alias := range family {
			for _, mode := range modes {
				if strings.EqualFold(mode.ID, alias) {
					return mode.ID
				}
			}
		}
	}
	return find(providerDefault)
}

func appendOrMergeCatalogModel(out *[]Model, indexByID map[string]int, model Model) {
	idx, exists := indexByID[model.ModelID]
	if !exists {
		indexByID[model.ModelID] = len(*out)
		*out = append(*out, model)
		return
	}
	if (*out)[idx].Name == "" || (*out)[idx].Name == (*out)[idx].ModelID {
		(*out)[idx].Name = firstNonEmpty(model.Name, model.ModelID)
	}
	(*out)[idx].Efforts = appendMissingStrings((*out)[idx].Efforts, model.Efforts...)
}

func appendMissingStrings(values []string, more ...string) []string {
	if len(more) == 0 {
		return values
	}
	seen := make(map[string]bool, len(values)+len(more))
	for _, value := range values {
		seen[value] = true
	}
	for _, value := range more {
		if value == "" || seen[value] {
			continue
		}
		values = append(values, value)
		seen[value] = true
	}
	return values
}
