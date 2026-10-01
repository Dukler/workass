package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	providercontract "workass/internal/provider"
)

func TestCodexActorConsumesRapidTextSteersDuringToolCalls(t *testing.T) {
	testCodexActorRapidSteers(t, 8, false, false)
}

func TestCodexActorAdmitsTwoSteersBeforeEitherToolBoundaryReceipt(t *testing.T) {
	testCodexActorRapidSteers(t, 2, true, false)
}

func TestCodexActorSteersQueuedMessageAlongsideDirectSteer(t *testing.T) {
	testCodexActorRapidSteers(t, 2, true, true)
}

func testCodexActorRapidSteers(t *testing.T, count int, batchReceipts, queuedSteer bool) {
	t.Helper()
	root, stateDir := repoRoot(t), t.TempDir()
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	env := map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args), "WORKASS_CODEX_FIXTURE_RAPID_STEER_TARGET": fmt.Sprint(count), "WORKASS_CODEX_FIXTURE_RAPID_STEER_RECEIPT_DELAY_MS": "20"}
	if batchReceipts {
		env["WORKASS_CODEX_FIXTURE_RAPID_STEER_BATCH_RECEIPTS"] = "1"
	}
	activity := make(chan struct{}, 1)
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Broadcast: func(string, any) {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true,
			Env: env}})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const tabID, chatID = "rapid-tools-tab", "rapid-tools-chat"
	if _, err := runtime.CreateRendererChat(map[string]any{"tabId": tabID, "chatId": chatID, "operationId": "rapid-tools-create", "cwd": root, "providerId": "codex"}); err != nil {
		t.Fatal(err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{TabID: tabID, ChatID: chatID, ProviderID: "codex", CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Start(ctx, map[string]any{"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID,
		"operationId": "rapid-tools-start", "userMessageId": "rapid-tools-user", "assistantMessageId": "rapid-tools-assistant",
		"prompt": "[fixture:rapid-steer-commentary] [fixture:rapid-steer-tools] keep the turn open"}, "human"); err != nil {
		t.Fatal(err)
	}
	sessionID := info.SessionID
	for {
		turns, _ := manager.TurnDiagnostics(tabID, chatID, 1)["turns"].([]any)
		state, _ := runtime.Snapshot(chatID)
		if lane, ok := state.Lanes[state.ActiveLaneID]; ok && lane.Attachment != nil {
			sessionID = lane.Attachment.ConnectionID
		}
		if sessionID != "" && len(turns) > 0 && mapFromAnyMain(turns[0])["firstUpdateMs"] != nil {
			break
		}
		select {
		case <-activity:
		case <-ctx.Done():
			t.Fatal("native tool-call turn did not start:", ctx.Err())
		}
	}
	for sequence := 1; sequence <= count; sequence++ {
		id := fmt.Sprintf("rapid-tools-steer-%d", sequence)
		arg := map[string]any{"tabId": tabID, "chatId": chatID, "sessionId": sessionID,
			"clientUserMessageId": id, "continuationAssistantMessageId": id + "-assistant", "prompt": id}
		if queuedSteer && sequence == 2 {
			state, _ := runtime.Snapshot(chatID)
			images := []any{map[string]any{"mimeType": "image/png", "name": "fixture.png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a6v8AAAAASUVORK5CYII="}}
			receipt, err := runtime.ReplaceStagedQueue(tabID, chatID, "queued-steer-owner", state.Presentation.AgentQueueRevision, []any{
				map[string]any{"id": "keep-queued", "text": "ordinary FIFO remains queued"},
				map[string]any{"id": "steer-queued", "text": id, "images": images},
			})
			if err != nil {
				t.Fatal(err)
			}
			arg["boundary"] = map[string]any{"queuedMessageId": "steer-queued", "expectedQueueRevision": receipt["agentQueueRevision"]}
			arg["images"] = images
		}
		result, handled, err := runtime.Steer(ctx, arg)
		if err != nil || !handled || result["ok"] != true {
			t.Fatalf("steer %d: handled=%v result=%#v err=%v", sequence, handled, result, err)
		}
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	state, _ := runtime.Snapshot(chatID)
	if queuedSteer && (len(state.StagedQueue) != 1 || state.StagedQueue[0].ID != "keep-queued") {
		t.Fatal("queued steering lost an unrelated FIFO row or retained the transferred owner")
	}
	previousIndex := -1
	for sequence := 1; sequence <= count; sequence++ {
		id := providercontract.OperationID(fmt.Sprintf("rapid-tools-steer-%d", sequence))
		found := 0
		for index, entry := range state.Ledger {
			if entry.OperationID == id && entry.Status == "done" {
				if queuedSteer && sequence == 2 && (entry.QueueID != "steer-queued" || len(entry.Attachments) != 1 || entry.Attachments[0].Name != "fixture.png") {
					t.Fatal("native queued steer lost its source identity or attachment")
				}
				found++
				if index <= previousIndex {
					t.Fatalf("steer %d consumed out of order", sequence)
				}
				previousIndex = index
			}
		}
		if found != 1 {
			t.Fatalf("steer %d consumed %d times", sequence, found)
		}
	}
	for _, effect := range state.Outbox {
		if effect.Kind == chat.EffectCancelTurn {
			t.Fatal("rapid steering emitted a cancel effect")
		}
	}
}
