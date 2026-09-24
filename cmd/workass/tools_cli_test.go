package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workass/internal/acp"
	"workass/internal/tlscert"
	"workass/internal/toolcli"
)

func toolCLIContextFixture(t *testing.T, handler http.Handler) (string, *atomic.Int32) {
	t.Helper()
	root := t.TempDir()
	cert, err := tlscert.Ensure(root)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tlscert.IssueLoopbackServerCertificate(cert, "tools.localhost")
	if err != nil {
		t.Fatal(err)
	}
	count := &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		if r.Header.Get("MCP-Protocol-Version") != "" || strings.Contains(r.Header.Get("Accept"), "event-stream") {
			t.Error("CLI used MCP protocol headers")
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	config := toolcli.Config{Endpoint: "https://tools.localhost:" + port + toolsPath, CAFile: filepath.Join(root, tlscert.CertFileName), Credential: "mcp-owner", ChatID: "mcp-chat", TabID: "mcp-tab"}
	raw, _ := json.Marshal(config)
	path := filepath.Join(root, "context.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, count
}

func TestToolsCLICatalogCallAndMutationReceiptsWithoutMCP(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	contextFile, count := toolCLIContextFixture(t, h.handler)
	// A proxy setting must never send the owner capability elsewhere.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	run := func(args []string, input string) (map[string]any, error) {
		t.Helper()
		var output, diagnostics bytes.Buffer
		before := count.Load()
		err := runToolsCommand(context.Background(), append([]string{"--context", contextFile}, args...), strings.NewReader(input), &output, &diagnostics)
		if count.Load() != before+1 {
			t.Fatal("CLI must issue exactly one request, without initialization or retry")
		}
		if strings.Contains(output.String(), `"mcp-owner"`) {
			t.Fatal("credential exposed in output")
		}
		var value map[string]any
		if err == nil {
			if decodeErr := json.Unmarshal(output.Bytes(), &value); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return value, err
	}
	catalog, err := run([]string{"list"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog["tools"].([]any)) != len(agentMCPTools())+len(browserMCPTools()) {
		t.Fatal("CLI did not expose the full catalog")
	}
	for _, name := range []string{"workass_get_chat_diagnostics", "workass_read_chat", "workass_browser_snapshot", "workass_apply_update", "workass_host_artifact", "workass_register_external_work"} {
		schema, err := run([]string{"list", name}, "")
		if err != nil || schema["name"] != name {
			t.Fatalf("missing schema %s", name)
		}
	}
	read, err := run([]string{"call", "workass_read_chat"}, `{"tab_id":"mcp-tab","chat_id":"mcp-chat","limit":1}`)
	if err != nil || read["chatId"] != "mcp-chat" {
		t.Fatalf("direct read failed: %v", err)
	}
	arguments := `{"tab_id":"mcp-tab","chat_id":"mcp-chat","title":"CLI renamed","operation_id":"cli-rename-once"}`
	diagnostics, err := run([]string{"call", "workass_get_chat_diagnostics"}, `{"tab_id":"mcp-tab","chat_id":"mcp-chat","limit":1}`)
	if err != nil || diagnostics["chatId"] != "mcp-chat" || diagnostics["available"] == nil || diagnostics["actor"] == nil {
		t.Fatalf("diagnostics must report measurement availability explicitly: %v %#v", err, diagnostics)
	}
	if _, err := run([]string{"call", "workass_rename_chat"}, arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := run([]string{"call", "workass_rename_chat"}, arguments); err != nil {
		t.Fatal(err)
	}
	if _, err := run([]string{"call", "workass_rename_chat"}, strings.Replace(arguments, "CLI renamed", "different", 1)); err == nil {
		t.Fatal("operation id allowed different arguments")
	}
	if _, err := run([]string{"call", "workass_rename_chat"}, `{"tab_id":"mcp-tab","chat_id":"mcp-chat","title":"missing id"}`); err == nil {
		t.Fatal("mutation accepted without operation_id")
	}
	state, _ := h.runtime.Snapshot("mcp-chat")
	if state.Presentation.Title != "CLI renamed" {
		t.Fatal("rejected call changed state")
	}
}

func TestToolsCLIBrowserImageAndArgumentsFile(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	dir := t.TempDir()
	controlFile := filepath.Join(dir, "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	h.handler.browserControlFile = controlFile
	const pixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aC0cAAAAASUVORK5CYII="
	h.handler.browserClient = &http.Client{Transport: browserRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		params := mapFromAnyMain(request["params"])
		if params["chatId"] != "mcp-chat" {
			t.Error("browser call lost exact chat")
		}
		clip := mapFromAnyMain(params["clip"])
		if params["mode"] != "clip" || clip["x"] != float64(1) || clip["height"] != float64(4) {
			t.Errorf("screenshot options were lost: %#v", params)
		}
		raw, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{
			"base64": pixel, "mimeType": "image/png", "metadata": map[string]any{
				"screenshot_id": "shot-cli-1", "mode": "clip", "tab_id": 23,
				"image": map[string]any{"width": 3, "height": 4},
			},
		}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	contextFile, _ := toolCLIContextFixture(t, h.handler)
	arguments := filepath.Join(dir, "arguments.json")
	_ = os.WriteFile(arguments, []byte(`{"mode":"clip","clip":{"x":1,"y":2,"width":3,"height":4}}`), 0o600)
	var output bytes.Buffer
	if err := runToolsCommand(context.Background(), []string{"--context", contextFile, "call", "workass_browser_screenshot", "--input", arguments}, strings.NewReader(""), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Result map[string]any `json:"result"`
		Images []struct {
			Path string `json:"path"`
		} `json:"images"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Images) != 1 {
		t.Fatal("missing screenshot path")
	}
	if result.Result["screenshot_id"] != "shot-cli-1" || result.Result["mode"] != "clip" || result.Result["tab_id"] != float64(23) {
		t.Fatalf("screenshot metadata was lost during CLI materialization: %#v", result.Result)
	}
	data, err := os.ReadFile(result.Images[0].Path)
	want, _ := base64.StdEncoding.DecodeString(pixel)
	if err != nil || !bytes.Equal(data, want) {
		t.Fatal("screenshot bytes changed")
	}
}

func TestToolsAPIRejectsMCPAndWrongOwner(t *testing.T) {
	h := newStatelessMCPTestHarness(t)
	for _, test := range []struct {
		path, body, owner, chat string
		want                    int
	}{
		{toolsPath, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, "mcp-owner", "mcp-chat", 400},
		{"/workass/mcp/agent", `{}`, "mcp-owner", "mcp-chat", 404},
		{"/workass/mcp/browser", `{}`, "mcp-owner", "mcp-chat", 404},
		{toolsPath, `{"name":"workass_list_chats","arguments":{}}`, "wrong", "mcp-chat", 401},
		{toolsPath, `{"name":"workass_list_chats","arguments":{}}`, "mcp-owner", "other-chat", 401},
		{toolsPath, `{"name":"workass_list_chats","arguments":{}} {}`, "mcp-owner", "mcp-chat", 400},
	} {
		request, _ := http.NewRequest(http.MethodPost, h.server.URL+test.path, strings.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer "+test.owner)
		request.Header.Set("X-Workass-Chat-ID", test.chat)
		request.Header.Set("X-Workass-Tab-ID", "mcp-tab")
		request.Header.Set("Content-Type", "application/json")
		reply, err := h.client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		reply.Body.Close()
		if reply.StatusCode != test.want {
			t.Fatalf("request %s got %d want %d", test.path, reply.StatusCode, test.want)
		}
	}
}

func TestToolsCLIEnvironmentDiscoveryAndExplicitOverride(t *testing.T) {
	path, _ := toolCLIContextFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"result":{"tools":[]}}`) }))
	t.Setenv("WORKASS_TOOL_CONTEXT", path)
	var output, diagnostics bytes.Buffer
	if err := runToolsCommand(context.Background(), []string{"list"}, strings.NewReader(""), &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if err := runToolsCommand(context.Background(), []string{"--context", filepath.Join(t.TempDir(), "missing"), "list"}, strings.NewReader(""), &output, &diagnostics); err == nil {
		t.Fatal("ignored explicit context override")
	}
	guide := filepath.Join(t.TempDir(), "guide.md")
	os.WriteFile(guide, []byte("fixture guide"), 0600)
	t.Setenv("WORKASS_TOOLS_GUIDE", guide)
	output.Reset()
	if err := runToolsCommand(context.Background(), []string{"guide"}, strings.NewReader(""), &output, &diagnostics); err != nil || output.String() != "fixture guide" {
		t.Fatal("guide not available", err)
	}
}

func TestToolsCLICrossProviderDelegationAndCrossChatMessaging(t *testing.T) {
	root := repoRoot(t)
	tracePath := filepath.Join(t.TempDir(), "cross-provider-mock-trace.jsonl")
	h := newStatelessMCPTestHarnessWithProviders(t, []acp.ProviderConfig{{
		ID: "custom", Command: "node", Args: []string{filepath.Join(root, "desktop", "acp", "mock-server.mjs")},
		CWD: root, Env: map[string]string{
			"WORKASS_MOCK_ACP_TRACE_FILE": tracePath,
			"WORKASS_MOCK_ACP_DELAY_MS":   "150",
		}, Enabled: true, Label: "Other fixture provider",
	}})
	contextFile, _ := toolCLIContextFixture(t, h.handler)
	t.Setenv("WORKASS_TOOL_CONTEXT", contextFile)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	call := func(name string, args map[string]any) (map[string]any, error) {
		raw, _ := json.Marshal(args)
		var output bytes.Buffer
		err := runToolsCommand(ctx, []string{"call", name}, bytes.NewReader(raw), &output, io.Discard)
		var result map[string]any
		if err == nil {
			err = json.Unmarshal(output.Bytes(), &result)
		}
		return result, err
	}
	mustCall := func(name string, args map[string]any) map[string]any {
		t.Helper()
		result, err := call(name, args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result
	}
	catalog := mustCall("workass_agent_catalog", map[string]any{})
	if catalog["schemaVersion"] != float64(2) {
		t.Fatal("missing live catalog")
	}
	spawn := mustCall("workass_spawn_subagent", map[string]any{
		"operation_id": "cross-provider-spawn", "provider_id": "custom", "model_id": "mock-deterministic", "mode_id": "ask",
		"task": "[mock:hold-until-steer] [mock:steer] harmless delegation fixture", "label": "cross-provider child",
	})
	id := toString(spawn["id"])
	if id == "" || spawn["providerId"] != "custom" {
		t.Fatal("spawn lost child selection")
	}
	listed := mustCall("workass_list_subagents", map[string]any{})
	rows := listed["subagents"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["providerId"] != "custom" {
		t.Fatal("cross-provider child is invisible or mislabeled")
	}
	state, _ := h.runtime.Snapshot("mcp-chat")
	origin := state.Background[id].Owner
	if string(state.Lanes[origin.LaneID].Identity.Realm.ProviderID) != "mock" {
		t.Fatal("child replaced its parent's origin lane")
	}

	// Wait once, while child startup proceeds. The mock trace is written as soon
	// as session/prompt reaches the provider, proving that live steering can now
	// reach the held prompt instead of being queued for its next turn.
	type result struct {
		value map[string]any
		err   error
	}
	waited := make(chan result, 1)
	go func() {
		v, err := call("workass_wait_subagents", map[string]any{"operation_id": "event-only-wait", "subagent_ids": []string{id}, "return_when": "all", "timeout_ms": -1})
		waited <- result{v, err}
	}()
	for {
		trace, err := os.ReadFile(tracePath)
		if err == nil && strings.Contains(string(trace), "harmless delegation fixture") {
			break
		}
		select {
		case got := <-waited:
			t.Fatalf("event-only wait woke before child prompt readiness: %v %#v", got.err, got.value)
		case <-ctx.Done():
			t.Fatal("child prompt did not reach the mock provider before the test deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case got := <-waited:
		t.Fatalf("event-only wait woke while the child prompt was held: %v %#v", got.err, got.value)
	default:
	}
	message := mustCall("workass_message_subagent", map[string]any{"operation_id": "cross-provider-message", "subagent_id": id, "message": "finish the harmless fixture"})
	if message["ok"] != true {
		t.Fatal("cross-provider messaging failed")
	}
	select {
	case got := <-waited:
		if got.err != nil || got.value["timedOut"] != false {
			t.Fatalf("event-only wait: %v %#v", got.err, got.value)
		}
		completed := got.value["completed"].([]any)
		if len(completed) != 1 || completed[0].(map[string]any)["status"] != "done" {
			t.Fatal("lost completion receipt")
		}
	case <-ctx.Done():
		t.Fatal("completion did not wake the coordinator")
	}
	receipts := mustCall("workass_list_subagent_receipts", map[string]any{})
	encoded, _ := json.Marshal(receipts)
	if !bytes.Contains(encoded, []byte(id)) || !bytes.Contains(encoded, []byte(`"providerId":"custom"`)) {
		t.Fatal("durable receipt lost child identity")
	}
	state, _ = h.runtime.Snapshot("mcp-chat")
	if state.Background[id].Owner != origin {
		t.Fatal("completion moved child ownership")
	}

	created := mustCall("workass_create_chat", map[string]any{"operation_id": "cross-chat-create", "title": "CLI destination"})
	tabID, chatID := toString(created["tabId"]), toString(created["chatId"])
	if tabID == "" || chatID == "" {
		t.Fatalf("create has no exact target: %#v", created)
	}
	args := map[string]any{"operation_id": "cross-chat-send", "tab_id": tabID, "chat_id": chatID, "message": "harmless cross-chat fixture"}
	mustCall("workass_send_chat_message", args)
	mustCall("workass_send_chat_message", args)
	waitProviderChatIdle(t, h.runtime, chatID, 5*time.Second)
	read := mustCall("workass_read_chat", map[string]any{"tab_id": tabID, "chat_id": chatID})
	matches := 0
	for _, raw := range read["messages"].([]any) {
		m := raw.(map[string]any)
		if m["role"] == "user" && m["content"] == "harmless cross-chat fixture" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("cross-chat send had %d visible owners", matches)
	}
}
