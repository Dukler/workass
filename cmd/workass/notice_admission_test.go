package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/fleetlog"
)

// bug-20261116-7ab17943: a notice that arrives while the chat's turn is running
// is folded into that turn, the way Claude Code delivers its own background
// notifications, instead of waiting behind it (and behind everything the owner
// typed meanwhile) as a turn of its own.

func noticeFixture(t *testing.T, env map[string]string) (*providerChatRuntime, string, string) {
	t.Helper()
	root, stateDir := repoRoot(t), t.TempDir()
	merged := map[string]string{"WORKASS_MOCK_ACP_DELAY_MS": "1", "WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-sessions.json")}
	for key, value := range env {
		merged[key] = value
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Name: "Mock", Command: "node", CWD: root, Enabled: true,
			Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")}, Env: merged,
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	const tabID, chatID = "notice-tab", "notice-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "notice-create", "title": "Notices",
		"cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic",
	}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(context.Background(), acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(context.Background(), map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "providerId": "mock", "sessionId": info.SessionID,
		"prompt": "[mock:hold-until-steer] deploy and wait", "userMessageId": "u-base", "assistantMessageId": "a-base",
	}, "human"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, runtime, chatID, "the base turn to run", func(state chat.State) bool {
		return state.Foreground != nil && state.Foreground.Status == chat.ForegroundRunning && state.Foreground.Turn.NativeID != ""
	})
	return runtime, tabID, chatID
}

func awaitState(t *testing.T, runtime *providerChatRuntime, chatID, what string, done func(chat.State) bool) chat.State {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		state, _ := runtime.Snapshot(chatID)
		if done(state) {
			return state
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: foreground=%#v queue=%d pending=%#v", what, state.Foreground, len(state.Queue), state.PendingSteer)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func deliverTestJobNotice(t *testing.T, runtime *providerChatRuntime, tabID, chatID, label string) {
	t.Helper()
	code := 0
	if err := runtime.deliverFleetMonitorNotice(fleetMonitorNotice{
		Kind: fleetlog.KindExit, TabID: tabID, ChatID: chatID, TargetID: "wa:m-test/process/xwnotice00001",
		Label: label, Outcome: fleetlog.OutcomeSettled,
		ProcessReceipt: &acp.SpawnedWorkReceipt{
			ReceiptID: "spawned-xwnotice00001", TaskID: "xwnotice00001", Label: label, Status: "exited",
			ExitCode: &code, Summary: "Finished with exit 0", OutputFile: "/tmp/xwnotice00001.output",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestANoticeForABusyChatJoinsItsRunningTurn(t *testing.T) {
	runtime, tabID, chatID := noticeFixture(t, nil)
	deliverTestJobNotice(t, runtime, tabID, chatID, "background deploy")

	state, _ := runtime.Snapshot(chatID)
	for _, entry := range state.Queue {
		if entry.Presentation.NoticeKind != "" {
			t.Fatalf("the notice waited behind the running turn as a turn of its own: %#v", entry)
		}
	}
	// The mock holds its turn until a steer arrives, so the turn ending at all
	// proves the notice reached the provider inside it.
	state = awaitState(t, runtime, chatID, "the steered turn to end", func(state chat.State) bool {
		return state.Foreground == nil && state.PendingSteer == nil
	})
	var notices []chat.LedgerEvent
	for _, event := range state.Ledger {
		if event.NoticeKind != "" {
			notices = append(notices, event)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("want exactly one notice row, got %#v", notices)
	}
	notice := notices[0]
	if notice.SteerState != "applied" || notice.TerminalState != "consumed" || notice.NoticeTitle != "background deploy" {
		t.Fatalf("the notice must be consumed inside the running turn, got %#v", notice)
	}
	if !strings.Contains(notice.Text, "<task-notification>") {
		t.Fatalf("the steered input is the notice itself: %q", notice.Text)
	}
	if projected := projectNotice(notice); projected == nil {
		t.Fatal("a steered notice must still be drawn as a notice, never as the user speaking")
	}
	// Redelivery of the same event is the same operation: no second row.
	deliverTestJobNotice(t, runtime, tabID, chatID, "background deploy")
	state, _ = runtime.Snapshot(chatID)
	if state.Foreground != nil || len(state.Queue) != 0 {
		t.Fatalf("a redelivered notice started another turn: foreground=%#v queue=%#v", state.Foreground, state.Queue)
	}
}

func TestANoticeForAChatWhoseProviderCannotTakeLiveInputStillQueues(t *testing.T) {
	runtime, tabID, chatID := noticeFixture(t, map[string]string{"WORKASS_MOCK_ACP_DISABLE_STEER": "1"})
	state, _ := runtime.Snapshot(chatID)
	if lane := state.Lanes[state.Foreground.LaneID]; lane.Delivery.LiveSteer {
		t.Skip("the mock still advertises live steering with steer disabled")
	}
	deliverTestJobNotice(t, runtime, tabID, chatID, "background deploy")
	state, _ = runtime.Snapshot(chatID)
	if state.PendingSteer != nil || len(state.Queue) != 1 || state.Queue[0].Presentation.NoticeKind == "" {
		t.Fatalf("with no live input the notice is owed its own turn: pending=%#v queue=%#v", state.PendingSteer, state.Queue)
	}
}
