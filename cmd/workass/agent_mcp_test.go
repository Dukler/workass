package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

func TestAgentMCPToolCatalogKeepsTypedSubagentContract(t *testing.T) {
	tools := agentMCPTools()
	// The full list is paid on every authenticated tools/list request. Pin the
	// count so new recurring context cost remains an explicit decision.
	if len(tools) != 24 {
		t.Fatalf("tool count = %d, want 24", len(tools))
	}
	byName := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		byName[toString(tool["name"])] = tool
		if toString(tool["name"]) == "workass_report_outcome" {
			t.Fatal("workass_report_outcome is advertised again")
		}
	}
	spawn := byName["workass_spawn_subagent"]
	if spawn == nil {
		t.Fatal("workass_spawn_subagent missing")
	}
	schema := mapFromAnyMain(spawn["inputSchema"])
	required := schema["required"].([]string)
	properties := mapFromAnyMain(schema["properties"])
	if len(required) != 2 || required[0] != "task" || required[1] != "operation_id" || properties["profile"] == nil ||
		properties["permission_intent"] == nil || properties["cwd"] == nil {
		t.Fatalf("spawn schema = %#v", schema)
	}
	if wait := byName["workass_wait_subagent"]; wait == nil || !strings.Contains(toString(wait["description"]), "forcibly ends the wait") {
		t.Fatalf("wait tool does not advertise permission attention: %#v", wait)
	}
	if host := byName["workass_host_artifact"]; host == nil || !strings.Contains(toString(host["description"]), "stable URL") {
		t.Fatalf("artifact hosting tool = %#v", host)
	}
	if byName["workass_host_html"] != nil {
		t.Fatal("legacy workass_host_html alias must not be advertised")
	}
}

func TestAgentMCPToolCallsDirectInProcessControl(t *testing.T) {
	root := repoRoot(t)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"), RuntimeProfile: "dev",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	t.Cleanup(func() { manager.Reset() })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	stateDir := manager.StateDir()
	runtime := newTestProviderChatRuntime(t, manager, sharedSessionStore(stateDir), stateDir)
	if _, err := runtime.CreateRendererChat(map[string]any{
		"tabId": "parent-tab", "chatId": "parent-chat", "operationId": "agent-mcp-read-create",
		"title": "MCP owner", "cwd": root, "providerId": "mock",
	}); err != nil {
		t.Fatalf("create actor-owned MCP chat: %v", err)
	}
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "parent-tab", ChatID: "parent-chat", ProviderID: "mock", CWD: root, AgentOwnerKey: "owner-1",
	}); err != nil {
		t.Fatalf("new owner session: %v", err)
	}
	if !manager.ValidateAgentOwner("owner-1", "parent-chat", "parent-tab") {
		t.Fatal("new session did not retain its MCP owner binding")
	}
	control := &agentControlHandler{manager: manager, chats: newChatControlCoordinator(manager, nil, runtime)}
	request := httptest.NewRequest(http.MethodPost, agentMCPPath, nil).WithContext(ctx)
	result, err := callAgentMCPTool(request, browserMCPCallParams{
		Name: "workass_list_subagents", Arguments: map[string]any{},
	}, agentMCPOptions{ChatID: "parent-chat", TabID: "parent-tab", OwnerKey: "owner-1"}, control)
	if err != nil {
		t.Fatal(err)
	}
	resultMap := mapFromAnyMain(result)
	if resultMap["isError"] == true {
		t.Fatalf("catalog call failed: %#v", resultMap)
	}
	content := resultMap["content"].([]any)
	text := toString(mapFromAnyMain(content[0])["text"])
	var listed map[string]any
	if err := json.Unmarshal([]byte(text), &listed); err != nil || listed["subagents"] == nil {
		t.Fatalf("subagent list result = %q err=%v", text, err)
	}
}

func TestAgentMCPRedactsReflectedToolErrors(t *testing.T) {
	result, err := callAgentMCPTool(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), browserMCPCallParams{
		Name: "api_key=do-not-echo", Arguments: map[string]any{},
	}, agentMCPOptions{}, &agentControlHandler{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "do-not-echo") || !strings.Contains(string(encoded), "[redacted]") {
		t.Fatalf("reflected error was not redacted: %s", encoded)
	}
}

func TestAgentMCPRejectsLegacyArgumentAliases(t *testing.T) {
	tests := []struct {
		name string
		call browserMCPCallParams
		want string
	}{
		{name: "camel operation id", call: browserMCPCallParams{Name: "workass_list_chats", Arguments: map[string]any{"operationId": "old"}}, want: "operation_id"},
		{name: "permission mode", call: browserMCPCallParams{Name: "workass_spawn_subagent", Arguments: map[string]any{
			"operation_id": "modern", "task": "task", "permission_mode": "old",
		}}, want: "mode_id"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := callAgentMCPTool(httptest.NewRequest(http.MethodPost, agentMCPPath, nil), test.call, agentMCPOptions{}, &agentControlHandler{})
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(result)
			if !strings.Contains(string(encoded), test.want) || !strings.Contains(string(encoded), `"isError":true`) {
				t.Fatalf("legacy alias result = %s", encoded)
			}
		})
	}
}
