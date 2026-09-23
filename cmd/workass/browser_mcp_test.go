package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
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
			result = map[string]any{"mimeType": "image/png", "base64": "ZmFrZS1wbmc=", "metadata": map[string]any{"screenshot_id": "shot-1", "image": map[string]any{"width": 1440, "height": 900}}}
		}
		body, _ := json.Marshal(map[string]any{"id": payload["id"], "result": result})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	tools := browserMCPTools()
	if len(tools) != 15 {
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
	content := anySlice(mapFromAnyMain(screenshot)["content"])
	metadataText := ""
	for _, raw := range content {
		part := mapFromAnyMain(raw)
		if part["type"] == "text" {
			metadataText = browserString(part["text"])
		}
	}
	if !strings.Contains(string(encoded), `"type":"image"`) || !strings.Contains(string(encoded), `"data":"ZmFrZS1wbmc="`) || !strings.Contains(metadataText, `"screenshot_id":"shot-1"`) {
		t.Fatalf("screenshot response = %s", encoded)
	}
}

func TestBrowserMCPViewportAndNestedBatchArgumentsMapOnlySupportedNames(t *testing.T) {
	prepared, err := prepareBrowserMCPCall(browserMCPCallParams{
		Name:      "workass_browser_set_viewport",
		Arguments: map[string]any{"operation_id": "viewport-once", "tab_id": 42, "width": float64(1280), "height": float64(800)},
	})
	if err != nil || !prepared.Mutating || prepared.Method != "browser.setViewport" {
		t.Fatalf("viewport preparation = %#v, err=%v", prepared, err)
	}
	if prepared.Params["tabId"] != 42 || prepared.Params["width"] != float64(1280) || prepared.Params["height"] != float64(800) {
		t.Fatalf("viewport params = %#v", prepared.Params)
	}
	batch, err := prepareBrowserMCPCall(browserMCPCallParams{
		Name:      "workass_browser_batch",
		Arguments: map[string]any{"operation_id": "batch-once", "observe_after": true, "actions": []any{map[string]any{"action": "click", "element_ref": "el-a", "snapshot_id": "snap-a"}}},
	})
	if err != nil || !batch.Mutating {
		t.Fatalf("batch preparation = %#v, err=%v", batch, err)
	}
	actions := anySlice(batch.Params["actions"])
	if mapFromAnyMain(actions[0])["elementRef"] != "el-a" || mapFromAnyMain(actions[0])["snapshotId"] != "snap-a" || batch.Params["observeAfter"] != true {
		t.Fatalf("mapped batch params = %#v", batch.Params)
	}
}

func TestBrowserMCPTypePreservesEmptyAndWhitespaceReplacementText(t *testing.T) {
	for _, value := range []string{"", "   \n\t"} {
		prepared, err := prepareBrowserMCPCall(browserMCPCallParams{
			Name: "workass_browser_type",
			Arguments: map[string]any{
				"operation_id": "type-clear-once", "selector": "#editor", "text": value,
			},
		})
		if err != nil {
			t.Fatalf("text %q rejected: %v", value, err)
		}
		if prepared.Params["text"] != value {
			t.Fatalf("text changed from %q to %#v", value, prepared.Params["text"])
		}
	}
	for _, value := range []string{"", " \t"} {
		prepared, err := prepareBrowserMCPCall(browserMCPCallParams{
			Name: "workass_browser_batch",
			Arguments: map[string]any{
				"operation_id": "batch-clear-once",
				"actions":      []any{map[string]any{"action": "type", "selector": "#editor", "text": value}},
			},
		})
		if err != nil {
			t.Fatalf("batch text %q rejected: %v", value, err)
		}
		actions := anySlice(prepared.Params["actions"])
		if mapFromAnyMain(actions[0])["text"] != value {
			t.Fatalf("batch text changed from %q to %#v", value, mapFromAnyMain(actions[0])["text"])
		}
	}
}

func TestBrowserMCPTargetValidationTreatsNullSelectorAsSupplied(t *testing.T) {
	params := map[string]any{"selector": nil, "elementRef": "el-a", "snapshotId": "snap-a"}
	if err := validateBrowserClickTarget(params, "browser click"); err == nil {
		t.Fatal("click accepted null selector alongside an element target")
	}
	if err := validateBrowserElementTarget(params, "browser type"); err == nil {
		t.Fatal("type accepted null selector alongside an element target")
	}
}

func TestBrowserMCPRejectsUnsupportedOrInvalidFieldsBeforeBrowserDispatch(t *testing.T) {
	dir := t.TempDir()
	controlFile := filepath.Join(dir, "browser-control.json")
	if err := os.WriteFile(controlFile, []byte(browserTestDescriptor), 0o600); err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	client := &http.Client{Transport: browserRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		dispatches++
		return nil, errors.New("invalid browser call reached the shell")
	})}
	tests := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"camel alias collision", "workass_browser_snapshot", map[string]any{"tab_id": 42, "tabId": 43}},
		{"internal owned chat", "workass_browser_snapshot", map[string]any{"chat_id": "other-chat"}},
		{"internal camel chat", "workass_browser_snapshot", map[string]any{"chatId": "other-chat"}},
		{"unknown argument", "workass_browser_snapshot", map[string]any{"frame_id": "frame-a"}},
		{"fractional width", "workass_browser_set_viewport", map[string]any{"operation_id": "bad-width-fraction", "width": 390.5, "height": 844}},
		{"out of range height", "workass_browser_set_viewport", map[string]any{"operation_id": "bad-height-range", "width": 390, "height": 2161}},
		{"wrong boolean type", "workass_browser_open", map[string]any{"operation_id": "bad-open-bool", "visible": "false"}},
		{"wrong options type", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-bool", "actions": []any{map[string]any{"action": "snapshot"}}, "observe_after": "true"}},
		{"numeric ref rejected", "workass_browser_click", map[string]any{"operation_id": "bad-click-ref", "element_ref": 12, "snapshot_id": "snap-a"}},
		{"coordinate NaN rejected", "workass_browser_click", map[string]any{"operation_id": "bad-click-nan", "x": math.NaN(), "y": float64(0), "screenshot_id": "shot-a"}},
		{"unknown screenshot option", "workass_browser_screenshot", map[string]any{"mode": "clip", "clip": map[string]any{"x": 0, "y": 0, "width": 10, "height": 10}, "deviceScaleFactor": 2}},
		{"invalid diagnostics limit", "workass_browser_diagnostics", map[string]any{"limit": 101}},
		{"invalid batch action after valid action", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-late-field", "actions": []any{
			map[string]any{"action": "click", "selector": "#save"},
			map[string]any{"action": "snapshot", "tabId": 44},
		}}},
		{"invalid batch action type", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-late-type", "actions": []any{
			map[string]any{"action": "snapshot"},
			map[string]any{"action": "type", "selector": "#input", "text": "x", "submit": "yes"},
		}}},
		{"invalid batch numeric target", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-late-numeric", "actions": []any{
			map[string]any{"action": "snapshot"},
			map[string]any{"action": "scroll", "x": "NaN"},
		}}},
		{"invalid batch ref type", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-late-ref", "actions": []any{
			map[string]any{"action": "snapshot"},
			map[string]any{"action": "click", "element_ref": 12, "snapshot_id": "snap-a"},
		}}},
		{"invalid null selector after valid action", "workass_browser_batch", map[string]any{"operation_id": "bad-batch-null-selector", "actions": []any{
			map[string]any{"action": "click", "selector": "#save"},
			map[string]any{"action": "click", "selector": nil, "element_ref": "el-a", "snapshot_id": "snap-a"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := dispatches
			result, err := callBrowserMCPTool(browserMCPCallParams{Name: test.tool, Arguments: test.args}, browserMCPOptions{
				ControlFile: controlFile, ChatID: "owned-chat", HTTPClient: client,
			}, client)
			if err != nil {
				t.Fatal(err)
			}
			if mapFromAnyMain(result)["isError"] != true {
				t.Fatalf("invalid call was accepted: %#v", result)
			}
			if dispatches != before {
				t.Fatalf("invalid call reached browser control (%d -> %d)", before, dispatches)
			}
		})
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

func TestBrowserMCPRejectsLegacyCamelOperationID(t *testing.T) {
	result, err := callBrowserMCPTool(browserMCPCallParams{
		Name: "workass_browser_click", Arguments: map[string]any{"operationId": "old", "selector": "#save"},
	}, browserMCPOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"isError":true`) || !strings.Contains(string(encoded), "operation_id") {
		t.Fatalf("legacy operation id result = %s", encoded)
	}
}
