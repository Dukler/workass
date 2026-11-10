package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"workass/internal/agenttext"
)

var flagNumberPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// internalWorkassTools are callable and have a schema, and are not for agents to
// reach for: `workass run` reports through workass_register_process_child, and
// every process it supervises would otherwise read "Do not call it" in the
// listing it pays for.
var internalWorkassTools = map[string]bool{"workass_register_process_child": true}

// toolShortDescriptions is one sentence per tool, for `tools list`. The whole
// schema is `tools list NAME`. A tool with no entry here falls back to the first
// sentence of its description, and TestEveryListedToolHasItsOwnShortSentence
// keeps that a fallback rather than a way to add a long line.
func toolShortDescriptions() map[string]string {
	return map[string]string{
		"workass_agent_catalog":              agenttext.Get("tools.short.workass_agent_catalog"),
		"workass_apply_update":               agenttext.Get("tools.short.workass_apply_update"),
		"workass_ask_user_question":          agenttext.Get("tools.short.workass_ask_user_question"),
		"workass_browser_batch":              agenttext.Get("tools.short.workass_browser_batch"),
		"workass_browser_click":              agenttext.Get("tools.short.workass_browser_click"),
		"workass_browser_close":              agenttext.Get("tools.short.workass_browser_close"),
		"workass_browser_diagnostics":        agenttext.Get("tools.short.workass_browser_diagnostics"),
		"workass_browser_dialog":             agenttext.Get("tools.short.workass_browser_dialog"),
		"workass_browser_drag":               agenttext.Get("tools.short.workass_browser_drag"),
		"workass_browser_evaluate":           agenttext.Get("tools.short.workass_browser_evaluate"),
		"workass_browser_history":            agenttext.Get("tools.short.workass_browser_history"),
		"workass_browser_hover":              agenttext.Get("tools.short.workass_browser_hover"),
		"workass_browser_key":                agenttext.Get("tools.short.workass_browser_key"),
		"workass_browser_list":               agenttext.Get("tools.short.workass_browser_list"),
		"workass_browser_navigate":           agenttext.Get("tools.short.workass_browser_navigate"),
		"workass_browser_open":               agenttext.Get("tools.short.workass_browser_open"),
		"workass_browser_read":               agenttext.Get("tools.short.workass_browser_read"),
		"workass_browser_reset_viewport":     agenttext.Get("tools.short.workass_browser_reset_viewport"),
		"workass_browser_screenshot":         agenttext.Get("tools.short.workass_browser_screenshot"),
		"workass_browser_scroll":             agenttext.Get("tools.short.workass_browser_scroll"),
		"workass_browser_select":             agenttext.Get("tools.short.workass_browser_select"),
		"workass_browser_set_viewport":       agenttext.Get("tools.short.workass_browser_set_viewport"),
		"workass_browser_snapshot":           agenttext.Get("tools.short.workass_browser_snapshot"),
		"workass_browser_type":               agenttext.Get("tools.short.workass_browser_type"),
		"workass_browser_upload":             agenttext.Get("tools.short.workass_browser_upload"),
		"workass_browser_wait":               agenttext.Get("tools.short.workass_browser_wait"),
		"workass_cancel_chat_turn":           agenttext.Get("tools.short.workass_cancel_chat_turn"),
		"workass_cancel_fork_child":          agenttext.Get("tools.short.workass_cancel_fork_child"),
		"workass_cancel_process_child":       agenttext.Get("tools.short.workass_cancel_process_child"),
		"workass_cancel_scheduled_message":   agenttext.Get("tools.short.workass_cancel_scheduled_message"),
		"workass_cancel_subagent":            agenttext.Get("tools.short.workass_cancel_subagent"),
		"workass_configure_chat":             agenttext.Get("tools.short.workass_configure_chat"),
		"workass_create_chat":                agenttext.Get("tools.short.workass_create_chat"),
		"workass_create_fork_point":          agenttext.Get("tools.short.workass_create_fork_point"),
		"workass_create_subsession":          agenttext.Get("tools.short.workass_create_subsession"),
		"workass_decide_subagent_permission": agenttext.Get("tools.short.workass_decide_subagent_permission"),
		"workass_delete_chat":                agenttext.Get("tools.short.workass_delete_chat"),
		"workass_discover_machines":          agenttext.Get("tools.short.workass_discover_machines"),
		"workass_focus_chat":                 agenttext.Get("tools.short.workass_focus_chat"),
		"workass_fork_run":                   agenttext.Get("tools.short.workass_fork_run"),
		"workass_get_chat_diagnostics":       agenttext.Get("tools.short.workass_get_chat_diagnostics"),
		"workass_get_update_status":          agenttext.Get("tools.short.workass_get_update_status"),
		"workass_host_artifact":              agenttext.Get("tools.short.workass_host_artifact"),
		"workass_list_chats":                 agenttext.Get("tools.short.workass_list_chats"),
		"workass_list_forks":                 agenttext.Get("tools.short.workass_list_forks"),
		"workass_list_process_children":      agenttext.Get("tools.short.workass_list_process_children"),
		"workass_issue_job_credential":       agenttext.Get("tools.short.workass_issue_job_credential"),
		"workass_list_job_credentials":       agenttext.Get("tools.short.workass_list_job_credentials"),
		"workass_revoke_job_credential":      agenttext.Get("tools.short.workass_revoke_job_credential"),
		"workass_report_bug":                 agenttext.Get("tools.short.workass_report_bug"),
		"workass_list_bug_reports":           agenttext.Get("tools.short.workass_list_bug_reports"),
		"workass_update_bug_report":          agenttext.Get("tools.short.workass_update_bug_report"),
		"workass_list_scheduled_messages":    agenttext.Get("tools.short.workass_list_scheduled_messages"),
		"workass_list_spawned_work":          agenttext.Get("tools.short.workass_list_spawned_work"),
		"workass_list_spawned_work_receipts": agenttext.Get("tools.short.workass_list_spawned_work_receipts"),
		"workass_list_subagent_receipts":     agenttext.Get("tools.short.workass_list_subagent_receipts"),
		"workass_list_subagents":             agenttext.Get("tools.short.workass_list_subagents"),
		"workass_list_update_targets":        agenttext.Get("tools.short.workass_list_update_targets"),
		"workass_message_subagent":           agenttext.Get("tools.short.workass_message_subagent"),
		"workass_read_chat":                  agenttext.Get("tools.short.workass_read_chat"),
		"workass_read_process_child":         agenttext.Get("tools.short.workass_read_process_child"),
		"workass_register_external_work":     agenttext.Get("tools.short.workass_register_external_work"),
		"workass_rename_chat":                agenttext.Get("tools.short.workass_rename_chat"),
		"workass_render_html":                agenttext.Get("tools.short.workass_render_html"),
		"workass_retry_fork_child":           agenttext.Get("tools.short.workass_retry_fork_child"),
		"workass_retry_subagent":             agenttext.Get("tools.short.workass_retry_subagent"),
		"workass_schedule_message":           agenttext.Get("tools.short.workass_schedule_message"),
		"workass_send_chat_message":          agenttext.Get("tools.short.workass_send_chat_message"),
		"workass_settle_chat":                agenttext.Get("tools.short.workass_settle_chat"),
		"workass_settle_external_work":       agenttext.Get("tools.short.workass_settle_external_work"),
		"workass_spawn_subagent":             agenttext.Get("tools.short.workass_spawn_subagent"),
		"workass_start_process_child":        agenttext.Get("tools.short.workass_start_process_child"),
		"workass_wait_fork_run":              agenttext.Get("tools.short.workass_wait_fork_run"),
		"workass_wait_subagent":              agenttext.Get("tools.short.workass_wait_subagent"),
		"workass_wait_subagents":             agenttext.Get("tools.short.workass_wait_subagents"),
	}
}

// toolListing is `tools list`: what exists, in one line each. It costs a caller
// a tenth of the catalog, and the catalog is one flag away.
func toolListing(catalog []map[string]any) map[string]any {
	shorts := toolShortDescriptions()
	rows := make([]any, 0, len(catalog))
	for _, tool := range catalog {
		name := toString(tool["name"])
		if internalWorkassTools[name] {
			continue
		}
		description := shorts[name]
		if description == "" {
			description, _, _ = strings.Cut(toString(tool["description"]), ". ")
		}
		rows = append(rows, map[string]any{"name": name, "description": description})
	}
	return map[string]any{"tools": rows, "usage": agenttext.Get("cli.list_usage")}
}

// toolCallUsage says exactly how to pass this tool's arguments, with an example
// that passes the tool's own schema. The shell differs by platform: PowerShell
// strips the quotes of a JSON argument handed to a native command, so there the
// example pipes it.
func toolCallUsage(tool map[string]any, goos string) string {
	name := toString(tool["name"])
	example, _ := json.Marshal(toolArgumentsExample(tool))
	command := `"$WORKASS_TOOLS_COMMAND" tools call ` + name + ` --input '` + string(example) + `'`
	if goos == "windows" {
		command = `'` + string(example) + `' | & $env:WORKASS_TOOLS_COMMAND tools call ` + name
	}
	return fmt.Sprintf(agenttext.Get("cli.call_usage"), command, name)
}

func toolUsageForThisMachine(tool map[string]any) string {
	return toolCallUsage(tool, runtime.GOOS)
}

// toolInputSchema is the tool's schema as plain JSON values, whichever Go types
// the catalog literal happens to use.
func toolInputSchema(tool map[string]any) map[string]any {
	raw, err := json.Marshal(tool["inputSchema"])
	if err != nil {
		return nil
	}
	var schema map[string]any
	if json.Unmarshal(raw, &schema) != nil {
		return nil
	}
	return schema
}

func toolArgumentsExample(tool map[string]any) map[string]any {
	example, _ := schemaExampleValue(toolInputSchema(tool), "", toString(tool["name"])).(map[string]any)
	if example == nil {
		example = map[string]any{}
	}
	return example
}

// schemaExampleValue builds the smallest value a schema accepts: required fields
// only, with a placeholder that says what belongs there.
func schemaExampleValue(schema map[string]any, key, toolName string) any {
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	switch kind, _ := schema["type"].(string); kind {
	case "object":
		out := map[string]any{}
		properties, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, item := range required {
			name, _ := item.(string)
			property, _ := properties[name].(map[string]any)
			out[name] = schemaExampleValue(property, name, toolName)
		}
		return out
	case "array":
		count := 0
		if minimum, ok := schemaNumber(schema["minItems"]); ok {
			count = int(minimum)
		}
		items, _ := schema["items"].(map[string]any)
		out := make([]any, 0, count)
		for index := 0; index < count; index++ {
			out = append(out, schemaExampleValue(items, key, toolName))
		}
		return out
	case "boolean":
		return true
	case "integer", "number":
		if minimum, ok := schemaNumber(schema["minimum"]); ok {
			return minimum
		}
		return 1
	}
	if key == "operation_id" {
		return strings.ReplaceAll(strings.TrimPrefix(toolName, "workass_"), "_", "-") + "-1"
	}
	return strings.ToUpper(key)
}

// coerceFlagArguments gives --key value text the type the tool's schema declares.
// Only the flag path comes here: JSON arguments keep their own types and a wrong
// one is refused. A value that is not of its type is left as text, so the
// validator refuses it by name, and an unknown key is left for it to refuse too.
func coerceFlagArguments(tool map[string]any, arguments map[string]any) map[string]any {
	properties, _ := toolInputSchema(tool)["properties"].(map[string]any)
	out := make(map[string]any, len(arguments))
	keys := make([]string, 0, len(arguments))
	for key := range arguments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := arguments[key]
		out[key] = value
		text, isText := value.(string)
		property, known := properties[key].(map[string]any)
		if !isText || !known {
			continue
		}
		switch kind, _ := property["type"].(string); kind {
		case "boolean":
			if text == "true" || text == "false" {
				out[key] = text == "true"
			}
		case "integer", "number":
			if flagNumberPattern.MatchString(text) {
				out[key] = json.Number(text)
			}
		case "array", "object":
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			var parsed any
			if decoder.Decode(&parsed) == nil && !decoder.More() {
				out[key] = parsed
			}
		}
	}
	return out
}
