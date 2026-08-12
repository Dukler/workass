package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/artifacthost"
)

func TestAgentControlIsInProcessAndCreatesNoLegacyDescriptor(t *testing.T) {
	descriptorPath := filepath.Join(t.TempDir(), "state", "agent-control.json")
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	store := newSessionStore(filepath.Join(stateDir, sessionStateFilename))
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	handler, err := newAgentControlHandler(manager, nil, newChatControlCoordinator(manager, nil, runtime))
	if err != nil {
		t.Fatal(err)
	}
	if handler.manager != manager || handler.chats == nil {
		t.Fatalf("in-process agent control = %#v", handler)
	}
	if _, err := os.Stat(descriptorPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy agent-control descriptor still created: %v", err)
	}
}

func TestAgentControlRejectsOutOfRangeWaitTimeout(t *testing.T) {
	handler := &agentControlHandler{manager: acp.NewManager(acp.Options{})}
	request := httptest.NewRequest(http.MethodPost, agentMCPPath, nil)
	for _, timeoutMS := range []int{999, 3600001} {
		_, err := handler.call(request, agentControlRequest{
			Method: "agent.wait",
			Params: map[string]any{"id": "child", "timeout_ms": timeoutMS},
		})
		if err == nil || !strings.Contains(err.Error(), "between 1000 and 3600000") {
			t.Fatalf("timeout %d error = %v", timeoutMS, err)
		}
	}
}

func TestAgentControlCreatedChatSurvivesStaleSessionSaveBeforeSend(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(t.TempDir(), sessionStateFilename)
	store := newSessionStore(path)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-control-parent",
		"tabId":       "control-parent-tab", "chatId": "control-parent-chat",
		"title": "Control parent", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native parent: %v", err)
	}
	stale, err := runtime.ProjectSession()
	if err != nil {
		t.Fatalf("project stale renderer snapshot: %v", err)
	}
	stale[globalPresentationOperationField] = "test:stale-control-renderer-save"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "control-parent-chat", TabID: "control-parent-tab", CWD: root, AgentOwnerKey: "control-owner",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	request := httptest.NewRequest(http.MethodPost, agentMCPPath, nil)
	ownerParams := map[string]any{"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab"}
	createdRaw, err := handler.call(request, agentControlRequest{
		Method: "chat.create",
		Params: map[string]any{
			"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab",
			"operation_id": "test:create-control-child",
			"title":        "Control durable child", "cwd": "inherit", "provider_id": "mock", "model_id": "mock-deterministic", "mode_id": "ask",
		},
	})
	if err != nil {
		t.Fatalf("chat.create: %v", err)
	}
	created := mapFromAnyMain(createdRaw)
	tabID, chatID := fieldString(created, "tabId"), fieldString(created, "chatId")
	listedRaw, err := handler.call(request, agentControlRequest{Method: "chat.list", Params: ownerParams})
	if err != nil {
		t.Fatalf("chat.list: %v", err)
	}
	if !agentControlListHasChat(listedRaw, tabID, chatID) {
		t.Fatalf("chat.list missing created chat: tab=%s chat=%s list=%#v", tabID, chatID, listedRaw)
	}
	if saved, err := runtime.ApplyRendererSnapshot(stale); err != nil || !saved {
		t.Fatalf("stale renderer presentation save: saved=%v err=%v", saved, err)
	}
	sentRaw, err := handler.call(request, agentControlRequest{
		Method: "chat.send",
		Params: map[string]any{
			"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab",
			"operation_id": "test:send-after-stale-save",
			"tab_id":       tabID, "chat_id": chatID, "message": "queued after stale save", "delivery": "queue",
		},
	})
	if err != nil {
		t.Fatalf("chat.send after stale save: %v", err)
	}
	if receipt := mapFromAnyMain(sentRaw); fieldString(receipt, "queueId") == "" {
		t.Fatalf("chat.send receipt missing queue id: %#v", sentRaw)
	}
}

func TestAgentControlTurnlessSpawnNeverUsesLegacySessionMirrorAsOwner(t *testing.T) {
	root := repoRoot(t)
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	if _, err := runtime.CreateRendererChat(map[string]any{
		"operationId": "test:create-turnless-control-chat",
		"tabId":       "control-tab", "chatId": "control-chat",
		"title": "Turnless control", "cwd": root, "providerId": "mock",
		"currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create actor-native turnless chat: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "control-chat", TabID: "control-tab", CWD: root, AgentOwnerKey: "known-control-owner",
	})
	if err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler, err := newAgentControlHandler(manager, nil, newChatControlCoordinator(manager, nil, runtime))
	if err != nil {
		t.Fatalf("new actor-owned agent control: %v", err)
	}
	result, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "agent.spawn",
		Params: map[string]any{
			"owner_key": "known-control-owner", "parent_chat_id": "control-chat", "parent_tab_id": "control-tab",
			"operation_id": "test:turnless-spawn-rejected", "prompt": "turnless control spawn", "label": "control-hinted",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "actor-owned turn") {
		t.Fatalf("legacy mirror was accepted as background ownership: result=%#v err=%v", result, err)
	}
}

func TestT7AgentControlExternalSettleIsIdempotentAndOwnerValidated(t *testing.T) {
	root := repoRoot(t)
	const tabID, chatID, ownerKey = "external-control-tab", "external-control-chat", "external-owner"
	stateDir := filepath.Join(t.TempDir(), "state")
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP", Env: map[string]string{
			"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "mock-native.json"),
		}},
		DefaultProviderID: "mock",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "external-chat-create", "focus": false,
		"title": "External owner", "titleLocked": true, "cwd": root,
		"providerId": "mock", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create external owner actor: %v", err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "mock", CWD: root, AgentOwnerKey: ownerKey,
	})
	if err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	if _, err := runtime.Start(ctx, map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "providerId": "mock", "cwd": root,
		"operationId": "external-owner-turn", "userMessageId": "external-owner-user", "assistantMessageId": "external-owner-assistant",
		"prompt": "establish the durable background owner",
	}, "human"); err != nil {
		state, _ := runtime.Snapshot(chatID)
		t.Fatalf("start actor owner turn: %v state=%#v", err, state)
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	request := httptest.NewRequest(http.MethodPost, agentMCPPath, nil)
	output := externalControlTestPath(t, "control.output")
	baseParams := map[string]any{"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID}
	registeredRaw, err := handler.call(request, agentControlRequest{
		Method: "external.register",
		Params: map[string]any{
			"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "external-register-op",
			"label": "control external", "output_file": output,
		},
	})
	if err != nil {
		t.Fatalf("external.register: %v", err)
	}
	registered := mapFromAnyMain(registeredRaw)
	workID := fieldString(registered, "workId")
	if workID == "" || fieldString(registered, "doneFile") != output+".done" {
		t.Fatalf("external.register result = %#v", registered)
	}
	settleParams := copyAnyMap(baseParams)
	settleParams["operation_id"] = "external-settle-op"
	settleParams["work_id"], settleParams["status"], settleParams["exit_code"], settleParams["summary"] = workID, "failed", 7, "failed with token=hidden"
	settledRaw, err := handler.call(request, agentControlRequest{Method: "external.settle", Params: settleParams})
	if err != nil {
		t.Fatalf("external.settle: %v", err)
	}
	settled := mapFromAnyMain(settledRaw)
	if settled["ok"] != true || settled["already"] != false || fieldString(settled, "status") != "failed" {
		t.Fatalf("external.settle result = %#v", settled)
	}
	againRaw, err := handler.call(request, agentControlRequest{Method: "external.settle", Params: settleParams})
	if err != nil {
		t.Fatalf("external.settle second call: %v", err)
	}
	if again := mapFromAnyMain(againRaw); again["ok"] != true || again["already"] != false || fieldString(again, "workId") != workID {
		t.Fatalf("idempotent settle result = %#v", again)
	}
	badOwner := copyAnyMap(settleParams)
	badOwner["owner_key"] = "not-the-owner"
	if _, err := handler.call(request, agentControlRequest{Method: "external.settle", Params: badOwner}); err == nil ||
		!strings.Contains(err.Error(), "no running Workass turn owns this external work request") {
		t.Fatalf("non-owning key error = %v", err)
	}
	item := manager.ListSpawnedWork(tabID, chatID)[0]
	if item.Status != "failed" || item.ExitCode == nil || *item.ExitCode != 7 ||
		strings.Contains(item.Summary, "hidden") || !strings.Contains(item.Summary, "[redacted]") {
		t.Fatalf("settled external item = %#v", item)
	}
}

func TestAgentControlCodexOwnerCanRegisterExternalHandoff(t *testing.T) {
	root := repoRoot(t)
	const tabID, chatID, ownerKey = "codex-handoff-tab", "codex-handoff-chat", "codex-handoff-owner"
	stateDir := filepath.Join(t.TempDir(), "state")
	store := sharedSessionStore(stateDir)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: stateDir, RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Codex ACP fixture", Env: map[string]string{
			"WORKASS_MOCK_ACP_SESSION_STORE": filepath.Join(stateDir, "codex-mock-native.json"),
		}},
		DefaultProviderID: "codex",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": tabID, "chatId": chatID, "operationId": "codex-handoff-chat-create", "focus": false,
		"title": "Codex handoff", "titleLocked": true, "cwd": root,
		"providerId": "codex", "currentModelId": "mock-deterministic", "currentModeId": "ask",
	}); err != nil {
		t.Fatalf("create Codex owner actor: %v", err)
	}
	info, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: tabID, ChatID: chatID, ProviderID: "codex", CWD: root, AgentOwnerKey: ownerKey,
	})
	if err != nil {
		t.Fatalf("new Codex owner session: %v", err)
	}
	if _, err := runtime.Start(ctx, map[string]any{
		"kind": "app-chat", "tabId": tabID, "chatId": chatID, "sessionId": info.SessionID, "providerId": "codex", "cwd": root,
		"operationId": "codex-owner-turn", "userMessageId": "codex-owner-user", "assistantMessageId": "codex-owner-assistant",
		"prompt": "establish the durable Codex background owner",
	}, "human"); err != nil {
		state, _ := runtime.Snapshot(chatID)
		t.Fatalf("start Codex actor owner turn: %v state=%#v", err, state)
	}
	waitProviderChatIdle(t, runtime, chatID, 5*time.Second)
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	output := externalControlTestPath(t, "codex-handoff.output")
	registeredRaw, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "external.register",
		Params: map[string]any{
			"owner_key": ownerKey, "parent_chat_id": chatID, "parent_tab_id": tabID, "operation_id": "codex-external-register-op",
			"label": "production handoff receipt", "output_file": output,
		},
	})
	if err != nil {
		t.Fatalf("Codex external.register endpoint: %v", err)
	}
	registered := mapFromAnyMain(registeredRaw)
	if fieldString(registered, "workId") == "" || fieldString(registered, "doneFile") != output+".done" {
		t.Fatalf("Codex external.register result = %#v", registered)
	}
	items := manager.ListSpawnedWork(tabID, chatID)
	if len(items) != 1 || items[0].ProviderID != "codex" || items[0].Kind != "external" || items[0].Status != "running" {
		t.Fatalf("Codex registered handoff = %#v", items)
	}
}

func TestAgentControlInvalidOwnerKeepsSubagentOwnershipError(t *testing.T) {
	stateDir := t.TempDir()
	manager := acp.NewManager(acp.Options{StateDir: stateDir})
	t.Cleanup(func() { manager.Reset() })
	store := sharedSessionStore(stateDir)
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "tab", "chatId": "chat", "operationId": "invalid-owner-chat",
		"title": "Invalid owner", "cwd": t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	_, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "agent.list",
		Params: map[string]any{"owner_key": "missing", "parent_chat_id": "chat", "parent_tab_id": "tab"},
	})
	if err == nil || err.Error() != "no running Workass turn owns this subagent request" {
		t.Fatalf("invalid owner error = %v", err)
	}
}

func TestAgentControlHostsArtifactsOnlyFromTheCallingAgentWorkspace(t *testing.T) {
	repo := repoRoot(t)
	workspace := t.TempDir()
	source := filepath.Join(workspace, "review.pdf")
	if err := os.WriteFile(source, []byte("%PDF-1.4\nreview\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := acp.NewManager(acp.Options{
		RootDir: workspace, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join(repo, "desktop", "acp", "mock-server.mjs")},
			CWD: repo, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	stateDir := manager.StateDir()
	store := sharedSessionStore(stateDir)
	runtime := newTestProviderChatRuntime(t, manager, store, stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "artifact-tab", "chatId": "artifact-chat", "operationId": "artifact-chat-create",
		"title": "Artifact workspace", "cwd": workspace, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create artifact actor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "artifact-tab", ChatID: "artifact-chat", ProviderID: "mock", CWD: workspace, AgentOwnerKey: "artifact-owner",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	registry, err := artifacthost.New(filepath.Join(t.TempDir(), "state"), "http://127.0.0.1:8788")
	if err != nil {
		t.Fatal(err)
	}
	handler := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime), artifacts: registry}
	result, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "artifact.host",
		Params: map[string]any{
			"owner_key": "artifact-owner", "parent_chat_id": "artifact-chat", "parent_tab_id": "artifact-tab",
			"source_path": "review.pdf", "name": "Review",
		},
	})
	if err != nil {
		t.Fatalf("artifact.host: %v", err)
	}
	hosted, ok := result.(artifacthost.Registration)
	if !ok || hosted.URLPath == "" || hosted.LocalURL != "http://127.0.0.1:8788"+hosted.URLPath ||
		hosted.ContentType != "application/pdf" || !strings.Contains(hosted.Markdown, hosted.URLPath) {
		t.Fatalf("artifact.host result = %#v", result)
	}
	legacyResult, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "html.host",
		Params: map[string]any{
			"owner_key": "artifact-owner", "parent_chat_id": "artifact-chat", "parent_tab_id": "artifact-tab",
			"source_path": "review.pdf", "name": "Review",
		},
	})
	if err != nil {
		t.Fatalf("legacy html.host alias: %v", err)
	}
	legacyHosted, ok := legacyResult.(artifacthost.Registration)
	if !ok || legacyHosted.ID != hosted.ID || !strings.HasPrefix(legacyHosted.URLPath, artifacthost.PathPrefix+"/") {
		t.Fatalf("legacy html.host result = %#v", legacyResult)
	}
	outside := filepath.Join(t.TempDir(), "outside.pdf")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.call(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), agentControlRequest{
		Method: "artifact.host",
		Params: map[string]any{
			"owner_key": "artifact-owner", "parent_chat_id": "artifact-chat", "parent_tab_id": "artifact-tab",
			"source_path": outside,
		},
	}); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Fatalf("outside artifact.host error = %v", err)
	}
}

func agentControlListHasChat(result any, tabID, chatID string) bool {
	root := mapFromAnyMain(result)
	if chats, ok := root["chats"].([]map[string]any); ok {
		for _, chat := range chats {
			if fieldString(chat, "tabId") == tabID && fieldString(chat, "chatId") == chatID {
				return true
			}
		}
		return false
	}
	for _, raw := range anySlice(root["chats"]) {
		chat := mapFromAnyMain(raw)
		if fieldString(chat, "tabId") == tabID && fieldString(chat, "chatId") == chatID {
			return true
		}
	}
	return false
}

func externalControlTestPath(t *testing.T, name string) string {
	t.Helper()
	root, err := os.MkdirTemp(os.TempDir(), "workass-external-control-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return filepath.Join(root, name)
}
