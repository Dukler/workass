package acp

import (
	"reflect"
	"testing"
)

func TestPermissionLevelModesNameEachProvidersNativeMode(t *testing.T) {
	tests := []struct {
		providerID string
		modes      []Mode
		want       map[string]string
	}{
		{"claude", []Mode{{ID: "default"}, {ID: "acceptEdits"}, {ID: "plan"}, {ID: "bypassPermissions"}},
			map[string]string{"ask": "default", "edit": "acceptEdits", "plan": "plan", "full": "bypassPermissions"}},
		// A read-only sandbox that asks before leaving it asks; it is not plan.
		{"codex", []Mode{{ID: "read-only"}, {ID: "agent"}, {ID: "agent-full-access"}},
			map[string]string{"ask": "read-only", "edit": "agent", "full": "agent-full-access"}},
		// OMP's default is whatever the user configured in OMP: no level.
		{"omp", []Mode{{ID: "default"}, {ID: "always-ask"}, {ID: "write"}, {ID: "yolo"}, {ID: "plan"}},
			map[string]string{"ask": "always-ask", "edit": "write", "plan": "plan", "full": "yolo"}},
		{"pi", []Mode{{ID: "native"}}, map[string]string{"full": "native"}},
		{"mock", []Mode{{ID: "ask"}, {ID: "bypass"}}, map[string]string{"ask": "ask", "full": "bypass"}},
		// One mode two levels name goes to the first.
		{"mock", []Mode{{ID: "auto"}}, map[string]string{"edit": "auto"}},
		{"mock", []Mode{}, map[string]string{}},
	}
	for _, tt := range tests {
		if got := permissionLevelModes(tt.providerID, tt.modes); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s levels = %#v, want %#v", tt.providerID, got, tt.want)
		}
	}
}

func TestOwnerPermissionLevelAndRank(t *testing.T) {
	tests := []struct {
		providerID, modeID, want string
		rank                     int
	}{
		{"claude", "plan", "plan", 1},
		{"claude", "default", "ask", 2},
		{"codex", "read-only", "ask", 2},
		{"claude", "acceptEdits", "edit", 3},
		{"codex", "agent", "edit", 3},
		{"claude", "bypassPermissions", "full", 4},
		{"pi", "native", "full", 4},
		{"omp", "default", "", 0},
		{"claude", "", "", 0},
		{"mock", "something-new", "", 0},
	}
	for _, tt := range tests {
		got := OwnerPermissionLevel(tt.providerID, tt.modeID)
		if got != tt.want || OwnerPermissionRank(got) != tt.rank {
			t.Errorf("%s/%s = %q rank %d, want %q rank %d", tt.providerID, tt.modeID, got, OwnerPermissionRank(got), tt.want, tt.rank)
		}
	}
}

func TestCatalogGroupCarriesPermissionLevels(t *testing.T) {
	runtime := &providerRuntime{Config: ProviderConfig{ID: "claude", Name: "Claude Code"}, Status: "ready",
		Modes: []Mode{{ID: "default", Name: "Ask before changes"}, {ID: "acceptEdits", Name: "Accept edits"}}}
	group := runtime.catalogGroupLocked()
	if group.PermissionLevels["ask"] != "default" || group.PermissionLevels["edit"] != "acceptEdits" {
		t.Fatalf("catalog group levels = %#v", group.PermissionLevels)
	}
}
