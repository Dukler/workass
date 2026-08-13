package chat

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"workass/internal/provider"
)

func storedLaneForTest(chatID, providerID, threadID string) StoredLane {
	identity := testLane(chatID, providerID)
	return StoredLane{
		Identity: identity,
		Thread: provider.ThreadRef{
			ProviderID: identity.Realm.ProviderID, RootID: threadID, HeadID: threadID, Lineage: 1,
		},
		Owner: provider.AttachmentOwner{TabID: "tab-" + chatID},
		CWD:   "/workspace/" + chatID, ModelID: "model-" + providerID, ModeID: "mode-" + providerID,
		Context: exactContext(provider.ContextImportUnsupported),
	}
}

func writeV19Actor(t *testing.T, path string, state State, complete bool, usage json.RawMessage, unattributed []bool) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(raw, &encoded); err != nil {
		t.Fatal(err)
	}
	blockedError := ""
	if complete {
		blockedError = "native_identity_conflict"
	}
	encoded["Migration"] = map[string]any{
		"Version": 2, "Digest": "v19-cutover", "Complete": complete,
		"LegacyObligationMigrated": true, "LegacyBackgroundMigrated": true,
		"BlockedError": blockedError,
	}
	presentation := encoded["Presentation"].(map[string]any)
	if len(usage) != 0 {
		var value any
		if err := json.Unmarshal(usage, &value); err != nil {
			t.Fatal(err)
		}
		presentation["LegacyUsage"] = value
	}
	ledger := encoded["Ledger"].([]any)
	if len(ledger) != len(unattributed) {
		t.Fatalf("ledger rows=%d flags=%d", len(ledger), len(unattributed))
	}
	for index := range ledger {
		ledger[index].(map[string]any)["Legacy"] = unattributed[index]
	}
	payload, err := json.Marshal(map[string]any{"v": 19, "state": encoded})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
}

func migratedV19State(t *testing.T, chatID, providerID string) State {
	t.Helper()
	state, err := NewState(chatID)
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: "tab-" + chatID, ProviderID: provider.ID(providerID)}
	state.Operations["turn"] = struct{}{}
	state.Ledger = []LedgerEvent{
		{EventID: "event-user", MessageID: "message-user", Sequence: 1, Role: "user", Text: "question", Status: "done", OperationID: "turn"},
		{EventID: "event-assistant", MessageID: "message-assistant", Sequence: 2, Role: "assistant", Text: "answer", Status: "done", OperationID: "turn", TerminalState: "done"},
	}
	return state
}

func TestUpgradeActorStoreV20BindsEveryRowToExactStoredThread(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state := migratedV19State(t, "chat", "codex")
	writeV19Actor(t, path, state, true, json.RawMessage(`{"used":17,"size":100}`), []bool{true, true})
	exact := storedLaneForTest("chat", "codex", "native-thread-unchanged")
	resolutions := 0
	resolve := func(chatID string) ([]StoredLane, error) {
		resolutions++
		if chatID != "chat" {
			t.Fatalf("resolved chat = %q", chatID)
		}
		return []StoredLane{exact}, nil
	}
	if err := UpgradeActorStoreV20(dir, resolve); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load("chat")
	if err != nil || !ok {
		t.Fatalf("load upgraded actor: ok=%v err=%v", ok, err)
	}
	lane, ok := loaded.Lanes[exact.Identity.ID]
	if !ok || !lane.Thread.Equal(exact.Thread) || lane.Phase != LaneDetached {
		t.Fatalf("exact lane was not retained: %#v", lane)
	}
	for index, event := range loaded.Ledger {
		if event.LaneID != exact.Identity.ID || event.ProviderID != exact.Identity.Realm.ProviderID {
			t.Fatalf("ledger row %d ownership = %#v", index, event)
		}
		record := lane.Coverage[uint64(index+1)]
		if record.EventID != event.EventID || record.Status != CoverageNativeSeen {
			t.Fatalf("coverage row %d = %#v", index, record)
		}
	}
	if loaded.ActiveLaneID != exact.Identity.ID || loaded.DesiredLaneID != exact.Identity.ID {
		t.Fatalf("selected lane = active:%q desired:%q", loaded.ActiveLaneID, loaded.DesiredLaneID)
	}
	if string(loaded.Presentation.ContextUsageByProvider) != `{"codex":{"size":100,"used":17}}` {
		t.Fatalf("provider usage = %s", loaded.Presentation.ContextUsageByProvider)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, retiredField := range [][]byte{[]byte(`"Migration"`), []byte(`"Legacy"`), []byte(`"LegacyUsage"`)} {
		if bytes.Contains(saved, retiredField) {
			t.Fatalf("v20 retained field %s", retiredField)
		}
	}
	if err := UpgradeActorStoreV20(dir, resolve); err != nil {
		t.Fatal(err)
	}
	if resolutions != 1 {
		t.Fatalf("v20 actor was resolved again: calls=%d", resolutions)
	}
}

func TestUpgradeActorStoreV20SelectsPresentationProviderAcrossStoredLanes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state := migratedV19State(t, "chat", "claude")
	writeV19Actor(t, path, state, true, nil, []bool{true, true})
	codex := storedLaneForTest("chat", "codex", "codex-thread")
	claude := storedLaneForTest("chat", "claude", "claude-thread")
	if err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return []StoredLane{codex, claude}, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := (FileStore{Path: path}).Load("chat")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ActiveLaneID != claude.Identity.ID || !loaded.Lanes[claude.Identity.ID].Thread.Equal(claude.Thread) {
		t.Fatalf("presentation provider did not select exact Claude lane: %#v", loaded)
	}
}

func TestUpgradeActorStoreV20RejectsActorLaneMismatchWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state := migratedV19State(t, "chat", "codex")
	actorLane := storedLaneForTest("chat", "codex", "actor-thread")
	state.Lanes[actorLane.Identity.ID] = LaneState{
		Identity: actorLane.Identity, Thread: actorLane.Thread, Phase: LaneDetached,
		Coverage: make(map[uint64]CoverageRecord), Context: actorLane.Context,
	}
	original := writeV19Actor(t, path, state, true, nil, []bool{true, true})
	storedLane := actorLane
	storedLane.Thread.RootID = "different-thread"
	storedLane.Thread.HeadID = "different-thread"
	if err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return []StoredLane{storedLane}, nil
	}); err == nil {
		t.Fatal("mismatched exact thread was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed conversion modified the actor file")
	}
}

func TestUpgradeActorStoreV20RejectsNonemptyActorWithoutExactStoredLaneWithoutWriting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state := migratedV19State(t, "chat", "codex")
	original := writeV19Actor(t, path, state, true, nil, []bool{true, true})

	err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return nil, nil
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("no exact stored provider lane")) {
		t.Fatalf("missing exact lane error = %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed conversion modified the actor file")
	}
}

func TestUpgradeActorStoreV20AllowsEmptyActorWithoutProviderLane(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("empty-chat")
	if err != nil {
		t.Fatal(err)
	}
	state.Initialized = true
	state.Presentation = PresentationState{TabID: "tab-empty-chat", ProviderID: "codex"}
	state.Ledger = []LedgerEvent{}
	writeV19Actor(t, path, state, true, nil, nil)

	if err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load("empty-chat")
	if err != nil || !ok {
		t.Fatalf("load empty upgraded actor: ok=%v err=%v", ok, err)
	}
	if loaded.ActiveLaneID != "" || loaded.DesiredLaneID != "" || len(loaded.Lanes) != 0 {
		t.Fatalf("empty actor invented a provider lane: %#v", loaded.Lanes)
	}
}

func TestUpgradeActorStoreV20PreservesEmptyUninitializedActorShell(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("empty-shell")
	if err != nil {
		t.Fatal(err)
	}
	state.Revision = 97
	state.Ledger = []LedgerEvent{}
	writeV19Actor(t, path, state, false, nil, nil)

	if err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := (FileStore{Path: path}).Load("empty-shell")
	if err != nil || !ok {
		t.Fatalf("load upgraded actor shell: ok=%v err=%v", ok, err)
	}
	if loaded.Initialized || loaded.Revision != 97 || len(loaded.Lanes) != 0 || len(loaded.Ledger) != 0 {
		t.Fatalf("uninitialized actor shell changed meaning: %#v", loaded)
	}
}

func TestUpgradeActorStoreV20RejectsUninitializedActorWithDurableState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "provider-chats")
	path := filepath.Join(dir, "actor.json")
	state, err := NewState("invalid-shell")
	if err != nil {
		t.Fatal(err)
	}
	state.Presentation.TabID = "tab-invalid"
	state.Ledger = []LedgerEvent{}
	original := writeV19Actor(t, path, state, false, nil, nil)

	if err := UpgradeActorStoreV20(dir, func(string) ([]StoredLane, error) {
		return nil, nil
	}); err == nil {
		t.Fatal("uninitialized actor with durable presentation state was accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("failed uninitialized conversion modified the actor file")
	}
}
