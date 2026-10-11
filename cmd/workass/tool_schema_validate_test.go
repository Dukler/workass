package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func catalogTool(t *testing.T, name string) map[string]any {
	t.Helper()
	for _, tool := range append(agentMCPTools(), browserMCPTools()...) {
		if tool["name"] == name {
			return tool
		}
	}
	t.Fatalf("no tool %s", name)
	return nil
}

func decodeToolArguments(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		t.Fatal(err)
	}
	return arguments
}

// 2026-10-11 QA B10: typos and out-of-range values ran with silent defaults.
func TestToolArgumentsAreValidatedAgainstTheDeclaredSchema(t *testing.T) {
	rejected := []struct{ tool, args, want string }{
		{"workass_send_chat_message", `{"tab_id":"t","chat_id":"c","message":"m","operation_id":"o","deliver":"steer"}`, "deliver is not a known argument"},
		{"workass_send_chat_message", `{"tab_id":"t","chat_id":"c","message":"m","operation_id":"o","delivery":"now"}`, "delivery must be one of"},
		{"workass_delete_chat", `{"tab_id":"t","chat_id":"c","operation_id":"o","forse":true}`, "forse is not a known argument"},
		{"workass_delete_chat", `{"tab_id":"t","chat_id":"c","operation_id":"o","force":"yes"}`, "force must be a boolean"},
		{"workass_read_chat", `{"tab_id":"t","chat_id":"c","limit":0}`, "limit must be at least 1"},
		{"workass_read_chat", `{"tab_id":"t","chat_id":"c","limit":1.5}`, "limit must be an integer"},
		{"workass_read_chat", `{"tab_id":"t","chat_id":"c","limit":null}`, "limit must be an integer"},
		{"workass_read_chat", `{"tab_id":"t"}`, "chat_id is required"},
		{"workass_list_chats", `{"bogus":1}`, "bogus is not a known argument"},
	}
	for _, tc := range rejected {
		err := validateToolArguments(catalogTool(t, tc.tool), decodeToolArguments(t, tc.args))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: err=%v, want %q", tc.tool, tc.args, err, tc.want)
		}
	}
	accepted := []struct{ tool, args string }{
		{"workass_send_chat_message", `{"tab_id":"t","chat_id":"c","message":"m","operation_id":"o","delivery":"steer"}`},
		{"workass_read_chat", `{"tab_id":"t","chat_id":"c","limit":200,"include_events":true}`},
		{"workass_list_chats", `{}`},
	}
	for _, tc := range accepted {
		if err := validateToolArguments(catalogTool(t, tc.tool), decodeToolArguments(t, tc.args)); err != nil {
			t.Errorf("%s %s rejected: %v", tc.tool, tc.args, err)
		}
	}
	if err := validateToolArguments(catalogTool(t, "workass_list_chats"), nil); err != nil {
		t.Errorf("absent arguments rejected: %v", err)
	}
}
