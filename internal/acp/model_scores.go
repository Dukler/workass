package acp

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// ModelScore is an explicitly user-authored preference. Workass never fills
// these values from vendor claims or model-quality guesses. Nil means
// "unscored". Authored scores are clamped to the documented 1..10 scale.
type ModelScore struct {
	Intelligence *int   `json:"intelligence,omitempty"`
	Taste        *int   `json:"taste,omitempty"`
	Cost         *int   `json:"cost,omitempty"`
	Note         string `json:"note,omitempty"`
}

// ModelScores is keyed provider id -> base model id. The provider boundary is
// part of the key because different adapters may legitimately expose the same
// model id with different behavior or access.
type ModelScores map[string]map[string]ModelScore

var modelScoreDimensions = []string{"intelligence", "taste", "cost"}

// NormalizeModelScores accepts decoded JSON-shaped settings and returns a
// bounded canonical structure safe to persist or expose to agents.
func NormalizeModelScores(raw any) ModelScores {
	out := ModelScores{}
	if typed, ok := raw.(ModelScores); ok {
		for providerID, models := range typed {
			providerID = normalizeProviderID(providerID)
			if providerID == "" {
				continue
			}
			for modelID, score := range models {
				modelID = strings.TrimSpace(modelID)
				if modelID == "" {
					continue
				}
				clean := normalizeTypedModelScore(score)
				if clean.empty() {
					continue
				}
				if out[providerID] == nil {
					out[providerID] = map[string]ModelScore{}
				}
				out[providerID][modelID] = clean
			}
		}
		return out
	}
	providers := mapFromAny(raw)
	for providerID, rawModels := range providers {
		providerID = normalizeProviderID(providerID)
		if providerID == "" {
			continue
		}
		models := mapFromAny(rawModels)
		cleanModels := map[string]ModelScore{}
		for modelID, rawScore := range models {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				continue
			}
			scoreMap := mapFromAny(rawScore)
			score := ModelScore{
				Intelligence: scorePointer(scoreMap["intelligence"]),
				Taste:        scorePointer(scoreMap["taste"]),
				Cost:         scorePointer(scoreMap["cost"]),
				Note:         boundedModelNote(asString(scoreMap["note"])),
			}
			if !score.empty() {
				cleanModels[modelID] = score
			}
		}
		if len(cleanModels) > 0 {
			out[providerID] = cleanModels
		}
	}
	return out
}

func boundedModelNote(note string) string {
	note = redactSensitiveText(strings.TrimSpace(note))
	runes := []rune(note)
	if len(runes) <= 500 {
		return note
	}
	return string(runes[:499]) + "…"
}

func normalizeTypedModelScore(score ModelScore) ModelScore {
	clone := func(value *int) *int {
		if value == nil {
			return nil
		}
		bounded := clampInt(*value, 1, 10)
		return &bounded
	}
	return ModelScore{
		Intelligence: clone(score.Intelligence), Taste: clone(score.Taste), Cost: clone(score.Cost),
		Note: boundedModelNote(score.Note),
	}
}

func scorePointer(raw any) *int {
	var value int
	switch typed := raw.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return nil
		}
		value = int(parsed)
	case float64:
		value = int(typed)
	case float32:
		value = int(typed)
	case int:
		value = typed
	case int64:
		value = int(typed)
	case int32:
		value = int(typed)
	default:
		return nil
	}
	value = clampInt(value, 1, 10)
	return &value
}

func (s ModelScore) empty() bool {
	return s.Intelligence == nil && s.Taste == nil && s.Cost == nil && s.Note == ""
}

func (s ModelScore) value(dimension string) (int, bool) {
	var value *int
	switch dimension {
	case "intelligence":
		value = s.Intelligence
	case "taste":
		value = s.Taste
	case "cost":
		value = s.Cost
	}
	if value == nil {
		return 0, false
	}
	return *value, true
}

func (m *Manager) SetModelScores(raw any) {
	scores := NormalizeModelScores(raw)
	m.mu.Lock()
	m.modelScores = scores
	m.mu.Unlock()
}

func (m *Manager) modelScore(providerID, modelID string) (ModelScore, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	provider := m.modelScores[normalizeProviderID(providerID)]
	if provider == nil {
		return ModelScore{}, false
	}
	score, ok := provider[strings.TrimSpace(modelID)]
	return score, ok
}

type recommendationProfile struct {
	ID          string
	Name        string
	Description string
	Weights     map[string]int
	Effort      string
}

var recommendationProfiles = []recommendationProfile{
	{ID: "smart", Name: "Smart", Description: "Prefer your intelligence rating; use the note for task-specific guidance.", Weights: map[string]int{"intelligence": 75, "taste": 20, "cost": 5}, Effort: "xhigh"},
	{ID: "tasteful", Name: "Tasteful", Description: "Prefer your taste rating for design and judgment-heavy work.", Weights: map[string]int{"taste": 70, "intelligence": 25, "cost": 5}, Effort: "high"},
	{ID: "budget", Name: "Budget", Description: "Prefer lower-cost models while retaining intelligence and taste.", Weights: map[string]int{"cost": 60, "intelligence": 25, "taste": 15}, Effort: "medium"},
	{ID: "balanced", Name: "Balanced", Description: "Balance your intelligence, taste, and inverse-cost ratings.", Weights: map[string]int{"intelligence": 40, "taste": 35, "cost": 25}, Effort: "high"},
	{ID: "independent-review", Name: "Independent review", Description: "Prefer a smart, tasteful model on another provider when possible.", Weights: map[string]int{"intelligence": 55, "taste": 40, "cost": 5}, Effort: "xhigh"},
}

func recommendationProfileByID(id string) (recommendationProfile, bool) {
	id = strings.TrimSpace(strings.ToLower(id))
	for _, profile := range recommendationProfiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return recommendationProfile{}, false
}

func scoreRecommendation(score ModelScore, weights map[string]int) (float64, int) {
	weighted := 0
	weightTotal := 0
	for dimension, weight := range weights {
		if value, ok := score.value(dimension); ok {
			if dimension == "cost" {
				// Cost is intentionally intuitive in Settings: 10 means expensive.
				// Recommendation desirability therefore uses its inverse.
				value = 11 - value
			}
			weighted += value * weight
			weightTotal += weight
		}
	}
	if weightTotal == 0 {
		return 0, 0
	}
	return float64(weighted) / float64(weightTotal), weightTotal
}

func publicRecommendationProfiles() []map[string]any {
	out := make([]map[string]any, 0, len(recommendationProfiles))
	for _, profile := range recommendationProfiles {
		weights := map[string]any{}
		keys := append([]string(nil), modelScoreDimensions...)
		sort.Strings(keys)
		for _, dimension := range keys {
			if weight := profile.Weights[dimension]; weight > 0 {
				weights[dimension] = weight
			}
		}
		out = append(out, map[string]any{
			"id": profile.ID, "name": profile.Name, "description": profile.Description,
			"weights": weights, "preferredEffort": profile.Effort,
		})
	}
	return out
}

func permissionIntentModes(providerID string, modes []Mode) map[string]string {
	available := map[string]bool{}
	for _, mode := range modes {
		available[mode.ID] = true
	}
	candidates := map[string][]string{
		"read": {"read-only", "plan", "default"},
		"edit": {"agent", "acceptEdits", "auto-edit", "auto", "default"},
		"full": {"agent-full-access", "bypassPermissions", "bypass", "dontAsk", "yolo", "auto"},
	}
	if normalizeProviderID(providerID) == "qwen" {
		candidates["read"] = []string{"plan", "default"}
	}
	out := map[string]string{}
	for intent, ids := range candidates {
		for _, id := range ids {
			if available[id] {
				out[intent] = id
				break
			}
		}
	}
	return out
}

// PermissionIntentModes exposes the provider-neutral read/edit/full mapping to
// the daemon's first-party chat controller. Provider-native ids remain the wire
// authority; this is only the validated translation layer shared with spawn.
func PermissionIntentModes(providerID string, modes []Mode) map[string]string {
	return permissionIntentModes(providerID, modes)
}

// permissionIntentForMode is the inverse bridge used by inherited subagent
// permissions. Provider-native ids are not portable: inheriting Codex
// `agent-full-access` into Claude must select `bypassPermissions`, not fall
// through to Claude's restrictive default and deadlock on an unattended
// permission request.
func permissionIntentForMode(providerID, modeID string) string {
	modeID = strings.TrimSpace(modeID)
	switch modeID {
	case "agent-full-access", "bypassPermissions", "bypass", "dontAsk", "yolo":
		return "full"
	case "agent", "acceptEdits", "auto-edit", "auto":
		return "edit"
	case "read-only", "plan", "default", "ask":
		return "read"
	}
	// Keep the provider argument in the contract: adapters can add an
	// unambiguous native alias here without changing spawn callers.
	_ = providerID
	return ""
}

func PermissionIntentForMode(providerID, modeID string) string {
	return permissionIntentForMode(providerID, modeID)
}

func inheritedPermissionMode(parentProviderID, parentModeID, targetProviderID string, targetModes []Mode) string {
	parentModeID = strings.TrimSpace(parentModeID)
	if normalizeProviderID(parentProviderID) == normalizeProviderID(targetProviderID) {
		for _, mode := range targetModes {
			if mode.ID == parentModeID {
				return parentModeID
			}
		}
	}
	intent := permissionIntentForMode(parentProviderID, parentModeID)
	if intent == "" {
		return ""
	}
	return permissionIntentModes(targetProviderID, targetModes)[intent]
}

type recommendedSelection struct {
	ProviderID string
	ModelID    string
	Effort     string
	Score      float64
	Coverage   int
}

func (m *Manager) recommendSubagentSelection(ctx context.Context, profileID, avoidProvider string) (recommendedSelection, bool) {
	profile, ok := recommendationProfileByID(profileID)
	if !ok {
		return recommendedSelection{}, false
	}
	catalog := m.Catalog(ctx)
	groups, _ := catalog["groups"].([]CatalogGroup)
	best := recommendedSelection{Score: -1}
	bestAvoided := recommendedSelection{Score: -1}
	for _, group := range groups {
		if group.Status != providerStatusReady {
			continue
		}
		for _, model := range group.Models {
			score, scored := m.modelScore(group.ProviderID, model.ModelID)
			if !scored {
				continue
			}
			value, coverage := scoreRecommendation(score, profile.Weights)
			if coverage == 0 {
				continue
			}
			candidate := recommendedSelection{
				ProviderID: group.ProviderID, ModelID: model.ModelID,
				Effort: preferredAvailableEffort(model.Efforts, profile.Effort),
				Score:  value, Coverage: coverage,
			}
			if candidate.Score > bestAvoided.Score {
				bestAvoided = candidate
			}
			if normalizeProviderID(group.ProviderID) != normalizeProviderID(avoidProvider) && candidate.Score > best.Score {
				best = candidate
			}
		}
	}
	if best.Score >= 0 {
		return best, true
	}
	if bestAvoided.Score >= 0 {
		return bestAvoided, true
	}
	return recommendedSelection{}, false
}

func preferredAvailableEffort(efforts []string, preferred string) string {
	if len(efforts) == 0 {
		return ""
	}
	preferredKey, _ := canonicalEffortKey(preferred)
	best := efforts[0]
	bestDistance := 1 << 30
	for _, effort := range efforts {
		key, ok := canonicalEffortKey(effort)
		if !ok {
			continue
		}
		distance := canonicalEffortIndex[key] - canonicalEffortIndex[preferredKey]
		if distance < 0 {
			distance = -distance
		}
		if distance < bestDistance {
			best, bestDistance = effort, distance
		}
	}
	return best
}

func (m *Manager) agentCatalogV2(ctx context.Context, parent *Job) map[string]any {
	catalog := m.Catalog(ctx)
	rawGroups, _ := catalog["groups"].([]CatalogGroup)
	groups := make([]map[string]any, 0, len(rawGroups))
	for _, group := range rawGroups {
		models := make([]map[string]any, 0, len(group.Models))
		for _, model := range group.Models {
			entry := map[string]any{"modelId": model.ModelID, "name": model.Name}
			if len(model.Efforts) > 0 {
				entry["efforts"] = append([]string(nil), model.Efforts...)
			}
			if score, ok := m.modelScore(group.ProviderID, model.ModelID); ok {
				entry["userScore"] = score
			}
			models = append(models, entry)
		}
		entry := map[string]any{
			"providerId": group.ProviderID, "providerName": group.ProviderName,
			"models": models, "modes": group.Modes, "status": group.Status,
			"permissionIntents": permissionIntentModes(group.ProviderID, group.Modes),
		}
		if group.LatencyMs != nil {
			entry["latencyMs"] = *group.LatencyMs
		}
		if group.Error != "" {
			entry["error"] = group.Error
		}
		if group.FixHint != "" {
			entry["fixHint"] = group.FixHint
		}
		if group.Badge != "" {
			entry["badge"] = group.Badge
		}
		groups = append(groups, entry)
	}

	activeModel, activeEffort := strings.TrimSpace(parent.startOpts.ModelID), ""
	activeMode := m.effectiveSubagentParentMode(parent)
	for _, group := range rawGroups {
		if normalizeProviderID(group.ProviderID) != normalizeProviderID(parent.ProviderID) {
			continue
		}
		for _, model := range group.Models {
			if base, effort, split := splitEffortSuffix(activeModel, model.Efforts); split && base == model.ModelID {
				activeModel, activeEffort = base, effort
				break
			}
		}
	}
	recommendations := make([]map[string]any, 0, len(recommendationProfiles))
	for _, profile := range recommendationProfiles {
		avoid := ""
		if profile.ID == "independent-review" {
			avoid = parent.ProviderID
		}
		selection, ok := m.recommendSubagentSelection(ctx, profile.ID, avoid)
		item := map[string]any{"profileId": profile.ID, "configured": ok}
		if ok {
			item["selection"] = map[string]any{
				"providerId": selection.ProviderID, "modelId": selection.ModelID,
				"effort": selection.Effort, "score": selection.Score,
			}
		}
		recommendations = append(recommendations, item)
	}
	return map[string]any{
		"schemaVersion": 2,
		"active": map[string]any{
			"providerId": parent.ProviderID, "modelId": activeModel,
			"effort": activeEffort, "modeId": activeMode,
			"cwd": parent.CWD,
		},
		"groups":          groups,
		"scoreDimensions": append([]string(nil), modelScoreDimensions...),
		"ratingScale":     map[string]any{"min": 1, "max": 10, "costDirection": "higher-is-more-expensive"},
		"profiles":        publicRecommendationProfiles(),
		"recommendations": recommendations,
		"limits": map[string]any{
			"maxConcurrentPerTurn": maxConcurrentSubagentsPerTurn,
			"maxConcurrentGlobal":  maxConcurrentSubagentsGlobal,
			"maxCompletedHistory":  maxCompletedSubagentHistory,
		},
	}
}
