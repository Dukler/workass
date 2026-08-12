package chat

import (
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"workass/internal/provider"
)

type workspaceAtomicFixture struct {
	state   State
	store   FileStore
	targets []DetachTarget
	oldCWD  string
	newCWD  string
}

func newWorkspaceAtomicFixture(t *testing.T) workspaceAtomicFixture {
	t.Helper()
	const chatID = "workspace-atomic-file-chat"
	state, err := NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation.TabID = "workspace-atomic-tab"
	state.Presentation.WorkspaceRevision = 4
	oldCWD, newCWD := t.TempDir(), t.TempDir()
	state.Presentation.CWD = &oldCWD

	realm := provider.Realm{
		ProviderID: "mock", MachineID: "machine", AccountScope: "account", InstallScope: "install",
	}.Normalize()
	laneIDs := make([]provider.LaneID, 0, 2)
	for index := 0; index < 2; index++ {
		epoch := provider.WorkspaceEpoch(fmt.Sprintf("workspace-epoch-%d", index))
		identity := provider.LaneIdentity{ChatID: chatID, Realm: realm, WorkspaceEpoch: epoch}.Normalize()
		connectionID := fmt.Sprintf("workspace-connection-%d", index)
		state.Lanes[identity.ID] = LaneState{
			Identity: identity,
			Owner:    provider.AttachmentOwner{TabID: state.Presentation.TabID},
			CWD:      oldCWD,
			Thread: provider.ThreadRef{
				ProviderID: realm.ProviderID,
				RootID:     fmt.Sprintf("workspace-root-%d", index),
				HeadID:     fmt.Sprintf("workspace-head-%d", index),
				Lineage:    uint64(index + 1),
			},
			Phase:                LaneReady,
			Coverage:             make(map[uint64]CoverageRecord),
			ConnectionGeneration: uint64(index + 7),
			Attachment: &provider.LaneAttachmentSnapshot{
				ConnectionID: connectionID, CWD: oldCWD, ProviderID: realm.ProviderID,
			},
		}
		laneIDs = append(laneIDs, identity.ID)
	}
	sort.Slice(laneIDs, func(i, j int) bool { return laneIDs[i] < laneIDs[j] })
	state.ActiveLaneID = laneIDs[0]
	state.DesiredLaneID = laneIDs[1]

	targets := make([]DetachTarget, 0, len(laneIDs))
	for _, laneID := range laneIDs {
		lane := state.Lanes[laneID]
		targets = append(targets, DetachTarget{
			OperationID:          DetachOperationID(chatID, laneID, lane.Attachment.ConnectionID, lane.ConnectionGeneration),
			LaneID:               laneID,
			Owner:                lane.Owner,
			ConnectionID:         lane.Attachment.ConnectionID,
			ConnectionGeneration: lane.ConnectionGeneration,
		})
	}
	store := FileStore{Path: filepath.Join(t.TempDir(), "provider-chat.json")}
	if err := store.Save(state); err != nil {
		t.Fatalf("seed file-backed workspace actor: %v", err)
	}
	return workspaceAtomicFixture{state: state, store: store, targets: targets, oldCWD: oldCWD, newCWD: newCWD}
}

func TestWorkspaceChangeFileCommitContainsOrderedDetachGroup(t *testing.T) {
	fixture := newWorkspaceAtomicFixture(t)
	engine, err := NewDurableEngine(fixture.state.ChatID, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	operationID := provider.OperationID("workspace-atomic-move")
	command := ChangeWorkspace{
		OperationID: operationID, Digest: "workspace-atomic-digest", CWD: fixture.newCWD,
		ExpectedRevision: before.Presentation.WorkspaceRevision, DetachTargets: fixture.targets,
	}
	if err := engine.Apply(command); err != nil {
		t.Fatalf("atomic workspace commit: %v", err)
	}

	persisted, ok, err := fixture.store.Load(fixture.state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load committed workspace actor: ok=%v err=%v", ok, err)
	}
	receipt, ok := persisted.WorkspaceMutationReceipts[operationID]
	if !ok || receipt.Digest != command.Digest || receipt.CWD != fixture.newCWD || receipt.Revision != before.Presentation.WorkspaceRevision+1 {
		t.Fatalf("durable workspace receipt = %#v", persisted.WorkspaceMutationReceipts)
	}
	wantIDs := make([]provider.OperationID, 0, len(fixture.targets))
	for _, target := range fixture.targets {
		wantIDs = append(wantIDs, target.OperationID)
	}
	if !sameOperationIDSequence(receipt.DetachOperationIDs, wantIDs) {
		t.Fatalf("receipt detach group = %#v, want %#v", receipt.DetachOperationIDs, wantIDs)
	}
	if persisted.Presentation.CWD == nil || *persisted.Presentation.CWD != fixture.newCWD ||
		persisted.Presentation.WorkspaceRevision != before.Presentation.WorkspaceRevision+1 ||
		persisted.ActiveLaneID != "" || persisted.DesiredLaneID != "" {
		t.Fatalf("durable workspace epoch = %#v active=%q desired=%q", persisted.Presentation, persisted.ActiveLaneID, persisted.DesiredLaneID)
	}
	if len(persisted.Outbox) != len(fixture.targets) {
		t.Fatalf("durable detach outbox count = %d, want %d", len(persisted.Outbox), len(fixture.targets))
	}
	for index, target := range fixture.targets {
		entry := persisted.Outbox[index]
		if entry.Kind != EffectDetachLane || entry.Status != OutboxPending || entry.ID != string(target.OperationID) ||
			entry.OperationID != target.OperationID || entry.LaneID != target.LaneID || entry.Owner != target.Owner ||
			entry.ConnectionID != target.ConnectionID || entry.Generation != target.ConnectionGeneration {
			t.Fatalf("durable detach outbox[%d] = %#v", index, entry)
		}
		lane := persisted.Lanes[target.LaneID]
		if lane.CWD != before.Lanes[target.LaneID].CWD || !lane.Thread.Equal(before.Lanes[target.LaneID].Thread) ||
			lane.Attachment == nil || lane.Attachment.ConnectionID != before.Lanes[target.LaneID].Attachment.ConnectionID {
			t.Fatalf("historical lane %s changed during workspace commit: before=%#v after=%#v", target.LaneID, before.Lanes[target.LaneID], lane)
		}
	}

	changedRetry := command
	changedRetry.CWD = t.TempDir()
	changedRetry.Digest = "workspace-atomic-digest-changed"
	if err := engine.Apply(changedRetry); err == nil {
		t.Fatal("changed workspace retry was accepted")
	}
	changedTargetRetry := command
	changedTargetRetry.DetachTargets = append([]DetachTarget(nil), fixture.targets...)
	changedTargetRetry.DetachTargets[0].Owner.TabID = "different-tab"
	if err := engine.Apply(changedTargetRetry); err == nil {
		t.Fatal("workspace retry with changed detach target was accepted")
	}
	afterRetry, ok, err := fixture.store.Load(fixture.state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load state after rejected retries: ok=%v err=%v", ok, err)
	}
	if afterRetry.Presentation.CWD == nil || *afterRetry.Presentation.CWD != fixture.newCWD ||
		afterRetry.Presentation.WorkspaceRevision != persisted.Presentation.WorkspaceRevision ||
		len(afterRetry.Outbox) != len(persisted.Outbox) ||
		!sameOperationIDSequence(afterRetry.WorkspaceMutationReceipts[operationID].DetachOperationIDs, receipt.DetachOperationIDs) {
		t.Fatalf("rejected retry changed durable transaction: before=%#v after=%#v", persisted, afterRetry)
	}
}

func TestWorkspaceChangeInvalidMultiTargetLeavesFileStateUntouched(t *testing.T) {
	fixture := newWorkspaceAtomicFixture(t)
	engine, err := NewDurableEngine(fixture.state.ChatID, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	invalidTargets := append([]DetachTarget(nil), fixture.targets...)
	invalidTargets[1].ConnectionGeneration++
	if err := engine.Apply(ChangeWorkspace{
		OperationID: "workspace-invalid-group", Digest: "workspace-invalid-digest", CWD: fixture.newCWD,
		ExpectedRevision: before.Presentation.WorkspaceRevision, DetachTargets: invalidTargets,
	}); err == nil {
		t.Fatal("invalid multi-target workspace commit was accepted")
	}
	persisted, ok, err := fixture.store.Load(fixture.state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load state after invalid group: ok=%v err=%v", ok, err)
	}
	if persisted.Presentation.CWD == nil || *persisted.Presentation.CWD != fixture.oldCWD ||
		persisted.Presentation.WorkspaceRevision != before.Presentation.WorkspaceRevision ||
		persisted.ActiveLaneID != before.ActiveLaneID || persisted.DesiredLaneID != before.DesiredLaneID ||
		len(persisted.WorkspaceMutationReceipts) != 0 || len(persisted.Outbox) != 0 {
		t.Fatalf("invalid group left partial durable state: presentation=%#v active=%q desired=%q receipts=%#v outbox=%#v",
			persisted.Presentation, persisted.ActiveLaneID, persisted.DesiredLaneID, persisted.WorkspaceMutationReceipts, persisted.Outbox)
	}
}

func TestWorkspaceChangeSameCWDKeepsEpochSelectionAndHistory(t *testing.T) {
	fixture := newWorkspaceAtomicFixture(t)
	engine, err := NewDurableEngine(fixture.state.ChatID, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	before := engine.Snapshot()
	if err := engine.Apply(ChangeWorkspace{
		OperationID: "workspace-same-cwd", Digest: "workspace-same-cwd-digest", CWD: fixture.oldCWD,
		ExpectedRevision: before.Presentation.WorkspaceRevision,
	}); err != nil {
		t.Fatalf("same-CWD workspace commit: %v", err)
	}
	persisted := engine.Snapshot()
	receipt := persisted.WorkspaceMutationReceipts["workspace-same-cwd"]
	if persisted.Presentation.CWD == nil || *persisted.Presentation.CWD != fixture.oldCWD ||
		persisted.Presentation.WorkspaceRevision != before.Presentation.WorkspaceRevision ||
		persisted.ActiveLaneID != before.ActiveLaneID || persisted.DesiredLaneID != before.DesiredLaneID ||
		len(receipt.DetachOperationIDs) != 0 || len(persisted.Outbox) != 0 {
		t.Fatalf("same-CWD workspace changed epoch or selection: presentation=%#v active=%q desired=%q receipt=%#v outbox=%#v",
			persisted.Presentation, persisted.ActiveLaneID, persisted.DesiredLaneID, receipt, persisted.Outbox)
	}
	for laneID, beforeLane := range before.Lanes {
		lane := persisted.Lanes[laneID]
		if lane.CWD != beforeLane.CWD || !lane.Thread.Equal(beforeLane.Thread) || lane.Attachment == nil {
			t.Fatalf("same-CWD workspace changed historical lane %s: before=%#v after=%#v", laneID, beforeLane, lane)
		}
	}
}

func TestWorkspaceChangePartialDispatchRecoversGroupWithoutResend(t *testing.T) {
	fixture := newWorkspaceAtomicFixture(t)
	engine, err := NewDurableEngine(fixture.state.ChatID, fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	command := ChangeWorkspace{
		OperationID: "workspace-partial-dispatch", Digest: "workspace-partial-dispatch-digest", CWD: fixture.newCWD,
		ExpectedRevision: fixture.state.Presentation.WorkspaceRevision, DetachTargets: fixture.targets,
	}
	if err := engine.Apply(command); err != nil {
		t.Fatalf("partial-dispatch workspace commit: %v", err)
	}
	if _, claimed, err := engine.ClaimEffect(string(fixture.targets[0].OperationID)); err != nil || !claimed {
		t.Fatalf("claim first detach before simulated crash: claimed=%v err=%v", claimed, err)
	}
	restarted, err := NewDurableEngine(fixture.state.ChatID, fixture.store)
	if err != nil {
		t.Fatalf("restart after partial dispatch: %v", err)
	}
	recovered := restarted.Snapshot()
	receipt := recovered.WorkspaceMutationReceipts[command.OperationID]
	if !sameOperationIDSequence(receipt.DetachOperationIDs, []provider.OperationID{fixture.targets[0].OperationID, fixture.targets[1].OperationID}) {
		t.Fatalf("restart changed receipt group: %#v", receipt)
	}
	first, firstFound := workspaceTestDetachEntry(recovered, fixture.targets[0].OperationID)
	second, secondFound := workspaceTestDetachEntry(recovered, fixture.targets[1].OperationID)
	if !firstFound || !secondFound || first.Status != OutboxAmbiguous || first.LastError != provider.ErrorAcceptanceAmbiguous || second.Status != OutboxPending {
		t.Fatalf("partial-dispatch recovery statuses: first=%#v second=%#v", first, second)
	}
	if recovered.Presentation.CWD == nil || *recovered.Presentation.CWD != fixture.newCWD || recovered.Presentation.WorkspaceRevision != fixture.state.Presentation.WorkspaceRevision+1 {
		t.Fatalf("partial-dispatch recovery lost committed workspace epoch: %#v", recovered.Presentation)
	}
	if _, claimed, err := restarted.ClaimEffect(string(fixture.targets[0].OperationID)); err != nil || claimed {
		t.Fatalf("ambiguous detach was resent after restart: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := restarted.ClaimEffect(string(fixture.targets[1].OperationID)); err != nil || !claimed {
		t.Fatalf("pending detach was not claimable through actor effect boundary: claimed=%v err=%v", claimed, err)
	}
	if err := restarted.Apply(command); err != nil {
		t.Fatalf("receipt retry after partial dispatch: %v", err)
	}
	final := restarted.Snapshot()
	if final.WorkspaceMutationReceipts[command.OperationID].Revision != receipt.Revision ||
		final.Presentation.WorkspaceRevision != recovered.Presentation.WorkspaceRevision {
		t.Fatalf("receipt retry changed the committed epoch: recovered=%#v final=%#v", recovered.Presentation, final.Presentation)
	}
}

func workspaceTestDetachEntry(state State, operationID provider.OperationID) (OutboxEntry, bool) {
	for _, entry := range state.Outbox {
		if entry.Kind == EffectDetachLane && entry.OperationID == operationID {
			return entry, true
		}
	}
	return OutboxEntry{}, false
}
