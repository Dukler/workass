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

// The 2026-10-10 incident end to end: an idle Claude chat whose background
// task finishes. Claude Code starts a turn by itself; the chat must show it and
// settle it like any other turn, and the session must stay alive. Before, the
// turn's terminal was rejected as an unowned completion, the lane was closed as
// a protocol failure, and the orphaned CLI kept acting out of sight.
func TestProviderStartedTurnSettlesInTheChatAndKeepsTheSessionAlive(t *testing.T) {
	t.Parallel()
	const tabID, chatID = "self-started-tab", "self-started-chat"
	var logMu sync.Mutex
	var rejections []string
	manager, _ := newFakeManager(t, "claude-cold-effort-stable-resume", Options{
		RSSSampleInterval: time.Hour, Provider: ProviderConfig{ID: "claude"},
		Logf: func(message string, fields map[string]any) {
			if strings.Contains(message, "rejected") || strings.Contains(message, "not handed to the chat") {
				logMu.Lock()
				rejections = append(rejections, message+": "+asString(fields["error"]))
				logMu.Unlock()
			}
		},
	})
	t.Cleanup(func() { manager.Reset() })
	engine, err := chatstate.NewEngine(chatID)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := chatstate.NewCoordinator(engine, manager)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close(context.Background()) })
	definition, err := manager.ProviderDefinition("claude")
	if err != nil {
		t.Fatal(err)
	}
	realm, err := definition.Realm.ResolveRealm(context.Background(), providercontract.RealmRequest{ProviderID: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	identity := providercontract.LaneIdentity{
		ChatID: chatID, Realm: realm, WorkspaceEpoch: nativeWorkspaceEpoch(manager.opts.RootDir),
	}.Normalize()
	if err := engine.Apply(chatstate.SelectLane{
		Identity: identity, Owner: providercontract.AttachmentOwner{TabID: tabID}, CWD: manager.opts.RootDir,
		Creation: providerAdapterForID("claude").creation,
	}); err != nil {
		t.Fatal(err)
	}
	// The human turn that launched the background work.
	if err := engine.Apply(chatstate.Submit{OperationID: "launch", Text: "[fake:claude-updates] start the waiter", Presentation: providercontract.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForState(t, engine, "launch turn settled on an established lane", func(state chatstate.State) bool {
		lane := state.Lanes[state.ActiveLaneID]
		return state.Foreground == nil && len(state.Ledger) == 2 && lane.Phase == chatstate.LaneReady && !lane.Thread.IsZero() && !lane.InitialSeedPending
	})

	lane := manager.providerLaneForChat(tabID, chatID, "claude")
	if lane == nil {
		t.Fatal("no live provider lane for the chat")
	}
	sessionID := lane.info.SessionID
	manager.mu.Lock()
	bridge := manager.sessionBridge[sessionID]
	manager.mu.Unlock()
	if bridge == nil {
		t.Fatal("no live bridge for the chat session")
	}

	// The background task finished while idle; Claude Code starts a turn.
	observeHarnessFixture(manager, bridge, tabID, chatID, sessionID, map[string]any{
		"phase": harnessTurnPhaseStarted, "promptId": "notification-1", "humanAuthored": false,
	})
	job := bridge.jobForSession(sessionID)
	if job == nil || !job.harnessTurn {
		t.Fatalf("self-started turn was not adopted: %#v", job)
	}
	operationID := providercontract.ProviderTurnOperationID(job.ID)
	if owned := engine.Snapshot().Foreground; owned == nil || owned.OperationID != operationID {
		t.Fatalf("chat does not own the self-started turn before its output: %#v", owned)
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]any{"sessionId": sessionID, "update": map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": "HG2 reached 4000."},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.acceptLine(line)
	observeHarnessFixture(manager, bridge, tabID, chatID, sessionID,
		harnessEndedUpdate("notification-1", []any{}, []any{}, "completed"))

	state := waitForState(t, engine, "self-started turn settled in the chat", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 3
	})
	reply := state.Ledger[2]
	if reply.Role != "assistant" || reply.OperationID != operationID || reply.NativeTurnID != job.ID || !strings.Contains(reply.Text, "HG2 reached 4000.") || reply.Status != "done" {
		t.Fatalf("self-started reply row = %#v", reply)
	}
	if got := state.Lanes[state.ActiveLaneID]; got.Phase != chatstate.LaneReady || got.Attachment == nil {
		t.Fatalf("self-started turn left the lane %q (attachment %v); the session must stay alive", got.Phase, got.Attachment != nil)
	}
	if bridge.Closed() || manager.providerLaneForSession(sessionID) == nil {
		t.Fatal("the self-started turn closed the Claude session")
	}
	logMu.Lock()
	rejected := append([]string(nil), rejections...)
	logMu.Unlock()
	if len(rejected) != 0 {
		t.Fatalf("self-started turn was rejected on its way to the chat: %q", rejected)
	}

	// The next prompt goes to the same live session.
	if err := engine.Apply(chatstate.Submit{OperationID: "after", Text: "thanks", Presentation: providercontract.TurnPresentation{Origin: "human"}}); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForState(t, engine, "next prompt settled", func(state chatstate.State) bool {
		return state.Foreground == nil && len(state.Ledger) == 5
	})
	if manager.providerLaneForSession(sessionID) != lane {
		t.Fatal("the next prompt needed a new session; the self-started turn's session did not survive")
	}
}

func waitForState(t *testing.T, engine *chatstate.Engine, what string, ready func(chatstate.State) bool) chatstate.State {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		state := engine.Snapshot()
		if ready(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out; foreground=%#v ledger=%d lane=%#v", what, state.Foreground, len(state.Ledger), state.Lanes[state.ActiveLaneID].Phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
