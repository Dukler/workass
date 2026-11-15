package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// bug-20261115-e090223f: a job notice queued behind a turn Claude started by
// itself was dispatched the moment that turn ended, while Claude was already
// starting its next self-started turn from a queued Monitor event. Claude folded
// the notice into that turn, and the chat closed the provider connection as a
// turn-protocol violation, losing the reply.
func TestQueuedInputDispatchedAsClaudeStartsItsNextTurnIsFoldedCleanly(t *testing.T) {
	t.Parallel()
	root, stateDir := repoRoot(t), t.TempDir()
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "claude", InitTimeout: 10 * time.Second,
		RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
		Provider: acp.ProviderConfig{ID: "claude", Command: "node", Args: []string{filepath.Join(root, "scripts", "claude-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{
				"WORKASS_CLAUDE_SDK_MODULE": filepath.Join(root, "desktop", "acp", "mock-claude-agent-sdk.mjs"),
				"WORKASS_CLAUDE_EXECUTABLE": "/fixture/claude",
				"WORKASS_CLAUDE_SESSION_ID": "fixture-queued-wake-session",
			}},
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const tabID, chatID = "queued-wake-tab", "queued-wake-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": tabID, "chatId": chatID, "operationId": "queued-wake-create", "cwd": root, "providerId": "claude"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "claude", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "operationId": "queued-wake-turn", "userMessageId": "queued-wake-user", "assistantMessageId": "queued-wake-assistant", "prompt": "[fixture:harness-chain=600] watch the job"}, "human"); err != nil {
		t.Fatal(err)
	}
	// Wait until the chat owns the first self-started turn, then queue input
	// behind it: it is dispatched the moment that turn ends.
	waitForState(t, runtime, chatID, 10*time.Second, func(state chat.State) bool {
		return state.Foreground != nil && providercontract.IsProviderTurnOperation(state.Foreground.OperationID)
	})
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "operationId": "queued-wake-notice", "userMessageId": "queued-wake-notice-user", "assistantMessageId": "queued-wake-notice-assistant", "prompt": "job finished", "busyMode": "queue-v1"}, "human"); err != nil {
		t.Fatal(err)
	}
	state := waitForState(t, runtime, chatID, 15*time.Second, func(state chat.State) bool {
		return state.Foreground == nil && len(state.Queue) == 0 && noticeRow(state) != nil
	})
	row := noticeRow(state)
	if row.TerminalState != "completed" || strings.Contains(row.Text, "turn protocol") {
		t.Fatalf("the queued input's turn did not end normally: %#v", row)
	}
	if !strings.Contains(row.Text, "job finished") {
		t.Fatalf("the reply to the folded input was lost: %q", row.Text)
	}
	for _, lane := range state.Lanes {
		if lane.LastError == providercontract.ErrorProtocolViolation {
			t.Fatalf("the lane was closed as a protocol violation: %#v", lane)
		}
	}
}

func noticeRow(state chat.State) *chat.LedgerEvent {
	for index := range state.Ledger {
		if row := state.Ledger[index]; row.OperationID == "queued-wake-notice" && row.Role == "assistant" && row.TerminalState != "" {
			return &state.Ledger[index]
		}
	}
	return nil
}

func waitForState(t *testing.T, runtime *providerChatRuntime, chatID string, limit time.Duration, done func(chat.State) bool) chat.State {
	t.Helper()
	deadline := time.Now().Add(limit)
	var state chat.State
	for time.Now().Before(deadline) {
		state, _ = runtime.Snapshot(chatID)
		if done(state) {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("chat never reached the expected state; foreground=%#v queue=%d ledger tail=%#v", state.Foreground, len(state.Queue), ledgerTail(state))
	return state
}

func ledgerTail(state chat.State) []chat.LedgerEvent {
	if len(state.Ledger) > 4 {
		return state.Ledger[len(state.Ledger)-4:]
	}
	return state.Ledger
}
