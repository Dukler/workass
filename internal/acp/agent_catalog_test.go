package acp

import "testing"

func TestPermissionIntentModesUsesProviderNativeIds(t *testing.T) {
	codex := permissionIntentModes("codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}})
	if codex["read"] != "read-only" || codex["edit"] != "agent" || codex["full"] != "agent-full-access" {
		t.Fatalf("codex intents = %#v", codex)
	}
	claude := permissionIntentModes("claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}})
	if claude["read"] != "plan" || claude["edit"] != "acceptEdits" || claude["full"] != "bypassPermissions" {
		t.Fatalf("claude intents = %#v", claude)
	}
	mock := permissionIntentModes("mock", []Mode{{ID: "ask"}, {ID: "bypass"}})
	if mock["full"] != "bypass" {
		t.Fatalf("mock full intent = %#v", mock)
	}
}

func TestPermissionIntentInheritanceTranslatesAcrossProviders(t *testing.T) {
	tests := []struct {
		providerID string
		modeID     string
		want       string
	}{
		{providerID: "codex", modeID: "agent-full-access", want: "full"},
		{providerID: "claude", modeID: "bypassPermissions", want: "full"},
		{providerID: "mock", modeID: "bypass", want: "full"},
		{providerID: "codex", modeID: "agent", want: "edit"},
		{providerID: "claude", modeID: "acceptEdits", want: "edit"},
		{providerID: "codex", modeID: "read-only", want: "read"},
		{providerID: "claude", modeID: "plan", want: "read"},
	}
	for _, tt := range tests {
		if got := permissionIntentForMode(tt.providerID, tt.modeID); got != tt.want {
			t.Fatalf("%s/%s intent = %q, want %q", tt.providerID, tt.modeID, got, tt.want)
		}
	}
	claude := permissionIntentModes("claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}})
	if got := claude[permissionIntentForMode("codex", "agent-full-access")]; got != "bypassPermissions" {
		t.Fatalf("Codex full inheritance selected Claude mode %q", got)
	}
	if got := inheritedPermissionMode("codex", "agent-full-access", "claude", []Mode{{ID: "plan"}, {ID: "acceptEdits"}, {ID: "bypassPermissions"}}); got != "bypassPermissions" {
		t.Fatalf("resolved Codex→Claude inherited mode = %q", got)
	}
	codex := permissionIntentModes("codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}})
	if got := codex[permissionIntentForMode("claude", "bypassPermissions")]; got != "agent-full-access" {
		t.Fatalf("Claude full inheritance selected Codex mode %q", got)
	}
	if got := inheritedPermissionMode("claude", "bypassPermissions", "codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}}); got != "agent-full-access" {
		t.Fatalf("resolved Claude→Codex inherited mode = %q", got)
	}
}
