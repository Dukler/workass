package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

// bug-20261118-afbfeed6 end to end: a tracked subagent is running when the
// daemon is replaced (an update). The next daemon lists it as failed, and
// workass_retry_subagent through the real handler used to answer "subagent not
// found for this owner" because only the dead process had its spawn intent.
func TestSubagentOrphanedByDaemonReplacementCanBeRetried(t *testing.T) {
	retryOrphanedSubagentAfterReplacement(t, false)
}

// The reporter's own orphans were spawned by 0.1.299, which kept no spawn spec:
// the retry is rebuilt from the spawn request the chat's actor kept.
func TestSubagentSpawnedBeforeSpecsCanBeRetriedAfterReplacement(t *testing.T) {
	retryOrphanedSubagentAfterReplacement(t, true)
}

func retryOrphanedSubagentAfterReplacement(t *testing.T, spawnedBeforeSpecs bool) {
	root := repoRoot(t)
	const tabID, chatID = "retry-restart-tab", "retry-restart-chat"
	newManager := func(stateDir string) *acp.Manager {
		return acp.NewManager(acp.Options{
			RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
			Provider: acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP", Enabled: true, Env: map[string]string{
				"WORKASS_MOCK_ACP_DELAY_MS": "1",
			}},
			DefaultProviderID: "mock", RSSSampleInterval: time.Hour, SpawnedWorkReconcileInterval: time.Hour,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	firstDir := filepath.Join(t.TempDir(), "state")
	first := newManager(firstDir)
	t.Cleanup(func() { first.Reset() })
	runtime := newTestProviderChatRuntime(t, first, sharedSessionStore(firstDir), firstDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "retry-restart-create", "focus": false,
		"title": "Retry after restart", "cwd": root, "providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, AgentOwnerKey: "retry-restart-owner"})
	if err != nil {
		t.Fatalf("attach lane: %v", err)
	}
	if _, err := runtime.Start(ctx, map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "providerId": "mock", "cwd": root,
		"operationId": "retry-restart-turn", "userMessageId": "retry-restart-turn-u", "assistantMessageId": "retry-restart-turn-a",
		"prompt": "[mock:hold-until-steer] coordinator turn",
	}, "human"); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	handler := &agentControlHandler{manager: first, chats: newChatControlCoordinator(first, nil, runtime)}
	request := httptest.NewRequest(http.MethodPost, toolsPath, nil)
	var childID string
	for deadline := time.Now().Add(10 * time.Second); childID == ""; {
		_, err := handler.call(request, agentControlRequest{Method: "agent.spawn", Params: map[string]any{
			"owner_key": "retry-restart-owner", "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "retry-restart-spawn",
			"prompt": "[mock:hold-until-steer] long task the update will cut", "label": "cut by the update",
		}})
		if err == nil {
			for _, run := range first.ListSubagents("retry-restart-owner", chatID, tabID) {
				childID = run.ID
			}
		}
		if childID == "" {
			if time.Now().After(deadline) {
				t.Fatalf("spawn never succeeded: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		state, _ := runtime.Snapshot(chatID)
		if state.Background[childID].Event.Status == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never ran: %#v", state.Background)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The coordinator's turn ends with the child still running, as a chat that
	// left subagents working does.
	if _, handled, err := runtime.Steer(ctx, map[string]any{
		"sessionId": info.SessionID, "tabId": tabID, "chatId": chatID, "prompt": "wrap up",
		"clientUserMessageId": "retry-restart-steer-u", "continuationAssistantMessageId": "retry-restart-steer-a",
	}); !handled || err != nil {
		t.Fatalf("end the coordinator turn: handled=%v err=%v", handled, err)
	}
	waitProviderChatIdle(t, runtime, chatID, 10*time.Second)
	if state, _ := runtime.Snapshot(chatID); state.Background[childID].Event.Status != "running" {
		t.Fatalf("fixture raced: the child settled before the update: %#v", state.Background[childID])
	}

	// The update replaces the daemon: the next one gets what is on disk now.
	secondDir := filepath.Join(t.TempDir(), "state")
	if err := copyLiveStateDir(secondDir, firstDir); err != nil {
		t.Fatal(err)
	}
	if spawnedBeforeSpecs {
		if err := os.RemoveAll(filepath.Join(secondDir, "subagent-specs")); err != nil {
			t.Fatal(err)
		}
	}
	second := newManager(secondDir)
	t.Cleanup(func() { second.Reset() })
	restarted := newTestProviderChatRuntime(t, second, sharedSessionStore(secondDir), secondDir)
	settled, _ := restarted.Snapshot(chatID)
	if status := settled.Background[childID].Event.Status; status == "running" || status == "" {
		t.Fatalf("the orphaned child reads %q after restart", status)
	}
	// The restarted daemon's handler, under the chat's own owner capability.
	restartedHandler := &agentControlHandler{manager: second, chats: newChatControlCoordinator(second, nil, restarted)}
	result, err := second.WithActorOwner(chatID, tabID, func(ownerKey string) (any, error) {
		return restartedHandler.call(request, agentControlRequest{Method: "agent.retry", Params: map[string]any{
			"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "retry-restart-retry",
			"id": childID, "message": "the update cut you off; finish it",
		}})
	})
	if err != nil {
		t.Fatalf("retry of the orphaned child after restart: %v", err)
	}
	encoded := mustJSON(t, result)
	if !strings.Contains(encoded, `"retryOf":"`+childID+`"`) || !strings.Contains(encoded, `"label":"cut by the update"`) ||
		!strings.Contains(encoded, `"modelId":"mock-deterministic"`) || !strings.Contains(encoded, `"modeId":"ask"`) {
		t.Fatalf("retry result = %s", encoded)
	}
}

func TestActorSpawnIntentFollowsRetriesBackToTheSpawn(t *testing.T) {
	completed := func(op string, action chat.BackgroundAction, id string) chat.OutboxEntry {
		background := action
		return chat.OutboxEntry{
			Kind: chat.EffectBackground, OperationID: providercontract.OperationID(op), Status: chat.OutboxCompleted,
			Background: &background, Result: json.RawMessage(`{"id":"` + id + `"}`),
		}
	}
	state := chat.State{Outbox: []chat.OutboxEntry{
		completed("spawn", chat.BackgroundAction{Kind: chat.BackgroundSpawnAgent, Spawn: &chat.SpawnAgentAction{
			Prompt: "original task", Label: "lineage", ModelID: "m", ModeID: "ask", CWD: "/w",
		}}, "a"),
		completed("retry-1", chat.BackgroundAction{Kind: chat.BackgroundRetryAgent, Retry: &chat.RetryAgentAction{WorkID: "a", Message: "first note"}}, "b"),
		completed("retry-2", chat.BackgroundAction{Kind: chat.BackgroundRetryAgent, Retry: &chat.RetryAgentAction{WorkID: "b"}}, "c"),
		completed("retry-3", chat.BackgroundAction{Kind: chat.BackgroundRetryAgent, Retry: &chat.RetryAgentAction{WorkID: "c", Message: "second note"}}, "d"),
		completed("hosted", chat.BackgroundAction{Kind: chat.BackgroundSpawnAgent, Spawn: &chat.SpawnAgentAction{
			Prompt: "runs elsewhere", OriginMachine: "m-other",
		}}, "h"),
		completed("loop", chat.BackgroundAction{Kind: chat.BackgroundRetryAgent, Retry: &chat.RetryAgentAction{WorkID: "loop-id"}}, "loop-id"),
	}}
	got, ok := actorSpawnIntentForSubagent(state, "d")
	want := "original task\n\nRetry guidance from the coordinator:\nfirst note\n\nRetry guidance from the coordinator:\nsecond note"
	if !ok || got.Prompt != want || got.Label != "lineage" || got.ModelID != "m" || got.ModeID != "ask" || got.CWD != "/w" {
		t.Fatalf("intent for d = %#v ok=%v", got, ok)
	}
	if got, ok := actorSpawnIntentForSubagent(state, "a"); !ok || got.Prompt != "original task" {
		t.Fatalf("intent for a = %#v ok=%v", got, ok)
	}
	for _, id := range []string{"h", "loop-id", "missing"} {
		if got, ok := actorSpawnIntentForSubagent(state, id); ok {
			t.Fatalf("intent for %s = %#v", id, got)
		}
	}
}
