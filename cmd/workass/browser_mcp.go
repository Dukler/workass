package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workass/internal/acp"
)

type browserControlDescriptor struct {
	Version    int    `json:"version"`
	URL        string `json:"url"`
	Token      string `json:"token"`
	PID        int    `json:"pid"`
	InstanceID string `json:"instanceId"`
}

type browserMCPCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type browserControlReply struct {
	Result        any    `json:"result"`
	Error         string `json:"error"`
	OperationID   string `json:"operationId"`
	RequestDigest string `json:"requestDigest"`
	Receipt       bool   `json:"receipt"`
}

func defaultBrowserControlFile(stateDirs ...string) string {
	if configured := strings.TrimSpace(os.Getenv("WORKASS_BROWSER_CONTROL_FILE")); configured != "" {
		return configured
	}
	if len(stateDirs) > 0 && strings.TrimSpace(stateDirs[0]) != "" {
		return filepath.Join(filepath.Dir(stateDirs[0]), "run", "browser-control.json")
	}
	return ""
}

type browserMCPOptions struct {
	ControlFile   string
	ChatID        string
	OperationID   string
	RequestDigest string
	HTTPClient    *http.Client
}

type preparedBrowserMCPCall struct {
	Method     string
	Params     map[string]any
	Mutating   bool
	Screenshot bool
}

func browserMCPTools() []map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	tabID := map[string]any{"type": "integer", "minimum": 1, "description": "Optional id of a Workass browser tab owned by this chat; defaults to this chat's browser."}
	operationID := map[string]any{"type": "string", "description": "Caller-stable logical operation id, distinct from the JSON-RPC transport id. Reuse it only for the same immutable request."}
	tool := func(name, description string, inputSchema map[string]any, readOnly, idempotent bool) map[string]any {
		return map[string]any{
			"name": name, "description": description, "inputSchema": inputSchema,
			"annotations": map[string]any{
				"readOnlyHint": readOnly, "destructiveHint": false,
				"idempotentHint": idempotent, "openWorldHint": true,
			},
		}
	}
	return []map[string]any{
		tool("workass_browser_list", "List Workass browser tabs owned by this chat.", object(map[string]any{}), true, true),
		tool("workass_browser_open", "Open this chat's Workass browser and optionally navigate it.", object(map[string]any{"url": map[string]any{"type": "string"}, "operation_id": operationID}, "operation_id"), false, false),
		tool("workass_browser_navigate", "Navigate this chat's Workass browser tab to an HTTP(S) URL.", object(map[string]any{"tab_id": tabID, "url": map[string]any{"type": "string"}, "operation_id": operationID}, "url", "operation_id"), false, false),
		tool("workass_browser_snapshot", "Read text and interactive elements from this chat's Workass browser.", object(map[string]any{"tab_id": tabID}), true, true),
		tool("workass_browser_click", "Click an element in this chat's Workass browser using a selector returned by snapshot.", object(map[string]any{"tab_id": tabID, "selector": map[string]any{"type": "string"}, "operation_id": operationID}, "selector", "operation_id"), false, false),
		tool("workass_browser_type", "Fill an input in this chat's Workass browser and optionally submit it.", object(map[string]any{"tab_id": tabID, "selector": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}, "submit": map[string]any{"type": "boolean"}, "operation_id": operationID}, "selector", "text", "operation_id"), false, false),
		tool("workass_browser_scroll", "Scroll this chat's Workass browser tab by pixel offsets.", object(map[string]any{"tab_id": tabID, "x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}, "operation_id": operationID}, "operation_id"), false, false),
		tool("workass_browser_key", "Send a keyboard key to this chat's Workass browser tab.", object(map[string]any{"tab_id": tabID, "key": map[string]any{"type": "string"}, "operation_id": operationID}, "key", "operation_id"), false, false),
		tool("workass_browser_screenshot", "Capture this chat's Workass browser tab as PNG.", object(map[string]any{"tab_id": tabID}), true, true),
		tool("workass_browser_batch", "Run 1-20 click, type, scroll, key, or snapshot actions against one browser tab owned by this chat.", object(map[string]any{"tab_id": tabID, "actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": map[string]any{"type": "object"}}, "operation_id": operationID}, "actions", "operation_id"), false, false),
		tool("workass_browser_history", "Navigate browser history or reload this chat's Workass browser tab.", object(map[string]any{"tab_id": tabID, "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload"}}, "operation_id": operationID}, "action", "operation_id"), false, false),
	}
}

func callBrowserMCPTool(call browserMCPCallParams, options browserMCPOptions, client *http.Client) (any, error) {
	prepared, err := prepareBrowserMCPCall(call)
	if err != nil {
		return browserMCPErrorResult(err.Error()), nil
	}
	operationID, operationErr := requiredStatelessMCPOperationID(browserMCPKind, call)
	if operationErr != nil {
		return browserMCPErrorResult(operationErr.Error()), nil
	}
	params := prepared.Params
	if options.ChatID != "" {
		params["chatId"] = options.ChatID
	}
	if prepared.Mutating {
		options.OperationID = string(operationID)
		digest := strings.TrimSpace(options.RequestDigest)
		if digest == "" {
			digest = browserMCPRequestDigest(prepared.Method, params)
		}
		reply, err := invokeBrowserControlMutation(options.ControlFile, prepared.Method, params, options.OperationID, digest, client)
		if err != nil {
			return browserMCPErrorResult(err.Error()), nil
		}
		if !reply.Receipt {
			return browserMCPErrorResult("browser mutation returned no durable receipt"), nil
		}
		return formatBrowserMCPReply(call, prepared, reply)
	}
	result, err := invokeBrowserControl(options.ControlFile, prepared.Method, params, client)
	if err != nil {
		return browserMCPErrorResult(err.Error()), nil
	}
	return formatBrowserMCPResult(call, prepared, result)
}

func prepareBrowserMCPCall(call browserMCPCallParams) (preparedBrowserMCPCall, error) {
	prepared := preparedBrowserMCPCall{Params: copyAnyMap(call.Arguments)}
	// MCP schemas are snake_case while the frozen shell control surface is
	// camelCase. Normalize at this boundary; otherwise an explicit tab choice is
	// silently ignored and the shell resolves its default tab instead.
	if tabID, exists := prepared.Params["tab_id"]; exists {
		prepared.Params["tabId"] = tabID
		delete(prepared.Params, "tab_id")
	}
	delete(prepared.Params, "operation_id")
	if _, exists := prepared.Params["operationId"]; exists {
		return preparedBrowserMCPCall{}, errors.New("MCP uses operation_id; operationId is not accepted")
	}
	switch call.Name {
	case "workass_browser_list":
		prepared.Method = "browser.list"
	case "workass_browser_open":
		prepared.Method, prepared.Mutating = "browser.open", true
	case "workass_browser_navigate":
		prepared.Method, prepared.Mutating = "browser.navigate", true
	case "workass_browser_snapshot":
		prepared.Method = "browser.snapshot"
	case "workass_browser_click":
		prepared.Method, prepared.Mutating = "browser.click", true
	case "workass_browser_type":
		prepared.Method, prepared.Mutating = "browser.type", true
	case "workass_browser_scroll":
		prepared.Method, prepared.Mutating = "browser.scroll", true
	case "workass_browser_key":
		prepared.Method, prepared.Mutating = "browser.key", true
	case "workass_browser_screenshot":
		prepared.Method, prepared.Screenshot = "browser.screenshot", false
	case "workass_browser_batch":
		prepared.Method, prepared.Mutating = "browser.batch", true
	case "workass_browser_history":
		action := browserString(prepared.Params["action"])
		if action != "back" && action != "forward" && action != "reload" {
			return preparedBrowserMCPCall{}, errors.New("action must be back, forward, or reload")
		}
		prepared.Method, prepared.Mutating = "browser."+action, true
		delete(prepared.Params, "action")
	default:
		return preparedBrowserMCPCall{}, errors.New("unknown browser tool: " + call.Name)
	}
	return prepared, nil
}

func browserMCPRequestDigest(method string, params map[string]any) string {
	payload := struct {
		Version uint32         `json:"version"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}{Version: 1, Method: strings.TrimSpace(method), Params: params}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(append([]byte("workass-browser-mcp-v1\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

func formatBrowserMCPResult(call browserMCPCallParams, prepared preparedBrowserMCPCall, result any) (any, error) {
	if prepared.Screenshot || call.Name == "workass_browser_screenshot" {
		item := mapFromAnyMain(result)
		data := browserString(item["base64"])
		mimeType := firstNonEmptyMain(browserString(item["mimeType"]), "image/png")
		if data == "" {
			// The shell names the cause when it can. This is the last resort, so
			// it still has to point somewhere: only the visible tab has a surface
			// to capture from.
			return browserMCPErrorResult("browser screenshot returned no image; only the visible browser tab can be captured — open the tab in its owning chat and retry"), nil
		}
		return map[string]any{"content": []any{
			map[string]any{"type": "image", "data": data, "mimeType": mimeType},
			map[string]any{"type": "text", "text": "Captured this chat's Workass browser tab."},
		}}, nil
	}
	encoded, _ := json.Marshal(redactValue(result))
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}}, nil
}

func formatBrowserMCPReply(call browserMCPCallParams, prepared preparedBrowserMCPCall, reply browserControlReply) (any, error) {
	if reply.Error != "" {
		return browserMCPErrorResult(reply.Error), nil
	}
	return formatBrowserMCPResult(call, prepared, reply.Result)
}

func browserMCPErrorResult(message string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": acp.RedactSensitiveText(message)}},
	}
}

func invokeBrowserControl(controlFile, method string, params map[string]any, client *http.Client) (any, error) {
	out, err := invokeBrowserControlRequest(controlFile, method, params, "", "", client)
	if err != nil {
		return nil, err
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	return out.Result, nil
}

func invokeBrowserControlMutation(controlFile, method string, params map[string]any, operationID, requestDigest string, client *http.Client) (browserControlReply, error) {
	return invokeBrowserControlRequest(controlFile, method, params, operationID, requestDigest, client)
}

func invokeBrowserControlReceipt(controlFile, operationID, requestDigest string, client *http.Client) (browserControlReply, error) {
	return invokeBrowserControlRequest(controlFile, "browser.receipt", map[string]any{}, operationID, requestDigest, client)
}

func probeBrowserControl(controlFile string, client *http.Client) error {
	result, err := invokeBrowserControl(controlFile, "browser.controlStatus", map[string]any{}, client)
	if err != nil {
		return err
	}
	status := mapFromAnyMain(result)
	if status["ready"] != true || status["controller"] != true || strings.TrimSpace(browserString(status["instanceId"])) == "" {
		return errors.New("Workass browser control is not ready")
	}
	return nil
}

func validBrowserControlDescriptor(descriptor browserControlDescriptor) bool {
	if descriptor.Version != 2 || strings.TrimSpace(descriptor.Token) == "" || descriptor.PID <= 0 ||
		!validHex32(strings.TrimSpace(descriptor.InstanceID)) {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(descriptor.URL))
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" ||
		parsed.Path != "/rpc" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return true
}

func validHex32(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func invokeBrowserControlRequest(controlFile, method string, params map[string]any, operationID, requestDigest string, client *http.Client) (browserControlReply, error) {
	data, err := os.ReadFile(controlFile)
	if err != nil {
		return browserControlReply{}, errors.New("Workass browser is not running")
	}
	var descriptor browserControlDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil || !validBrowserControlDescriptor(descriptor) {
		return browserControlReply{}, errors.New("Workass browser control descriptor is invalid")
	}
	payload := map[string]any{
		"id": time.Now().UnixNano(), "method": method, "params": params, "instanceId": descriptor.InstanceID,
	}
	if strings.TrimSpace(operationID) != "" {
		payload["operationId"] = strings.TrimSpace(operationID)
	}
	if strings.TrimSpace(requestDigest) != "" {
		payload["requestDigest"] = strings.TrimSpace(requestDigest)
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, descriptor.URL, bytes.NewReader(body))
	if err != nil {
		return browserControlReply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+descriptor.Token)
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	reply, err := client.Do(req)
	if err != nil {
		return browserControlReply{}, errors.New("Workass browser control is unavailable")
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		return browserControlReply{}, fmt.Errorf("Workass browser control rejected the request (%d)", reply.StatusCode)
	}
	var out browserControlReply
	dec := json.NewDecoder(io.LimitReader(reply.Body, 16*1024*1024))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return browserControlReply{}, errors.New("Workass browser control returned invalid JSON")
	}
	return out, nil
}

func copyAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func browserString(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func firstNonEmptyMain(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
