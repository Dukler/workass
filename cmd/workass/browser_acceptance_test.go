package main

// Acceptance tests for the review of the webview browser branch, written by
// the reviewer apart from the builder's own tests: main's browser tool
// contracts hold (parity), and the tool text says what each tool returns and
// how it fails, without mentioning thinking or reasoning.

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"workass/internal/agenttext"
)

type mainBrowserTool struct {
	method     string
	required   []string
	properties []string
	readOnly   bool
}

// The browser tools as main (13ad08f5) declares them.
var mainBrowserTools = map[string]mainBrowserTool{
	"workass_browser_list":     {"browser.list", nil, nil, true},
	"workass_browser_open":     {"browser.open", []string{"operation_id"}, []string{"url", "visible", "operation_id"}, false},
	"workass_browser_navigate": {"browser.navigate", []string{"url", "operation_id"}, []string{"tab_id", "url", "operation_id"}, false},
	"workass_browser_snapshot": {"browser.snapshot", nil, []string{"tab_id", "format", "since_snapshot_id"}, true},
	"workass_browser_read": {"browser.read", nil, []string{"tab_id", "offset", "max_bytes", "selector", "element_ref", "frame_ref",
		"snapshot_id", "include_frames"}, true},
	"workass_browser_click": {"browser.click", []string{"operation_id"}, []string{"tab_id", "selector", "element_ref", "snapshot_id", "x", "y",
		"screenshot_id", "operation_id"}, false},
	"workass_browser_type": {"browser.type", []string{"text", "operation_id"}, []string{"tab_id", "selector", "element_ref", "snapshot_id",
		"text", "submit", "operation_id"}, false},
	"workass_browser_scroll":         {"browser.scroll", []string{"operation_id"}, []string{"tab_id", "x", "y", "element_ref", "snapshot_id", "operation_id"}, false},
	"workass_browser_key":            {"browser.key", []string{"key", "operation_id"}, []string{"tab_id", "key", "operation_id"}, false},
	"workass_browser_screenshot":     {"browser.screenshot", nil, []string{"tab_id", "mode", "clip"}, true},
	"workass_browser_batch":          {"browser.batch", []string{"actions", "operation_id"}, []string{"tab_id", "actions", "observe_after", "operation_id"}, false},
	"workass_browser_history":        {"", []string{"action", "operation_id"}, []string{"tab_id", "action", "operation_id"}, false},
	"workass_browser_set_viewport":   {"browser.setViewport", []string{"width", "height", "operation_id"}, []string{"tab_id", "width", "height", "operation_id"}, false},
	"workass_browser_reset_viewport": {"browser.resetViewport", []string{"operation_id"}, []string{"tab_id", "operation_id"}, false},
	"workass_browser_wait":           {"browser.wait", []string{"condition"}, []string{"tab_id", "condition", "selector", "timeout_ms"}, true},
	"workass_browser_diagnostics":    {"browser.diagnostics", nil, []string{"tab_id", "after_sequence", "limit"}, true},
}

func acceptanceToolsByName(t *testing.T) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, tool := range browserMCPTools() {
		out[tool["name"].(string)] = tool
	}
	return out
}

func acceptanceStrings(value any) []string {
	switch list := value.(type) {
	case []string:
		return append([]string(nil), list...)
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, item.(string))
		}
		return out
	}
	return nil
}

func TestBrowserAcceptanceParityMainToolContractsHold(t *testing.T) {
	tools := acceptanceToolsByName(t)
	for name, want := range mainBrowserTools {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("%s is gone", name)
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		required := acceptanceStrings(schema["required"])
		sort.Strings(required)
		wantRequired := append([]string(nil), want.required...)
		sort.Strings(wantRequired)
		if strings.Join(required, ",") != strings.Join(wantRequired, ",") {
			t.Errorf("%s required = %v, main = %v", name, required, wantRequired)
		}
		properties := schema["properties"].(map[string]any)
		for _, property := range want.properties {
			if _, ok := properties[property]; !ok {
				t.Errorf("%s lost its %s argument", name, property)
			}
		}
		annotations := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != want.readOnly {
			t.Errorf("%s readOnlyHint = %v, main = %v", name, annotations["readOnlyHint"], want.readOnly)
		}
		method, _, _, _, err := browserMCPToolBoundary(name)
		if err != nil || method != want.method {
			t.Errorf("%s routes to %q (%v), main routes to %q", name, method, err, want.method)
		}
	}
	enumHas := func(tool, property string, values ...string) {
		schema := tools[tool]["inputSchema"].(map[string]any)["properties"].(map[string]any)[property].(map[string]any)
		if property == "actions" {
			schema = schema["items"].(map[string]any)["properties"].(map[string]any)["action"].(map[string]any)
		}
		have := map[string]bool{}
		for _, value := range acceptanceStrings(schema["enum"]) {
			have[value] = true
		}
		for _, value := range values {
			if !have[value] {
				t.Errorf("%s %s no longer takes %q", tool, property, value)
			}
		}
	}
	enumHas("workass_browser_snapshot", "format", "json", "compact")
	enumHas("workass_browser_screenshot", "mode", "viewport", "full_page", "clip")
	enumHas("workass_browser_history", "action", "back", "forward", "reload")
	enumHas("workass_browser_wait", "condition", "dom_ready", "load", "element_visible", "element_hidden")
	enumHas("workass_browser_batch", "actions", "click", "type", "scroll", "key", "snapshot")
}

func TestBrowserAcceptanceParityMainStyleCallsStillPass(t *testing.T) {
	calls := []struct {
		name   string
		args   map[string]any
		method string
	}{
		{"workass_browser_snapshot", map[string]any{}, "browser.snapshot"},
		{"workass_browser_snapshot", map[string]any{"format": "compact", "since_snapshot_id": "snap-1"}, "browser.snapshot"},
		{"workass_browser_click", map[string]any{"element_ref": "el_9f2c", "snapshot_id": "snap-1", "operation_id": "op-1"}, "browser.click"},
		{"workass_browser_click", map[string]any{"selector": "#buy", "operation_id": "op-2"}, "browser.click"},
		{"workass_browser_click", map[string]any{"x": 10.0, "y": 20.0, "screenshot_id": "shot-1", "operation_id": "op-3"}, "browser.click"},
		{"workass_browser_type", map[string]any{"element_ref": "el_1", "snapshot_id": "snap-1", "text": "hi", "submit": true, "operation_id": "op-4"}, "browser.type"},
		{"workass_browser_scroll", map[string]any{"x": 0.0, "y": 400.0, "operation_id": "op-5"}, "browser.scroll"},
		{"workass_browser_scroll", map[string]any{"element_ref": "el_2", "snapshot_id": "snap-1", "y": 100.0, "operation_id": "op-6"}, "browser.scroll"},
		{"workass_browser_read", map[string]any{"element_ref": "el_3", "snapshot_id": "snap-1"}, "browser.read"},
		{"workass_browser_batch", map[string]any{"actions": []any{
			map[string]any{"action": "click", "selector": "#a"},
			map[string]any{"action": "type", "element_ref": "el_4", "snapshot_id": "snap-1", "text": "x"},
			map[string]any{"action": "key", "key": "Enter"},
			map[string]any{"action": "snapshot"},
		}, "operation_id": "op-7"}, "browser.batch"},
		{"workass_browser_history", map[string]any{"action": "back", "operation_id": "op-8"}, "browser.back"},
	}
	for _, call := range calls {
		prepared, err := prepareBrowserMCPCall(browserMCPCallParams{Name: call.name, Arguments: call.args})
		if err != nil {
			t.Errorf("%s %v: main's call is refused: %v", call.name, call.args, err)
			continue
		}
		if prepared.Method != call.method {
			t.Errorf("%s routes to %s, main to %s", call.name, prepared.Method, call.method)
		}
	}
	// A json snapshot element_ref still needs its snapshot_id, as on main.
	if _, err := prepareBrowserMCPCall(browserMCPCallParams{Name: "workass_browser_click", Arguments: map[string]any{
		"element_ref": "el_9f2c", "operation_id": "op-9"}}); err == nil {
		t.Error("a json element_ref without its snapshot_id is accepted")
	}
}

func TestBrowserAcceptanceFinding5CompactRefsNeedTheirSnapshot(t *testing.T) {
	if _, err := prepareBrowserMCPCall(browserMCPCallParams{Name: "workass_browser_click", Arguments: map[string]any{
		"element_ref": "c7", "operation_id": "op-1"}}); err == nil {
		t.Error("a bare compact ref (c7) is accepted")
	}
	if !strings.Contains(agenttext.Get("schema.browserMCPTools.03"), "c12") {
		t.Error("the element_ref text does not tell compact refs (c12) apart")
	}
}

func TestBrowserAcceptanceToolTextSaysHowEachToolFails(t *testing.T) {
	want := map[string][]string{
		"tools.workass_browser_hover":    {"reported by status"},
		"tools.workass_browser_drag":     {"unsupported", "end"},
		"tools.workass_browser_dialog":   {"no_dialog"},
		"tools.workass_browser_close":    {"Fails"},
		"tools.workass_browser_scroll":   {"not_scrollable"},
		"tools.workass_browser_navigate": {"20 seconds", "fails"},
		"tools.workass_browser_history":  {"loading"},
		"tools.workass_browser_select":   {"not_select"},
		"tools.workass_browser_evaluate": {"12 seconds", "unavailable"},
		"tools.workass_browser_upload":   {"real path", "linked folder", "hard links"},
		"tools.workass_browser_snapshot": {"autocomplete", "c12", "dialog"},
		"tools.workass_browser_batch":    {"dialog_index", "unexecuted_indexes"},
	}
	for key, fragments := range want {
		text := agenttext.Get(key)
		for _, fragment := range fragments {
			if !strings.Contains(text, fragment) {
				t.Errorf("%s does not say %q: %s", key, fragment, text)
			}
		}
	}
}

func TestBrowserAcceptanceToolTextNeverMentionsThinking(t *testing.T) {
	banned := regexp.MustCompile(`(?i)\b(think|thinks|thinking|thought|thoughts|reasoning|reasons through)\b`)
	for _, tool := range browserMCPTools() {
		name := tool["name"].(string)
		if banned.MatchString(tool["description"].(string)) {
			t.Errorf("%s description mentions thinking or reasoning", name)
		}
		properties := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for property, raw := range properties {
			if schema, ok := raw.(map[string]any); ok {
				if description, ok := schema["description"].(string); ok && banned.MatchString(description) {
					t.Errorf("%s.%s description mentions thinking or reasoning", name, property)
				}
			}
		}
	}
}
