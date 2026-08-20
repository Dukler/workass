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
	"time"
)

type browserRoundTripFunc func(*http.Request) (*http.Response, error)

const browserTestInstanceID = "0123456789abcdef0123456789abcdef"

const browserTestDescriptor = `{"version":2,"url":"http://127.0.0.1:43123/rpc","token":"test-control-value","pid":123,"instanceId":"` + browserTestInstanceID + `"}`

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

func TestBrowserMCPListsToolsAndRoutesProviderNeutralCalls(t *testing.T) {
	dir := t.TempDir()
	controlFile := filepath.Join(dir, "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
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
		if payload["instanceId"] != browserTestInstanceID {
			t.Fatalf("browser control instance = %#v", payload["instanceId"])
		}
		requests = append(requests, payload)
		result := map[string]any{"ok": true, "method": payload["method"]}
		if payload["method"] == "browser.screenshot" {
			result = map[string]any{"mimeType": "image/png", "base64": "ZmFrZS1wbmc="}
		}
		body, _ := json.Marshal(map[string]any{"id": payload["id"], "result": result})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	tools := browserMCPTools()
	if len(tools) != 11 {
		t.Fatalf("tool count = %d", len(tools))
	}
	snapshot, err := callBrowserMCPTool(browserMCPCallParams{
		Name: "workass_browser_snapshot", Arguments: map[string]any{"tab_id": 42},
	}, browserMCPOptions{ControlFile: controlFile, ChatID: "chat-a", HTTPClient: client}, client)
	if err != nil {
		t.Fatal(err)
	}
	screenshot, err := callBrowserMCPTool(browserMCPCallParams{
		Name: "workass_browser_screenshot", Arguments: map[string]any{"tab_id": 42},
	}, browserMCPOptions{ControlFile: controlFile, ChatID: "chat-a", HTTPClient: client}, client)
	if err != nil {
		t.Fatal(err)
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
	if mapFromAnyMain(snapshot)["isError"] == true {
		t.Fatalf("snapshot result = %#v", snapshot)
	}
	encoded, _ := json.Marshal(screenshot)
	if !strings.Contains(string(encoded), `"type":"image"`) || !strings.Contains(string(encoded), `"data":"ZmFrZS1wbmc="`) {
		t.Fatalf("screenshot response = %s", encoded)
	}
}

func TestBrowserMCPMutationCarriesOperationIdentityAndDigest(t *testing.T) {
	dir := t.TempDir()
	controlFile := filepath.Join(dir, "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["instanceId"] != browserTestInstanceID {
			t.Fatalf("browser control instance = %#v", payload["instanceId"])
		}
		body, _ := json.Marshal(map[string]any{
			"id": payload["id"], "operationId": payload["operationId"], "requestDigest": payload["requestDigest"],
			"receipt": true, "result": map[string]any{"clicked": true},
		})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	result, err := callBrowserMCPTool(browserMCPCallParams{
		Name: "workass_browser_click", Arguments: map[string]any{"operation_id": "agent-mcp:click-once", "tab_id": 42, "selector": "#save"},
	}, browserMCPOptions{
		ControlFile: controlFile, ChatID: "chat-a", HTTPClient: client,
	}, client)
	if err != nil {
		t.Fatal(err)
	}
	if mapFromAnyMain(result)["isError"] == true {
		t.Fatalf("mutation result = %#v", result)
	}
	if payload["operationId"] != "agent-mcp:click-once" || len(browserString(payload["requestDigest"])) != 64 {
		t.Fatalf("mutation identity = %#v", payload)
	}
}

func TestBrowserMCPReturnsToolErrorWhenBrowserIsUnavailable(t *testing.T) {
	client := &http.Client{Timeout: time.Second}
	response, err := callBrowserMCPTool(browserMCPCallParams{
		Name: "workass_browser_list", Arguments: map[string]any{},
	}, browserMCPOptions{ControlFile: filepath.Join(t.TempDir(), "missing.json"), HTTPClient: client}, client)
	if err != nil {
		t.Fatal(err)
	}
	result := mapFromAnyMain(response)
	if result["isError"] != true {
		t.Fatalf("result = %#v", result)
	}
}
