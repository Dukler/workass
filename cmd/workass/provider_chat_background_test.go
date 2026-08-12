package main

import (
	"testing"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestRuntimeBackgroundOwnerRequiresExactOrigin(t *testing.T) {
	state, err := chat.NewState("chat")
	if err != nil {
		t.Fatal(err)
	}
	apply := func(command chat.Command) {
		t.Helper()
		next, _, reduceErr := chat.Reduce(state, command)
		if reduceErr != nil {
			t.Fatalf("apply %T: %v", command, reduceErr)
		}
		state = next
	}
	realm := providercontract.Realm{
		ProviderID: "codex", MachineID: "machine", AccountScope: "account", InstallScope: "install", Verified: true,
	}
	identity := providercontract.LaneIdentity{ChatID: "chat", WorkspaceEpoch: "workspace-1", Realm: realm}.Normalize()
	apply(chat.InitializeChat{Presentation: chat.PresentationState{TabID: "tab"}, OperationID: "create:background", Digest: "create-background"})
	apply(chat.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "tab"}})
	apply(chat.LaneOpened{
		LaneID: identity.ID, Thread: providercontract.ThreadRef{ProviderID: "codex", RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1, Context: providercontract.ContextCapabilities{ExactResume: true},
	})
	apply(chat.Submit{OperationID: "turn-op", Text: "run it"})
	apply(chat.TurnAdmitted{OperationID: "turn-op", Accepted: true, Turn: providercontract.TurnRef{OperationID: "turn-op", NativeID: "native-turn"}})

	if owner, ok := exactBackgroundOwner(state, acp.SpawnedWorkItem{ID: "work", ProviderID: "codex"}); ok {
		t.Fatalf("ownerless live item guessed historical owner %#v", owner)
	}
	item := acp.SpawnedWorkItem{ID: "work", ProviderID: "codex", OriginOperationID: "turn-op"}
	owner, ok := exactBackgroundOwner(state, item)
	if !ok || owner.LaneID != identity.ID || owner.OperationID != "turn-op" || owner.TurnID != "native-turn" {
		t.Fatalf("exact operation owner = %#v, %v", owner, ok)
	}
	item.OriginTurnID = "another-turn"
	if owner, ok := exactBackgroundOwner(state, item); ok {
		t.Fatalf("mismatched operation/turn origin was accepted: %#v", owner)
	}

	// The single-lane synthesis exists only in this named migration helper. It
	// is never reachable from applySpawnedWorkSnapshot.
	owner, ok = legacyBackgroundOwner(state, acp.SpawnedWorkItem{ID: "legacy", ProviderID: "codex"}, "legacy")
	if !ok || owner.OperationID != "migrated-background:legacy" {
		t.Fatalf("one-time legacy owner = %#v, %v", owner, ok)
	}
}
