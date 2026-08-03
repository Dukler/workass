package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type browserRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn browserRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func TestDefaultBrowserControlFileUsesStateDirectoryProfile(t *testing.T) {
	t.Setenv("WORKASS_BROWSER_CONTROL_FILE", "")
	dataRoot := t.TempDir()
	stateDir := filepath.Join(dataRoot, "state")
	want := filepath.Join(dataRoot, "run", "browser-control.json")
	if got := defaultBrowserControlFile(stateDir); got != want {
		t.Fatalf("default browser control file = %q, want %q", got, want)
	}
}

func TestDefaultBrowserControlFileEnvironmentOverrideWins(t *testing.T) {
	want := filepath.Join(t.TempDir(), "prod", "run", "browser-control.json")
	t.Setenv("WORKASS_BROWSER_CONTROL_FILE", want)
	if got := defaultBrowserControlFile(filepath.Join(t.TempDir(), "state")); got != want {
		t.Fatalf("default browser control file = %q, want %q", got, want)
	}
}

func TestInvokeBrowserControlUsesLegacyOnlyWhenPrimaryIsMissing(t *testing.T) {
	t.Setenv("WORKASS_BROWSER_CONTROL_FILE", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyFile := filepath.Join(home, ".workass", "browser-control.json")
	if err := os.MkdirAll(filepath.Dir(legacyFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyFile, []byte(`{"version":1,"url":"http://legacy.invalid/rpc","token":"legacy-control-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "legacy.invalid" {
			t.Fatalf("control host = %q", request.URL.Host)
		}
		body := []byte(`{"result":{"source":"legacy"}}`)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	primaryFile := filepath.Join(t.TempDir(), "run", "browser-control.json")
	result, err := invokeBrowserControl(primaryFile, "browser.list", map[string]any{}, client)
	if err != nil {
		t.Fatalf("invoke with missing primary: %v", err)
	}
	if source := browserString(mapFromAnyMain(result)["source"]); source != "legacy" || calls != 1 {
		t.Fatalf("result = %#v calls = %d", result, calls)
	}

	if err := os.MkdirAll(filepath.Dir(primaryFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(primaryFile, []byte(`{"version":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = invokeBrowserControl(primaryFile, "browser.list", map[string]any{}, client)
	if err == nil || err.Error() != "Workass browser control descriptor is invalid" {
		t.Fatalf("invalid primary error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("legacy control called %d times; invalid primary must not fall back", calls)
	}
}

func TestBrowserMCPListsToolsAndRoutesProviderNeutralCalls(t *testing.T) {
	dir := t.TempDir()
	controlFile := filepath.Join(dir, "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(`{"version":1,"url":"http://workass-browser.invalid/rpc","token":"test-control-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	requests := []map[string]any{}
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-control-value" {
			t.Fatalf("authorization = %q", got)
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, payload)
		result := map[string]any{"ok": true, "method": payload["method"]}
		if payload["method"] == "browser.screenshot" {
			result = map[string]any{"mimeType": "image/png", "base64": "ZmFrZS1wbmc="}
		}
		body, _ := json.Marshal(map[string]any{"id": payload["id"], "result": result})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"workass_browser_snapshot","arguments":{"tab_id":42}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"workass_browser_screenshot","arguments":{"tab_id":42}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	if err := serveBrowserMCP(strings.NewReader(input), &output, browserMCPOptions{ControlFile: controlFile, ChatID: "chat-a", HTTPClient: client}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("responses = %d\n%s", len(lines), output.String())
	}
	var listed map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatal(err)
	}
	tools := mapFromAnyMain(listed["result"])["tools"].([]any)
	if len(tools) != 11 {
		t.Fatalf("tool count = %d", len(tools))
	}
	if len(requests) != 2 || requests[0]["method"] != "browser.snapshot" || requests[1]["method"] != "browser.screenshot" {
		t.Fatalf("requests = %#v", requests)
	}
	params := mapFromAnyMain(requests[0]["params"])
	if params["chatId"] != "chat-a" || browserString(params["tabId"]) != "42" {
		t.Fatalf("snapshot params = %#v", params)
	}
	if _, leaked := params["tab_id"]; leaked {
		t.Fatalf("wire-internal snake_case tab id leaked to the shell: %#v", params)
	}
	if strings.Contains(output.String(), "test-control-value") {
		t.Fatal("control credential leaked to MCP stdout")
	}
	if !strings.Contains(lines[3], `"type":"image"`) || !strings.Contains(lines[3], `"data":"ZmFrZS1wbmc="`) {
		t.Fatalf("screenshot response = %s", lines[3])
	}
}

func TestBrowserMCPStdoutStaysJSONRPCOnlyWhenBrowserIsUnavailable(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workass_browser_list","arguments":{}}}` + "\n"
	var output bytes.Buffer
	if err := serveBrowserMCP(strings.NewReader(input), &output, browserMCPOptions{ControlFile: filepath.Join(t.TempDir(), "missing.json")}); err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &response); err != nil {
		t.Fatalf("stdout is not JSON-RPC: %v\n%s", err, output.String())
	}
	result := mapFromAnyMain(response["result"])
	if result["isError"] != true {
		t.Fatalf("result = %#v", result)
	}
}
