package chat

import (
	"testing"

	"workass/internal/provider"
)

func newDetachTestState(t *testing.T) (State, provider.LaneIdentity, provider.AttachmentOwner, provider.ThreadRef, string) {
	t.Helper()
	state, err := NewState("detach-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "detach-tab"
	identity := provider.LaneIdentity{
		ChatID: "detach-chat",
		Realm: provider.Realm{
			ProviderID: "mock", MachineID: "machine", AccountScope: "account", InstallScope: "install",
		},
		WorkspaceEpoch: "workspace",
	}.Normalize()
	owner := provider.AttachmentOwner{TabID: "detach-tab"}
	thread := provider.ThreadRef{ProviderID: "mock", RootID: "native-thread", HeadID: "native-thread", Lineage: 1}
	connectionID := "native-connection"
	state.Lanes[identity.ID] = LaneState{
		Identity: identity, Owner: owner, Thread: thread, Phase: LaneReady, ConnectionGeneration: 7,
		Attachment: &provider.LaneAttachmentSnapshot{ConnectionID: connectionID, ProviderID: "mock"},
	}
	state.ActiveLaneID, state.DesiredLaneID = identity.ID, identity.ID
	return state, identity, owner, thread, connectionID
}

func TestDetachLaneJournalsExactTargetAndSettlesReceiptOnce(t *testing.T) {
	state, identity, owner, thread, connectionID := newDetachTestState(t)
	operationID := DetachOperationID(state.ChatID, identity.ID, connectionID, 7)

	state, effects, err := Reduce(state, DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: owner,
		ConnectionID: connectionID, ConnectionGeneration: 7,
	})
	if err != nil {
		t.Fatalf("journal detach: %v", err)
	}
	if len(effects) != 1 {
		t.Fatalf("journal effects = %#v", effects)
	}
	entry := state.Outbox[0]
	if entry.Kind != EffectDetachLane || entry.Status != OutboxPending || entry.LaneID != identity.ID ||
		entry.ConnectionID != connectionID || entry.Generation != 7 || entry.OperationID != operationID {
		t.Fatalf("detach journal lost exact target: %#v", entry)
	}

	state, effects, err = Reduce(state, ClaimEffect{EffectID: string(operationID)})
	if err != nil || len(effects) != 1 {
		t.Fatalf("claim detach: effects=%#v err=%v", effects, err)
	}
	if _, ok := effects[0].(DetachLaneEffect); !ok || state.Outbox[0].Status != OutboxDispatched {
		t.Fatalf("claimed detach = %#v state=%#v", effects[0], state.Outbox[0])
	}

	state, effects, err = Reduce(state, HostLost{LaneID: identity.ID, ConnectionGeneration: 7})
	if err != nil || len(effects) != 0 {
		t.Fatalf("settle detach: effects=%#v err=%v", effects, err)
	}
	lane := state.Lanes[identity.ID]
	if lane.Phase != LaneDetached || lane.Attachment != nil || lane.ConnectionGeneration != 8 || !lane.Thread.Equal(thread) {
		t.Fatalf("detach changed the wrong lane state: %#v", lane)
	}
	if state.Outbox[0].Status != OutboxCompleted {
		t.Fatalf("detach receipt did not complete: %#v", state.Outbox[0])
	}

	state, effects, err = Reduce(state, HostLost{LaneID: identity.ID, ConnectionGeneration: 7})
	if err != nil || len(effects) != 0 || state.Lanes[identity.ID].ConnectionGeneration != 8 || state.Outbox[0].Status != OutboxCompleted {
		t.Fatalf("duplicate HostLost was not idempotent: effects=%#v err=%v state=%#v", effects, err, state)
	}
	state, effects, err = Reduce(state, ProviderEventReceived{
		ConnectionGeneration: 7,
		Event: provider.Event{Kind: provider.EventLaneDetached, Identity: provider.EventIdentity{
			ChatID: state.ChatID, LaneID: identity.ID, Sequence: 1,
		}},
	})
	if err != nil || len(effects) != 0 || state.Lanes[identity.ID].ConnectionGeneration != 8 {
		t.Fatalf("duplicate LaneDetached changed the newer generation: effects=%#v err=%v lane=%#v", effects, err, state.Lanes[identity.ID])
	}
}

func TestDetachLaneRecoveryNeverRetriesChangedAttachment(t *testing.T) {
	state, identity, owner, _, connectionID := newDetachTestState(t)
	operationID := DetachOperationID(state.ChatID, identity.ID, connectionID, 7)
	var err error
	state, _, err = Reduce(state, DetachTarget{
		OperationID: operationID, LaneID: identity.ID, Owner: owner,
		ConnectionID: connectionID, ConnectionGeneration: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err = Reduce(state, ClaimEffect{EffectID: string(operationID)})
	if err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Reduce(state, RecoverOutbox{})
	if err != nil {
		t.Fatalf("recover exact detach: %v", err)
	}
	if recovered.Outbox[0].Status != OutboxAmbiguous || recovered.Outbox[0].LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("dispatched detach was replayable after unknown acceptance: %#v", recovered.Outbox[0])
	}

	changed := state.Clone()
	lane := changed.Lanes[identity.ID]
	lane.ConnectionGeneration = 8
	lane.Attachment = &provider.LaneAttachmentSnapshot{ConnectionID: "new-connection", ProviderID: "mock"}
	lane.Phase = LaneReady
	changed.Lanes[identity.ID] = lane
	changed, _, err = Reduce(changed, RecoverOutbox{})
	if err != nil {
		t.Fatalf("recover changed detach: %v", err)
	}
	if changed.Outbox[0].Status != OutboxAmbiguous || changed.Outbox[0].LastError != provider.ErrorAcceptanceAmbiguous {
		t.Fatalf("changed detach was retried instead of fenced: %#v", changed.Outbox[0])
	}
}
