package acp

type providerPermissionPolicy interface {
	Candidates(intent string) []string
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

type qwenProviderPermissionPolicy struct {
	genericProviderPermissionPolicy
}

func (qwenProviderPermissionPolicy) Candidates(intent string) []string {
	if intent == "read" {
		return []string{"plan", "default"}
	}
	return genericProviderPermissionPolicy{}.Candidates(intent)
}
