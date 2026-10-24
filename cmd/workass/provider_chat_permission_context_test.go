package main

import (
	"testing"

	providercontract "workass/internal/provider"
)

// The stored reason and command reach the card's request payload; an empty
// context does not appear at all, so older cards stay exactly as they were.
func TestProjectPermissionCarriesItsContext(t *testing.T) {
	projected := projectPermission(&providercontract.PermissionEvent{
		RequestID: "perm-1", Title: "Bash", Kind: "execute", Status: "pending",
		Context: &providercontract.PermissionContext{Tool: "Bash", Reason: "Dangerous rm operation", Command: "rm -f $V/*.png"},
	})
	context, _ := projected["context"].(map[string]any)
	if context["tool"] != "Bash" || context["reason"] != "Dangerous rm operation" || context["command"] != "rm -f $V/*.png" {
		t.Fatalf("context = %+v", projected["context"])
	}
	if _, present := context["path"]; present {
		t.Fatalf("empty field projected: %+v", context)
	}
	plain := projectPermission(&providercontract.PermissionEvent{RequestID: "perm-2", Title: "Bash", Status: "pending"})
	if _, present := plain["context"]; present {
		t.Fatalf("permission without context grew one: %+v", plain)
	}
}
