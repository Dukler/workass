package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type browserControlDescriptor struct {
	Version int    `json:"version"`
	URL     string `json:"url"`
	Token   string `json:"token"`
}

type browserMCPCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type browserControlReply struct {
	Result any    `json:"result"`
	Error  string `json:"error"`
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

func legacyBrowserControlFile() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".workass", "browser-control.json")
	}
	return filepath.Join(home, ".workass", "browser-control.json")
}

type browserMCPOptions struct {
	ControlFile string
	ChatID      string
	HTTPClient  *http.Client
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
		tool("workass_browser_open", "Open this chat's Workass browser and optionally navigate it.", object(map[string]any{"url": map[string]any{"type": "string"}}), false, false),
		tool("workass_browser_navigate", "Navigate this chat's Workass browser tab to an HTTP(S) URL.", object(map[string]any{"tab_id": tabID, "url": map[string]any{"type": "string"}}, "url"), false, false),
		tool("workass_browser_snapshot", "Read text and interactive elements from this chat's Workass browser.", object(map[string]any{"tab_id": tabID}), true, true),
		tool("workass_browser_click", "Click an element in this chat's Workass browser using a selector returned by snapshot.", object(map[string]any{"tab_id": tabID, "selector": map[string]any{"type": "string"}}, "selector"), false, false),
		tool("workass_browser_type", "Fill an input in this chat's Workass browser and optionally submit it.", object(map[string]any{"tab_id": tabID, "selector": map[string]any{"type": "string"}, "text": map[string]any{"type": "string"}, "submit": map[string]any{"type": "boolean"}}, "selector", "text"), false, false),
		tool("workass_browser_scroll", "Scroll this chat's Workass browser tab by pixel offsets.", object(map[string]any{"tab_id": tabID, "x": map[string]any{"type": "number"}, "y": map[string]any{"type": "number"}}), false, false),
		tool("workass_browser_key", "Send a keyboard key to this chat's Workass browser tab.", object(map[string]any{"tab_id": tabID, "key": map[string]any{"type": "string"}}, "key"), false, false),
		tool("workass_browser_screenshot", "Capture this chat's Workass browser tab as PNG.", object(map[string]any{"tab_id": tabID}), true, true),
		tool("workass_browser_batch", "Run 1-20 click, type, scroll, key, or snapshot actions against one browser tab owned by this chat.", object(map[string]any{"tab_id": tabID, "actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": map[string]any{"type": "object"}}}, "actions"), false, false),
		tool("workass_browser_history", "Navigate browser history or reload this chat's browser tab.", object(map[string]any{"tab_id": tabID, "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload"}}}, "action"), false, false),
	}
}

func callBrowserMCPTool(call browserMCPCallParams, options browserMCPOptions, client *http.Client) (any, error) {
	method := ""
	params := copyAnyMap(call.Arguments)
	// MCP schemas are snake_case while the frozen shell control surface is
	// camelCase. Normalize at this boundary; otherwise an explicit tab choice is
	// silently ignored and the shell resolves its default tab instead.
	if tabID, exists := params["tab_id"]; exists {
		params["tabId"] = tabID
		delete(params, "tab_id")
	}
	switch call.Name {
	case "workass_browser_list":
		method = "browser.list"
	case "workass_browser_open":
		method = "browser.open"
	case "workass_browser_navigate":
		method = "browser.navigate"
	case "workass_browser_snapshot":
		method = "browser.snapshot"
	case "workass_browser_click":
		method = "browser.click"
	case "workass_browser_type":
		method = "browser.type"
	case "workass_browser_scroll":
		method = "browser.scroll"
	case "workass_browser_key":
		method = "browser.key"
	case "workass_browser_screenshot":
		method = "browser.screenshot"
	case "workass_browser_batch":
		method = "browser.batch"
	case "workass_browser_history":
		action := browserString(params["action"])
		if action != "back" && action != "forward" && action != "reload" {
			return browserMCPErrorResult("action must be back, forward, or reload"), nil
		}
		method = "browser." + action
		delete(params, "action")
	default:
		return browserMCPErrorResult("unknown browser tool: " + call.Name), nil
	}
	if options.ChatID != "" {
		params["chatId"] = options.ChatID
	}
	result, err := invokeBrowserControl(options.ControlFile, method, params, client)
	if err != nil {
		return browserMCPErrorResult(err.Error()), nil
	}
	if call.Name == "workass_browser_screenshot" {
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

func browserMCPErrorResult(message string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": message}},
	}
}

func invokeBrowserControl(controlFile, method string, params map[string]any, client *http.Client) (any, error) {
	data, err := os.ReadFile(controlFile)
	if errors.Is(err, os.ErrNotExist) {
		legacyFile := legacyBrowserControlFile()
		if filepath.Clean(controlFile) != filepath.Clean(legacyFile) {
			data, err = os.ReadFile(legacyFile)
		}
	}
	if err != nil {
		return nil, errors.New("Workass browser is not running")
	}
	var descriptor browserControlDescriptor
	if err := json.Unmarshal(data, &descriptor); err != nil || descriptor.Version != 1 || descriptor.URL == "" || descriptor.Token == "" {
		return nil, errors.New("Workass browser control descriptor is invalid")
	}
	body, _ := json.Marshal(map[string]any{"id": time.Now().UnixNano(), "method": method, "params": params})
	req, err := http.NewRequest(http.MethodPost, descriptor.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+descriptor.Token)
	req.Header.Set("Content-Type", "application/json")
	reply, err := client.Do(req)
	if err != nil {
		return nil, errors.New("Workass browser control is unavailable")
	}
	defer reply.Body.Close()
	if reply.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Workass browser control rejected the request (%d)", reply.StatusCode)
	}
	var out browserControlReply
	dec := json.NewDecoder(io.LimitReader(reply.Body, 16*1024*1024))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, errors.New("Workass browser control returned invalid JSON")
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	return out.Result, nil
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
