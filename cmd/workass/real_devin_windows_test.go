//go:build windows

package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/chat"
	"workass/internal/httpserve"
	"workass/internal/wire"
)

// TestRealWindowsDaemonWireDevinTurn is an opt-in packaged-path canary. The
// mock server remains the deterministic correctness oracle; this test proves
// that the real authenticated Windows Devin CLI can traverse daemon startup,
// provider selection, actor admission, frozen wire delivery, and durable
// projection as one path.
func TestRealWindowsDaemonWireDevinTurn(t *testing.T) {
	runRealWindowsDaemonWireDevinTurn(t, false)
}

func TestRealWindowsDevinStopWhileAnotherActorSaves(t *testing.T) {
	runRealWindowsDaemonWireDevinTurn(t, true)
}

func runRealWindowsDaemonWireDevinTurn(t *testing.T, cancelTurn bool) {
	if os.Getenv("WORKASS_REAL_DEVIN") != "1" {
		t.Skip("set WORKASS_REAL_DEVIN=1 on a Windows Devin installation")
	}
	devin := strings.TrimSpace(os.Getenv("WORKASS_REAL_DEVIN_BIN"))
	if devin == "" {
		t.Fatal("WORKASS_REAL_DEVIN_BIN is required")
	}
	if info, err := os.Stat(devin); err != nil || info.IsDir() {
		t.Fatalf("real Devin executable is unavailable: %v", err)
	}

	root := t.TempDir()
	renderer := t.TempDir()
	if err := os.WriteFile(filepath.Join(renderer, "index.html"), []byte("<!doctype html><body></body>"), 0o644); err != nil {
		t.Fatalf("write renderer fixture: %v", err)
	}
	stateDir := t.TempDir()
	hub := wire.NewHub()
	manager := acp.NewManager(acp.Options{
		RootDir:  root,
		StateDir: stateDir,
		Providers: []acp.ProviderConfig{{
			ID: "devin", Name: "Devin ACP", Command: devin, Args: []string{"acp"}, Enabled: true,
		}},
		DefaultProviderID:  "devin",
		ProviderConfigFile: filepath.Join(stateDir, "providers.json"),
		Broadcast:          hub.Broadcast,
		InitTimeout:        120 * time.Second,
		RSSSampleInterval:  time.Hour,
	})
	sessions := sharedSessionStore(stateDir)
	providerChats := newProviderChatRuntime(manager, sessions, stateDir, hub.Broadcast)
	if err := providerChats.StartupError(); err != nil {
		t.Fatalf("initialize Windows actor runtime: %v", err)
	}
	t.Cleanup(func() {
		_ = providerChats.Close(context.Background())
		manager.Reset()
	})
	registerDaemonHandlers(hub, root, manager, daemonOptions{StateDir: stateDir, ProviderChats: providerChats})

	manager.DetectProviders(context.Background(), acp.DetectOptions{ProviderID: "devin"})
	ready := false
	for _, provider := range manager.ProvidersList() {
		if fmt.Sprint(provider["id"]) == "devin" && provider["status"] == "ready" && provider["enabled"] == true {
			ready = true
		}
	}
	if !ready {
		t.Fatal("real Devin provider was not ready after detection")
	}

	server := httptest.NewServer(httpserve.New(renderer, hub, nil))
	defer server.Close()
	client := dialTestWS(t, server.URL)
	defer client.conn.Close()

	const tabID = "real-wire-devin-tab"
	const chatID = "real-wire-devin-chat"
	createWireActorChat(t, client, 100, tabID, chatID, "devin")
	client.invoke(t, 1, "app-chat:new-session", map[string]any{
		"tabId": tabID, "chatId": chatID, "providerId": "devin", "operationId": "real-wire-devin:select",
	})
	sessionReply := client.waitReply(t, 1, 120*time.Second)
	if sessionReply.Error != nil {
		t.Fatalf("real Devin daemon new-session failed: %s", *sessionReply.Error)
	}
	session := mapFromAnyMain(sessionReply.Result)
	sessionID := strings.TrimSpace(fieldString(session, "sessionId"))
	if fieldString(session, "providerId") != "devin" || sessionID != "" {
		t.Fatalf("real Devin daemon created a native session before input: %#v", session)
	}

	prompt := "Reply with a short acknowledgement."
	if cancelTurn {
		prompt = "Cancellation diagnostic only. Immediately run PowerShell: Start-Sleep -Seconds 60. Do not read files, change files, or do other work. The client will cancel this turn."
	}
	startArgs := map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": sessionID, "providerId": "devin",
		"prompt":      prompt,
		"operationId": "real-wire-devin:turn", "userMessageId": "real-wire-devin:user", "assistantMessageId": "real-wire-devin:assistant",
	}
	if cancelTurn {
		startArgs["modelId"] = "claude-opus-4-8-high"
		startArgs["modeId"] = "bypass"
	}
	client.invoke(t, 2, "job:start", startArgs)
	startReply := client.waitReply(t, 2, 120*time.Second)
	if startReply.Error != nil {
		t.Fatalf("real Devin daemon turn admission failed: %s", *startReply.Error)
	}
	jobID := strings.TrimSpace(fieldString(mapFromAnyMain(startReply.Result), "id"))
	if jobID == "" {
		t.Fatalf("real Devin daemon first input was not admitted: %#v", startReply.Result)
	}
	var cancelledAt time.Time
	if cancelTurn {
		deadline := time.Now().Add(90 * time.Second)
		for {
			event := client.waitJobEvent(t, jobID, "acp", time.Until(deadline))
			if fieldString(mapFromAnyMain(event["event"]), "kind") == "tool" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("native tool did not start")
			}
		}
		blockedEngine, err := chat.NewEngine("blocked-save-chat")
		if err != nil {
			t.Fatal(err)
		}
		entered, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		providerChats.mu.Lock()
		providerChats.actors["blocked-save-chat"] = &providerChatActor{engine: blockedEngine}
		providerChats.mu.Unlock()
		defer func() {
			close(release)
			if err := <-committed; err != nil {
				t.Errorf("blocked fixture commit: %v", err)
			}
			providerChats.mu.Lock()
			delete(providerChats.actors, "blocked-save-chat")
			providerChats.mu.Unlock()
		}()
		go func() {
			committed <- blockedEngine.ApplyPrepared(chat.InitializeChat{
				Presentation: chat.PresentationState{TabID: "blocked-save-tab"}, OperationID: "blocked-create", Digest: "blocked-digest",
			}, func() error { close(entered); <-release; return nil })
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("fixture did not enter actor storage")
		}
		cancelledAt = time.Now()
		client.invoke(t, 3, "job:cancel", jobID)
		reply := client.waitReply(t, 3, 5*time.Second)
		if reply.Error != nil || mapFromAnyMain(reply.Result)["cancelled"] != true {
			t.Fatal("native cancellation was not accepted")
		}
		t.Logf("native cancellation wire reply_ms=%d", time.Since(cancelledAt).Milliseconds())
	}
	terminalTimeout := 180 * time.Second
	if cancelTurn {
		terminalTimeout = 20 * time.Second
	}
	end := client.waitJobEvent(t, jobID, "end", terminalTimeout)
	job := mapFromAnyMain(end["job"])
	if cancelTurn {
		if fieldString(job, "stopReason") != "cancelled" {
			t.Fatal("native terminal was not cancelled")
		}
		t.Logf("native cancellation wire terminal_ms=%d unrelated_actor_still_blocked=true", time.Since(cancelledAt).Milliseconds())
		return
	}
	if fieldString(job, "providerId") != "devin" || fieldString(job, "status") != "done" {
		t.Fatalf("real Devin daemon terminal receipt provider=%q status=%q stopReason=%q",
			fieldString(job, "providerId"), fieldString(job, "status"), fieldString(job, "stopReason"))
	}
	actor, ok := providerChats.Snapshot(chatID)
	if !ok {
		t.Fatal("real Devin actor disappeared after its first turn")
	}
	lane := actor.Lanes[actor.ActiveLaneID]
	if lane.Identity.Realm.ProviderID != "devin" || lane.Thread.HeadID == "" {
		t.Fatalf("real Devin first input did not establish its exact native thread: %#v", lane)
	}

	snapshot, err := providerChats.ProjectSession()
	if err != nil {
		t.Fatalf("project real Devin actor: %v", err)
	}
	projected := chatFromSnapshot(snapshot, tabID)
	if got := len(messageSlice(projected)); got < 2 {
		t.Fatalf("real Devin actor transcript rows=%d, want at least 2", got)
	}
	t.Log("canary receipt: full Windows daemon wire/actor path completed a real authenticated Devin turn")
}
