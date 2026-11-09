package acp

import "strings"

type providerPermissionPolicy interface {
	Candidates(intent string) []string
	Intent(modeID string) string
}

type genericProviderPermissionPolicy struct{}

func (genericProviderPermissionPolicy) Candidates(intent string) []string {
	switch intent {
	case "read":
		return []string{"read-only", "plan", "default"}
	case "edit":
		return []string{"agent", "acceptEdits", "auto-edit", "auto", "default"}
	case "full":
		return []string{"agent-full-access", "bypassPermissions", "bypass", "dontAsk", "yolo", "auto"}
	default:
		return nil
	}
}

func (policy genericProviderPermissionPolicy) Intent(modeID string) string {
	modeID = strings.TrimSpace(modeID)
	if modeID == "" {
		return ""
	}
	for _, intent := range []string{"read", "edit", "full"} {
		for _, candidate := range policy.Candidates(intent) {
			if strings.EqualFold(candidate, modeID) {
				return intent
			}
		}
	}
	return ""
}

type qwenProviderPermissionPolicy struct {
	genericProviderPermissionPolicy
}

func (qwenProviderPermissionPolicy) Candidates(intent string) []string {
	if intent == "read" {
		return []string{"plan", "default"}
	}
	return genericProviderPermissionPolicy{}.Candidates(intent)
}

func (qwenProviderPermissionPolicy) Intent(modeID string) string {
	return genericProviderPermissionPolicy{}.Intent(modeID)
}

// permissionLevelAliases ranks modes by the owner's word rather than by the
// read/edit/full mapping, which selects a mode BY intent and is a different question.
// A mode that asks before acting sits with edit, because a person is in front of
// whatever it does: a default-mode chat creating, configuring, sending to or spawning
// an editing child must not cost a card. `guardian` acts on its own within bounds.
// Keys are lower case.
var permissionLevelAliases = map[string]string{
	"ask": "edit", "always-ask": "edit", "default": "edit",
	"guardian": "edit",
}

// PermissionLevel ranks one mode in the read/edit/full vocabulary every provider
// already maps to; the aliases above go first. A mode nothing knows
// returns empty, which means unknown and never high: PermissionLevelRank keeps
// it apart from every real level, and callers decide what unknown costs them.
func PermissionLevel(providerID, modeID string) string {
	modeID = strings.TrimSpace(modeID)
	if modeID == "" {
		return ""
	}
	if level := permissionLevelAliases[strings.ToLower(modeID)]; level != "" {
		return level
	}
	return permissionIntentForMode(providerID, modeID)
}

// PermissionLevelRank orders levels: read 1, edit 2, full 3, anything else 0.
func PermissionLevelRank(level string) int {
	return hostedIntentRank(level)
}

// The owner's permission levels (Ajustes · Permisos): one word per level for every
// agent, each policy naming which of its native modes sits at which level. This is
// a classification of what a mode does, not the read/edit/full intent mapping
// above, which picks a mode BY intent: a mode that asks before editing is "ask"
// here even where it is also the closest thing a provider has to read-only.
const (
	OwnerLevelAsk  = "ask"
	OwnerLevelEdit = "edit"
	OwnerLevelPlan = "plan"
	OwnerLevelFull = "full"
)

// OwnerPermissionLevels is the order the levels are shown and assigned in. A mode
// two levels name goes to the first.
var OwnerPermissionLevels = []string{OwnerLevelAsk, OwnerLevelEdit, OwnerLevelPlan, OwnerLevelFull}

// providerPermissionLevelPolicy is the optional half of a permission policy that
// names native modes by owner level. A policy without it classifies nothing.
type providerPermissionLevelPolicy interface {
	LevelCandidates(level string) []string
}

func (genericProviderPermissionPolicy) LevelCandidates(level string) []string {
	switch level {
	case OwnerLevelAsk:
		// Asks before an edit or a command. A read-only sandbox that asks for
		// approval to leave it (on-request) is this, not plan.
		return []string{"default", "ask", "always-ask", "read-only"}
	case OwnerLevelEdit:
		return []string{"agent", "acceptEdits", "auto-edit", "auto"}
	case OwnerLevelPlan:
		return []string{"plan"}
	case OwnerLevelFull:
		return genericProviderPermissionPolicy{}.Candidates("full")
	default:
		return nil
	}
}
