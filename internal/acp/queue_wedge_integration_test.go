package acp

import (
	"context"
	"testing"
	"time"

	chatstate "workass/internal/chat"
	providercontract "workass/internal/provider"
)

// 2026-10-11 QA: Stop on a turn still waiting for its lane wrote that turn's
// rows on the proposed lane; session/new then canonicalized the lane id, the
// rows named a lane that no longer existed, opening failed validation and the
// queued message behind it wedged forever, across restarts.
func TestCancelBeforeLaneOpensDoesNotWedgeTheQueueBehindIt(t *testing.T) {
	t.Parallel()
	manager, _ := newFakeManager(t, "echo-prompt-resume", Options{RSSSampleInterval: time.Hour})
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatstate.NewEngine("wedge-chat")
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	definition, err := manager.ProviderDefinition("custom")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "custom"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{ChatID: "wedge-chat", Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir)}.Normalize()
	for _, command := range []chatstate.Command{
		chatstate.SelectLane{Identity: identity, Owner: providercontract.AttachmentOwner{TabID: "wedge-tab"}, CWD: manager.opts.RootDir},
		chatstate.Submit{OperationID: "head", Text: "head", Presentation: providercontract.TurnPresentation{Origin: "human"}},
		chatstate.Submit{OperationID: "behind", Text: "behind", Presentation: providercontract.TurnPresentation{Origin: "human", QueueID: "q:behind"}},
		chatstate.CancelPendingTurn{OperationID: "head"},
	} {
		if err := engine.Apply(command); err != nil {
			t.Fatalf("%T: %v", command, err)
		}
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatalf("lane open after a pre-admission cancel failed: %v", err)
	}
	waitForState(t, engine, "queued turn behind the cancelled one ran", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Queue) == 0 && len(state.Ledger) == 4
	})
	state := engine.Snapshot()
	if lane := state.Lanes[state.ActiveLaneID]; lane.Phase != chatstate.LaneReady {
		t.Fatalf("lane after the queued turn = %q", lane.Phase)
	}
	for _, row := range state.Ledger {
		if _, ok := state.Lanes[row.LaneID]; !ok {
			t.Fatalf("ledger row names a lane that does not exist: %#v", row)
		}
	}
}
