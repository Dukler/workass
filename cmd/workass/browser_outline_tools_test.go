package main

import (
	"strings"
	"testing"
)

// The outline tools: short refs on their own, the new actions' arguments.
func TestBrowserMCPOutlineRefsAndNewActionsValidate(t *testing.T) {
	ok := func(name string, args map[string]any) {
		t.Helper()
		if _, err := prepareBrowserMCPCall(browserMCPCallParams{Name: name, Arguments: args}); err != nil {
			t.Fatalf("%s %v: unexpected error %v", name, args, err)
		}
	}
	bad := func(name string, args map[string]any, want string) {
		t.Helper()
		_, err := prepareBrowserMCPCall(browserMCPCallParams{Name: name, Arguments: args})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s %v: error %v, want %q", name, args, err, want)
		}
	}
	op := func(args map[string]any) map[string]any { args["operation_id"] = "op-1"; return args }
	ok("workass_browser_snapshot", map[string]any{"format": "outline", "max_bytes": 8000})
	ok("workass_browser_snapshot", map[string]any{"format": "outline", "cursor": "oAbc:40"})
	ok("workass_browser_snapshot", map[string]any{"format": "outline", "since_snapshot_id": "oAbc"})
	bad("workass_browser_snapshot", map[string]any{"cursor": "oAbc:40"}, "format outline")
	bad("workass_browser_snapshot", map[string]any{"format": "yaml"}, "outline, json or compact")
	ok("workass_browser_click", op(map[string]any{"element_ref": "e12"}))
	ok("workass_browser_click", op(map[string]any{"element_ref": "e12", "snapshot_id": "oAbc", "click_count": 2, "button": "right"}))
	ok("workass_browser_click", op(map[string]any{"element_ref": "el_opaque", "snapshot_id": "snap-1"}))
	bad("workass_browser_click", op(map[string]any{"element_ref": "el_opaque"}), "element_ref")
	bad("workass_browser_click", op(map[string]any{"element_ref": "e12", "click_count": 4}), "click_count")
	ok("workass_browser_type", op(map[string]any{"element_ref": "e3", "text": "hi"}))
	ok("workass_browser_scroll", op(map[string]any{"element_ref": "e3", "y": 400}))
	ok("workass_browser_select", op(map[string]any{"element_ref": "e3", "labels": []any{"Green"}}))
	bad("workass_browser_select", op(map[string]any{"element_ref": "e3"}), "exactly one of values, labels, or indexes")
	bad("workass_browser_select", op(map[string]any{"element_ref": "e3", "values": []any{"a"}, "labels": []any{"A"}}), "exactly one")
	ok("workass_browser_hover", op(map[string]any{"selector": "#menu"}))
	ok("workass_browser_drag", op(map[string]any{"from_element_ref": "e1", "to_element_ref": "e2"}))
	ok("workass_browser_drag", op(map[string]any{"from_selector": "#handle", "delta_x": 200, "delta_y": 0}))
	bad("workass_browser_drag", op(map[string]any{"from_selector": "#handle"}), "drop target")
	bad("workass_browser_drag", op(map[string]any{"from_selector": "#a", "to_selector": "#b", "delta_x": 5}), "not both")
	ok("workass_browser_upload", op(map[string]any{"element_ref": "e9", "paths": []any{"/tmp/report.pdf"}}))
	bad("workass_browser_upload", op(map[string]any{"element_ref": "e9", "paths": []any{"report.pdf"}}), "absolute")
	ok("workass_browser_dialog", op(map[string]any{"accept": true, "prompt_text": "Ada"}))
	bad("workass_browser_dialog", op(map[string]any{}), "accept")
	ok("workass_browser_close", op(map[string]any{}))
	ok("workass_browser_evaluate", op(map[string]any{"expression": "document.title"}))
	bad("workass_browser_evaluate", op(map[string]any{"expression": " "}), "expression")
	ok("workass_browser_batch", op(map[string]any{"actions": []any{
		map[string]any{"action": "select", "element_ref": "e3", "values": []any{"g"}},
		map[string]any{"action": "hover", "element_ref": "e4"},
		map[string]any{"action": "click", "element_ref": "e5", "click_count": 2},
	}}))
	for _, name := range []string{"workass_browser_select", "workass_browser_hover", "workass_browser_drag", "workass_browser_upload",
		"workass_browser_dialog", "workass_browser_close", "workass_browser_evaluate"} {
		if !workassToolMutates(browserToolKind, name) {
			t.Fatalf("%s must be a durable mutation", name)
		}
		bad(name, map[string]any{}, "operation_id")
	}
}
