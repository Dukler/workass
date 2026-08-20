package acp

import (
	"regexp"
	"slices"
	"sort"
	"strings"
)

var claudeModelVersionPattern = regexp.MustCompile(`(?i)\b(fable|opus|sonnet|haiku)\s+([0-9]+(?:\.[0-9]+)?)\b`)
var claudeModelIDVersionPattern = regexp.MustCompile(`(?i)(?:^|-)(fable|opus|sonnet|haiku)-([0-9]+(?:\.[0-9]+)?)(?:\[|$)`)

func normalizeClaudeCatalogModels(models []Model) []Model {
	models = normalizeCatalogModels(models)
	if len(models) < 2 {
		return models
	}
	// The native adapter's default entry aliases one explicit model. Showing
	// both creates two labels for one selection, so keep explicit ids only.
	models = slices.DeleteFunc(models, func(model Model) bool {
		return strings.EqualFold(strings.TrimSpace(model.ModelID), "default")
	})
	sort.SliceStable(models, func(i, j int) bool {
		return claudeModelPowerRank(models[i].ModelID) < claudeModelPowerRank(models[j].ModelID)
	})
	return models
}

// resolveClaudeSyntheticDefaultAlias accepts an alias only when exactly one
// explicit model has the same provider-authored display name. Order is never a
// fallback.
func resolveClaudeSyntheticDefaultAlias(models []Model) (string, bool) {
	models = normalizeCatalogModels(append([]Model(nil), models...))
	defaultName := ""
	defaults := 0
	for _, model := range models {
		if !strings.EqualFold(strings.TrimSpace(model.ModelID), "default") {
			continue
		}
		defaults++
		defaultName = normalizedCatalogModelName(model.Name)
	}
	if defaults != 1 || defaultName == "" {
		return "", false
	}

	match := ""
	matches := 0
	for _, model := range models {
		modelID := strings.TrimSpace(model.ModelID)
		if modelID == "" || strings.EqualFold(modelID, "default") {
			continue
		}
		if normalizedCatalogModelName(model.Name) != defaultName {
			continue
		}
		match = modelID
		matches++
	}
	if matches != 1 {
		return "", false
	}
	return match, true
}

func normalizedCatalogModelName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// reconcileClaudeLiveCatalog keeps the complete probe's exact model-id set
// when a live session reports only its current root alias/control surface.
func reconcileClaudeLiveCatalog(previous, current []Model, known map[string]bool) ([]Model, map[string]bool) {
	previous = normalizeClaudeCatalogModels(append([]Model(nil), previous...))
	current = normalizeClaudeCatalogModels(append([]Model(nil), current...))

	literalByRoot := make(map[string][]Model)
	for _, model := range previous {
		root, ok := claudeLiteralContextModelRoot(model.ModelID)
		if ok {
			literalByRoot[root] = append(literalByRoot[root], model)
		}
	}

	adjustedKnown := make(map[string]bool, len(known))
	adjusted := make([]Model, 0, len(current)+len(previous))
	emitted := make(map[string]bool, len(current)+len(previous))
	for _, model := range current {
		id := strings.TrimSpace(model.ModelID)
		if variants := literalByRoot[id]; len(variants) == 1 {
			replacement := variants[0]
			replacement.Name = firstNonEmpty(strings.TrimSpace(model.Name), replacement.Name)
			replacement.Efforts = append([]string(nil), model.Efforts...)
			adjusted = append(adjusted, replacement)
			emitted[replacement.ModelID] = true
			if known[id] {
				adjustedKnown[replacement.ModelID] = true
			}
			continue
		}
		adjusted = append(adjusted, model)
		emitted[id] = true
		if known[id] {
			adjustedKnown[id] = true
		}
	}
	for _, model := range previous {
		id := strings.TrimSpace(model.ModelID)
		if id == "" || strings.EqualFold(id, "default") || emitted[id] {
			continue
		}
		adjusted = append(adjusted, model)
		emitted[id] = true
		if known[id] {
			adjustedKnown[id] = true
		}
	}

	adjusted = preserveUnknownModelEfforts(previous, adjusted, adjustedKnown)
	return normalizeClaudeCatalogModels(adjusted), adjustedKnown
}

func claudeLiteralContextModelRoot(modelID string) (string, bool) {
	match := effortModelIDPattern.FindStringSubmatch(strings.TrimSpace(modelID))
	if match == nil {
		return "", false
	}
	root := strings.TrimSpace(match[1])
	qualifier := strings.TrimSpace(match[2])
	if root == "" || qualifier == "" {
		return "", false
	}
	if _, isEffort := canonicalEffortKey(qualifier); isEffort {
		return "", false
	}
	return root, true
}

func presentClaudeCatalogModel(model Model, description string) Model {
	description = strings.TrimSpace(description)
	match := claudeModelVersionPattern.FindStringSubmatch(description)
	if match == nil {
		match = claudeModelIDVersionPattern.FindStringSubmatch(model.ModelID)
	}
	if match == nil {
		return model
	}
	family := strings.ToUpper(match[1][:1]) + strings.ToLower(match[1][1:])
	model.Name = family + " " + match[2]
	return model
}

func claudeModelPowerRank(modelID string) int {
	id := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(id, "fable"):
		return 0
	case strings.Contains(id, "opus"):
		return 1
	case strings.Contains(id, "sonnet"):
		return 2
	case strings.Contains(id, "haiku"):
		return 3
	case id == "default":
		return 4
	default:
		return 5
	}
}
