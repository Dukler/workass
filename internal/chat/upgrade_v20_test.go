package chat

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/provider"
)

func storedLaneForUpgradeTest(chatID, providerID, threadID string, established bool) StoredLane {
	identity := testLane(chatID, providerID)
	creation := provider.CreationCapabilities{}
	if providerID == "codex" || providerID == "claude" {
		creation.DeferredUntilInput = true
	}
	return StoredLane{
		Identity: identity,
		Thread: provider.ThreadRef{
			ProviderID: identity.Realm.ProviderID, RootID: threadID, HeadID: threadID, Lineage: 1,
		},
		Owner: provider.AttachmentOwner{TabID: "tab-" + chatID},
		CWD:   "/workspace/" + chatID, ModelID: "model-" + providerID, ModeID: "mode-" + providerID,
		Context: exactContext(provider.ContextImportUnsupported), Creation: creation, Established: established,
	}
}

func writeV20Actor(t *testing.T, path string, state State) []byte {
	t.Helper()
	if err := saveStateEnvelope(path, state, actorStateEnvelopeV20); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUpgradeActorStoreV21PreservesExactEstablishedThread(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	stored := storedLaneForUpgradeTest("chat", "codex", "native-thread", true)
	state, err := NewState(stored.Identity.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: stored.Owner.TabID, ProviderID: stored.Identity.Realm.ProviderID}
	state.Lanes[stored.Identity.ID] = LaneState{
		Identity: stored.Identity, Thread: stored.Thread, Owner: stored.Owner,
		CWD: stored.CWD, ModelID: stored.ModelID, ModeID: stored.ModeID,
		Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord), Context: stored.Context,
	}
	state.ActiveLaneID = stored.Identity.ID
	state.DesiredLaneID = stored.Identity.ID
	writeV20Actor(t, path, state)
	resolutions := 0
	if err := UpgradeActorStoreV21(dir, func(chatID string) ([]StoredLane, error) {
		resolutions++
		if chatID != state.ChatID {
			t.Fatalf("resolved chat = %q", chatID)
		}
		return []StoredLane{stored}, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load upgraded actor: ok=%v err=%v", ok, err)
	}
	lane := loaded.Lanes[stored.Identity.ID]
	if !lane.Thread.Equal(stored.Thread) || lane.Provision != nil || lane.Phase != LaneDetached ||
		lane.Creation != stored.Creation {
		t.Fatalf("exact established lane changed: %#v", lane)
	}
	if err := UpgradeActorStoreV21(dir, func(string) ([]StoredLane, error) {
		resolutions++
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if resolutions != 1 {
		t.Fatalf("current actor consulted old storage again: calls=%d", resolutions)
	}
}

func TestUpgradeActorStoreV21KeepsReceiptlessDeferredReferenceProvisional(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	stored := storedLaneForUpgradeTest("candidate-chat", "codex", "candidate-thread", false)
	state, err := NewState(stored.Identity.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: stored.Owner.TabID, ProviderID: "codex"}
	state.Lanes[stored.Identity.ID] = LaneState{
		Identity: stored.Identity, Thread: stored.Thread, Owner: stored.Owner,
		CWD: stored.CWD, ModelID: stored.ModelID, ModeID: stored.ModeID,
		Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord), Context: stored.Context,
	}
	state.ActiveLaneID = stored.Identity.ID
	state.DesiredLaneID = stored.Identity.ID
	writeV20Actor(t, path, state)
	if err := UpgradeActorStoreV21(dir, func(string) ([]StoredLane, error) {
		return []StoredLane{stored}, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load upgraded candidate actor: ok=%v err=%v", ok, err)
	}
	lane := loaded.Lanes[stored.Identity.ID]
	if !lane.Thread.IsZero() || lane.Provision == nil || !lane.Provision.Equal(stored.Thread) ||
		lane.Phase != LaneAbsent || !lane.Creation.DeferredUntilInput ||
		!lane.Delivery.StableInputIdentity || !lane.Delivery.ConsumptionReceipt ||
		!lane.CreateAfterCandidateAbsence {
		t.Fatalf("receiptless provider reference became established: %#v", lane)
	}
	_, effects, err := Reduce(loaded, RecoverOutbox{})
	if err != nil {
		t.Fatal(err)
	}
	if len(effects) != 0 {
		t.Fatalf("idle candidate spawned provider work during recovery: %#v", effects)
	}
}

func TestUpgradeActorStoreV21NeverCreatesOverNativeLaneHistory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	stored := storedLaneForUpgradeTest("history-chat", "codex", "saved-thread", false)
	state, err := NewState(stored.Identity.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: stored.Owner.TabID, ProviderID: "codex"}
	state.Operations["historical-operation"] = struct{}{}
	state.Ledger = []LedgerEvent{{
		EventID: "historical-event", MessageID: "historical-message", Sequence: 1,
		Role: "user", Text: "history", Status: "done", LaneID: stored.Identity.ID,
		ProviderID: "codex", OperationID: "historical-operation", NativeTurnID: "native-turn",
		TerminalState: "consumed",
	}}
	state.Lanes[stored.Identity.ID] = LaneState{
		Identity: stored.Identity, Thread: stored.Thread, Owner: stored.Owner,
		CWD: stored.CWD, ModelID: stored.ModelID, ModeID: stored.ModeID,
		Phase: LaneDetached, CoveredThrough: 1,
		Coverage: map[uint64]CoverageRecord{1: {
			Sequence: 1, EventID: "historical-event", Status: CoverageNativeSeen,
			DeliveryID: "historical-operation",
		}},
		Context: stored.Context,
	}
	state.ActiveLaneID = stored.Identity.ID
	state.DesiredLaneID = stored.Identity.ID
	writeV20Actor(t, path, state)
	if err := UpgradeActorStoreV21(dir, func(string) ([]StoredLane, error) {
		return []StoredLane{stored}, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load upgraded historical actor: ok=%v err=%v", ok, err)
	}
	lane := loaded.Lanes[stored.Identity.ID]
	if lane.Provision == nil || lane.CreateAfterCandidateAbsence {
		t.Fatalf("historical provider reference acquired create permission: %#v", lane)
	}
	_, effects, err := Reduce(loaded, Submit{OperationID: "next-operation", Text: "continue"})
	if err != nil || len(effects) != 1 {
		t.Fatalf("continue historical lane: effects=%#v err=%v", effects, err)
	}
	create, ok := effects[0].(CreateLaneEffect)
	if !ok || !create.Reconcile || create.CreateAfterCandidateAbsence {
		t.Fatalf("historical lane verification effect = %#v", effects[0])
	}
}

func TestUpgradeActorStoreV21RejectsThreadMismatchWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	actorLane := storedLaneForUpgradeTest("chat", "codex", "actor-thread", true)
	state, err := NewState(actorLane.Identity.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: actorLane.Owner.TabID, ProviderID: "codex"}
	state.Lanes[actorLane.Identity.ID] = LaneState{
		Identity: actorLane.Identity, Thread: actorLane.Thread, Owner: actorLane.Owner,
		Phase: LaneDetached, Coverage: make(map[uint64]CoverageRecord), Context: actorLane.Context,
	}
	state.ActiveLaneID = actorLane.Identity.ID
	state.DesiredLaneID = actorLane.Identity.ID
	original := writeV20Actor(t, path, state)
	stored := actorLane
	stored.Thread.RootID = "different-thread"
	stored.Thread.HeadID = "different-thread"
	if err := UpgradeActorStoreV21(dir, func(string) ([]StoredLane, error) {
		return []StoredLane{stored}, nil
	}); err == nil {
		t.Fatal("mismatched exact thread was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed upgrade modified the actor file")
	}
}

func TestUpgradeActorStoreV21AllowsEmptyActorWithoutProviderLane(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("empty-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: "tab-empty-chat", ProviderID: "codex"}
	writeV20Actor(t, path, state)
	if err := UpgradeActorStoreV21(dir, func(string) ([]StoredLane, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !ok {
		t.Fatalf("load empty upgraded actor: ok=%v err=%v", ok, err)
	}
	if loaded.ActiveLaneID != "" || loaded.DesiredLaneID != "" || len(loaded.Lanes) != 0 {
		t.Fatalf("empty actor invented a provider lane: %#v", loaded.Lanes)
	}
}
