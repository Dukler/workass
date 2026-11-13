package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"workass/internal/provider"
	"workass/internal/store"
)

// controlChatID is the chat every control-journal fixture uses.
const controlChatID = "control-chat"

// controlSendStep is one durable boundary of a send: the commands job:start
// and the coordinator commit before the prompt reaches the provider.
type controlSendStep struct {
	name string
	run  func(t testing.TB, engine *Engine)
}

// controlFixture opens an actor with history rows and a ready lane. Every
// command so far is a checkpoint (none is a send-path command).
func controlFixture(t testing.TB, store StateStore) (*Engine, provider.LaneIdentity) {
	t.Helper()
	engine, err := NewDurableEngine(controlChatID, store)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]LedgerEvent, 24)
	for index := range rows {
		rows[index] = LedgerEvent{
			EventID: fmt.Sprintf("history-%d", index), MessageID: fmt.Sprintf("message-%d", index),
			OperationID: provider.OperationID(fmt.Sprintf("operation-%d", index)), Role: "assistant",
			Text: strings.Repeat("history body ", 200), Status: "done",
		}
	}
	if err := engine.Apply(InitializeFork{Presentation: PresentationState{TabID: "control-tab"}, SourceChatID: "control-source", OperationID: "fixture", Digest: "fixture", Messages: rows}); err != nil {
		t.Fatal(err)
	}
	lane := testLane(controlChatID, "mock")
	openReadyDurableLane(t, engine, lane)
	return engine, lane
}

// controlSendSteps are the four commands of one send, as the runtime issues
// them (provider_chat_runtime.go selectTurnLocked, admitPreparedLocked; the
// coordinator's claim and CommitAdmission).
func controlSendSteps(lane provider.LaneIdentity, operation string) []controlSendStep {
	operationID := provider.OperationID(operation)
	return []controlSendStep{
		{"select", func(t testing.TB, engine *Engine) {
			state := engine.ReadSnapshot()
			if err := engine.Apply(CommitLaneSelection{
				OperationID: operationID, Digest: "digest-" + operation, Identity: lane,
				Owner: provider.AttachmentOwner{TabID: "control-tab"}, CWD: "/work", ModelID: "model", ModeID: "mode",
				Context: exactContext(provider.ContextImportNonSampling),
				Update: UpdateRuntimeControls{
					ProviderID: lane.Realm.ProviderID, ModelID: "model", ModeID: "mode", ReplaceModelID: true, ReplaceModeID: true,
					ExpectedRevision: state.Presentation.RuntimeControlRevision, RequireRevision: true,
				},
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"submit", func(t testing.TB, engine *Engine) {
			prepared := false
			if err := engine.ApplyPrepared(Submit{
				OperationID: operationID, LaneID: lane.ID, Text: "question " + operation, ModelID: "model", ModeID: "mode",
				Presentation: provider.TurnPresentation{
					UserMessageID: "user-" + operation, AssistantMessageID: "assistant-" + operation, Origin: "human",
					StartedAt: "2026-11-12T20:00:00.123Z",
				},
			}, func() error { prepared = true; return nil }); err != nil || !prepared {
				t.Fatalf("submit: prepared=%v err=%v", prepared, err)
			}
		}},
		{"claim", func(t testing.TB, engine *Engine) {
			effect, ok, err := engine.ClaimNext()
			if _, start := effect.(StartTurnEffect); err != nil || !ok || !start {
				t.Fatalf("claim start: %T ok=%v err=%v", effect, ok, err)
			}
		}},
		{"admitted", func(t testing.TB, engine *Engine) {
			if err := engine.Apply(TurnAdmitted{
				OperationID: operationID, Accepted: true,
				Turn: provider.TurnRef{OperationID: operationID, NativeID: "native-" + operation},
			}); err != nil {
				t.Fatal(err)
			}
		}},
	}
}

// controlTerminal ends the running turn the way the provider does; it is an
// ordinary checkpoint (it adds ledger rows).
func controlTerminal(t testing.TB, engine *Engine, lane provider.LaneIdentity, operation string, sequence uint64) {
	t.Helper()
	operationID := provider.OperationID(operation)
	identity := provider.EventIdentity{ChatID: controlChatID, LaneID: lane.ID, OperationID: operationID, TurnID: "native-" + operation}
	identity.Sequence = sequence
	if err := engine.Apply(ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind: provider.EventAssistantChunk, Identity: identity,
		Assistant: &provider.AssistantEvent{Phase: provider.AssistantPhaseFinal, Text: "answer " + operation, TypedPhase: true},
	}}); err != nil {
		t.Fatal(err)
	}
	identity.Sequence = sequence + 1
	if err := engine.Apply(ProviderEventReceived{ConnectionGeneration: 1, Event: provider.Event{
		Kind: provider.EventTurnTerminal, Identity: identity,
		Terminal: &provider.TerminalEvent{Status: "completed", StopReason: "end_turn", FinishedAt: "2026-11-12T20:00:01Z"},
	}}); err != nil {
		t.Fatal(err)
	}
}

// checkpointOnlyStore is a FileStore with no journal: every command is a
// checkpoint, as every send-path command was before the control journal.
type checkpointOnlyStore struct{ path string }

func (s checkpointOnlyStore) Load(chatID string) (State, bool, error) {
	return FileStore{Path: s.path}.Load(chatID)
}
func (s checkpointOnlyStore) Save(state State) error { return FileStore{Path: s.path}.Save(state) }

// recoverAs is what a restart makes of the durable state: a new engine over
// it (its RecoverOutbox included).
func recoverAs(t testing.TB, state State) State {
	t.Helper()
	engine, err := NewDurableEngine(state.ChatID, &memoryStateStore{state: state.Clone(), ok: true})
	if err != nil {
		t.Fatal(err)
	}
	return engine.Snapshot()
}

func assertSameRecovery(t testing.TB, label string, want, got State) {
	t.Helper()
	assertSameDurableState(t, label, recoverAs(t, want), recoverAs(t, got), true)
}

func assertSameStateJSON(t *testing.T, label string, want, got State) {
	t.Helper()
	if !equalCancelDurableState(t, want, got) {
		x, _ := json.Marshal(want.Clone())
		y, _ := json.Marshal(got.Clone())
		t.Fatalf("%s: %s", label, firstJSONDifference(x, y))
	}
}

// Every send-path boundary journals instead of rewriting the actor, and what
// a reload (a restart, a crash) recovers from the journal is exactly what it
// recovers from a checkpoint of the same command, as before the journal.
func TestControlJournalKeepsEverySendBoundaryWithoutRewritingTheActor(t *testing.T) {
	for _, backend := range []string{"sqlite", "file"} {
		t.Run(backend, func(t *testing.T) {
			var (
				actors  *SQLActors
				path    string
				durable StateStore
				fresh   func() StateStore
			)
			if backend == "sqlite" {
				actors = openTestActors(t)
				durable = actors.Store(controlChatID)
				fresh = func() StateStore { return actors.Store(controlChatID) }
			} else {
				path = filepath.Join(t.TempDir(), "actor.json")
				durable = FileStore{Path: path}
				fresh = func() StateStore { return FileStore{Path: path} }
			}
			// The reference commits the same commands, every one a checkpoint.
			reference := checkpointOnlyStore{path: filepath.Join(t.TempDir(), "reference.json")}
			engine, lane := controlFixture(t, durable)
			referenceEngine, _ := controlFixture(t, reference)
			checkpoint := func() []byte {
				if backend == "sqlite" {
					var head []byte
					if err := actors.db.Read(context.Background(), func(tx *store.Tx) error {
						return tx.QueryRow(`SELECT state_json FROM chats WHERE chat_id = ?`, controlChatID).Scan(&head)
					}); err != nil {
						t.Fatal(err)
					}
					return head
				}
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			journal := func() int {
				if backend == "sqlite" {
					return sqlJournalCount(t, actors, controlChatID)
				}
				return journalFrameCount(t, path)
			}
			for turn := 1; turn <= 2; turn++ {
				operation := fmt.Sprintf("send-%d", turn)
				before, records := checkpoint(), journal()
				for _, step := range controlSendSteps(lane, operation) {
					step.run(t, engine)
					step.run(t, referenceEngine)
					records++
					if got := journal(); got != records {
						t.Fatalf("%s %s: journal has %d records, want %d", operation, step.name, got, records)
					}
					if string(checkpoint()) != string(before) {
						t.Fatalf("%s %s rewrote the actor checkpoint", operation, step.name)
					}
					loaded, found, err := fresh().Load(controlChatID)
					if err != nil || !found {
						t.Fatalf("%s %s: reload found=%v err=%v", operation, step.name, found, err)
					}
					referenceLoaded, _, err := reference.Load(controlChatID)
					if err != nil {
						t.Fatal(err)
					}
					assertSameStateJSON(t, operation+" "+step.name+" reload", engine.Snapshot(), loaded)
					assertSameStateJSON(t, operation+" "+step.name+" against a checkpoint", referenceLoaded, loaded)
					assertSameRecovery(t, operation+" "+step.name+" recovery", referenceLoaded, loaded)
				}
				controlTerminal(t, engine, lane, operation, uint64(2*turn-1))
				controlTerminal(t, referenceEngine, lane, operation, uint64(2*turn-1))
			}
			// A restarted engine folds the journal into one checkpoint and
			// carries on from the same state.
			reopened, err := NewDurableEngine(controlChatID, fresh())
			if err != nil {
				t.Fatal(err)
			}
			referenceLoaded, _, err := reference.Load(controlChatID)
			if err != nil {
				t.Fatal(err)
			}
			assertSameRecovery(t, "reopened", referenceLoaded, reopened.Snapshot())
		})
	}
}

// What cannot replay exactly, or must keep the SQLite rows equal to the
// actor, is a checkpoint as before.
func TestControlJournalFallsBackToACheckpoint(t *testing.T) {
	for _, scenario := range []string{"unstamped-submit", "ambiguous-admission", "lossy-command", "ineligible-claim"} {
		t.Run(scenario, func(t *testing.T) {
			actors := openTestActors(t)
			engine, lane := controlFixture(t, actors.Store(controlChatID))
			steps := controlSendSteps(lane, "fallback")
			var err error
			switch scenario {
			case "unstamped-submit":
				// The reducer stamps the obligation with the clock: a replay
				// would not rebuild the acknowledged state.
				steps[0].run(t, engine)
				err = engine.Apply(Submit{OperationID: "fallback", LaneID: lane.ID, Text: "question",
					Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant", Origin: "human"}})
			case "ambiguous-admission":
				for _, step := range steps[:3] {
					step.run(t, engine)
				}
				err = engine.Apply(TurnAdmitted{OperationID: "fallback", Ambiguous: true,
					Turn: provider.TurnRef{OperationID: "fallback", NativeID: "native-fallback"}})
			case "lossy-command":
				// RawMessage("null") decodes as nil: the record would not
				// replay this exact command.
				state := engine.ReadSnapshot()
				err = engine.Apply(CommitLaneSelection{
					OperationID: "fallback", Digest: "digest", Identity: lane, Owner: provider.AttachmentOwner{TabID: "control-tab"},
					Update: UpdateRuntimeControls{ProviderID: lane.Realm.ProviderID, ModelControls: json.RawMessage("null"),
						ExpectedRevision: state.Presentation.RuntimeControlRevision, RequireRevision: true},
				})
			case "ineligible-claim":
				// A lane creation claim is not a send-path command.
				other := testLane(controlChatID, "other")
				if err := engine.Apply(SelectLane{Identity: other}); err != nil {
					t.Fatal(err)
				}
				_, _, err = engine.ClaimNext()
			}
			if err != nil {
				t.Fatal(err)
			}
			records := sqlJournalCount(t, actors, controlChatID)
			if scenario == "unstamped-submit" || scenario == "ambiguous-admission" {
				// The commands before it journaled; this one checkpointed,
				// which empties the journal.
				if records != 0 {
					t.Fatalf("journal has %d records after the checkpointed command", records)
				}
			} else if records != 0 {
				t.Fatalf("journal has %d records, want a checkpoint", records)
			}
			loaded, _, err := actors.Store(controlChatID).Load(controlChatID)
			if err != nil {
				t.Fatal(err)
			}
			assertSameStateJSON(t, scenario, engine.Snapshot(), loaded)
		})
	}
}

// A journal record is replayed only for its own chat and only when it is a
// command the journal may carry.
func TestControlJournalReplayRejectsForeignOrIneligibleRecords(t *testing.T) {
	for _, scenario := range []string{"wrong-chat", "unstamped-submit", "two-commands"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "actor.json")
			engine, lane := controlFixture(t, FileStore{Path: path})
			command := &controlJournalCommand{ChatID: "other-chat", Claim: &ClaimEffect{EffectID: startTurnEffectID("x")}}
			switch scenario {
			case "unstamped-submit":
				command = &controlJournalCommand{ChatID: controlChatID, Submit: &Submit{OperationID: "x", LaneID: lane.ID, Text: "question",
					Presentation: provider.TurnPresentation{UserMessageID: "user", AssistantMessageID: "assistant"}}}
			case "two-commands":
				command = &controlJournalCommand{ChatID: controlChatID, Claim: &ClaimEffect{EffectID: "x"}, Admitted: &TurnAdmitted{OperationID: "x", Accepted: true}}
			}
			record := providerEventJournalRecord{Version: controlJournalVersion, BaseRevision: engine.state.Revision, Revision: engine.state.Revision + 1, Control: command}
			frame, err := encodeProviderEventJournalFrame(record)
			if scenario == "two-commands" {
				if err == nil {
					t.Fatal("a record with two commands was encoded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appendProviderEventJournalFrame(path, frame, false); err != nil {
				t.Fatal(err)
			}
			if _, _, err := (FileStore{Path: path}).Load(controlChatID); err == nil {
				t.Fatal("an invalid control journal record was replayed")
			}
		})
	}
}

const (
	controlKillStateEnv = "WORKASS_TEST_CONTROL_KILL_STATE"
	controlKillAtEnv    = "WORKASS_TEST_CONTROL_KILL_AT"
)

// TestControlJournalKillHelper is the child of
// TestControlJournalSurvivesAKillAtEverySendBoundary: it commits a send up to
// one boundary on a SQLite actor and kills itself (SIGKILL: no close, no
// deferred code, no checkpoint on the way out).
func TestControlJournalKillHelper(t *testing.T) {
	stateDir := os.Getenv(controlKillStateEnv)
	if stateDir == "" {
		t.Skip("child process of the kill-at-send-boundary test")
	}
	killAt, _ := strconv.Atoi(os.Getenv(controlKillAtEnv))
	db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "kill-child"})
	if err != nil {
		t.Fatal(err)
	}
	engine, lane := controlFixture(t, NewSQLActors(db).Store(controlChatID))
	for index, step := range controlSendSteps(lane, "killed-send") {
		step.run(t, engine)
		if index == killAt {
			self, _ := os.FindProcess(os.Getpid())
			_ = self.Kill()
			time.Sleep(time.Minute)
		}
	}
	t.Fatal("child finished the send without being killed")
}

// A send acknowledged at any boundary survives kill -9 exactly as it did when
// every boundary was a checkpoint: the restarted actor equals the one a
// restart makes of the checkpointed state at the same boundary.
func TestControlJournalSurvivesAKillAtEverySendBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for killAt, step := range controlSendSteps(provider.LaneIdentity{}, "killed-send") {
		t.Run(step.name, func(t *testing.T) {
			stateDir := t.TempDir()
			child := exec.Command(executable, "-test.run", "^TestControlJournalKillHelper$", "-test.count=1")
			child.Env = append(os.Environ(), controlKillStateEnv+"="+stateDir, controlKillAtEnv+"="+strconv.Itoa(killAt))
			output, err := child.CombinedOutput()
			exit, killed := err.(*exec.ExitError)
			if !killed || exit.ExitCode() != -1 {
				t.Fatalf("child was not killed at %s: %v\n%s", step.name, err, output)
			}
			db, err := store.Open(store.Options{Path: filepath.Join(stateDir, store.FileName), BinaryVersion: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			actors := NewSQLActors(db)
			if records := sqlJournalCount(t, actors, controlChatID); records != killAt+1 {
				t.Fatalf("killed after %s with %d journal records, want %d", step.name, records, killAt+1)
			}
			restarted, err := NewDurableEngine(controlChatID, actors.Store(controlChatID))
			if err != nil {
				t.Fatal(err)
			}
			reference := checkpointOnlyStore{path: filepath.Join(t.TempDir(), "reference.json")}
			referenceEngine, lane := controlFixture(t, reference)
			for _, done := range controlSendSteps(lane, "killed-send")[:killAt+1] {
				done.run(t, referenceEngine)
			}
			referenceLoaded, _, err := reference.Load(controlChatID)
			if err != nil {
				t.Fatal(err)
			}
			assertSameDurableState(t, "after kill at "+step.name, recoverAs(t, referenceLoaded), restarted.Snapshot(), true)
			state := restarted.Snapshot()
			if _, admitted := state.Operations["killed-send"]; killAt >= 1 && !admitted {
				t.Fatal("the acknowledged send was lost")
			}
		})
	}
}
