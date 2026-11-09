package acp

import (
	"context"
	"errors"
	"strings"
)

// The model catalog a coordinating agent reads (workass_agent_catalog), and
// the provider-neutral read/edit/full permission mapping subagents share.

func permissionIntentModes(providerID string, modes []Mode) map[string]string {
	available := map[string]bool{}
	for _, mode := range modes {
		available[mode.ID] = true
	}
	policy := providerAdapterForID(providerID).permission
	out := map[string]string{}
	for _, intent := range []string{"read", "edit", "full"} {
		ids := policy.Candidates(intent)
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

// permissionLevelModes names, for each owner level (OwnerPermissionLevels), the
// native mode of this provider that sits there, among the modes it offers. A mode
// is named for one level at most; a level the provider has no mode for is absent.
func permissionLevelModes(providerID string, modes []Mode) map[string]string {
	policy, ok := providerAdapterForID(providerID).permission.(providerPermissionLevelPolicy)
	if !ok {
		return map[string]string{}
	}
	available := map[string]bool{}
	for _, mode := range modes {
		available[mode.ID] = true
	}
	used := map[string]bool{}
	out := map[string]string{}
	for _, level := range OwnerPermissionLevels {
		for _, id := range policy.LevelCandidates(level) {
			if available[id] && !used[id] {
				out[level], used[id] = id, true
				break
			}
		}
	}
	return out
}

// OwnerPermissionLevel is the owner level one native mode sits at, or empty when
// the provider's policy places it nowhere.
func OwnerPermissionLevel(providerID, modeID string) string {
	modeID = strings.TrimSpace(modeID)
	policy, ok := providerAdapterForID(providerID).permission.(providerPermissionLevelPolicy)
	if !ok || modeID == "" {
		return ""
	}
	for _, level := range OwnerPermissionLevels {
		for _, candidate := range policy.LevelCandidates(level) {
			if strings.EqualFold(candidate, modeID) {
				return level
			}
		}
	}
	return ""
}

// OwnerPermissionRank orders the owner levels by what a mode may do without a
// person: plan 1, ask 2, edit 3, full 4, anything else 0 (unknown, never low).
func OwnerPermissionRank(level string) int {
	switch level {
	case OwnerLevelPlan:
		return 1
	case OwnerLevelAsk:
		return 2
	case OwnerLevelEdit:
		return 3
	case OwnerLevelFull:
		return 4
	default:
		return 0
	}
}

// permissionIntentForMode is the inverse bridge used by inherited subagent
// permissions. Provider-native ids are not portable: inheriting Codex
// `agent-full-access` into Claude must select `bypassPermissions`, not fall
// through to Claude's restrictive default and deadlock on an unattended
// permission request.
func permissionIntentForMode(providerID, modeID string) string {
	return providerAdapterForID(providerID).permission.Intent(modeID)
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
			models = append(models, entry)
		}
		entry := map[string]any{
			"providerId": group.ProviderID, "providerName": group.ProviderName,
			"models": models, "modes": group.Modes, "status": group.Status,
			"permissionIntents": permissionIntentModes(group.ProviderID, group.Modes),
			"fork":              ForkCapability(group.ProviderID),
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
	return map[string]any{
		"schemaVersion": 2,
		"active": map[string]any{
			"providerId": parent.ProviderID, "modelId": activeModel,
			"effort": activeEffort, "modeId": activeMode,
			"cwd": parent.CWD,
		},
		"groups": groups,
		"limits": map[string]any{
			"maxConcurrentPerTurn": maxConcurrentSubagentsPerTurn,
			"maxConcurrentGlobal":  maxConcurrentSubagentsGlobal,
			"maxCompletedHistory":  maxCompletedSubagentHistory,
		},
	}
}

// ChatPermissionMode is the mode one chat's session is actually running with, for
// a chat with no durable mode to read. It is the same reading inherited subagent
// permission does: the running job's effective mode, or the live session's when
// no turn is running. Empty means unknown, never full.
func (m *Manager) ChatPermissionMode(chatID, tabID string) (string, string) {
	if m == nil {
		return "", ""
	}
	chatID, tabID = strings.TrimSpace(chatID), strings.TrimSpace(tabID)
	m.mu.Lock()
	job := m.runningJobForIdentityLocked(chatID, tabID)
	m.mu.Unlock()
	if job == nil {
		job = m.subagentParentDefaults(chatID, tabID)
	}
	providerID := firstNonEmpty(strings.TrimSpace(job.ProviderID), strings.TrimSpace(job.startOpts.ProviderID))
	return providerID, strings.TrimSpace(m.effectiveSubagentParentMode(job))
}

// SubagentPermissionMode is the selection one tracked subagent was started with.
func (m *Manager) SubagentPermissionMode(subagentID string) (string, string, bool) {
	subagentID = strings.TrimSpace(subagentID)
	if m == nil || subagentID == "" {
		return "", "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	run := m.subagents[subagentID]
	if run == nil {
		return "", "", false
	}
	return run.ProviderID, strings.TrimSpace(run.ModeID), true
}

// ResolveSubagentPermission answers what a spawn with these options would run at,
// by the same resolution the spawn performs, and starts nothing.
func (m *Manager) ResolveSubagentPermission(ctx context.Context, opts SubagentSpawnOptions) (string, string, error) {
	if m == nil {
		return "", "", errors.New("subagent spawn requires the ACP manager")
	}
	chatID, tabID, parent, ok := m.subagentOwnerContext(opts.OwnerKey, opts.ParentChatID, opts.ParentTabID)
	if !ok {
		return "", "", errors.New("no running Workass turn owns this subagent request")
	}
	if parent == nil {
		parent = m.subagentParentDefaults(chatID, tabID)
	}
	providerID, _, _, modeID, err := m.resolveSubagentSelection(ctx, parent, opts)
	return providerID, modeID, err
}
