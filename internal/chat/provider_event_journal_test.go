package chat

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workass/internal/provider"
)

func newJournalReadyEngine(t *testing.T, path string) (*Engine, provider.LaneIdentity) {
	t.Helper()
	engine, err := NewDurableEngine("journal-chat", FileStore{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	lane := testLane("journal-chat", "codex")
	openReadyDurableLane(t, engine, lane)
	if err := engine.Apply(Submit{
		OperationID: "journal-turn", Text: "question",
		Presentation: provider.TurnPresentation{
			UserMessageID: "journal-user", AssistantMessageID: "journal-assistant", Origin: "human",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := engine.ClaimEffect(startTurnEffectID("journal-turn")); err != nil || !ok {
		t.Fatalf("claim start turn: ok=%v err=%v", ok, err)
	}
	if err := engine.Apply(TurnAdmitted{
		OperationID: "journal-turn", Accepted: true,
		Turn: provider.TurnRef{OperationID: "journal-turn", NativeID: "journal-native-turn"},
	}); err != nil {
		t.Fatal(err)
	}
	return engine, lane
}

func journalAssistantEvent(lane provider.LaneIdentity, sequence uint64, text string) ProviderEventReceived {
	return ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind: provider.EventAssistantChunk,
		Identity: provider.EventIdentity{
			ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn",
			TurnID: "journal-native-turn", Sequence: sequence, ObservedAtUnixMS: int64(1000 + sequence),
		},
		Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: text, TypedPhase: true},
	}}
}

func TestProviderEventJournalCommitsChunkWithoutRewritingActorSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := engine.Snapshot().Revision
	if err := engine.Apply(journalAssistantEvent(lane, 1, "streamed final")); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("streamed provider chunk rewrote the whole actor snapshot")
	}
	journalInfo, err := os.Stat(providerEventJournalPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if journalInfo.Mode().Perm() != 0o600 || journalInfo.Size() <= int64(len(providerEventJournalMagic)) {
		t.Fatalf("provider event journal metadata = mode %o size %d", journalInfo.Mode().Perm(), journalInfo.Size())
	}
	loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
	if err != nil || !found {
		t.Fatalf("load journaled actor: found=%v err=%v", found, err)
	}
	if loaded.Revision != baseRevision+1 || loaded.Foreground == nil || loaded.Foreground.AssistantResult != "streamed final" {
		t.Fatalf("journaled actor state = revision %d foreground %#v", loaded.Revision, loaded.Foreground)
	}
	restarted, err := NewDurableEngine("journal-chat", FileStore{Path: path})
	if err != nil {
		t.Fatalf("restart journaled actor: %v", err)
	}
	restartedState := restarted.Snapshot()
	if restartedState.Foreground == nil || restartedState.Foreground.AssistantResult != "streamed final" {
		t.Fatalf("restart lost journaled provider output: %#v", restartedState.Foreground)
	}
	if _, err := os.Stat(providerEventJournalPath(path)); !os.IsNotExist(err) {
		t.Fatalf("restart checkpoint left provider event journal behind: %v", err)
	}
}

func TestProviderEventJournalIgnoresOnlyTornTrailingFrame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	if err := engine.Apply(journalAssistantEvent(lane, 1, "durable")); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(providerEventJournalPath(path), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	var partial [7]byte
	binary.BigEndian.PutUint32(partial[:4], 128)
	copy(partial[4:], []byte("bad"))
	if _, err := journal.Write(partial[:]); err != nil {
		journal.Close()
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
	if err != nil || !found {
		t.Fatalf("load actor with torn tail: found=%v err=%v", found, err)
	}
	if loaded.Foreground == nil || loaded.Foreground.AssistantResult != "durable" {
		t.Fatalf("torn tail lost acknowledged event: %#v", loaded.Foreground)
	}
}

func TestProviderEventJournalRejectsCompleteCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	if err := engine.Apply(journalAssistantEvent(lane, 1, "durable")); err != nil {
		t.Fatal(err)
	}
	record := providerEventJournalRecord{
		Version: providerEventJournalVersion, BaseRevision: engine.Snapshot().Revision,
		Revision: engine.Snapshot().Revision + 1, Command: journalAssistantEvent(lane, 2, "corrupt"),
	}
	frame, err := encodeProviderEventJournalFrame(record)
	if err != nil {
		t.Fatal(err)
	}
	frame[len(frame)-1] ^= 0xff
	journal, err := os.OpenFile(providerEventJournalPath(path), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Write(frame); err != nil {
		journal.Close()
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := (FileStore{Path: path}).Load("journal-chat"); err == nil || found || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("corrupt provider event journal was accepted: found=%v err=%v", found, err)
	}
}

func TestProviderEventJournalContinuesAfterCheckpointLeavesStaleRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	if err := engine.Apply(journalAssistantEvent(lane, 1, "first")); err != nil {
		t.Fatal(err)
	}
	checkpoint := engine.Snapshot()
	// This is the safe crash boundary between the authoritative snapshot rename
	// and journal cleanup. Replaying must skip the already-checkpointed record,
	// while a later append remains contiguous from the snapshot revision.
	if err := saveStateEnvelope(path, checkpoint, currentStateEnvelopeVersion); err != nil {
		t.Fatal(err)
	}
	reopened := &Engine{state: checkpoint, store: FileStore{Path: path}}
	if err := reopened.Apply(journalAssistantEvent(lane, 2, " second")); err != nil {
		t.Fatal(err)
	}
	loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
	if err != nil || !found {
		t.Fatalf("load actor across stale journal checkpoint: found=%v err=%v", found, err)
	}
	if loaded.Foreground == nil || loaded.Foreground.AssistantResult != "first second" {
		t.Fatalf("stale journal replay duplicated or lost output: %#v", loaded.Foreground)
	}
}

func TestOversizedProviderEventFallsBackToBoundedActorCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	text := strings.Repeat("x", providerEventJournalCheckpointBytes)
	if err := engine.Apply(journalAssistantEvent(lane, 1, text)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(providerEventJournalPath(path)); !os.IsNotExist(err) {
		t.Fatalf("oversized provider event bypassed bounded checkpoint: %v", err)
	}
	loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
	if err != nil || !found {
		t.Fatalf("load oversized checkpoint: found=%v err=%v", found, err)
	}
	if loaded.Foreground == nil || loaded.Foreground.AssistantResult != text {
		t.Fatal("oversized checkpoint lost provider output")
	}
}

func TestTerminalProviderEventCheckpointsAndClearsJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-chats", "actor.json")
	engine, lane := newJournalReadyEngine(t, path)
	if err := engine.Apply(journalAssistantEvent(lane, 1, "complete answer")); err != nil {
		t.Fatal(err)
	}
	terminal := ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind: provider.EventTurnTerminal,
		Identity: provider.EventIdentity{
			ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn",
			TurnID: "journal-native-turn", Sequence: 2, ObservedAtUnixMS: 2000,
		},
		Terminal: &provider.TerminalEvent{Status: "completed", FinishedAt: "2026-09-02T22:00:00Z"},
	}}
	if err := engine.Apply(terminal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(providerEventJournalPath(path)); !os.IsNotExist(err) {
		t.Fatalf("terminal checkpoint left provider event journal behind: %v", err)
	}
	loaded, found, err := (FileStore{Path: path}).Load("journal-chat")
	if err != nil || !found {
		t.Fatalf("load terminal actor: found=%v err=%v", found, err)
	}
	if loaded.Foreground != nil || len(loaded.Ledger) < 2 || loaded.Ledger[len(loaded.Ledger)-1].Result != "complete answer" {
		t.Fatalf("terminal checkpoint lost streamed answer: foreground=%#v ledger=%#v", loaded.Foreground, loaded.Ledger)
	}
}
