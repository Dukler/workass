package main

import (
	"strings"
)

// Ajustes · Permisos: the owner's rules for what agents may do without them. They
// live in the app-settings blob under agentPermissionsSettingsKey and are read at
// the moment they apply, so a change holds from the next call on.
//
//   - subagentRaise: an agent may raise the permissions of the chats and subagents
//     it starts or changes, never above its own (the permission ceiling). Off, any
//     raise above where that agent starts or already is goes to the owner.
//   - browserNavigate, browserUpload: allow, ask (a question card the owner
//     answers) or block, for the agent browser's opening of pages and its file
//     uploads.
//   - browserDownload: allow or block. A download starts in the page after an
//     action has already returned, outside any call that could hold a card, so
//     there is no "ask" for it.
const agentPermissionsSettingsKey = "agentPermissions"

const (
	browserPolicyAllow = "allow"
	browserPolicyAsk   = "ask"
	browserPolicyBlock = "block"
)

type agentPermissionSettings struct {
	SubagentRaise   bool
	BrowserNavigate string
	BrowserDownload string
	BrowserUpload   string
}

// The defaults are what Workass did before the settings existed.
func defaultAgentPermissionSettings() agentPermissionSettings {
	return agentPermissionSettings{
		SubagentRaise:   true,
		BrowserNavigate: browserPolicyAllow,
		BrowserDownload: browserPolicyAllow,
		BrowserUpload:   browserPolicyAllow,
	}
}

func (s agentPermissionSettings) settingsValue() map[string]any {
	return map[string]any{
		"subagentRaise":   s.SubagentRaise,
		"browserNavigate": s.BrowserNavigate,
		"browserDownload": s.BrowserDownload,
		"browserUpload":   s.BrowserUpload,
	}
}

func parseAgentPermissionSettings(value any) agentPermissionSettings {
	out := defaultAgentPermissionSettings()
	raw, _ := value.(map[string]any)
	if raw == nil {
		return out
	}
	if v, ok := raw["subagentRaise"].(bool); ok {
		out.SubagentRaise = v
	}
	out.BrowserNavigate = browserPolicy(raw["browserNavigate"], out.BrowserNavigate, true)
	out.BrowserDownload = browserPolicy(raw["browserDownload"], out.BrowserDownload, false)
	out.BrowserUpload = browserPolicy(raw["browserUpload"], out.BrowserUpload, true)
	return out
}

func browserPolicy(value any, fallback string, canAsk bool) string {
	switch v := strings.ToLower(strings.TrimSpace(toString(value))); v {
	case browserPolicyAllow, browserPolicyBlock:
		return v
	case browserPolicyAsk:
		if canAsk {
			return v
		}
	}
	return fallback
}

// normalizeAgentPermissions is the settings blob's form of the key.
func normalizeAgentPermissions(value any) map[string]any {
	return parseAgentPermissionSettings(value).settingsValue()
}

func agentPermissionsFromSettings(settings map[string]any) agentPermissionSettings {
	if settings == nil {
		return defaultAgentPermissionSettings()
	}
	return parseAgentPermissionSettings(settings[agentPermissionsSettingsKey])
}

// agentPermissions reads the owner's rules now. With no settings reader (tests,
// an in-process caller) the defaults hold.
func (h *agentControlHandler) agentPermissions() agentPermissionSettings {
	if h == nil || h.settings == nil {
		return defaultAgentPermissionSettings()
	}
	return agentPermissionsFromSettings(h.settings())
}
