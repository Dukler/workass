package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

func TestFileStoreRejectsUnsupportedVersionWithoutWriting(t *testing.T) {
	state, err := NewState("unsupported-version-chat")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion - 1, State: state})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "provider-chats", "unsupported-version-chat.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, found, err := (FileStore{Path: path}).Load(state.ChatID); err == nil || found || !strings.Contains(err.Error(), "unsupported chat state version 21") {
		t.Fatalf("unsupported actor schema was accepted: found=%v err=%v", found, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("unsupported actor state was rewritten: got=%q want=%q", got, raw)
	}
}

func TestFileStoreRejectsCurrentStateWithMissingOutboxPresentationOriginWithoutWriting(t *testing.T) {
	state, err := NewState("missing-origin-chat")
	if err != nil {
		t.Fatal(err)
	}
	laneID := testLane(state.ChatID, "provider")
	state, _ = apply(t, state, SelectLane{Identity: laneID})
	state, _ = apply(t, state, LaneOpened{
		LaneID:               laneID.ID,
		Thread:               provider.ThreadRef{ProviderID: laneID.Realm.ProviderID, RootID: "thread", HeadID: "thread", Lineage: 1},
		ConnectionGeneration: 1,
		Context:              exactContext(provider.ContextImportUnsupported),
	})
	state, _ = apply(t, state, Submit{
		OperationID: "turn", Text: "current schema input",
		Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", Origin: "human"},
	})
	foundStart := false
	for index := range state.Outbox {
		if state.Outbox[index].Kind != EffectStartTurn || state.Outbox[index].Input == nil {
			continue
		}
		state.Outbox[index].Input.Presentation.Origin = ""
		foundStart = true
		break
	}
	if !foundStart {
		t.Fatal("fixture did not create a durable start-turn input")
	}

	raw, err := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion, State: state})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "provider-chats", state.ChatID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, found, err := (FileStore{Path: path}).Load(state.ChatID); err == nil || found || !strings.Contains(err.Error(), "start_turn outbox input: turn presentation origin is required") {
		t.Fatalf("current actor with missing input origin was accepted: found=%v err=%v", found, err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("invalid current actor state was rewritten: got=%q want=%q", got, raw)
	}
}
