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
	root, stateDir := repoRoot(t), t.TempDir()
	args, _ := json.Marshal([]string{filepath.Join(root, "desktop", "acp", "mock-codex-app-server.mjs")})
	activity := make(chan struct{}, 1)
	manager := acp.NewManager(acp.Options{RootDir: root, StateDir: stateDir, RuntimeProfile: "dev", DefaultProviderID: "codex", InitTimeout: 10 * time.Second, RSSSampleInterval: time.Hour,
		Broadcast: func(string, any) {
			select {
			case activity <- struct{}{}:
			default:
			}
		},
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join(root, "scripts", "codex-native-host.mjs")}, CWD: root, Enabled: true,
			Env: map[string]string{"WORKASS_CODEX_EXECUTABLE": "node", "WORKASS_CODEX_APP_SERVER_ARGS": string(args), "WORKASS_CODEX_FIXTURE_RAPID_STEER_TARGET": "8", "WORKASS_CODEX_FIXTURE_RAPID_STEER_RECEIPT_DELAY_MS": "20"}}})
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
	for sequence := 1; sequence <= 8; sequence++ {
		id := fmt.Sprintf("rapid-tools-steer-%d", sequence)
		result, handled, err := runtime.Steer(ctx, map[string]any{"tabId": tabID, "chatId": chatID, "sessionId": sessionID,
			"clientUserMessageId": id, "continuationAssistantMessageId": id + "-assistant", "prompt": id})
		if err != nil || !handled || result["ok"] != true {
			t.Fatalf("steer %d: handled=%v result=%#v err=%v", sequence, handled, result, err)
		}
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	state, _ := runtime.Snapshot(chatID)
	previousIndex := -1
	for sequence := 1; sequence <= 8; sequence++ {
		id := providercontract.OperationID(fmt.Sprintf("rapid-tools-steer-%d", sequence))
		found := 0
		for index, entry := range state.Ledger {
			if entry.OperationID == id && entry.Status == "done" {
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
