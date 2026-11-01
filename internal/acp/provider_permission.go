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

// permissionLevelAliases ranks the modes no provider policy claims as an intent.
// They are real modes that the read/edit/full mapping has no use for, because
// nothing ever selects them BY intent: a mode that asks before everything sits
// with `default`, and `guardian` with the other modes that act on their own within
// bounds. Keys are lower case.
var permissionLevelAliases = map[string]string{
	"ask": "read", "always-ask": "read", "default": "read",
	"guardian": "edit",
}

// PermissionLevel ranks one mode by what it allows without a human, in the
// read/edit/full vocabulary every provider already maps to. A mode nothing knows
// returns empty, which means unknown and never high: PermissionLevelRank keeps
// it apart from every real level, and callers decide what unknown costs them.
func PermissionLevel(providerID, modeID string) string {
	modeID = strings.TrimSpace(modeID)
	if modeID == "" {
		return ""
	}
	if level := permissionIntentForMode(providerID, modeID); level != "" {
		return level
	}
	return permissionLevelAliases[strings.ToLower(modeID)]
}

// PermissionLevelRank orders levels: read 1, edit 2, full 3, anything else 0.
func PermissionLevelRank(level string) int {
	return hostedIntentRank(level)
}
