package mcpprotocol

import "testing"

func TestLegacyNegotiationPreservesEverySupportedRevision(t *testing.T) {
	for _, version := range LegacyVersions() {
		if !IsLegacy(version) {
			t.Fatalf("legacy version %q is not recognized", version)
		}
		if negotiated := NegotiateLegacy(version); negotiated != version {
			t.Fatalf("negotiate %q = %q", version, negotiated)
		}
	}
	if IsLegacy(ModernVersion) {
		t.Fatalf("modern version %q was classified as legacy", ModernVersion)
	}
	if negotiated := NegotiateLegacy("2099-01-01"); negotiated != LatestLegacyVersion {
		t.Fatalf("unknown legacy negotiation = %q", negotiated)
	}
}

func TestVersionListsAreDefensiveCopies(t *testing.T) {
	versions := LegacyVersions()
	versions[0] = "changed"
	if LegacyVersions()[0] != LatestLegacyVersion {
		t.Fatal("LegacyVersions exposed mutable package state")
	}
	all := AllVersions()
	if all[0] != ModernVersion || len(all) != len(LegacyVersions())+1 {
		t.Fatalf("all versions = %#v", all)
	}
}
