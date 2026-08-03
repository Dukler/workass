package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAgentMCPListsCatalogAndRoutesExplicitAsyncSpawn(t *testing.T) {
	controlFile := filepath.Join(t.TempDir(), "agent-control.json")
	if err := os.WriteFile(controlFile, []byte(`{"version":1,"url":"http://workass-agent.invalid/rpc","token":"agent-test-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []map[string]any
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer agent-test-value" {
			t.Fatalf("authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requests = append(requests, payload)
		mu.Unlock()
		body, _ := json.Marshal(map[string]any{"result": map[string]any{"ok": true, "method": payload["method"]}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"workass_agent_catalog","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"workass_spawn_subagent","arguments":{"task":"audit","label":"logic","provider_id":"codex","model_id":"gpt-5.6-sol","effort":"xhigh","permission_mode":"agent-full-access","cwd":"/workspace"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"workass_wait_subagents","arguments":{"subagent_ids":["child-a","child-b"],"return_when":"first","timeout_ms":1200}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"workass_message_subagent","arguments":{"subagent_id":"child-a","message":"check tests"}}}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"workass_retry_subagent","arguments":{"subagent_id":"child-a","message":"retry cleanly"}}}`,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"workass_list_subagent_receipts","arguments":{"limit":12}}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"workass_list_chats","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"workass_configure_chat","arguments":{"tab_id":"tab-target","chat_id":"chat-target","provider_id":"claude","model_id":"opus[1m]","effort":"xhigh","permission_intent":"full"}}}`,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"workass_list_spawned_work","arguments":{"tab_id":"parent-tab","chat_id":"parent-chat","tail_chars":4000}}}`,
		`{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"workass_register_external_work","arguments":{"label":"Lane G","pid":4242,"output_file":"/tmp/lane.output","done_file":"/tmp/lane.output.done","tab_id":"parent-tab","chat_id":"parent-chat"}}}`,
		`{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"workass_settle_external_work","arguments":{"work_id":"xw-test","status":"failed","exit_code":3,"summary":"done","tab_id":"parent-tab","chat_id":"parent-chat"}}}`,
		`{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"workass_list_spawned_work_receipts","arguments":{"tab_id":"parent-tab","chat_id":"parent-chat","limit":8}}}`,
		`{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"workass_host_artifact","arguments":{"source_path":"desktop/docs/mocks/plan.html","entry":"index.html","name":"Plan review"}}}`,
		`{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"workass_host_html","arguments":{"source_path":"desktop/docs/mocks/legacy.html","name":"Legacy review"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := serveAgentMCP(strings.NewReader(input), &output, agentMCPOptions{ControlFile: controlFile, ChatID: "parent-chat", TabID: "parent-tab", OwnerKey: "owner-1", HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	responses := map[int]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("stdout is not JSON-RPC: %v\n%s", err, output.String())
		}
		responses[int(response["id"].(float64))] = response
	}
	tools := mapFromAnyMain(responses[2]["result"])["tools"].([]any)
	// Every tool schema here ships in the tool list of every request, for every
	// chat and every subagent. The count is pinned so a tool cannot be added
	// without someone deciding that recurring cost is worth it.
	if len(tools) != 24 {
		t.Fatalf("tool count = %d, want 24", len(tools))
	}
	var spawnTool map[string]any
	var waitTool map[string]any
	var waitManyTool map[string]any
	var registerExternalTool map[string]any
	var hostArtifactTool map[string]any
	legacyHTMLAdvertised := false
	for _, raw := range tools {
		candidate := mapFromAnyMain(raw)
		switch candidate["name"] {
		case "workass_spawn_subagent":
			spawnTool = candidate
		case "workass_wait_subagent":
			waitTool = candidate
		case "workass_wait_subagents":
			waitManyTool = candidate
		case "workass_register_external_work":
			registerExternalTool = candidate
		case "workass_host_artifact":
			hostArtifactTool = candidate
		case "workass_host_html":
			legacyHTMLAdvertised = true
		}
	}
	if spawnTool == nil {
		t.Fatal("workass_spawn_subagent missing")
	}
	spawnSchema := mapFromAnyMain(spawnTool["inputSchema"])
	required := spawnSchema["required"].([]any)
	if len(required) != 1 || required[0] != "task" || spawnSchema["anyOf"] != nil {
		t.Fatalf("spawn schema is not the typed inherit/profile contract: %#v", spawnSchema)
	}
	properties := mapFromAnyMain(spawnSchema["properties"])
	if properties["profile"] == nil || properties["permission_intent"] == nil || properties["cwd"] == nil {
		t.Fatalf("spawn schema lacks typed orchestration properties: %#v", properties)
	}
	if waitTool == nil || waitManyTool == nil ||
		!strings.Contains(toString(waitTool["description"]), "forcibly ends the wait") ||
		!strings.Contains(toString(waitManyTool["description"]), "latched attention list") {
		t.Fatalf("wait tools do not advertise permission attention: wait=%#v waitMany=%#v", waitTool, waitManyTool)
	}
	if registerExternalTool == nil ||
		!strings.Contains(toString(registerExternalTool["description"]), "must be registered in the same turn") ||
		!strings.Contains(toString(registerExternalTool["description"]), "prefer workass_spawn_subagent") ||
		!strings.Contains(toString(registerExternalTool["description"]), "every ACP provider") ||
		strings.Contains(toString(registerExternalTool["description"]), "Claude-provider") {
		t.Fatalf("external registration tool does not advertise the tracking law: %#v", registerExternalTool)
	}
	// Nothing may re-advertise a turn-reporting tool: its schema is a per-request
	// tax on every chat and every subagent, and calling it at a turn's end costs
	// an extra sampling round trip over the whole conversation.
	for _, entry := range tools {
		if toString(mapFromAnyMain(entry)["name"]) == "workass_report_outcome" {
			t.Fatal("workass_report_outcome is advertised again; the harness reports this as fact for free")
		}
	}
	if hostArtifactTool == nil || !strings.Contains(toString(hostArtifactTool["description"]), "stable URL") ||
		!strings.Contains(toString(hostArtifactTool["description"]), "file or static directory") {
		t.Fatalf("artifact hosting tool is missing or underspecified: %#v", hostArtifactTool)
	}
	if legacyHTMLAdvertised {
		t.Fatal("legacy workass_host_html alias must not be advertised")
	}
	hostArtifactSchema := mapFromAnyMain(hostArtifactTool["inputSchema"])
	hostArtifactRequired := hostArtifactSchema["required"].([]any)
	if len(hostArtifactRequired) != 1 || hostArtifactRequired[0] != "source_path" ||
		mapFromAnyMain(hostArtifactSchema["properties"])["entry"] == nil {
		t.Fatalf("artifact hosting schema = %#v", hostArtifactSchema)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 14 {
		t.Fatalf("control requests = %#v", requests)
	}
	var spawn map[string]any
	var artifactRequests []map[string]any
	methods := map[string]map[string]any{}
	for _, request := range requests {
		methods[toString(request["method"])] = mapFromAnyMain(request["params"])
		if request["method"] == "agent.spawn" {
			spawn = mapFromAnyMain(request["params"])
		}
		if request["method"] == "artifact.host" {
			artifactRequests = append(artifactRequests, mapFromAnyMain(request["params"]))
		}
	}
	if spawn == nil || spawn["owner_key"] != "owner-1" || spawn["provider_id"] != "codex" ||
		spawn["model_id"] != "gpt-5.6-sol" || spawn["effort"] != "xhigh" ||
		spawn["mode_id"] != "agent-full-access" || spawn["prompt"] != "audit" {
		t.Fatalf("spawn params = %#v", spawn)
	}
	if len(methods["agent.wait_many"]["ids"].([]any)) != 2 || methods["agent.message"]["id"] != "child-a" ||
		methods["agent.retry"]["message"] != "retry cleanly" || methods["agent.receipts"]["limit"] != float64(12) {
		t.Fatalf("dream orchestration routing = %#v", methods)
	}
	if methods["chat.list"]["parent_chat_id"] != "parent-chat" || methods["chat.list"]["parent_tab_id"] != "parent-tab" ||
		methods["chat.configure"]["tab_id"] != "tab-target" || methods["chat.configure"]["chat_id"] != "chat-target" ||
		methods["chat.configure"]["permission_intent"] != "full" {
		t.Fatalf("chat control routing = %#v", methods)
	}
	if methods["spawned_work.list"]["tab_id"] != "parent-tab" || methods["spawned_work.list"]["chat_id"] != "parent-chat" ||
		methods["spawned_work.list"]["tail_chars"] != float64(4000) || methods["spawned_work.receipts"]["limit"] != float64(8) {
		t.Fatalf("spawned-work routing = %#v", methods)
	}
	if methods["external.register"]["label"] != "Lane G" || methods["external.register"]["pid"] != float64(4242) ||
		methods["external.register"]["output_file"] != "/tmp/lane.output" || methods["external.settle"]["work_id"] != "xw-test" ||
		methods["external.settle"]["exit_code"] != float64(3) {
		t.Fatalf("external work routing = %#v", methods)
	}
	artifactBySource := map[string]map[string]any{}
	for _, request := range artifactRequests {
		artifactBySource[toString(request["source_path"])] = request
	}
	currentArtifact := artifactBySource["desktop/docs/mocks/plan.html"]
	legacyArtifact := artifactBySource["desktop/docs/mocks/legacy.html"]
	if len(artifactRequests) != 2 || currentArtifact["name"] != "Plan review" || currentArtifact["owner_key"] != "owner-1" ||
		legacyArtifact["name"] != "Legacy review" || legacyArtifact["owner_key"] != "owner-1" {
		t.Fatalf("artifact hosting routing = %#v", artifactRequests)
	}
	if strings.Contains(output.String(), "agent-test-value") {
		t.Fatal("control credential leaked to MCP stdout")
	}
}

func TestAgentMCPStdoutStaysJSONRPCOnlyWhenCoordinatorUnavailable(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workass_agent_catalog","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := serveAgentMCP(strings.NewReader(input), &output, agentMCPOptions{ControlFile: filepath.Join(t.TempDir(), "missing.json")}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatalf("stdout is not JSON-RPC: %v\n%s", err, output.String())
	}
	if mapFromAnyMain(response["result"])["isError"] != true {
		t.Fatalf("response = %#v", response)
	}
}

func TestAgentMCPCancelsPendingWaitWhenStdinCloses(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		startedOnce.Do(func() { close(started) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	controlFile := filepath.Join(t.TempDir(), "agent-control.json")
	descriptor, _ := json.Marshal(map[string]any{"version": 1, "url": "http://workass-agent.invalid/rpc", "token": "ephemeral-value"})
	if err := os.WriteFile(controlFile, descriptor, 0o600); err != nil {
		t.Fatal(err)
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workass_wait_subagent","arguments":{"subagent_id":"child-1","timeout_ms":3600000}}}` + "\n"
	var output bytes.Buffer
	startedAt := time.Now()
	if err := serveAgentMCP(strings.NewReader(input), &output, agentMCPOptions{ControlFile: controlFile, HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("agent MCP waited after stdin EOF: %s", elapsed)
	}
	select {
	case <-started:
	default:
		t.Fatal("pending wait never reached the coordinator")
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatalf("stdout is not JSON-RPC after cancellation: %v\n%s", err, output.String())
	}
}

func TestAgentMCPRedactsReflectedErrorText(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"api_key=do-not-echo","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := serveAgentMCP(strings.NewReader(input), &output, agentMCPOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "do-not-echo") {
		t.Fatalf("secret-shaped tool name leaked to MCP stdout: %s", output.String())
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatalf("redacted response is not JSON-RPC: %v", err)
	}
}
