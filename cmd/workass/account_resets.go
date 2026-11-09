package main

import (
	"context"
	"strings"

	"workass/internal/acp"
	"workass/internal/wire"
)

// The Cuentas page's consent to use a provider's stored login for its account
// resets (internal/acp/account_resets.go). It lives in the app-settings blob
// as a list of the provider ids the owner pressed the consent button for; the
// renderer writes it with the usual read-modify-write of settings:set, and the
// daemon reads it here before any login is touched.

const (
	accountResetConsentKey   = "accountResetConsent"
	maxAccountResetConsented = 20
)

func normalizeAccountResetConsent(value any) []any {
	items, _ := value.([]any)
	out := make([]any, 0, min(len(items), maxAccountResetConsented))
	seen := make(map[string]bool, len(items))
	for _, raw := range items {
		id, _ := raw.(string)
		id = strings.ToLower(strings.TrimSpace(id))
		if !accountResetProviderIDShape(id) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == maxAccountResetConsented {
			break
		}
	}
	return out
}

func accountResetProviderIDShape(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func accountResetConsentGiven(settings map[string]any, providerID string) bool {
	providerID = strings.ToLower(strings.TrimSpace(providerID))
	if providerID == "" {
		return false
	}
	for _, raw := range normalizeAccountResetConsent(settings[accountResetConsentKey]) {
		if raw == providerID {
			return true
		}
	}
	return false
}

// registerAccountResetHandlers hands the manager the stored consent and adds
// accounts:resets-refresh, which brings one provider's account resets into
// its plan-usage snapshot through the daemon's cache: {providerId, force}
// replies {ok, providerId, readAt, retryAt, reason, message}; force is the
// owner's retry and never skips a 429's wait. Claims go through
// app-chat:use-rate-limit-reset.
func registerAccountResetHandlers(hub *wire.Hub, manager *acp.Manager, settings *appSettingsStore) int {
	manager.SetAccountResetConsent(func(providerID string) bool {
		return accountResetConsentGiven(settings.get(), providerID)
	})
	hub.Register("accounts:resets-refresh", func(args []any) (any, error) {
		arg := firstMapArg(args)
		providerID := strings.ToLower(strings.TrimSpace(fieldString(arg, "providerId")))
		force, _ := boolField(arg, "force")
		return manager.RefreshAccountResets(context.Background(), providerID, force)
	})
	return 1
}
