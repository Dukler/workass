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
	"workass/internal/artifacthost"
)

func TestAgentControlDescriptorIsPrivateAndEndpointIsLoopbackAuthenticated(t *testing.T) {
	descriptorPath := filepath.Join(t.TempDir(), "state", "agent-control.json")
	handler, err := newAgentControlHandler(acp.NewManager(acp.Options{}), newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename)), nil, "http://127.0.0.1:8788"+agentControlPath, descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(descriptorPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("descriptor permissions = %o, want private", info.Mode().Perm())
	}
	var descriptor browserControlDescriptor
	data, _ := os.ReadFile(descriptorPath)
	if err := json.Unmarshal(data, &descriptor); err != nil || descriptor.Token == "" {
		t.Fatalf("descriptor = %s err=%v", data, err)
	}

	external := httptest.NewRequest(http.MethodPost, agentControlPath, strings.NewReader(`{"method":"agent.catalog","params":{}}`))
	external.RemoteAddr = "192.0.2.44:5000"
	external.Header.Set("Authorization", "Bearer "+descriptor.Token)
	externalReply := httptest.NewRecorder()
	handler.ServeHTTP(externalReply, external)
	if externalReply.Code != http.StatusNotFound {
		t.Fatalf("external status = %d", externalReply.Code)
	}

	unauthorized := httptest.NewRequest(http.MethodPost, agentControlPath, strings.NewReader(`{"method":"agent.catalog","params":{}}`))
	unauthorized.RemoteAddr = "127.0.0.1:5000"
	unauthorized.Header.Set("Authorization", "Bearer wrong")
	unauthorizedReply := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedReply, unauthorized)
	if unauthorizedReply.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d body=%s", unauthorizedReply.Code, unauthorizedReply.Body.String())
	}
	if strings.Contains(unauthorizedReply.Body.String(), descriptor.Token) {
		t.Fatal("descriptor token leaked in response")
	}
}

func TestAgentControlRejectsOutOfRangeWaitTimeout(t *testing.T) {
	handler := &agentControlHandler{manager: acp.NewManager(acp.Options{})}
	request := httptest.NewRequest(http.MethodPost, agentControlPath, nil)
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
	parent := sessionMirrorFixture("control-parent-tab", "control-parent-chat", "parent prompt")
	parentChat := chatFromSnapshot(parent, "control-parent-tab")
	parentChat["cwd"] = root
	parentChat["currentModelId"] = "mock-deterministic"
	parentChat["currentModeId"] = "ask"
	stale := cloneJSON(parent).(map[string]any)
	if !store.Save(parent) {
		t.Fatal("initial parent session save failed")
	}
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "control-parent-chat", TabID: "control-parent-tab", CWD: root, AgentOwnerKey: "control-owner",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler := &agentControlHandler{manager: manager, state: store, chats: newChatControlCoordinator(manager, store, nil)}
	request := httptest.NewRequest(http.MethodPost, agentControlPath, nil)
	ownerParams := map[string]any{"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab"}
	createdRaw, err := handler.call(request, agentControlRequest{
		Method: "chat.create",
		Params: map[string]any{
			"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab",
			"title": "Control durable child", "cwd": "inherit", "provider_id": "mock", "model_id": "mock-deterministic", "mode_id": "ask",
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
	if !store.Save(stale) {
		t.Fatal("stale renderer session save failed")
	}
	handler.chats.mu.Lock()
	handler.chats.draining[tabID+"\x00"+chatID] = true
	handler.chats.mu.Unlock()
	sentRaw, err := handler.call(request, agentControlRequest{
		Method: "chat.send",
		Params: map[string]any{
			"owner_key": "control-owner", "parent_chat_id": "control-parent-chat", "parent_tab_id": "control-parent-tab",
			"tab_id": tabID, "chat_id": chatID, "message": "queued after stale save", "delivery": "queue",
		},
	})
	if err != nil {
		t.Fatalf("chat.send after stale save: %v", err)
	}
	if receipt := mapFromAnyMain(sentRaw); fieldString(receipt, "queueId") == "" {
		t.Fatalf("chat.send receipt missing queue id: %#v", sentRaw)
	}
}

func TestAgentControlTurnlessSpawnUsesSessionMirrorVisibleRootHint(t *testing.T) {
	root := repoRoot(t)
	store := newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename))
	if !store.Save(sessionMirrorFixture("control-tab", "control-chat", "anchor prompt")) {
		t.Fatal("initial session mirror save failed")
	}
	store.PrepareTurn(map[string]any{"tabId": "control-tab", "chatId": "control-chat", "prompt": "anchor prompt"})
	store.RecordJobEvent("job:event", map[string]any{"type": "start", "job": map[string]any{
		"id": "visible-control-job", "tabId": "control-tab", "chatId": "control-chat", "startedAt": "2026-07-16T10:00:00Z",
	}})
	store.RecordJobEvent("job:event", map[string]any{"type": "end", "job": map[string]any{
		"id": "visible-control-job", "tabId": "control-tab", "chatId": "control-chat", "status": "done", "finishedAt": "2026-07-16T10:00:01Z",
	}})
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"),
		Provider:          acp.ProviderConfig{Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Label: "Workass Mock ACP"},
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, err := manager.NewSession(ctx, acp.SessionOptions{
		ChatID: "control-chat", TabID: "control-tab", CWD: root, AgentOwnerKey: "known-control-owner",
	})
	if err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler := &agentControlHandler{manager: manager, state: store}
	result, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
		Method: "agent.spawn",
		Params: map[string]any{
			"owner_key": "known-control-owner", "parent_chat_id": "control-chat", "parent_tab_id": "control-tab",
			"prompt": "turnless control spawn", "label": "control-hinted",
		},
	})
	if err != nil {
		t.Fatalf("turnless control spawn: %v", err)
	}
	run, ok := result.(acp.SubagentRun)
	if !ok || !run.Adopted || run.ParentJobID != "" || run.RootJobID != "visible-control-job" {
		t.Fatalf("turnless control result = %#v", result)
	}
	finished, err := manager.WaitSubagent(ctx, "known-control-owner", "control-chat", "control-tab", run.ID, 6*time.Second)
	if err != nil || finished.Status != "done" {
		t.Fatalf("turnless control child = %#v err=%v", finished, err)
	}
}

func TestT7AgentControlExternalSettleIsIdempotentAndOwnerValidated(t *testing.T) {
	root := repoRoot(t)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"), RuntimeProfile: "dev",
		Provider:          acp.ProviderConfig{ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Workass Mock ACP"},
		DefaultProviderID: "mock",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "external-control-tab", ChatID: "external-control-chat", ProviderID: "mock", CWD: root, AgentOwnerKey: "external-owner",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	handler := &agentControlHandler{manager: manager}
	request := httptest.NewRequest(http.MethodPost, agentControlPath, nil)
	output := externalControlTestPath(t, "control.output")
	baseParams := map[string]any{"owner_key": "external-owner", "parent_chat_id": "external-control-chat", "parent_tab_id": "external-control-tab"}
	registeredRaw, err := handler.call(request, agentControlRequest{
		Method: "external.register",
		Params: map[string]any{
			"owner_key": "external-owner", "parent_chat_id": "external-control-chat", "parent_tab_id": "external-control-tab",
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
	if again := mapFromAnyMain(againRaw); again["ok"] != true || again["already"] != true {
		t.Fatalf("idempotent settle result = %#v", again)
	}
	badOwner := copyAnyMap(settleParams)
	badOwner["owner_key"] = "not-the-owner"
	if _, err := handler.call(request, agentControlRequest{Method: "external.settle", Params: badOwner}); err == nil ||
		!strings.Contains(err.Error(), "no running Workass turn owns this external work request") {
		t.Fatalf("non-owning key error = %v", err)
	}
	item := manager.ListSpawnedWork("external-control-tab", "external-control-chat")[0]
	if item.Status != "failed" || item.ExitCode == nil || *item.ExitCode != 7 || item.Wake != "pending" ||
		strings.Contains(item.Summary, "hidden") || !strings.Contains(item.Summary, "[redacted]") {
		t.Fatalf("settled external item = %#v", item)
	}
}

func TestAgentControlCodexOwnerCanRegisterExternalHandoff(t *testing.T) {
	root := repoRoot(t)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"), RuntimeProfile: "dev",
		Provider:          acp.ProviderConfig{ID: "codex", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")}, CWD: root, Enabled: true, Label: "Codex ACP fixture"},
		DefaultProviderID: "codex",
		RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "codex-handoff-tab", ChatID: "codex-handoff-chat", ProviderID: "codex", CWD: root, AgentOwnerKey: "codex-handoff-owner",
	}); err != nil {
		t.Fatalf("new Codex owner session: %v", err)
	}

	handler := &agentControlHandler{manager: manager}
	output := externalControlTestPath(t, "codex-handoff.output")
	registeredRaw, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
		Method: "external.register",
		Params: map[string]any{
			"owner_key": "codex-handoff-owner", "parent_chat_id": "codex-handoff-chat", "parent_tab_id": "codex-handoff-tab",
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
	items := manager.ListSpawnedWork("codex-handoff-tab", "codex-handoff-chat")
	if len(items) != 1 || items[0].ProviderID != "codex" || items[0].Kind != "external" || items[0].Status != "running" {
		t.Fatalf("Codex registered handoff = %#v", items)
	}
}

func TestAgentControlInvalidOwnerKeepsSubagentOwnershipError(t *testing.T) {
	handler := &agentControlHandler{manager: acp.NewManager(acp.Options{})}
	t.Cleanup(func() { handler.manager.Reset() })
	_, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
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
	handler := &agentControlHandler{manager: manager, artifacts: registry}
	result, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
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
	legacyResult, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
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
	if _, err := handler.call(httptest.NewRequest(http.MethodPost, agentControlPath, nil), agentControlRequest{
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
