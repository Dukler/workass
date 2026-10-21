package chat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"workass/internal/provider"
	"workass/internal/store"
)

func openTestActors(t testing.TB) *SQLActors {
	t.Helper()
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), store.FileName), BusyTimeout: 200 * time.Millisecond, BinaryVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLActors(db)
}

func actorFileName(chatID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(chatID)))
	return hex.EncodeToString(digest[:]) + ".json"
}

func stateJSON(t testing.TB, state State) []byte {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// wallClockStamp matches the reducer's own time.Now() stamps (RFC3339Nano
// with a fraction). Fixtures use whole-second stamps, so only clock reads
// made while replaying a journal are masked.
var wallClockStamp = regexp.MustCompile(`"20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]+Z"`)

// assertSameDurableState is the golden equality: the State a file load and a
// SQLite load return must be identical as Go values and as JSON bytes. When
// the load replayed a journal through the reducer, the reducer's wall-clock
// stamps are the one permitted difference (two file loads differ there too).
func assertSameDurableState(t testing.TB, label string, fromFile, fromSQL State, replayed bool) {
	t.Helper()
	fileJSON, sqlJSON := stateJSON(t, fromFile), stateJSON(t, fromSQL)
	if replayed {
		fileJSON = wallClockStamp.ReplaceAll(fileJSON, []byte(`"<clock>"`))
		sqlJSON = wallClockStamp.ReplaceAll(sqlJSON, []byte(`"<clock>"`))
		if !bytes.Equal(fileJSON, sqlJSON) {
			t.Fatalf("%s: SQLite state differs from the file state: %s", label, firstJSONDifference(fileJSON, sqlJSON))
		}
		return
	}
	if !bytes.Equal(fileJSON, sqlJSON) {
		t.Fatalf("%s: SQLite state JSON differs from the file state: %s", label, firstJSONDifference(fileJSON, sqlJSON))
	}
	if !reflect.DeepEqual(fromFile, fromSQL) {
		t.Fatalf("%s: SQLite state differs from the file state as a Go value", label)
	}
}

// teeStore commits every engine write to a FileStore and a SQLStore, each
// through its own journal policy, so both receive byte-identical states.
type teeStore struct {
	file FileStore
	sql  *SQLStore
}

func (s teeStore) Load(chatID string) (State, bool, error) { return s.file.Load(chatID) }
func (s teeStore) Save(state State) error {
	return errors.Join(s.file.Save(state), s.sql.Save(state))
}
func (s teeStore) commitProviderEvent(base uint64, next State, command ProviderEventReceived) error {
	return errors.Join(s.file.commitProviderEvent(base, next, command), s.sql.commitProviderEvent(base, next, command))
}
func (s teeStore) commitCancelCommand(base uint64, next State, command Command) error {
	return errors.Join(s.file.commitCancelCommand(base, next, command), s.sql.commitCancelCommand(base, next, command))
}

func firstJSONDifference(a, b []byte) string {
	index := 0
	for index < len(a) && index < len(b) && a[index] == b[index] {
		index++
	}
	start := index - 120
	if start < 0 {
		start = 0
	}
	end := func(raw []byte) int {
		if index+120 < len(raw) {
			return index + 120
		}
		return len(raw)
	}
	return fmt.Sprintf("offset %d\nA: %s\nB: %s", index, a[start:end(a)], b[start:end(b)])
}

func journalFrameCount(t testing.TB, actorPath string) int {
	t.Helper()
	file, err := os.Open(providerEventJournalPath(actorPath))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	frames, err := newProviderEventJournalFrameReader(file)
	if err != nil || frames == nil {
		return 0
	}
	count := 0
	for {
		payload, err := frames.next()
		if err != nil {
			t.Fatal(err)
		}
		if payload == nil {
			return count
		}
		count++
	}
}

func sqlJournalCount(t testing.TB, actors *SQLActors, chatID string) int {
	t.Helper()
	count := 0
	if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM chat_events WHERE chat_id = ?`, chatID).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// actorScript drives one engine through a turn lifecycle that exercises
// every store path: plain checkpoints, streamed events in the journal, the
// 1 MiB checkpoint threshold, the urgent region (cancel and terminal), a
// queued turn promoted by a terminal and its replay after restart.
type actorStep struct {
	name string
	run  func(t *testing.T, engine *Engine)
}

func parityEvent(lane provider.LaneIdentity, operation provider.OperationID, turn string, sequence uint64, event provider.Event) ProviderEventReceived {
	event.Identity = provider.EventIdentity{
		ChatID: lane.ChatID, LaneID: lane.ID, OperationID: operation, TurnID: turn,
		Sequence: sequence, ObservedAtUnixMS: 1_786_446_000_000 + int64(sequence),
	}
	return ProviderEventReceived{ConnectionGeneration: 1, Event: event}
}

func actorLifecycleScript(chatID string) []actorStep {
	lane := testLane(chatID, "codex")
	var sequence uint64
	event := func(operation provider.OperationID, turn string, value provider.Event) func(t *testing.T, engine *Engine) {
		return func(t *testing.T, engine *Engine) {
			t.Helper()
			sequence++
			if err := engine.Apply(parityEvent(lane, operation, turn, sequence, value)); err != nil {
				t.Fatalf("event %s: %v", value.Kind, err)
			}
		}
	}
	apply := func(command Command) func(t *testing.T, engine *Engine) {
		return func(t *testing.T, engine *Engine) {
			t.Helper()
			if err := engine.Apply(command); err != nil {
				t.Fatalf("%T: %v", command, err)
			}
		}
	}
	claim := func(effectID string) func(t *testing.T, engine *Engine) {
		return func(t *testing.T, engine *Engine) {
			t.Helper()
			if _, ok, err := engine.ClaimEffect(effectID); err != nil || !ok {
				t.Fatalf("claim %s: ok=%v err=%v", effectID, ok, err)
			}
		}
	}
	presentation := PresentationState{TabID: "tab-" + chatID, Title: "Parity <chat> & \"quotes\" 😀"}
	return []actorStep{
		{"initialize", apply(InitializeChat{Presentation: presentation, OperationID: provider.OperationID("chat-create:" + chatID), Digest: "create-digest"})},
		{"open-lane", func(t *testing.T, engine *Engine) {
			sequence = 0
			openReadyDurableLane(t, engine, lane)
		}},
		{"submit", apply(Submit{
			OperationID: "turn-1", Text: "first question <b>bold</b> & ünïcödé 😀",
			Attachments:  []provider.Attachment{{ID: "user-image", Name: "shot.png", MIMEType: "image/png", Ref: "workass-session-image:images/abc"}},
			Presentation: provider.TurnPresentation{UserMessageID: "user-1", AssistantMessageID: "assistant-1", Origin: "human", StartedAt: "2026-10-21T10:00:00Z"},
		})},
		{"claim-turn", claim(startTurnEffectID("turn-1"))},
		{"admit", apply(TurnAdmitted{OperationID: "turn-1", Accepted: true, Turn: provider.TurnRef{OperationID: "turn-1", NativeID: "native-1"}})},
		{"chunk", event("turn-1", "native-1", provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: "streamed ", TypedPhase: true}})},
		{"thinking", event("turn-1", "native-1", provider.Event{Kind: provider.EventThinkingUpdate, Thinking: &provider.ThinkingEvent{Text: "reasoning"}})},
		{"tool-running", event("turn-1", "native-1", provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "tool-1", Title: "Run tests", Status: "running", Command: "go test ./..."}})},
		{"tool-done", event("turn-1", "native-1", provider.Event{Kind: provider.EventToolUpdate, Tool: &provider.ToolEvent{ToolCallID: "tool-1", Status: "completed", Output: "ok\n"}})},
		{"plan", event("turn-1", "native-1", provider.Event{Kind: provider.EventPlanUpdate, Plan: &provider.PlanEvent{Entries: []provider.PlanEntry{{ID: "p1", Text: "Verify", Status: "in_progress"}}}})},
		{"usage", event("turn-1", "native-1", provider.Event{Kind: provider.EventUsageUpdated, Usage: &provider.UsageEvent{Used: 10, Size: 100, InputTokens: 7, OutputTokens: 3}})},
		{"background", event("turn-1", "native-1", provider.Event{Kind: provider.EventBackgroundWork, Background: &provider.BackgroundEvent{WorkID: "work-1", Status: "running", Title: "Background check"}})},
		{"media", event("turn-1", "native-1", provider.Event{Kind: provider.EventAssistantMedia, Media: &provider.AssistantMediaEvent{Attachments: []provider.Attachment{{ID: "assistant-image", MIMEType: "image/png", Ref: "workass-session-image:images/def"}}}})},
		// Crosses the 1 MiB journal budget: both stores must checkpoint here.
		{"huge-chunk", event("turn-1", "native-1", provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseContent, Text: strings.Repeat("large streamed text ", 60_000), TypedPhase: true}})},
		{"after-checkpoint", event("turn-1", "native-1", provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "final answer", TypedPhase: true}})},
		{"queue-second", apply(Submit{
			OperationID: "turn-2", Text: "queued follow-up",
			Presentation: provider.TurnPresentation{UserMessageID: "user-2", AssistantMessageID: "assistant-2", Origin: "human"},
		})},
		{"cancel", apply(CancelTurn{OperationID: "cancel-1"})},
		{"claim-cancel", claim(cancelEffectID("cancel-1"))},
		{"cancel-ack", apply(CancelAcknowledged{OperationID: "cancel-1"})},
		{"terminal", event("turn-1", "native-1", provider.Event{Kind: provider.EventTurnTerminal, Terminal: &provider.TerminalEvent{Status: "cancelled", StopReason: "cancelled", FinishedAt: "2026-10-21T10:01:00Z"}})},
		{"claim-second", claim(startTurnEffectID("turn-2"))},
		{"admit-second", apply(TurnAdmitted{OperationID: "turn-2", Accepted: true, Turn: provider.TurnRef{OperationID: "turn-2", NativeID: "native-2"}})},
		{"second-final", event("turn-2", "native-2", provider.Event{Kind: provider.EventAssistantChunk, Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "second answer", TypedPhase: true}})},
		{"second-terminal", event("turn-2", "native-2", provider.Event{Kind: provider.EventTurnTerminal, Terminal: &provider.TerminalEvent{Status: "completed", StopReason: "end_turn", FinishedAt: "2026-10-21T10:02:00Z"}})},
		{"presentation", func(t *testing.T, engine *Engine) {
			next := engine.Snapshot().Presentation.Clone()
			next.Draft = "draft after turns"
			if err := engine.Apply(SavePresentation{OperationID: "presentation-1", Digest: "presentation-digest", Presentation: next}); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

func TestSQLStoreMatchesFileStoreThroughTheEngineLifecycle(t *testing.T) {
	const chatID = "parity-chat"
	actors := openTestActors(t)
	path := filepath.Join(t.TempDir(), "provider-chats", actorFileName(chatID))
	// One engine, both stores: each store sees the identical commit stream.
	engine, err := NewDurableEngine(chatID, teeStore{file: FileStore{Path: path}, sql: actors.Store(chatID)})
	if err != nil {
		t.Fatal(err)
	}
	sawJournal, sawCheckpointAfterJournal := false, false
	for _, step := range actorLifecycleScript(chatID) {
		step.run(t, engine)
		snapshot := engine.Snapshot()
		ledgerRows := len(snapshot.Ledger)
		fileLoaded, found, err := (FileStore{Path: path}).Load(chatID)
		if err != nil || !found {
			t.Fatalf("%s: file load: found=%v err=%v", step.name, found, err)
		}
		sqlLoaded, found, err := actors.Store(chatID).Load(chatID)
		if err != nil || !found {
			t.Fatalf("%s: sql load: found=%v err=%v", step.name, found, err)
		}
		frames, rows := journalFrameCount(t, path), sqlJournalCount(t, actors, chatID)
		assertSameDurableState(t, step.name, fileLoaded, sqlLoaded, frames > 0 || rows > 0)
		if rows == 0 && !equalCancelDurableState(t, sqlLoaded, snapshot) {
			t.Fatalf("%s: SQLite readback lost a committed boundary", step.name)
		}
		// Same journal budget, except that SQLite checkpoints every record
		// that changes the ledger (the file store journals the terminal).
		if rows > frames {
			t.Fatalf("%s: SQLite journaled %d records, the file store %d", step.name, rows, frames)
		}
		if step.name == "terminal" && (rows != 0 || frames == 0) {
			t.Fatalf("terminal receipt: SQLite journal rows %d (want a checkpoint), file frames %d", rows, frames)
		}
		// The message rows are the committed ledger at every step.
		page, found, err := actors.ReadMessages(chatID, 0, ledgerRows+1)
		if err != nil || !found || int(page.Total) != ledgerRows || len(page.Events) != ledgerRows {
			t.Fatalf("%s: paged rows = %d/%d found=%v err=%v, want %d", step.name, len(page.Events), page.Total, found, err, ledgerRows)
		}
		// Snapshot() is a Clone (nil and empty slices normalized), so compare
		// it clone-normalized; with no file journal the file holds the
		// committed ledger exactly, so compare that byte for byte.
		if ledgerRows > 0 && !reflect.DeepEqual(cloneLedgerEvents(page.Events), snapshot.Ledger) {
			x, _ := json.Marshal(cloneLedgerEvents(page.Events))
			y, _ := json.Marshal(snapshot.Ledger)
			t.Fatalf("%s: message rows differ from the committed ledger: %s", step.name, firstJSONDifference(x, y))
		}
		if frames == 0 && ledgerRows > 0 && !bytes.Equal(stateJSON(t, State{Ledger: page.Events}), stateJSON(t, State{Ledger: fileLoaded.Ledger})) {
			t.Fatalf("%s: message rows are not the file store's ledger bytes", step.name)
		}
		if page.Revision != snapshot.Revision {
			t.Fatalf("%s: page revision %d, actor revision %d", step.name, page.Revision, snapshot.Revision)
		}
		if rows > 0 {
			sawJournal = true
		} else if sawJournal {
			sawCheckpointAfterJournal = true
		}
	}
	if !sawJournal || !sawCheckpointAfterJournal {
		t.Fatalf("script did not exercise both the journal and a checkpoint (journal=%v checkpoint=%v)", sawJournal, sawCheckpointAfterJournal)
	}

	// Restart: a fresh engine over each store recovers the same actor.
	fileRestarted, err := NewDurableEngine(chatID, FileStore{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	sqlRestarted, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	assertSameDurableState(t, "restart", fileRestarted.Snapshot(), sqlRestarted.Snapshot(), true)
}

func TestSQLStoreRecoversTheStateACrashLeavesBehind(t *testing.T) {
	for _, killAfter := range []string{"media", "cancel-ack", "terminal"} {
		t.Run(killAfter, func(t *testing.T) {
			chatID := "crash-chat-" + killAfter
			actors := openTestActors(t)
			path := filepath.Join(t.TempDir(), "provider-chats", actorFileName(chatID))
			engine, err := NewDurableEngine(chatID, teeStore{file: FileStore{Path: path}, sql: actors.Store(chatID)})
			if err != nil {
				t.Fatal(err)
			}
			// Stop with whatever journal the step left: streamed events,
			// cancellation records, or (file store only) the terminal.
			for _, step := range actorLifecycleScript(chatID) {
				step.run(t, engine)
				if step.name == killAfter {
					break
				}
			}
			if killAfter != "terminal" && sqlJournalCount(t, actors, chatID) == 0 {
				t.Fatal("fixture left no SQLite journal to replay")
			}
			fileRestarted, err := NewDurableEngine(chatID, FileStore{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			sqlRestarted, err := NewDurableEngine(chatID, actors.Store(chatID))
			if err != nil {
				t.Fatal(err)
			}
			assertSameDurableState(t, "restart after "+killAfter, fileRestarted.Snapshot(), sqlRestarted.Snapshot(), true)
			if killAfter == "terminal" {
				// The promoted queued turn is reconstructed exactly once.
				pending := 0
				for _, entry := range sqlRestarted.Snapshot().Outbox {
					if entry.ID == startTurnEffectID("turn-2") {
						pending++
					}
				}
				if pending != 1 {
					t.Fatalf("queued turn effect present %d times", pending)
				}
			}
		})
	}
}

// corpusActor is one file of the synthetic provider-chats corpus.
type corpusActor struct {
	chatID string
	shape  string
}

// buildActorCorpus writes, through FileStore and the real reducer, one actor
// per shape the store has to carry. It returns the actors whose files are
// addressable (named by their chat id).
func buildActorCorpus(t *testing.T, dir string) []corpusActor {
	t.Helper()
	var actors []corpusActor
	engineFor := func(chatID string) (*Engine, string) {
		path := filepath.Join(dir, actorFileName(chatID))
		engine, err := NewDurableEngine(chatID, FileStore{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		return engine, path
	}
	runSteps := func(engine *Engine, chatID string, until string) {
		for _, step := range actorLifecycleScript(chatID) {
			step.run(t, engine)
			if step.name == until {
				return
			}
		}
	}

	// A settled chat: two turns, checkpoints, timeline, attachments, unicode.
	engine, _ := engineFor("settled-chat")
	runSteps(engine, "settled-chat", "")
	actors = append(actors, corpusActor{"settled-chat", "settled history"})

	// Mid-stream: the .events journal holds streamed chunks.
	engine, path := engineFor("streaming-chat")
	runSteps(engine, "streaming-chat", "media")
	if journalFrameCount(t, path) == 0 {
		t.Fatal("streaming fixture has no journal")
	}
	actors = append(actors, corpusActor{"streaming-chat", "open journal: streamed events"})

	// Killed after the urgent records (cancel + terminal in the journal).
	engine, path = engineFor("terminal-journal-chat")
	runSteps(engine, "terminal-journal-chat", "terminal")
	if journalFrameCount(t, path) == 0 {
		t.Fatal("terminal fixture has no journal")
	}
	actors = append(actors, corpusActor{"terminal-journal-chat", "open journal: cancel + terminal"})

	// An actor file that never committed InitializeChat.
	engineFor("empty-actor")
	actors = append(actors, corpusActor{"empty-actor", "empty, never initialized"})

	// A durable tombstone.
	engine, _ = engineFor("deleted-chat")
	if err := engine.Apply(InitializeChat{Presentation: PresentationState{TabID: "deleted-tab", Title: "gone"}, OperationID: "chat-create:deleted-chat", Digest: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(DeleteChat{OperationID: "delete-1"}); err != nil {
		t.Fatal(err)
	}
	actors = append(actors, corpusActor{"deleted-chat", "deleted tombstone"})

	// A fork: every row is copied with ContextExcluded.
	source := engineForSnapshot(t, dir, "settled-chat")
	engine, _ = engineFor("fork-chat")
	if err := engine.Apply(InitializeFork{
		Presentation: PresentationState{TabID: "fork-tab", Title: "fork"}, SourceChatID: "settled-chat",
		Messages: source.Ledger, OperationID: "fork-1", Digest: "fork-digest",
	}); err != nil {
		t.Fatal(err)
	}
	actors = append(actors, corpusActor{"fork-chat", "fork with excluded rows"})

	// One multi-megabyte message (inline media in real files).
	engine, _ = engineFor("big-row-chat")
	if err := engine.Apply(InitializeChat{Presentation: PresentationState{TabID: "big-tab"}, OperationID: "chat-create:big-row-chat", Digest: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(SelectLane{Identity: testLane("big-row-chat", "codex")}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Apply(Submit{OperationID: "big", Text: strings.Repeat("data:image/png;base64,iVBORw0KGgo", 120_000), Presentation: provider.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	actors = append(actors, corpusActor{"big-row-chat", "multi-megabyte row"})

	// Hand-written envelopes for shapes the reducer does not produce today.
	writeEnvelope := func(chatID string, mutate func(map[string]any)) {
		state, err := NewState(chatID)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(stateJSON(t, state), &object); err != nil {
			t.Fatal(err)
		}
		mutate(object)
		raw, err := json.Marshal(map[string]any{"v": currentStateEnvelopeVersion, "state": object})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, actorFileName(chatID)), append(raw, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeEnvelope("empty-array-ledger", func(object map[string]any) { object["Ledger"] = []any{} })
	actors = append(actors, corpusActor{"empty-array-ledger", "ledger [] instead of null"})
	writeEnvelope("legacy-field-chat", func(object map[string]any) {
		object["QueueControl"] = map[string]any{"Paused": true, "Revision": 7}
	})
	actors = append(actors, corpusActor{"legacy-field-chat", "removed legacy field"})
	return actors
}

func engineForSnapshot(t *testing.T, dir, chatID string) State {
	t.Helper()
	state, found, err := (FileStore{Path: filepath.Join(dir, actorFileName(chatID))}).Load(chatID)
	if err != nil || !found {
		t.Fatalf("load %s: found=%v err=%v", chatID, found, err)
	}
	return state
}

func corpusImportOptions(stateDir string) ImportOptions {
	return ImportOptions{StateDir: stateDir, ExpectedFileName: actorFileName}
}

// dumpActorTables renders every chat table, ordered, for byte comparison of
// two import runs.
func dumpActorTables(t testing.TB, db *store.DB) string {
	t.Helper()
	var out strings.Builder
	err := db.Read(context.Background(), func(tx *store.Tx) error {
		for _, query := range []string{
			`SELECT chat_id, envelope_version, revision, state_json, ledger_len, journal_bytes FROM chats ORDER BY chat_id`,
			`SELECT chat_id, seq, message_id, role, event_json FROM messages ORDER BY chat_id, seq`,
			`SELECT chat_id, revision, base_revision, record FROM chat_events ORDER BY chat_id, revision`,
			`SELECT store, source, sha256, size, mtime_ns, companion, companion_sha256, item_key, outcome, detail FROM import_receipts ORDER BY store, source`,
		} {
			rows, err := tx.Query(query)
			if err != nil {
				return err
			}
			columns, _ := rows.Columns()
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			for rows.Next() {
				if err := rows.Scan(pointers...); err != nil {
					rows.Close()
					return err
				}
				fmt.Fprintf(&out, "%v\n", values)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestImportRoundTripsEveryCorpusShapeAndIsIdempotent(t *testing.T) {
	stateDir := t.TempDir()
	actorDir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(actorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corpus := buildActorCorpus(t, actorDir)
	// A file not named by its chat id is not addressable today.
	misnamedRaw, err := os.ReadFile(filepath.Join(actorDir, actorFileName("legacy-field-chat")))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actorDir, "misnamed.json"), misnamedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDir(t, actorDir)

	actors := openTestActors(t)
	result, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != len(corpus) || result.Skipped != 1 || !result.Live {
		t.Fatalf("import result = %+v, want %d imported and 1 skipped", result, len(corpus))
	}
	if after := snapshotDir(t, actorDir); !reflect.DeepEqual(before, after) {
		t.Fatal("import modified the provider-chats JSON")
	}

	for _, actor := range corpus {
		fileState, found, err := (FileStore{Path: filepath.Join(actorDir, actorFileName(actor.chatID))}).Load(actor.chatID)
		if err != nil || !found {
			t.Fatalf("%s: file load: found=%v err=%v", actor.shape, found, err)
		}
		sqlState, found, err := actors.Store(actor.chatID).Load(actor.chatID)
		if err != nil || !found {
			t.Fatalf("%s: sql load: found=%v err=%v", actor.shape, found, err)
		}
		assertSameDurableState(t, actor.shape, fileState, sqlState, strings.Contains(actor.shape, "journal"))
	}

	// Discovery: the same accepted set as the file directory, from heads only.
	fileStates, err := DiscoverFileStates(actorDir)
	if err == nil {
		t.Fatal("file discovery accepted the duplicate misnamed actor")
	}
	if err := os.Remove(filepath.Join(actorDir, "misnamed.json")); err != nil {
		t.Fatal(err)
	}
	fileStates, err = DiscoverFileStates(actorDir)
	if err != nil {
		t.Fatal(err)
	}
	heads, err := actors.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != len(fileStates) {
		t.Fatalf("discovered %d heads, %d file actors", len(heads), len(fileStates))
	}
	for index, head := range heads {
		want := fileStates[index]
		if head.State.ChatID != want.ChatID || head.LedgerLength != len(want.Ledger) || head.State.Initialized != want.Initialized || head.State.Deleted != want.Deleted {
			t.Fatalf("head %d = %s/%d, want %s/%d", index, head.State.ChatID, head.LedgerLength, want.ChatID, len(want.Ledger))
		}
	}

	// Idempotent: a second run on the live store changes nothing.
	dump := dumpActorTables(t, actors.db)
	again, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Live || again.Imported != 0 {
		t.Fatalf("second import = %+v", again)
	}
	if dumpActorTables(t, actors.db) != dump {
		t.Fatal("re-running the import changed the database")
	}
}

func TestImportResumesIdempotentlyWhileStillImporting(t *testing.T) {
	stateDir := t.TempDir()
	actorDir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(actorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corpus := buildActorCorpus(t, actorDir)
	actors := openTestActors(t)

	// Fail the third actor's transaction: two receipts commit, the store
	// stays importing.
	failing := corpusImportOptions(stateDir)
	seen := 0
	injected := errors.New("injected import failure")
	failing.afterRows = func(string, int) error {
		seen++
		if seen == 3 {
			return injected
		}
		return nil
	}
	if _, err := ImportFileActors(context.Background(), actors.db, failing); !errors.Is(err, injected) {
		t.Fatalf("injected failure: %v", err)
	}
	var receipts int
	var status store.MigratedStore
	if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
		var err error
		status, _, err = tx.MigratedStoreStatus(ActorStoreName)
		if err != nil {
			return err
		}
		return tx.QueryRow(`SELECT count(*) FROM import_receipts`).Scan(&receipts)
	}); err != nil {
		t.Fatal(err)
	}
	if receipts != 2 || status.Status != store.StoreImporting {
		t.Fatalf("after a failed actor: %d receipts, status %q", receipts, status.Status)
	}
	firstPass := dumpActorTables(t, actors.db)

	// While importing, the JSON is still authoritative: a changed source is
	// re-imported in place, unchanged ones are not touched.
	names := make([]string, 0, len(corpus))
	for _, actor := range corpus {
		names = append(names, actorFileName(actor.chatID))
	}
	sort.Strings(names)
	changedChat := ""
	for _, actor := range corpus {
		if actorFileName(actor.chatID) == names[0] {
			changedChat = actor.chatID
		}
	}
	changed, err := NewDurableEngine(changedChat, FileStore{Path: filepath.Join(actorDir, names[0])})
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Snapshot().Deleted {
		next := changed.Snapshot().Presentation.Clone()
		next.Draft = "edited while importing"
		if err := changed.Apply(SavePresentation{OperationID: "resume-edit", Digest: "resume-edit", Presentation: next}); err != nil {
			t.Fatal(err)
		}
	}

	result, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Live || result.Imported+result.Unchanged != len(corpus) || result.Unchanged < 1 {
		t.Fatalf("resumed import = %+v", result)
	}
	if dumpActorTables(t, actors.db) == firstPass {
		t.Fatal("resume did not import the remaining actors")
	}
	for _, actor := range corpus {
		fileState, _, err := (FileStore{Path: filepath.Join(actorDir, actorFileName(actor.chatID))}).Load(actor.chatID)
		if err != nil {
			t.Fatal(err)
		}
		sqlState, found, err := actors.Store(actor.chatID).Load(actor.chatID)
		if err != nil || !found {
			t.Fatalf("%s after resume: found=%v err=%v", actor.shape, found, err)
		}
		assertSameDurableState(t, actor.shape+" after resume", fileState, sqlState, strings.Contains(actor.shape, "journal"))
	}
}

func TestVerifyLiveFileActorsDetectsJSONWrittenAfterTheCutover(t *testing.T) {
	stateDir := t.TempDir()
	actorDir := filepath.Join(stateDir, "provider-chats")
	if err := os.MkdirAll(actorDir, 0o700); err != nil {
		t.Fatal(err)
	}
	engine, err := NewDurableEngine("cutover-chat", FileStore{Path: filepath.Join(actorDir, actorFileName("cutover-chat"))})
	if err != nil {
		t.Fatal(err)
	}
	actors := openTestActors(t)
	if _, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLiveFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err != nil {
		t.Fatalf("fresh cutover reported stale: %v", err)
	}
	// An old build keeps writing the JSON after the cutover.
	if err := engine.Apply(InitializeChat{Presentation: PresentationState{TabID: "late"}, OperationID: "chat-create:cutover-chat", Digest: "late"}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLiveFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); !errors.Is(err, ErrFileActorsChangedAfterCutover) {
		t.Fatalf("changed JSON after the cutover: %v", err)
	}
	if _, err := NewDurableEngine("new-after-cutover", FileStore{Path: filepath.Join(actorDir, actorFileName("new-after-cutover"))}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLiveFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); !errors.Is(err, ErrFileActorsChangedAfterCutover) {
		t.Fatalf("new JSON after the cutover: %v", err)
	}
}

func TestImportFailsClosedLikeFileDiscovery(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"unsupported version": func(t *testing.T, dir string) {
			state, _ := NewState("old-version")
			raw, _ := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion - 1, State: state})
			if err := os.WriteFile(filepath.Join(dir, actorFileName("old-version")), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"truncated file": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, actorFileName("truncated")), []byte(`{"v":22,"state":{"ChatID":"truncated","Ledger":[{"EventID":"x"`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"trailing data": func(t *testing.T, dir string) {
			state, _ := NewState("trailing")
			raw, _ := json.Marshal(stateEnvelope{Version: currentStateEnvelopeVersion, State: state})
			if err := os.WriteFile(filepath.Join(dir, actorFileName("trailing")), append(raw, []byte(" {}")...), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"no chat id": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "anonymous.json"), []byte(`{"v":22,"state":{"Revision":1}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, write := range cases {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			dir := filepath.Join(stateDir, "provider-chats")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, dir)
			if _, err := DiscoverFileStates(dir); err == nil {
				t.Fatal("file discovery accepted the fixture")
			}
			actors := openTestActors(t)
			if _, err := ImportFileActors(context.Background(), actors.db, corpusImportOptions(stateDir)); err == nil {
				t.Fatal("import accepted what file discovery refuses")
			}
			var chats int
			if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
				return tx.QueryRow(`SELECT count(*) FROM chats`).Scan(&chats)
			}); err != nil || chats != 0 {
				t.Fatalf("failed import left %d chats (%v)", chats, err)
			}
		})
	}
}

func TestSQLStoreLoadFailsClosedOnDamagedRows(t *testing.T) {
	const chatID = "damaged-chat"
	actors := openTestActors(t)
	engine, err := NewDurableEngine(chatID, actors.Store(chatID))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range actorLifecycleScript(chatID) {
		step.run(t, engine)
	}
	if len(engine.Snapshot().Ledger) == 0 {
		t.Fatal("fixture has no ledger rows to damage")
	}
	damage := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if err := actors.db.Write(context.Background(), func(tx *store.Tx) error {
			_, err := tx.Exec(statement, args...)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(t *testing.T) {
		t.Helper()
		if err := actors.db.Write(context.Background(), func(tx *store.Tx) error {
			_, err := tx.Exec(`DELETE FROM chats`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := actors.Store(chatID).Save(engine.Snapshot()); err != nil {
			t.Fatal(err)
		}
		if loaded, found, err := actors.Store(chatID).Load(chatID); err != nil || !found || !equalCancelDurableState(t, loaded, engine.Snapshot()) {
			t.Fatalf("restore did not reproduce the actor: found=%v err=%v", found, err)
		}
	}
	for name, statement := range map[string]string{
		"missing ledger row":    `DELETE FROM messages WHERE chat_id = ? AND seq = 1`,
		"newer envelope":        `UPDATE chats SET envelope_version = envelope_version + 1 WHERE chat_id = ?`,
		"revision disagreement": `UPDATE chats SET revision = revision + 5 WHERE chat_id = ?`,
		"corrupt row json":      `UPDATE messages SET event_json = '{' WHERE chat_id = ? AND seq = 1`,
	} {
		t.Run(name, func(t *testing.T) {
			damage(t, statement, chatID)
			if _, _, err := actors.Store(chatID).Load(chatID); err == nil {
				t.Fatal("damaged actor loaded")
			}
			restore(t)
		})
	}
	if _, _, err := actors.Store("other-chat").Load(chatID); err == nil {
		t.Fatal("a store loaded another chat's actor")
	}
}

func snapshotDir(t testing.TB, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		digest, _, err := fileSHA256(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = fmt.Sprintf("%s %d %d", digest, info.Size(), info.ModTime().UnixNano())
	}
	return out
}
