package mcpprotocol

import "testing"

func TestModernVersionIsStable(t *testing.T) {
	if CurrentInitializedVersion != "2025-06-18" {
		t.Fatalf("CurrentInitializedVersion = %q", CurrentInitializedVersion)
	}
	if ModernVersion != "2026-07-28" {
		t.Fatalf("ModernVersion = %q", ModernVersion)
	}
	if InitializedVersion != "2025-11-25" {
		t.Fatalf("InitializedVersion = %q", InitializedVersion)
	}
	for _, version := range []string{CurrentInitializedVersion, InitializedVersion} {
		if !IsInitializedVersion(version) {
			t.Fatalf("IsInitializedVersion(%q) = false", version)
		}
	}
	if IsInitializedVersion("2024-11-05") {
		t.Fatal("old MCP revision was accepted")
	}
}
