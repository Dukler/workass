package chat

import (
	"testing"

	"workass/internal/provider"
)

func providerActivityOwnerBase(t *testing.T) (State, provider.LaneIdentity) {
	t.Helper()
	state, _ := NewState("provider-owner-chat")
	state, _ = apply(t, state, InitializeChat{
		Presentation: PresentationState{TabID: "provider-owner-tab"},
		OperationID:  "provider-owner-create", Digest: "provider-owner-digest",
	})
	lane := testLane(state.ChatID, "codex")
	state, _ = apply(t, state, SelectLane{Identity: lane, Owner: provider.AttachmentOwner{TabID: state.Presentation.TabID}})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               lane.ID,
		Thread:               provider.ThreadRef{ProviderID: "codex", RootID: "provider-owner-thread", HeadID: "provider-owner-thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	return state, lane
}

func providerActivityActiveState(t *testing.T) (State, ProviderActivityOwner) {
	t.Helper()
	state, lane := providerActivityOwnerBase(t)
	state, _ = apply(t, state, Submit{OperationID: "active-operation", LaneID: lane.ID, Text: "active owner", Presentation: provider.TurnPresentation{Origin: "human"}})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "active-operation", Accepted: true,
		Turn: provider.TurnRef{OperationID: "active-operation", NativeID: "active-native-turn"},
	})
	foreground := state.Foreground
	if foreground == nil {
		t.Fatal("active owner fixture has no foreground turn")
	}
	return state, ProviderActivityOwner{
		LaneID: foreground.LaneID, OperationID: foreground.OperationID,
		TurnID:               foreground.Turn.NativeID,
		ConnectionGeneration: state.Lanes[foreground.LaneID].ConnectionGeneration,
	}
}

func providerActivityHistoricalState(t *testing.T, nativeTurnID string) (State, ProviderActivityOwner) {
	t.Helper()
	state, lane := providerActivityOwnerBase(t)
	state.Ledger = append(state.Ledger, LedgerEvent{
		EventID: "provider-owner-historical-event", MessageID: "provider-owner-historical-message",
		Sequence: 1, Role: "assistant", Text: "historical owner", Status: "done",
		LaneID: lane.ID, ProviderID: lane.Realm.ProviderID,
		OperationID: "historical-operation", NativeTurnID: nativeTurnID,
	})
	state.Operations["historical-operation"] = struct{}{}
	return state, ProviderActivityOwner{
		LaneID: lane.ID, OperationID: "historical-operation", TurnID: nativeTurnID,
		ConnectionGeneration: state.Lanes[lane.ID].ConnectionGeneration,
	}
}

func addProviderActivityRows(state *State, owner ProviderActivityOwner) {
	state.Tools["tool-call"] = ToolState{Owner: owner}
	state.Plans[owner.OperationID] = PlanState{Owner: owner}
	state.Permissions["permission-request"] = PermissionState{Owner: owner}
	state.Background["background-work"] = BackgroundState{Owner: owner}
	state.Compactions[owner.LaneID] = CompactionState{Owner: owner}
}

func TestProviderActivityOwnerValidationRequiresExactActorTurn(t *testing.T) {
	activeState, activeOwner := providerActivityActiveState(t)
	historicalState, historicalOwner := providerActivityHistoricalState(t, "historical-native-turn")
	tests := []struct {
		name    string
		state   State
		owner   ProviderActivityOwner
		wantErr bool
	}{
		{name: "unknown operation", state: activeState, owner: ProviderActivityOwner{
			LaneID: activeOwner.LaneID, OperationID: "unknown-operation", TurnID: activeOwner.TurnID,
			ConnectionGeneration: activeOwner.ConnectionGeneration,
		}, wantErr: true},
		{name: "mismatched turn", state: activeState, owner: ProviderActivityOwner{
			LaneID: activeOwner.LaneID, OperationID: activeOwner.OperationID, TurnID: "different-native-turn",
			ConnectionGeneration: activeOwner.ConnectionGeneration,
		}, wantErr: true},
		{name: "active exact owner", state: activeState, owner: activeOwner},
		{name: "historical exact owner", state: historicalState, owner: historicalOwner},
		{name: "empty native does not wildcard known historical turn", state: historicalState, owner: ProviderActivityOwner{
			LaneID: historicalOwner.LaneID, OperationID: historicalOwner.OperationID,
			ConnectionGeneration: historicalOwner.ConnectionGeneration,
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := test.state.Clone()
			addProviderActivityRows(&state, test.owner)
			// Usage and transport deliberately remain lane-scoped and do not
			// need a semantic turn owner.
			state.Usage[test.owner.LaneID] = provider.UsageEvent{}
			state.Transport[test.owner.LaneID] = provider.TransportHealthEvent{}
			err := state.Validate()
			if test.wantErr && err == nil {
				t.Fatal("forged provider activity owner was accepted")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("exact provider activity owner was rejected: %v", err)
			}
		})
	}
}

func TestProviderActivityOwnerValidationAcceptsHistoricalOperationWithoutNativeTurn(t *testing.T) {
	state, owner := providerActivityHistoricalState(t, "")
	state.Background["historical-work"] = BackgroundState{
		Owner: owner, Event: provider.BackgroundEvent{WorkID: "historical-work", Status: "exited"},
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("historical owner without a native turn was rejected: %v", err)
	}

	forged := state.Clone()
	forged.Background["historical-work"] = BackgroundState{
		Owner: ProviderActivityOwner{LaneID: owner.LaneID, OperationID: "unknown-operation", ConnectionGeneration: owner.ConnectionGeneration},
		Event: provider.BackgroundEvent{WorkID: "historical-work", Status: "exited"},
	}
	if err := forged.Validate(); err == nil {
		t.Fatal("background row with an unknown operation was accepted")
	}
}
func TestBackgroundAdmissionRequiresExactProviderActivityOwner(t *testing.T) {
	state, exact := providerActivityActiveState(t)
	newAction := func(owner ProviderActivityOwner) BackgroundAction {
		return BackgroundAction{
			Kind: BackgroundSpawnAgent, OperationID: "background-operation",
			Owner: owner, TabID: state.Presentation.TabID, ChatID: state.ChatID,
			Spawn: &SpawnAgentAction{Prompt: "background work"},
		}
	}
	if err := newAction(exact).Validate(state); err != nil {
		t.Fatalf("active exact owner was rejected: %v", err)
	}
	unknown := exact
	unknown.OperationID = "unknown-background-operation"
	if err := newAction(unknown).Validate(state); err == nil {
		t.Fatal("background action accepted unknown operation")
	}
	mismatched := exact
	mismatched.TurnID = "mismatched-background-turn"
	if err := newAction(mismatched).Validate(state); err == nil {
		t.Fatal("background action accepted mismatched turn")
	}

	historical, historicalOwner := providerActivityHistoricalState(t, "historical-background-turn")
	if err := newActionForState(historical, historicalOwner).Validate(historical); err != nil {
		t.Fatalf("historical exact owner was rejected: %v", err)
	}
	if _, _, err := Reduce(state, ReconcileBackgroundSnapshot{Items: []BackgroundState{{
		Owner: exact, Event: provider.BackgroundEvent{WorkID: "snapshot-work", Status: "running"},
	}}}); err != nil {
		t.Fatalf("exact background snapshot was rejected: %v", err)
	}
	if _, _, err := Reduce(state, ReconcileBackgroundSnapshot{Items: []BackgroundState{{
		Owner: mismatched, Event: provider.BackgroundEvent{WorkID: "snapshot-work", Status: "running"},
	}}}); err == nil {
		t.Fatal("background snapshot accepted mismatched turn")
	}
}

func newActionForState(state State, owner ProviderActivityOwner) BackgroundAction {
	return BackgroundAction{
		Kind: BackgroundSpawnAgent, OperationID: "historical-background-operation",
		Owner: owner, TabID: state.Presentation.TabID, ChatID: state.ChatID,
		Spawn: &SpawnAgentAction{Prompt: "historical background work"},
	}
}
