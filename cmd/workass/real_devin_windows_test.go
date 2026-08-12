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
	"workass/internal/httpserve"
	"workass/internal/wire"
)

// TestRealWindowsDaemonWireDevinTurn is an opt-in packaged-path canary. The
// mock server remains the deterministic correctness oracle; this test proves
// that the real authenticated Windows Devin CLI can traverse daemon startup,
// provider selection, actor admission, frozen wire delivery, and durable
// projection as one path.
func TestRealWindowsDaemonWireDevinTurn(t *testing.T) {
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
	if fieldString(session, "providerId") != "devin" || sessionID == "" {
		t.Fatalf("real Devin daemon session receipt is incomplete: %#v", session)
	}

	client.invoke(t, 2, "job:start", map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": sessionID, "providerId": "devin",
		"prompt":      "Reply with a short acknowledgement.",
		"operationId": "real-wire-devin:turn", "userMessageId": "real-wire-devin:user", "assistantMessageId": "real-wire-devin:assistant",
	})
	startReply := client.waitReply(t, 2, 120*time.Second)
	if startReply.Error != nil {
		t.Fatalf("real Devin daemon turn admission failed: %s", *startReply.Error)
	}
	jobID := strings.TrimSpace(fieldString(mapFromAnyMain(startReply.Result), "id"))
	if jobID == "" {
		t.Fatalf("real Devin daemon turn receipt has no job identity: %#v", startReply.Result)
	}
	end := client.waitJobEvent(t, jobID, "end", 180*time.Second)
	job := mapFromAnyMain(end["job"])
	if fieldString(job, "providerId") != "devin" || fieldString(job, "status") != "done" {
		t.Fatalf("real Devin daemon terminal receipt provider=%q status=%q stopReason=%q",
			fieldString(job, "providerId"), fieldString(job, "status"), fieldString(job, "stopReason"))
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
