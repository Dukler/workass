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
