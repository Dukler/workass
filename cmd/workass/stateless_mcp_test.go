package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workass/internal/acp"
)

type statelessMCPTestHarness struct {
	manager *acp.Manager
	server  *httptest.Server
	client  *http.Client
}

func newStatelessMCPTestHarness(t *testing.T) statelessMCPTestHarness {
	t.Helper()
	root := repoRoot(t)
	manager := acp.NewManager(acp.Options{
		RootDir: root, StateDir: filepath.Join(t.TempDir(), "state"), RuntimeProfile: "test",
		Provider: acp.ProviderConfig{
			ID: "mock", Command: "node", Args: []string{filepath.Join("desktop", "acp", "mock-server.mjs")},
			CWD: root, Enabled: true, Label: "Workass Mock ACP",
		},
		DefaultProviderID: "mock", RSSSampleInterval: time.Hour,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	if _, err := manager.NewSession(ctx, acp.SessionOptions{
		TabID: "mcp-tab", ChatID: "mcp-chat", ProviderID: "mock", CWD: root, AgentOwnerKey: "mcp-owner",
	}); err != nil {
		manager.Reset()
		t.Fatalf("new MCP owner session: %v", err)
	}
	control, err := newAgentControlHandler(manager, newSessionStore(filepath.Join(t.TempDir(), sessionStateFilename)), nil)
	if err != nil {
		manager.Reset()
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(newAgentStatelessMCPHandler(manager, control))
	t.Cleanup(func() {
		server.Close()
		manager.Reset()
	})
	return statelessMCPTestHarness{manager: manager, server: server, client: server.Client()}
}

func (h statelessMCPTestHarness) request(t *testing.T, id int, method, name, version string, params map[string]any) (int, map[string]any) {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    version,
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		"io.modelcontextprotocol/clientInfo": map[string]any{
			"name": "workass-test", "version": "1",
		},
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	request, err := http.NewRequest(http.MethodPost, h.server.URL+agentMCPPath, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer mcp-owner")
	request.Header.Set("X-Workass-Chat-ID", "mcp-chat")
	request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("MCP-Protocol-Version", version)
	request.Header.Set("Mcp-Method", method)
	if name != "" {
		request.Header.Set("Mcp-Name", name)
	}
	reply, err := h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	data, err := io.ReadAll(io.LimitReader(reply.Body, 8*1024*1024))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("invalid JSON-RPC response (%d): %v: %s", reply.StatusCode, err, data)
	}
	return reply.StatusCode, response
}

func TestStatelessMCP20260728DiscoverListAndNoLegacyHandshake(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	status, response := harness.request(t, 1, "server/discover", "", statelessMCPProtocolVersion, map[string]any{})
	result := mapFromAnyMain(response["result"])
	meta := mapFromAnyMain(result["_meta"])
	if status != http.StatusOK || result["resultType"] != "complete" || result["cacheScope"] != "private" ||
		toString(result["supportedVersions"].([]any)[0]) != statelessMCPProtocolVersion ||
		mapFromAnyMain(meta["io.modelcontextprotocol/serverInfo"])["name"] != "workass-agent" {
		t.Fatalf("discover status=%d response=%#v", status, response)
	}

	status, response = harness.request(t, 2, "tools/list", "", statelessMCPProtocolVersion, map[string]any{})
	result = mapFromAnyMain(response["result"])
	if status != http.StatusOK || result["resultType"] != "complete" || len(result["tools"].([]any)) != 24 {
		t.Fatalf("tools/list status=%d response=%#v", status, response)
	}

	status, response = harness.request(t, 3, "initialize", "", statelessMCPProtocolVersion, map[string]any{})
	protocolError := mapFromAnyMain(response["error"])
	if status != http.StatusNotFound || int(protocolError["code"].(float64)) != -32601 {
		t.Fatalf("legacy initialize status=%d response=%#v", status, response)
	}

	get, err := http.NewRequest(http.MethodGet, harness.server.URL+agentMCPPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	getReply, err := harness.client.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer getReply.Body.Close()
	if getReply.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("legacy GET status = %d", getReply.StatusCode)
	}
}

func TestStatelessMCPRejectsHeaderMismatchUnsupportedVersionAndBadAuth(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	status, response := harness.request(t, 1, "tools/list", "", "2099-01-01", map[string]any{})
	protocolError := mapFromAnyMain(response["error"])
	if status != http.StatusBadRequest || int(protocolError["code"].(float64)) != -32022 ||
		toString(mapFromAnyMain(protocolError["data"])["supported"].([]any)[0]) != statelessMCPProtocolVersion {
		t.Fatalf("unsupported version status=%d response=%#v", status, response)
	}

	params := map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    statelessMCPProtocolVersion,
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": params})
	request, _ := http.NewRequest(http.MethodPost, harness.server.URL+agentMCPPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer mcp-owner")
	request.Header.Set("X-Workass-Chat-ID", "mcp-chat")
	request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("MCP-Protocol-Version", statelessMCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	reply, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	var mismatch map[string]any
	if err := json.NewDecoder(reply.Body).Decode(&mismatch); err != nil {
		t.Fatal(err)
	}
	if reply.StatusCode != http.StatusBadRequest || int(mapFromAnyMain(mismatch["error"])["code"].(float64)) != -32020 {
		t.Fatalf("header mismatch status=%d response=%#v", reply.StatusCode, mismatch)
	}

	request, _ = http.NewRequest(http.MethodPost, harness.server.URL+agentMCPPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer wrong")
	request.Header.Set("X-Workass-Chat-ID", "mcp-chat")
	request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
	reply, err = harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad owner status = %d", reply.StatusCode)
	}
}

func TestStatelessMCPSpawnsAndWaitsForTrackedSubagent(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	status, response := harness.request(t, 1, "tools/call", "workass_spawn_subagent", statelessMCPProtocolVersion, map[string]any{
		"name": "workass_spawn_subagent",
		"arguments": map[string]any{
			"task": "reply with the deterministic mock result", "label": "stateless child",
		},
	})
	result := mapFromAnyMain(response["result"])
	if status != http.StatusOK || result["resultType"] != "complete" || result["isError"] == true {
		t.Fatalf("spawn status=%d response=%#v", status, response)
	}
	content := result["content"].([]any)
	var spawned map[string]any
	if err := json.Unmarshal([]byte(toString(mapFromAnyMain(content[0])["text"])), &spawned); err != nil {
		t.Fatal(err)
	}
	childID := toString(spawned["id"])
	if childID == "" {
		t.Fatalf("spawn result = %#v", spawned)
	}

	status, response = harness.request(t, 2, "tools/call", "workass_wait_subagent", statelessMCPProtocolVersion, map[string]any{
		"name": "workass_wait_subagent",
		"arguments": map[string]any{
			"subagent_id": childID, "timeout_ms": 6000,
		},
	})
	result = mapFromAnyMain(response["result"])
	if status != http.StatusOK || result["isError"] == true || result["resultType"] != "complete" {
		t.Fatalf("wait status=%d response=%#v", status, response)
	}
	content = result["content"].([]any)
	if !strings.Contains(toString(mapFromAnyMain(content[0])["text"]), `"status":"done"`) {
		t.Fatalf("wait result = %#v", result)
	}
}

func TestStatelessMCPRefusesPlaintextAndBrowserOrigin(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	plain := httptest.NewRequest(http.MethodPost, agentMCPPath, strings.NewReader(body))
	plain.RemoteAddr = "127.0.0.1:1234"
	plainReply := httptest.NewRecorder()
	newAgentStatelessMCPHandler(harness.manager, &agentControlHandler{manager: harness.manager}).ServeHTTP(plainReply, plain)
	if plainReply.Code != http.StatusUpgradeRequired {
		t.Fatalf("plaintext status = %d", plainReply.Code)
	}

	request, _ := http.NewRequest(http.MethodPost, harness.server.URL+agentMCPPath, strings.NewReader(body))
	request.Header.Set("Origin", "https://evil.invalid")
	reply, err := harness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusForbidden {
		t.Fatalf("browser origin status = %d", reply.StatusCode)
	}
}
