package acp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	chatstate "workass/internal/chat"
	providercontract "workass/internal/provider"
)

// selfStartedFixture is an idle actor-owned Claude chat on a live session, the
// state in which Claude Code starts turns by itself.
type selfStartedFixture struct {
	t           *testing.T
	manager     *Manager
	engine      *chatstate.Engine
	coordinator *chatstate.Coordinator
	bridge      *Bridge
	tabID       string
	chatID      string
	sessionID   string
}

func newSelfStartedFixture(t *testing.T, name string, mode ...string) *selfStartedFixture {
	t.Helper()
	f := &selfStartedFixture{t: t, tabID: name + "-tab", chatID: name + "-chat"}
	fakeMode := "claude-cold-effort-stable-resume"
	if len(mode) > 0 {
		fakeMode = mode[0]
	}
	f.manager, _ = newFakeManager(t, fakeMode, Options{
		RSSSampleInterval: time.Hour, Provider: ProviderConfig{ID: "claude"},
	})
	t.Cleanup(func() { f.manager.Reset() })
	var err error
	if f.engine, err = chatstate.NewEngine(f.chatID); err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(f.engine, f.manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	f.coordinator = coordinator
	definition, err := f.manager.ProviderDefinition("claude")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: f.chatID, Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(f.manager.opts.RootDir),
	}.Normalize()
	if err := f.engine.Apply(chatstate.SelectLane{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: f.tabID}, CWD: f.manager.opts.RootDir,
		Creation: providerAdapterForID("claude").creation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Apply(chatstate.Submit{OperationID: "launch", Text: "start the waiter", Presentation: providercontract.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForState(t, f.engine, "launch turn settled", func(state chatstate.State) bool {
		lane := state.Lanes[state.ActiveLaneID]
		return state.Foreground == nil && len(state.Ledger) == 2 && lane.Phase == chatstate.LaneReady
	})
	lane := f.manager.providerLaneForChat(f.tabID, f.chatID, "claude")
	if lane == nil {
		t.Fatal("no live provider lane")
	}
	f.sessionID = lane.info.SessionID
	f.manager.mu.Lock()
	f.bridge = f.manager.sessionBridge[f.sessionID]
	f.manager.mu.Unlock()
	if f.bridge == nil {
		t.Fatal("no live bridge")
	}
	return f
}

func (f *selfStartedFixture) start(promptID string) *Job {
	f.t.Helper()
	observeHarnessFixture(f.manager, f.bridge, f.tabID, f.chatID, f.sessionID, map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": promptID, "humanAuthored": false,
	})
	return f.bridge.jobForSession(f.sessionID)
}

func (f *selfStartedFixture) say(text string) {
	f.t.Helper()
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": f.sessionID, "update": map[string]any{
			"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text},
		}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.bridge.acceptLine(line)
}

func (f *selfStartedFixture) end(promptID, terminalReason string) {
	update := harnessEndedUpdate(promptID, []any{}, []any{}, terminalReason)
	observeHarnessFixture(f.manager, f.bridge, f.tabID, f.chatID, f.sessionID, update)
}

func TestProviderStartedTurnFailureSettlesAsFailed(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "failed-terminal")
	if f.start("native-1") == nil {
		t.Fatal("turn not adopted")
	}
	f.say("partial before the API error")
	f.end("native-1", "api_error")
	state := waitForState(t, f.engine, "failed provider turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 3
	})
	if row := state.Ledger[2]; row.Status != "failed" || !strings.Contains(row.Text, "partial before the API error") {
		t.Fatalf("a failed provider-started turn must settle as failed with its output: %#v", row)
	}
}

func TestReplayedHarnessLifecycleNeitherEndsNorReopensTurns(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "replayed-lifecycle")
	first := f.start("native-1")
	f.end("native-1", "completed")
	waitForState(t, f.engine, "first provider turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 3
	})
	if again := f.start("native-1"); again != nil {
		t.Fatalf("a replayed start for an ended turn was adopted: %#v (first %s)", again, first.ID)
	}
	current := f.start("native-2")
	if current == nil {
		t.Fatal("the next real provider turn was not adopted")
	}
	f.end("native-1", "completed")
	if job := f.bridge.jobForSession(f.sessionID); job != current || job.Status != "running" {
		t.Fatalf("a replayed end for an earlier turn ended the running one: %#v", job)
	}
	f.end("native-2", "completed")
	waitForState(t, f.engine, "second provider turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 4
	})
}

func TestCrashedProviderStartedTurnKeepsItsBufferedOutput(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "crash-buffer")
	f.manager.opts.StdoutFlushInterval = time.Hour
	if f.start("native-1") == nil {
		t.Fatal("turn not adopted")
	}
	f.say("last words before the crash")
	f.manager.abandonAdoptedHarnessTurn(f.bridge, f.sessionID)
	state := waitForState(t, f.engine, "crashed provider turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 3
	})
	if row := state.Ledger[2]; row.Status != "failed" || !strings.Contains(row.Text, "last words before the crash") {
		t.Fatalf("buffered output was lost when the provider-started turn crashed: %#v", row)
	}
}

func TestProviderStartedTurnEndDoesNotRaceJobReads(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "end-race")
	if f.start("native-1") == nil {
		t.Fatal("turn not adopted")
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				f.manager.RunningJobForChat(f.tabID, f.chatID)
			}
		}
	}()
	f.end("native-1", "completed")
	close(stop)
	readers.Wait()
	waitForState(t, f.engine, "provider turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 3
	})
}

// 2026-10-11 QA: a turn the harness starts right after a human prompt's reply
// found the human job still finishing, was not adopted, and its output was
// appended to the human turn's row ("Fixture answerFixture harness-born
// answer"). It must get its own row.
func TestProviderStartedTurnRightAfterAHumanTurnGetsItsOwnRow(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "back-to-back")
	if err := f.engine.Apply(chatstate.Submit{OperationID: "human-2", Text: "[fake:harness-after] go", Presentation: providercontract.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := waitForState(t, f.engine, "human turn and the provider-started turn settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 5
	})
	human, harness := state.Ledger[3], state.Ledger[4]
	if strings.Contains(human.Text, "harness answer") || !strings.Contains(human.Text, "human answer") {
		t.Fatalf("provider-started output leaked into the human turn's row: %#v", human)
	}
	if !providercontract.IsProviderTurnOperation(harness.OperationID) || !strings.Contains(harness.Text, "harness answer") {
		t.Fatalf("provider-started turn did not get its own row: %#v", harness)
	}
}

// 2026-10-11: a direction sent while a turn Claude Code started on its own is
// running is a steer into that turn. The adopted job never dispatched input of
// its own, so the steer gate refused it ("no response in progress").
func TestSteerReachesAProviderStartedTurn(t *testing.T) {
	t.Parallel()
	f := newSelfStartedFixture(t, "steer-adopted", "claude-steer-stable-resume")
	if f.start("native-1") == nil {
		t.Fatal("turn not adopted")
	}
	waitForState(t, f.engine, "provider-started turn owns the foreground", func(state chatstate.State) bool {
		return state.Foreground != nil && providercontract.IsProviderTurnOperation(state.Foreground.OperationID)
	})
	if err := f.engine.Apply(chatstate.Steer{OperationID: "steer-into-adopted", Text: "also check the logs", Presentation: providercontract.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatalf("the chat refused a steer into its provider-started turn: %v", err)
	}
	if pending := f.engine.Snapshot().PendingSteer; pending == nil || pending.Turn.NativeID == "" {
		t.Fatalf("steer did not target the provider-started turn: %#v", pending)
	}
	outcome := f.manager.Steer(f.sessionID, "also check the logs", nil, "steer-into-adopted")
	if ok, _ := outcome["ok"].(bool); !ok {
		t.Fatalf("steer into a provider-started turn was refused: %#v", outcome)
	}
	f.end("native-1", "completed")
}
