package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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

type statelessMCPTestHarness struct {
	manager *acp.Manager
	runtime *providerChatRuntime
	handler *statelessMCPHandler
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
	store := newSessionStore(filepath.Join(manager.StateDir(), sessionStateFilename))
	runtime := newTestProviderChatRuntime(t, manager, store, manager.StateDir())
	if _, err := runtime.actorForNewChatOperation("mcp-chat", chat.PresentationState{
		TabID: "mcp-tab", Title: "MCP owner", ProviderID: "mock", CWD: stringPointerValueOrNil(root),
	}, "test:create-mcp-owner", false); err != nil {
		manager.Reset()
		t.Fatalf("create actor-owned MCP chat: %v", err)
	}
	if _, err := runtime.Select(ctx, acp.SessionOptions{
		TabID: "mcp-tab", ChatID: "mcp-chat", ProviderID: "mock", CWD: root, AgentOwnerKey: "mcp-owner",
	}); err != nil {
		manager.Reset()
		t.Fatalf("create actor-owned MCP session: %v", err)
	}
	if _, err := runtime.Start(ctx, map[string]any{
		"tabId": "mcp-tab", "chatId": "mcp-chat", "providerId": "mock", "cwd": root,
		"prompt":        "[mock:permission] hold the actor-owned parent turn",
		"userMessageId": "mcp-owner-turn", "assistantMessageId": "mcp-owner-assistant",
	}, "human"); err != nil {
		manager.Reset()
		t.Fatalf("start actor-owned MCP parent turn: %v", err)
	}
	control, err := newAgentControlHandler(manager, nil, newChatControlCoordinator(manager, nil, runtime))
	if err != nil {
		manager.Reset()
		t.Fatal(err)
	}
	handler, ok := newAgentStatelessMCPHandler(manager, control).(*statelessMCPHandler)
	if !ok {
		manager.Reset()
		t.Fatal("agent stateless MCP handler has unexpected concrete type")
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() {
		server.Close()
		// Close durable actor coordinators while their provider manager is still
		// alive. Resetting the manager first lets a later lane detach recreate
		// provider-lanes.json while testing.TempDir is removing the state tree.
		_ = runtime.Close(context.Background())
		manager.Reset()
	})
	return statelessMCPTestHarness{manager: manager, runtime: runtime, handler: handler, server: server, client: server.Client()}
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
			"operation_id": "stateless-spawn-once", "task": "reply with the deterministic mock result", "label": "stateless child",
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
			"operation_id": "stateless-wait-once", "subagent_id": childID, "timeout_ms": 6000,
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

func TestStatelessMCPMutationsRequireCallerStableOperationID(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	call := func(id int, arguments map[string]any) map[string]any {
		status, response := harness.request(t, id, "tools/call", "workass_rename_chat", statelessMCPProtocolVersion, map[string]any{
			"name": "workass_rename_chat", "arguments": arguments,
		})
		if status != http.StatusOK {
			t.Fatalf("rename status = %d, response = %#v", status, response)
		}
		return mapFromAnyMain(response["result"])
	}

	for _, arguments := range []map[string]any{
		{"tab_id": "mcp-tab", "chat_id": "mcp-chat", "title": "missing operation"},
		{"operation_id": map[string]any{"token": "do-not-store"}, "tab_id": "mcp-tab", "chat_id": "mcp-chat", "title": "malformed operation"},
		{"operation_id": "api_key=do-not-store", "tab_id": "mcp-tab", "chat_id": "mcp-chat", "title": "secret-shaped operation"},
	} {
		result := call(100+len(arguments), arguments)
		if result["isError"] != true {
			t.Fatalf("invalid mutation was accepted: %#v", result)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "do-not-store") || !strings.Contains(strings.ToLower(string(encoded)), "operation") {
			t.Fatalf("invalid operation result was unsafe or unhelpful: %s", encoded)
		}
	}

	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	state := actor.engine.Snapshot()
	if state.Presentation.Title != "MCP owner" {
		t.Fatalf("invalid operation mutated chat title = %q", state.Presentation.Title)
	}
	for _, operationID := range []string{"api_key=do-not-store", "missing operation", "malformed operation"} {
		if _, exists := state.Operations[providercontract.OperationID(operationID)]; exists {
			t.Fatalf("invalid operation %q reached durable state", operationID)
		}
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

func TestAgentStatelessMCPFencesDeletedActorBeforeOwnerValidation(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	ownerValidations := 0
	harness.handler.validateOwner = func(string, string, string) bool {
		ownerValidations++
		return true
	}
	if status, _ := harness.request(t, 1, "tools/list", "", statelessMCPProtocolVersion, nil); status != http.StatusOK {
		t.Fatalf("actor-owned agent MCP status = %d", status)
	}
	if ownerValidations != 1 {
		t.Fatalf("agent MCP owner-validation hook count = %d, want 1 for the live actor", ownerValidations)
	}
	ownerValidations = 0
	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: "delete-agent-owner", Force: true}); err != nil {
		t.Fatal(err)
	}
	if status, _ := harness.request(t, 2, "tools/list", "", statelessMCPProtocolVersion, nil); status != http.StatusUnauthorized {
		t.Fatalf("agent MCP accepted deleted actor: %d", status)
	}
	if ownerValidations != 0 {
		t.Fatalf("agent MCP validated the transient owner %d time(s) after actor deletion", ownerValidations)
	}
}

func TestBrowserStatelessMCPRejectsLiveManagerOwnerAfterActorDeletion(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	handler, ok := newBrowserStatelessMCPHandler(
		harness.manager, filepath.Join(t.TempDir(), "browser-control.json"), harness.runtime,
	).(*statelessMCPHandler)
	if !ok {
		t.Fatal("browser stateless MCP handler has unexpected concrete type")
	}
	ownerValidations := 0
	handler.validateOwner = func(string, string, string) bool {
		ownerValidations++
		return true
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()

	request := func(id int) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/list",
			"params": map[string]any{"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    statelessMCPProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			}},
		})
		req, err := http.NewRequest(http.MethodPost, server.URL+browserMCPPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer mcp-owner")
		req.Header.Set("X-Workass-Chat-ID", "mcp-chat")
		req.Header.Set("X-Workass-Tab-ID", "mcp-tab")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", statelessMCPProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/list")
		reply, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		return reply.StatusCode
	}

	if status := request(1); status != http.StatusOK {
		t.Fatalf("actor-owned browser MCP status = %d", status)
	}
	if ownerValidations != 1 {
		t.Fatalf("browser MCP owner-validation hook count = %d, want 1 for the live actor", ownerValidations)
	}
	ownerValidations = 0
	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: "delete-mcp-owner", Force: true}); err != nil {
		t.Fatal(err)
	}
	if status := request(2); status != http.StatusUnauthorized {
		t.Fatalf("browser MCP accepted transient manager authority after actor tombstone: %d", status)
	}
	if ownerValidations != 0 {
		t.Fatalf("browser MCP validated the transient owner %d time(s) after actor deletion", ownerValidations)
	}
}

func TestBrowserStatelessMCPMutationJournalReadbackConflictAndActorFence(t *testing.T) {
	harness := newStatelessMCPTestHarness(t)
	controlFile := filepath.Join(t.TempDir(), "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(`{"version":1,"url":"http://browser-control.invalid/rpc","token":"browser-control-token"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var externalCalls int
	var receiptCalls int
	receiptAvailable := true
	var lastOperationID, lastDigest string
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		operationID := toString(payload["operationId"])
		digest := toString(payload["requestDigest"])
		if payload["method"] == "browser.receipt" {
			receiptCalls++
			if !receiptAvailable {
				body, _ := json.Marshal(map[string]any{
					"id": payload["id"], "operationId": operationID, "requestDigest": digest, "receipt": false,
				})
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			}
			body, _ := json.Marshal(map[string]any{
				"id": payload["id"], "operationId": lastOperationID, "requestDigest": lastDigest,
				"receipt": true, "result": map[string]any{"readback": true},
			})
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
		}
		externalCalls++
		lastOperationID, lastDigest = operationID, digest
		if externalCalls == 1 {
			return nil, errors.New("simulated lost browser reply")
		}
		params := mapFromAnyMain(payload["params"])
		response := map[string]any{
			"id": payload["id"], "operationId": operationID, "requestDigest": digest, "receipt": true,
			"result": map[string]any{"clicked": true},
		}
		if toString(params["selector"]) == "#reject" {
			response["error"] = "browser rejected request"
			delete(response, "result")
		}
		body, _ := json.Marshal(response)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	handler, ok := newBrowserStatelessMCPHandler(harness.manager, controlFile, harness.runtime).(*statelessMCPHandler)
	if !ok {
		t.Fatal("browser stateless MCP handler has unexpected concrete type")
	}
	handler.browserClient = client
	server := httptest.NewTLSServer(handler)
	defer server.Close()

	request := func(id int, operationID, selector string) (int, map[string]any) {
		t.Helper()
		params := map[string]any{
			"name":      "workass_browser_click",
			"arguments": map[string]any{"operation_id": operationID, "tab_id": 7, "selector": selector},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    statelessMCPProtocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": params})
		req, err := http.NewRequest(http.MethodPost, server.URL+browserMCPPath, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer mcp-owner")
		req.Header.Set("X-Workass-Chat-ID", "mcp-chat")
		req.Header.Set("X-Workass-Tab-ID", "mcp-tab")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", statelessMCPProtocolVersion)
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "workass_browser_click")
		reply, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Body.Close()
		var response map[string]any
		if err := json.NewDecoder(reply.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return reply.StatusCode, response
	}

	status, response := request(1, "browser-lost-once", "#lost")
	firstResult := mapFromAnyMain(response["result"])
	if status != http.StatusOK || firstResult["isError"] != true {
		t.Fatalf("lost-reply mutation status=%d response=%#v", status, response)
	}
	status, response = request(2, "browser-lost-once", "#lost")
	secondResult := mapFromAnyMain(response["result"])
	if status != http.StatusOK || secondResult["isError"] == true || externalCalls != 1 || receiptCalls != 1 {
		t.Fatalf("receipt retry status=%d calls=%d receiptCalls=%d response=%#v", status, externalCalls, receiptCalls, response)
	}

	status, response = request(3, "browser-first-once", "#first")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] == true {
		t.Fatalf("initial mutation status=%d response=%#v", status, response)
	}
	status, response = request(4, "browser-first-once", "#changed")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] != true || externalCalls != 2 {
		t.Fatalf("changed operation reuse status=%d calls=%d response=%#v", status, externalCalls, response)
	}

	status, response = request(5, "browser-completed-once", "#completed")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] == true || externalCalls != 3 {
		t.Fatalf("completed mutation status=%d calls=%d response=%#v", status, externalCalls, response)
	}
	receiptAvailable = false
	receiptCalls = 0
	status, response = request(6, "browser-completed-once", "#completed")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] == true || externalCalls != 3 || receiptCalls != 0 {
		t.Fatalf("completed actor receipt status=%d calls=%d receiptCalls=%d response=%#v", status, externalCalls, receiptCalls, response)
	}

	status, response = request(7, "browser-reject-once", "#reject")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] != true || externalCalls != 4 {
		t.Fatalf("failed mutation status=%d calls=%d response=%#v", status, externalCalls, response)
	}
	receiptCalls = 0
	status, response = request(8, "browser-reject-once", "#reject")
	if status != http.StatusOK || mapFromAnyMain(response["result"])["isError"] != true || externalCalls != 4 || receiptCalls != 0 {
		t.Fatalf("failed actor receipt status=%d calls=%d receiptCalls=%d response=%#v", status, externalCalls, receiptCalls, response)
	}

	actor, err := harness.runtime.actor("mcp-chat")
	if err != nil {
		t.Fatal(err)
	}
	if err := actor.engine.Apply(chat.DeleteChat{OperationID: "delete-browser-mutation", Force: true}); err != nil {
		t.Fatal(err)
	}
	status, _ = request(9, "browser-deleted", "#deleted")
	if status != http.StatusUnauthorized || externalCalls != 4 {
		t.Fatalf("deleted actor browser mutation status=%d calls=%d", status, externalCalls)
	}
}
