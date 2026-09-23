package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"workass/internal/agenttext"

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
	tabID := map[string]any{"type": "integer", "minimum": 1, "description": agenttext.Get("schema.browserMCPTools.01")}
	operationID := map[string]any{"type": "string", "description": agenttext.Get("schema.browserMCPTools.02")}
	tool := func(name, description string, inputSchema map[string]any, readOnly, idempotent bool) map[string]any {
		return map[string]any{
			"name": name, "description": description, "inputSchema": inputSchema,
			"annotations": map[string]any{
				"readOnlyHint": readOnly, "destructiveHint": false,
				"idempotentHint": idempotent, "openWorldHint": true,
			},
		}
	}
	selector := map[string]any{"type": "string"}
	elementRef := map[string]any{"type": "string", "description": agenttext.Get("schema.browserMCPTools.03")}
	snapshotID := map[string]any{"type": "string"}
	coordinates := map[string]any{"type": "number"}
	clip := object(map[string]any{
		"x": coordinates, "y": coordinates, "width": coordinates, "height": coordinates,
	}, "x", "y", "width", "height")
	actionItem := object(map[string]any{
		"action":   map[string]any{"type": "string", "enum": []string{"click", "type", "scroll", "key", "snapshot"}},
		"selector": selector, "element_ref": elementRef, "snapshot_id": snapshotID,
		"x": coordinates, "y": coordinates, "screenshot_id": map[string]any{"type": "string"},
		"text": map[string]any{"type": "string"}, "submit": map[string]any{"type": "boolean"},
		"key": map[string]any{"type": "string"},
	}, "action")
	return []map[string]any{
		tool("workass_browser_list", agenttext.Get("tools.workass_browser_list"), object(map[string]any{}), true, true),
		tool("workass_browser_open", agenttext.Get("tools.workass_browser_open"), object(map[string]any{"url": map[string]any{"type": "string"}, "visible": map[string]any{"type": "boolean"}, "operation_id": operationID}, "operation_id"), false, false),
		tool("workass_browser_navigate", agenttext.Get("tools.workass_browser_navigate"), object(map[string]any{"tab_id": tabID, "url": map[string]any{"type": "string"}, "operation_id": operationID}, "url", "operation_id"), false, false),
		tool("workass_browser_snapshot", agenttext.Get("tools.workass_browser_snapshot"), object(map[string]any{"tab_id": tabID}), true, true),
		tool("workass_browser_click", agenttext.Get("tools.workass_browser_click"), object(map[string]any{
			"tab_id": tabID, "selector": selector, "element_ref": elementRef, "snapshot_id": snapshotID,
			"x": coordinates, "y": coordinates, "screenshot_id": map[string]any{"type": "string"}, "operation_id": operationID,
		}, "operation_id"), false, false),
		tool("workass_browser_type", agenttext.Get("tools.workass_browser_type"), object(map[string]any{
			"tab_id": tabID, "selector": selector, "element_ref": elementRef, "snapshot_id": snapshotID,
			"text": map[string]any{"type": "string"}, "submit": map[string]any{"type": "boolean"}, "operation_id": operationID,
		}, "text", "operation_id"), false, false),
		tool("workass_browser_scroll", agenttext.Get("tools.workass_browser_scroll"), object(map[string]any{
			"tab_id": tabID, "x": coordinates, "y": coordinates, "element_ref": elementRef, "snapshot_id": snapshotID, "operation_id": operationID,
		}, "operation_id"), false, false),
		tool("workass_browser_key", agenttext.Get("tools.workass_browser_key"), object(map[string]any{"tab_id": tabID, "key": map[string]any{"type": "string"}, "operation_id": operationID}, "key", "operation_id"), false, false),
		tool("workass_browser_screenshot", agenttext.Get("tools.workass_browser_screenshot"), object(map[string]any{
			"tab_id": tabID, "mode": map[string]any{"type": "string", "enum": []string{"viewport", "full_page", "clip"}}, "clip": clip,
		}), true, true),
		tool("workass_browser_batch", agenttext.Get("tools.workass_browser_batch"), object(map[string]any{
			"tab_id": tabID, "actions": map[string]any{"type": "array", "minItems": 1, "maxItems": 20, "items": actionItem},
			"observe_after": map[string]any{"type": "boolean"}, "operation_id": operationID,
		}, "actions", "operation_id"), false, false),
		tool("workass_browser_history", agenttext.Get("tools.workass_browser_history"), object(map[string]any{"tab_id": tabID, "action": map[string]any{"type": "string", "enum": []string{"back", "forward", "reload"}}, "operation_id": operationID}, "action", "operation_id"), false, false),
		tool("workass_browser_set_viewport", agenttext.Get("tools.workass_browser_set_viewport"), object(map[string]any{
			"tab_id": tabID, "width": map[string]any{"type": "integer", "minimum": 320, "maximum": 3840},
			"height": map[string]any{"type": "integer", "minimum": 240, "maximum": 2160}, "operation_id": operationID,
		}, "width", "height", "operation_id"), false, false),
		tool("workass_browser_reset_viewport", agenttext.Get("tools.workass_browser_reset_viewport"), object(map[string]any{"tab_id": tabID, "operation_id": operationID}, "operation_id"), false, false),
		tool("workass_browser_wait", agenttext.Get("tools.workass_browser_wait"), object(map[string]any{
			"tab_id": tabID, "condition": map[string]any{"type": "string", "enum": []string{"dom_ready", "load", "element_visible", "element_hidden"}},
			"selector": selector, "timeout_ms": map[string]any{"type": "integer", "minimum": 0, "maximum": 15000},
		}, "condition"), true, true),
		tool("workass_browser_diagnostics", agenttext.Get("tools.workass_browser_diagnostics"), object(map[string]any{
			"tab_id": tabID, "after_sequence": map[string]any{"type": "integer", "minimum": 0},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
		}), true, true),
	}
}

func callBrowserMCPTool(call browserMCPCallParams, options browserMCPOptions, client *http.Client) (any, error) {
	prepared, err := prepareBrowserMCPCall(call)
	if err != nil {
		return browserMCPErrorResult(err.Error()), nil
	}
	operationID, operationErr := requiredToolOperationID(browserToolKind, call)
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
	method, mutating, screenshot, names, err := browserMCPToolBoundary(call.Name)
	if err != nil {
		return preparedBrowserMCPCall{}, err
	}
	if _, exists := call.Arguments["operationId"]; exists {
		return preparedBrowserMCPCall{}, errors.New("Workass tools use operation_id; operationId is not accepted")
	}
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	if _, exists := arguments["operation_id"]; exists {
		if !mutating {
			return preparedBrowserMCPCall{}, errors.New("operation_id is only accepted by browser mutations")
		}
		if _, ok := arguments["operation_id"].(string); !ok {
			return preparedBrowserMCPCall{}, errors.New("Workass operation_id must be a string")
		}
	} else if mutating {
		return preparedBrowserMCPCall{}, errors.New("mutating Workass tool requires a caller-stable operation_id")
	}
	params := make(map[string]any, len(arguments))
	for _, name := range sortedBrowserKeys(arguments) {
		if name == "operation_id" {
			continue
		}
		wireName, supported := names[name]
		if !supported {
			return preparedBrowserMCPCall{}, fmt.Errorf("unsupported %s argument: %s", call.Name, name)
		}
		params[wireName] = arguments[name]
	}
	if err := validateBrowserMCPParams(call.Name, params); err != nil {
		return preparedBrowserMCPCall{}, err
	}
	if call.Name == "workass_browser_batch" {
		actions, err := prepareBrowserBatchActions(params["actions"])
		if err != nil {
			return preparedBrowserMCPCall{}, err
		}
		params["actions"] = actions
	}
	if call.Name == "workass_browser_history" {
		method = "browser." + browserString(params["action"])
		delete(params, "action")
	}
	prepared := preparedBrowserMCPCall{Method: method, Params: params, Mutating: mutating, Screenshot: screenshot}
	return prepared, nil
}

func browserMCPToolBoundary(name string) (string, bool, bool, map[string]string, error) {
	// Only these public names are accepted. The values are the corresponding
	// frozen shell-control names; no generic case conversion is performed.
	fields := func(pairs ...string) map[string]string {
		out := make(map[string]string, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i]] = pairs[i+1]
		}
		return out
	}
	switch name {
	case "workass_browser_list":
		return "browser.list", false, false, fields(), nil
	case "workass_browser_open":
		return "browser.open", true, false, fields("url", "url", "visible", "visible"), nil
	case "workass_browser_navigate":
		return "browser.navigate", true, false, fields("tab_id", "tabId", "url", "url"), nil
	case "workass_browser_snapshot":
		return "browser.snapshot", false, false, fields("tab_id", "tabId"), nil
	case "workass_browser_click":
		return "browser.click", true, false, fields("tab_id", "tabId", "selector", "selector", "element_ref", "elementRef", "snapshot_id", "snapshotId", "x", "x", "y", "y", "screenshot_id", "screenshotId"), nil
	case "workass_browser_type":
		return "browser.type", true, false, fields("tab_id", "tabId", "selector", "selector", "element_ref", "elementRef", "snapshot_id", "snapshotId", "text", "text", "submit", "submit"), nil
	case "workass_browser_scroll":
		return "browser.scroll", true, false, fields("tab_id", "tabId", "x", "x", "y", "y", "element_ref", "elementRef", "snapshot_id", "snapshotId"), nil
	case "workass_browser_key":
		return "browser.key", true, false, fields("tab_id", "tabId", "key", "key"), nil
	case "workass_browser_screenshot":
		return "browser.screenshot", false, true, fields("tab_id", "tabId", "mode", "mode", "clip", "clip"), nil
	case "workass_browser_batch":
		return "browser.batch", true, false, fields("tab_id", "tabId", "actions", "actions", "observe_after", "observeAfter"), nil
	case "workass_browser_history":
		return "", true, false, fields("tab_id", "tabId", "action", "action"), nil
	case "workass_browser_set_viewport":
		return "browser.setViewport", true, false, fields("tab_id", "tabId", "width", "width", "height", "height"), nil
	case "workass_browser_reset_viewport":
		return "browser.resetViewport", true, false, fields("tab_id", "tabId"), nil
	case "workass_browser_wait":
		return "browser.wait", false, false, fields("tab_id", "tabId", "condition", "condition", "selector", "selector", "timeout_ms", "timeoutMs"), nil
	case "workass_browser_diagnostics":
		return "browser.diagnostics", false, false, fields("tab_id", "tabId", "after_sequence", "afterSequence", "limit", "limit"), nil
	default:
		return "", false, false, nil, errors.New("unknown browser tool: " + name)
	}
}

func validateBrowserMCPParams(name string, params map[string]any) error {
	if value, present := params["tabId"]; present {
		if err := validateBrowserInteger(value, "tab_id", 1, 1<<53-1); err != nil {
			return err
		}
	}
	stringField := func(key string, required bool) error {
		value, present := params[key]
		if !present {
			if required {
				return fmt.Errorf("%s is required", key)
			}
			return nil
		}
		text, ok := value.(string)
		if !ok || (required && strings.TrimSpace(text) == "") {
			return fmt.Errorf("%s must be a non-empty string", key)
		}
		return nil
	}
	boolField := func(key string) error {
		if value, present := params[key]; present {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("%s must be a boolean", key)
			}
		}
		return nil
	}
	numberField := func(key string) error {
		if value, present := params[key]; present {
			if _, ok := browserFiniteNumber(value); !ok {
				return fmt.Errorf("%s must be a finite number", key)
			}
		}
		return nil
	}
	for _, key := range []string{"selector", "elementRef", "snapshotId", "screenshotId", "url", "key", "text", "condition", "mode", "action"} {
		if _, present := params[key]; present {
			if err := stringField(key, false); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{"visible", "submit", "observeAfter"} {
		if err := boolField(key); err != nil {
			return err
		}
	}
	for _, key := range []string{"x", "y"} {
		if err := numberField(key); err != nil {
			return err
		}
	}
	switch name {
	case "workass_browser_navigate":
		return stringField("url", true)
	case "workass_browser_click":
		return validateBrowserClickTarget(params, "browser click")
	case "workass_browser_type":
		if _, present := params["text"]; !present {
			return errors.New("text is required")
		}
		if _, ok := params["text"].(string); !ok {
			return errors.New("text must be a string")
		}
		return validateBrowserElementTarget(params, "browser type")
	case "workass_browser_scroll":
		_, err := validateOptionalBrowserElementRef(params, "browser scroll")
		return err
	case "workass_browser_key":
		return stringField("key", true)
	case "workass_browser_screenshot":
		mode := browserString(params["mode"])
		if mode != "" && mode != "viewport" && mode != "full_page" && mode != "clip" {
			return errors.New("mode must be viewport, full_page, or clip")
		}
		clip, hasClip := params["clip"]
		if mode == "clip" && !hasClip {
			return errors.New("clip is required when mode is clip")
		}
		if mode != "clip" && hasClip {
			return errors.New("clip is only accepted when mode is clip")
		}
		if hasClip {
			return validateBrowserClip(clip)
		}
	case "workass_browser_batch":
		if err := boolField("observeAfter"); err != nil {
			return err
		}
		if _, err := browserActionsValue(params["actions"]); err != nil {
			return err
		}
	case "workass_browser_history":
		action := browserString(params["action"])
		if action != "back" && action != "forward" && action != "reload" {
			return errors.New("action must be back, forward, or reload")
		}
	case "workass_browser_set_viewport":
		if err := validateBrowserInteger(params["width"], "width", 320, 3840); err != nil {
			return err
		}
		if err := validateBrowserInteger(params["height"], "height", 240, 2160); err != nil {
			return err
		}
	case "workass_browser_wait":
		condition := browserString(params["condition"])
		switch condition {
		case "dom_ready", "load":
			if _, present := params["selector"]; present {
				return errors.New("selector is only accepted for element wait conditions")
			}
		case "element_visible", "element_hidden":
			if err := stringField("selector", true); err != nil {
				return errors.New("selector is required for element wait conditions")
			}
		default:
			return errors.New("condition must be dom_ready, load, element_visible, or element_hidden")
		}
		if timeout, present := params["timeoutMs"]; present {
			if err := validateBrowserInteger(timeout, "timeout_ms", 0, 15000); err != nil {
				return err
			}
		}
	case "workass_browser_diagnostics":
		if sequence, present := params["afterSequence"]; present {
			if err := validateBrowserInteger(sequence, "after_sequence", 0, 1<<53-1); err != nil {
				return err
			}
		}
		if limit, present := params["limit"]; present {
			if err := validateBrowserInteger(limit, "limit", 1, 100); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBrowserInteger(value any, field string, minimum, maximum float64) error {
	number, ok := browserFiniteNumber(value)
	if !ok || math.Trunc(number) != number || number < minimum || number > maximum {
		if strings.Contains(field, "width") {
			return errors.New("width must be an integer from 320 to 3840")
		}
		if strings.Contains(field, "height") {
			return errors.New("height must be an integer from 240 to 2160")
		}
		return fmt.Errorf("%s must be an integer from %s to %s", field, strconv.FormatFloat(minimum, 'f', -1, 64), strconv.FormatFloat(maximum, 'f', -1, 64))
	}
	return nil
}

func browserFiniteNumber(value any) (float64, bool) {
	var number float64
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	case float64:
		number = typed
	case float32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case int8:
		number = float64(typed)
	case int16:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int64:
		number = float64(typed)
	case uint:
		number = float64(typed)
	case uint8:
		number = float64(typed)
	case uint16:
		number = float64(typed)
	case uint32:
		number = float64(typed)
	case uint64:
		number = float64(typed)
	default:
		return 0, false
	}
	return number, !math.IsNaN(number) && !math.IsInf(number, 0)
}

func validateBrowserClickTarget(params map[string]any, label string) error {
	selector := params["selector"]
	_, hasSelector := params["selector"]
	ref, hasRef := params["elementRef"]
	snapshot, hasSnapshot := params["snapshotId"]
	x, hasX := params["x"]
	y, hasY := params["y"]
	shot, hasShot := params["screenshotId"]
	selectorTarget := hasSelector
	refTarget := hasRef || hasSnapshot
	coordinateTarget := hasX || hasY || hasShot
	forms := 0
	if selectorTarget {
		if text, ok := selector.(string); !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s selector must be a non-empty string", label)
		}
		forms++
	}
	if refTarget {
		refText, refString := ref.(string)
		snapshotText, snapshotString := snapshot.(string)
		if !hasRef || !hasSnapshot || !refString || !snapshotString || strings.TrimSpace(refText) == "" || strings.TrimSpace(snapshotText) == "" {
			return fmt.Errorf("%s requires element_ref with snapshot_id", label)
		}
		forms++
	}
	if coordinateTarget {
		shotText, shotString := shot.(string)
		if !hasX || !hasY || !hasShot || !shotString || strings.TrimSpace(shotText) == "" {
			return fmt.Errorf("%s screenshot coordinates require x, y, and screenshot_id", label)
		}
		xNumber, xFinite := browserFiniteNumber(x)
		if !xFinite {
			return fmt.Errorf("%s x must be a finite number", label)
		}
		yNumber, yFinite := browserFiniteNumber(y)
		if !yFinite {
			return fmt.Errorf("%s y must be a finite number", label)
		}
		if xNumber < 0 || yNumber < 0 {
			return fmt.Errorf("%s screenshot coordinates must be zero or greater", label)
		}
		forms++
	}
	if forms != 1 {
		return fmt.Errorf("%s requires exactly one selector, element_ref with snapshot_id, or screenshot coordinate target", label)
	}
	return nil
}

func validateBrowserElementTarget(params map[string]any, label string) error {
	selector := params["selector"]
	_, hasSelector := params["selector"]
	ref, hasRef := params["elementRef"]
	snapshot, hasSnapshot := params["snapshotId"]
	selectorTarget := hasSelector
	refTarget := hasRef || hasSnapshot
	if selectorTarget {
		if text, ok := selector.(string); !ok || strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s selector must be a non-empty string", label)
		}
	}
	if refTarget {
		refText, refString := ref.(string)
		snapshotText, snapshotString := snapshot.(string)
		if !hasRef || !hasSnapshot || !refString || !snapshotString || strings.TrimSpace(refText) == "" || strings.TrimSpace(snapshotText) == "" {
			return fmt.Errorf("%s requires element_ref with snapshot_id", label)
		}
	}
	if selectorTarget == refTarget {
		return fmt.Errorf("%s requires selector or element_ref with snapshot_id", label)
	}
	return nil
}

func validateBrowserClip(value any) error {
	clip, ok := value.(map[string]any)
	if !ok {
		return errors.New("clip must be an object with x, y, width, and height")
	}
	for _, field := range sortedBrowserKeys(clip) {
		if field != "x" && field != "y" && field != "width" && field != "height" {
			return fmt.Errorf("clip has unsupported field: %s", field)
		}
	}
	for _, field := range []string{"x", "y", "width", "height"} {
		number, present := clip[field]
		value, finite := browserFiniteNumber(number)
		if !present || !finite {
			return fmt.Errorf("clip %s must be a finite number", field)
		}
		if (field == "x" || field == "y") && value < 0 {
			return fmt.Errorf("clip %s must be zero or greater", field)
		}
		if (field == "width" || field == "height") && value <= 0 {
			return fmt.Errorf("clip %s must be greater than zero", field)
		}
	}
	return nil
}

func browserActionsValue(value any) ([]any, error) {
	actions, ok := value.([]any)
	if !ok || len(actions) < 1 || len(actions) > 20 {
		return nil, errors.New("browser batch requires 1-20 actions")
	}
	return actions, nil
}

func prepareBrowserBatchActions(value any) ([]any, error) {
	actions, err := browserActionsValue(value)
	if err != nil {
		return nil, err
	}
	prepared := make([]any, len(actions))
	for index, raw := range actions {
		action, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("browser batch action %d must be an object", index)
		}
		kind, ok := action["action"].(string)
		if !ok {
			return nil, fmt.Errorf("browser batch action %d requires action", index)
		}
		fields := map[string]string{"action": "action"}
		switch kind {
		case "click":
			fields["selector"], fields["element_ref"], fields["snapshot_id"] = "selector", "elementRef", "snapshotId"
			fields["x"], fields["y"], fields["screenshot_id"] = "x", "y", "screenshotId"
		case "type":
			fields["selector"], fields["element_ref"], fields["snapshot_id"] = "selector", "elementRef", "snapshotId"
			fields["text"], fields["submit"] = "text", "submit"
		case "scroll":
			fields["x"], fields["y"], fields["element_ref"], fields["snapshot_id"] = "x", "y", "elementRef", "snapshotId"
		case "key":
			fields["key"] = "key"
		case "snapshot":
		default:
			return nil, fmt.Errorf("unsupported browser batch action at index %d: %s", index, kind)
		}
		converted := make(map[string]any, len(action))
		for _, field := range sortedBrowserKeys(action) {
			wireName, supported := fields[field]
			if !supported {
				return nil, fmt.Errorf("browser batch action %d has unsupported field: %s", index, field)
			}
			converted[wireName] = action[field]
		}
		if err := validateBrowserBatchAction(kind, converted, index); err != nil {
			return nil, err
		}
		prepared[index] = converted
	}
	return prepared, nil
}

func validateBrowserBatchAction(kind string, action map[string]any, index int) error {
	switch kind {
	case "click":
		return validateBrowserMCPParams("workass_browser_click", action)
	case "type":
		return validateBrowserMCPParams("workass_browser_type", action)
	case "scroll":
		return validateBrowserMCPParams("workass_browser_scroll", action)
	case "key":
		return validateBrowserMCPParams("workass_browser_key", action)
	case "snapshot":
		return nil
	default:
		return fmt.Errorf("unsupported browser batch action at index %d: %s", index, kind)
	}
}

func validateOptionalBrowserElementRef(params map[string]any, label string) (bool, error) {
	ref, hasRef := params["elementRef"]
	snapshot, hasSnapshot := params["snapshotId"]
	if hasRef != hasSnapshot {
		return false, fmt.Errorf("%s element_ref requires snapshot_id", label)
	}
	if hasRef {
		refText, refString := ref.(string)
		snapshotText, snapshotString := snapshot.(string)
		if !refString || !snapshotString || strings.TrimSpace(refText) == "" || strings.TrimSpace(snapshotText) == "" {
			return false, fmt.Errorf("%s requires non-empty element_ref and snapshot_id", label)
		}
	}
	return hasRef, nil
}

func sortedBrowserKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
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
			return browserMCPErrorResult("browser screenshot returned no image; the owned page may be blank, detached, or still rendering"), nil
		}
		text := "Captured this chat's Workass browser tab."
		if metadata, ok := item["metadata"]; ok && metadata != nil {
			encoded, _ := json.Marshal(redactValue(metadata))
			text = string(encoded)
		}
		return map[string]any{"content": []any{
			map[string]any{"type": "image", "data": data, "mimeType": mimeType},
			map[string]any{"type": "text", "text": text},
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
