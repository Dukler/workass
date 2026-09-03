package chat

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"workass/internal/provider"
)

type failingProviderEventStore struct {
	err             error
	providerCommits int
	snapshotCommits int
}

func (s *failingProviderEventStore) Load(string) (State, bool, error) {
	return State{}, false, nil
}

func (s *failingProviderEventStore) Save(State) error {
	s.snapshotCommits++
	return nil
}

func (s *failingProviderEventStore) commitProviderEvent(uint64, State, ProviderEventReceived) error {
	s.providerCommits++
	return s.err
}

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

func largeJournalReadyState(t *testing.T) (State, provider.LaneIdentity) {
	t.Helper()
	engine, lane := newJournalReadyEngine(t, filepath.Join(t.TempDir(), "provider-chats", "actor.json"))
	state := engine.Snapshot()
	const rows = 4_096
	for len(state.Ledger) < rows {
		sequence := len(state.Ledger) + 1
		state.Ledger = append(state.Ledger, LedgerEvent{
			EventID: fmt.Sprintf("history-event-%d", sequence), MessageID: fmt.Sprintf("history-message-%d", sequence),
			Sequence: uint64(sequence), Role: "assistant", Status: "done",
			OperationID: provider.OperationID(fmt.Sprintf("history-operation-%d", sequence)), ContextExcluded: true,
			Timeline: []TimelineEntry{},
		})
	}
	if err := state.Validate(); err != nil {
		t.Fatalf("large journal-ready state: %v", err)
	}
	return state, lane
}

func TestJournaledProviderEventsMatchReducerWithoutCloningHistory(t *testing.T) {
	base, lane := largeJournalReadyState(t)
	events := []provider.Event{
		{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: "streamed content", TypedPhase: true}},
		{Kind: provider.EventAssistantMedia, Media: &provider.AssistantMediaEvent{Attachments: []provider.Attachment{{ID: "assistant-image", MIMEType: "image/png", Ref: "workass-session-image:assistant-image"}}}},
		{Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: "bounded reasoning"}},
		{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "tool-1", Title: "Run tests", Status: "running"}},
		{Kind: provider.EventPlanUpdate, Plan: &provider.PlanEvent{Entries: []provider.PlanEntry{{ID: "one", Text: "Verify", Status: "in_progress"}}}},
		{Kind: provider.EventUsageUpdated, Usage: &provider.UsageEvent{Used: 10, Size: 100, InputTokens: 7, OutputTokens: 3}},
		{Kind: provider.EventCompactionStarted, Compaction: &provider.CompactionEvent{Coverage: 1}},
		{Kind: provider.EventCompactionCheckpoint, Compaction: &provider.CompactionEvent{CheckpointID: "checkpoint", Coverage: 1, Digest: "digest"}},
		{Kind: provider.EventBackgroundWork, Background: &provider.BackgroundEvent{WorkID: "work-1", Status: "running", Title: "Background check"}},
	}
	for _, event := range events {
		event := event
		t.Run(string(event.Kind), func(t *testing.T) {
			event.Identity = provider.EventIdentity{
				ChatID: "journal-chat", LaneID: lane.ID, OperationID: "journal-turn",
				TurnID: "journal-native-turn", Sequence: 1, ObservedAtUnixMS: 1_786_446_010_001,
			}
			command := ProviderEventReceived{ConnectionGeneration: 1, Event: event}
			expected, effects, err := Reduce(base, command)
			if err != nil {
				t.Fatalf("reference reducer: %v", err)
			}
			if len(effects) != 0 {
				t.Fatalf("journalable event produced effects: %#v", effects)
			}
			before, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := reduceJournaledProviderEvent(base, command)
			if err != nil {
				t.Fatalf("journaled reducer: %v", err)
			}
			after, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("journaled reducer mutated the installed actor state")
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatal("journaled reducer diverged from canonical reducer")
			}
			if err := actual.Validate(); err != nil {
				t.Fatalf("journaled transition produced invalid state: %v", err)
			}
			if &actual.Ledger[0] != &base.Ledger[0] {
				t.Fatal("journaled provider event deep-cloned immutable history")
			}
		})
	}
}

func TestProviderEventJournalFailureDoesNotPublishCopyOnWriteState(t *testing.T) {
	base, lane := largeJournalReadyState(t)
	wantErr := errors.New("injected provider journal failure")
	store := &failingProviderEventStore{err: wantErr}
	engine := &Engine{state: base, store: store}
	before, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(journalAssistantEvent(lane, 1, "must remain unpublished")); !errors.Is(err, wantErr) {
		t.Fatalf("provider journal failure = %v, want %v", err, wantErr)
	}
	after, err := json.Marshal(engine.state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed provider journal append changed installed actor state")
	}
	if store.providerCommits != 1 || store.snapshotCommits != 0 {
		t.Fatalf("provider commits=%d snapshot commits=%d", store.providerCommits, store.snapshotCommits)
	}
}

func TestJournaledSideActivityRejectsUnownedHistoryWithoutMutation(t *testing.T) {
	base, lane := largeJournalReadyState(t)
	before, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []provider.Event{
		{Kind: provider.EventCompactionCheckpoint, Compaction: &provider.CompactionEvent{CheckpointID: "wrong-owner", Coverage: 1, Digest: "digest"}},
		{Kind: provider.EventBackgroundWork, Background: &provider.BackgroundEvent{WorkID: "wrong-owner", Status: "running"}},
	} {
		event.Identity = provider.EventIdentity{
			ChatID: "journal-chat", LaneID: lane.ID, OperationID: "not-a-real-operation",
			TurnID: "not-a-real-turn", Sequence: 1, ObservedAtUnixMS: 1_786_446_010_001,
		}
		if _, err := reduceJournaledProviderEvent(base, ProviderEventReceived{ConnectionGeneration: 1, Event: event}); err == nil {
			t.Fatalf("unowned %s event was accepted", event.Kind)
		}
	}
	after, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected side activity mutated the installed actor state")
	}
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
	if restartedState.Foreground != nil || len(restartedState.Ledger) < 2 || restartedState.Ledger[len(restartedState.Ledger)-1].Result != "streamed final" {
		t.Fatalf("restart did not terminalize the turn with its journaled provider output: %#v", restartedState)
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
