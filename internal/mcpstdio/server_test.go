package mcpstdio

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workass/internal/tlscert"
)

func TestStdioModernRevisionPassesThroughToStatelessWorkassMCP(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var message map[string]any
		decoder := json.NewDecoder(request.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		method, _ := message["method"].(string)
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		if request.Header.Get("Authorization") != "Bearer owner-value" ||
			request.Header.Get("X-Workass-Chat-ID") != "chat-1" || request.Header.Get("X-Workass-Tab-ID") != "tab-1" {
			t.Errorf("attachment headers were not forwarded")
		}
		if request.Header.Get("MCP-Protocol-Version") != protocol2026 || request.Header.Get("Mcp-Method") != method {
			t.Errorf("protocol headers = %#v", request.Header)
		}
		params, _ := message["params"].(map[string]any)
		meta, _ := params["_meta"].(map[string]any)
		if meta["io.modelcontextprotocol/protocolVersion"] != protocol2026 {
			t.Errorf("request metadata = %#v", meta)
		}
		id := message["id"]
		var result map[string]any
		switch method {
		case "server/discover":
			result = map[string]any{
				"resultType": "complete", "supportedVersions": []string{protocol2026},
				"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
				"instructions": "fixture instructions",
				"_meta":        map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "workass-fixture", "version": "1"}},
			}
		case "tools/list":
			result = map[string]any{
				"resultType": "complete", "ttlMs": 1000, "cacheScope": "private",
				"tools": []any{map[string]any{"name": "fixture_tool", "inputSchema": map[string]any{"type": "object"}}},
			}
		case "tools/call":
			if request.Header.Get("Mcp-Name") != "fixture_tool" {
				t.Errorf("Mcp-Name = %q", request.Header.Get("Mcp-Name"))
			}
			result = map[string]any{
				"resultType": "complete", "content": []any{map[string]any{"type": "text", "text": "done"}},
				"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "workass-fixture", "version": "1"}},
			}
		default:
			t.Errorf("unexpected method %q", method)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	})
	httpServer := httptest.NewTLSServer(handler)
	t.Cleanup(httpServer.Close)
	endpoint, _ := url.Parse(httpServer.URL + "/workass/mcp/agent")
	bridge := &server{config: config{
		endpoint: endpoint, authorization: "Bearer owner-value", chatID: "chat-1", tabID: "tab-1", client: httpServer.Client(),
	}}

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fixture_tool","arguments":{"value":1},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
	}, "\n") + "\n"
	var stdout bytes.Buffer
	if err := bridge.serve(context.Background(), strings.NewReader(input), &stdout); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout lines = %d\n%s", len(lines), stdout.String())
	}
	responses := make([]map[string]any, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &responses[i]); err != nil {
			t.Fatalf("stdout line %d is not JSON-RPC: %v", i, err)
		}
	}
	discovered := responses[0]["result"].(map[string]any)
	if discovered["resultType"] != "complete" || discovered["supportedVersions"].([]any)[0] != protocol2026 {
		t.Fatalf("discover result = %#v", discovered)
	}
	listed := responses[1]["result"].(map[string]any)
	if listed["resultType"] != "complete" || len(listed["tools"].([]any)) != 1 {
		t.Fatalf("modern tools/list result = %#v", listed)
	}
	called := responses[2]["result"].(map[string]any)
	if called["resultType"] != "complete" || called["content"].([]any)[0].(map[string]any)["text"] != "done" {
		t.Fatalf("modern tools/call result = %#v", called)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(methods) != "[server/discover tools/list tools/call]" {
		t.Fatalf("forwarded methods = %v", methods)
	}
}

func TestStdio2026RevisionPassesOneSelfDescribingRequest(t *testing.T) {
	httpServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var message map[string]any
		_ = json.NewDecoder(request.Body).Decode(&message)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{
				"resultType": "complete", "supportedVersions": []string{protocol2026},
				"capabilities": map[string]any{"tools": map[string]any{}},
			},
		})
	}))
	t.Cleanup(httpServer.Close)
	endpoint, _ := url.Parse(httpServer.URL + "/workass/mcp/browser")
	bridge := &server{config: config{
		endpoint: endpoint, authorization: "Bearer owner-value", chatID: "chat-1", tabID: "tab-1", client: httpServer.Client(),
	}}
	input := `{"jsonrpc":"2.0","id":"discover","method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}` + "\n"
	var stdout bytes.Buffer
	if err := bridge.serve(context.Background(), strings.NewReader(input), &stdout); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(stdout.Bytes())) {
		t.Fatalf("modern response = %q", stdout.String())
	}
}

func TestStdioRejectsUnsupportedInitializeRevision(t *testing.T) {
	bridge := &server{}
	response, emit := bridge.handle(context.Background(), []byte(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"old-fixture","version":"1"}}}`,
	))
	if !emit {
		t.Fatal("modern initialize emitted no response")
	}
	var message map[string]any
	if err := json.Unmarshal(response, &message); err != nil {
		t.Fatalf("modern initialize response is invalid JSON-RPC: %v", err)
	}
	rpcErr, _ := message["error"].(map[string]any)
	if code, _ := rpcErr["code"].(float64); int(code) != -32022 {
		t.Fatalf("modern initialize response = %#v", message)
	}
}

func TestServeEnvironmentTrustsOnlyPinnedMCPIdentity(t *testing.T) {
	stateDir := t.TempDir()
	root, err := tlscert.Ensure(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tlscert.IssueLoopbackServerCertificate(root, "mcp.localhost")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13})
	httpServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var message map[string]any
		_ = json.NewDecoder(request.Body).Decode(&message)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": message["id"], "result": map[string]any{
				"resultType": "complete", "supportedVersions": []string{protocol2026}, "capabilities": map[string]any{"tools": map[string]any{}},
				"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "pinned", "version": "1"}},
			},
		})
	})}
	go func() { _ = httpServer.Serve(tlsListener) }()
	t.Cleanup(func() { _ = httpServer.Close() })
	port := listener.Addr().(*net.TCPAddr).Port
	environment := map[string]string{
		envEndpoint:        fmt.Sprintf("https://mcp.localhost:%d/workass/mcp/agent", port),
		envCACertFile:      filepath.Join(stateDir, tlscert.CertFileName),
		envOwnerCredential: "Bearer owner-value",
		envChatID:          "chat-1",
		envTabID:           "tab-1",
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}` + "\n"
	var stdout bytes.Buffer
	if err := ServeEnvironment(context.Background(), strings.NewReader(input), &stdout, func(key string) string { return environment[key] }); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(bytes.TrimSpace(stdout.Bytes())) || strings.Contains(stdout.String(), "owner-value") {
		t.Fatalf("stdio output = %q", stdout.String())
	}
}

func TestEnvironmentRejectsUnpinnedOrIncompleteConfiguration(t *testing.T) {
	environment := map[string]string{
		envEndpoint:        "http://127.0.0.1:8788/workass/mcp/agent",
		envOwnerCredential: "Bearer should-not-appear",
		envChatID:          "chat-1",
		envTabID:           "tab-1",
	}
	_, err := environmentConfig(func(key string) string { return environment[key] })
	if err == nil || strings.Contains(err.Error(), "should-not-appear") {
		t.Fatalf("configuration error = %v", err)
	}
}
