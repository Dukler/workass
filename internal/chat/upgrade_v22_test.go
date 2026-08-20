package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/provider"
)

func writeV21Actor(t *testing.T, path string, state State) []byte {
	t.Helper()
	raw, err := json.Marshal(stateEnvelope{Version: actorStateEnvelopeV21, State: state})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUpgradeActorStoreV22MakesV21OriginSemanticsExplicit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("origin-upgrade-chat")
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane(state.ChatID, "provider")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               lane.ID,
		Thread:               provider.ThreadRef{ProviderID: lane.Realm.ProviderID, RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "turn", Text: "work",
		Presentation: provider.TurnPresentation{UserMessageID: "turn-user", AssistantMessageID: "turn-assistant", Origin: "human"},
	})
	state, _ = apply(t, state, TurnAdmitted{
		OperationID: "turn", Accepted: true,
		Turn: provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	state, _ = apply(t, state, Steer{
		OperationID: "pending-steer", Text: "change direction",
		Presentation: provider.TurnPresentation{UserMessageID: "agent-user", AssistantMessageID: "agent-assistant", Origin: "agent"},
	})

	var pendingIndex int
	foundPending := false
	for index := range state.Outbox {
		if state.Outbox[index].Kind == EffectSteerTurn && state.Outbox[index].OperationID == "pending-steer" {
			pendingIndex = index
			foundPending = true
			state.Outbox[index].Input.Presentation = provider.TurnPresentation{}
			break
		}
	}
	if !foundPending || state.PendingSteer == nil {
		t.Fatal("fixture did not create a pending steer receipt")
	}
	state.Outbox = append(state.Outbox, OutboxEntry{
		ID: "turn-steer:completed-steer", Kind: EffectSteerTurn, Status: OutboxCompleted,
		LaneID: lane.ID, OperationID: "completed-steer",
		Input: &QueueEntry{OperationID: "completed-steer", LaneID: lane.ID, Text: "old human steer"},
		Turn:  provider.TurnRef{OperationID: "turn", NativeID: "native-turn"},
	})
	writeV21Actor(t, path, state)

	if err := UpgradeActorStoreV22(dir); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := (FileStore{Path: path}).Load(state.ChatID)
	if err != nil || !found {
		t.Fatalf("load upgraded actor: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(loaded.Outbox[pendingIndex].Input.Presentation, loaded.PendingSteer.Presentation) ||
		loaded.Outbox[pendingIndex].Input.Presentation.Origin != "agent" {
		t.Fatalf("pending steer lost its exact presentation: outbox=%#v pending=%#v", loaded.Outbox[pendingIndex].Input.Presentation, loaded.PendingSteer.Presentation)
	}
	completed := loaded.Outbox[len(loaded.Outbox)-1]
	if completed.Input == nil || completed.Input.Presentation.Origin != "human" {
		t.Fatalf("v21 human compatibility was not made explicit: %#v", completed.Input)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := UpgradeActorStoreV22(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("current actor was rewritten by a repeated upgrade")
	}
}

func TestUpgradeActorStoreV22RejectsInvalidV21OriginWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("invalid-origin-chat")
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane(state.ChatID, "provider")
	state, _ = apply(t, state, SelectLane{Identity: lane})
	state.Queue = append(state.Queue, QueueEntry{
		OperationID: "queued", LaneID: lane.ID, Text: "invalid",
		Presentation: provider.TurnPresentation{Origin: "robot"}, Revision: 1,
	})
	original := writeV21Actor(t, path, state)

	if err := UpgradeActorStoreV22(dir); err == nil || !strings.Contains(err.Error(), "turn presentation origin is invalid") {
		t.Fatalf("invalid v21 origin was accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed upgrade modified the actor file")
	}
}
