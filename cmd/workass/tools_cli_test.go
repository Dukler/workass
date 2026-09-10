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
		raw, _ := json.Marshal(map[string]any{"id": request["id"], "result": map[string]any{"base64": pixel, "mimeType": "image/png"}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})}
	contextFile, _ := toolCLIContextFixture(t, h.handler)
	arguments := filepath.Join(dir, "arguments.json")
	_ = os.WriteFile(arguments, []byte(`{}`), 0o600)
	var output bytes.Buffer
	if err := runToolsCommand(context.Background(), []string{"--context", contextFile, "call", "workass_browser_screenshot", "--input", arguments}, strings.NewReader(""), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Images []struct {
			Path string `json:"path"`
		} `json:"images"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Images) != 1 {
		t.Fatal("missing screenshot path")
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
