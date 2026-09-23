package main

import (
	"encoding/json"
	"net/http"
	"workass/internal/agenttext"

	"workass/internal/acp"
)

type agentMCPOptions struct {
	ChatID      string
	TabID       string
	OwnerKey    string
	OperationID string
}

func subagentWaitTimeoutSchema() map[string]any {
	return map[string]any{
		"type": "integer", "description": agenttext.Get("schema.agentMCPTools.03"),
		"anyOf": []any{
			map[string]any{"enum": []int{-1, 0}},
			map[string]any{"minimum": 1000, "maximum": 3600000},
		},
	}
}

func agentMCPTools() []map[string]any {
	object := func(properties map[string]any, required ...string) map[string]any {
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		return schema
	}
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	enum := func(description string, values ...string) map[string]any {
		items := make([]any, len(values))
		for i, value := range values {
			items[i] = value
		}
		return map[string]any{"type": "string", "description": description, "enum": items}
	}
	boolean := func(description string) map[string]any {
		return map[string]any{"type": "boolean", "description": description}
	}
	integer := func(description string, min int) map[string]any {
		return map[string]any{"type": "integer", "minimum": min, "description": description}
	}
	operationID := func() map[string]any {
		return str(agenttext.Get("schema.browserMCPTools.02"))
	}
	mutationObject := func(properties map[string]any, required ...string) map[string]any {
		properties["operation_id"] = operationID()
		required = append(required, "operation_id")
		return object(properties, required...)
	}
	tool := func(name, description string, inputSchema map[string]any, readOnly, destructive, idempotent, openWorld bool) map[string]any {
		return map[string]any{
			"name": name, "description": description, "inputSchema": inputSchema,
			"annotations": map[string]any{
				"readOnlyHint": readOnly, "destructiveHint": destructive,
				"idempotentHint": idempotent, "openWorldHint": openWorld,
			},
		}
	}
	return []map[string]any{
		tool("workass_get_chat_diagnostics", agenttext.Get("tools.workass_get_chat_diagnostics"), object(map[string]any{
			"tab_id":  str(agenttext.Get("schema.agentMCPTools.guidance.02")),
			"chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.03")),
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 20, "description": agenttext.Get("schema.agentMCPTools.01")},
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_list_chats", agenttext.Get("tools.workass_list_chats"), object(map[string]any{}), true, false, true, false),
		tool("workass_list_update_targets", agenttext.Get("tools.workass_list_update_targets"), object(map[string]any{}), true, false, true, false),
		tool("workass_get_update_status", agenttext.Get("tools.workass_get_update_status"), object(map[string]any{
			"machine_id": str(agenttext.Get("schema.agentMCPTools.guidance.04")),
		}, "machine_id"), true, false, true, false),
		tool("workass_apply_update", agenttext.Get("tools.workass_apply_update"), mutationObject(map[string]any{
			"machine_id":               str(agenttext.Get("schema.agentMCPTools.guidance.04")),
			"expected_current_version": str(agenttext.Get("schema.agentMCPTools.guidance.06")),
			"expected_target_version":  str(agenttext.Get("schema.agentMCPTools.guidance.07")),
			"authorization":            str(agenttext.Get("schema.agentMCPTools.guidance.08")),
		}, "machine_id", "expected_current_version", "expected_target_version", "authorization"), false, true, true, true),
		tool("workass_read_chat", agenttext.Get("tools.workass_read_chat"), object(map[string]any{
			"tab_id":         str(agenttext.Get("schema.agentMCPTools.guidance.09")),
			"chat_id":        str(agenttext.Get("schema.agentMCPTools.guidance.10")),
			"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": agenttext.Get("schema.agentMCPTools.02")},
			"include_events": boolean(agenttext.Get("schema.agentMCPTools.guidance.11")),
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_create_chat", agenttext.Get("tools.workass_create_chat"), mutationObject(map[string]any{
			"title":             str(agenttext.Get("schema.agentMCPTools.guidance.12")),
			"cwd":               str(agenttext.Get("schema.agentMCPTools.guidance.13")),
			"provider_id":       str(agenttext.Get("schema.agentMCPTools.guidance.14")),
			"model_id":          str(agenttext.Get("schema.agentMCPTools.guidance.15")),
			"effort":            str(agenttext.Get("schema.agentMCPTools.guidance.16")),
			"mode_id":           str(agenttext.Get("schema.agentMCPTools.guidance.17")),
			"permission_intent": enum(agenttext.Get("schema.agentMCPTools.guidance.18"), "read", "edit", "full"),
			"focus":             boolean(agenttext.Get("schema.agentMCPTools.guidance.19")),
		}), false, false, false, false),
		tool("workass_rename_chat", agenttext.Get("tools.workass_rename_chat"), mutationObject(map[string]any{
			"tab_id": str(agenttext.Get("schema.agentMCPTools.guidance.09")), "chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.21")), "title": str(agenttext.Get("schema.agentMCPTools.guidance.22")),
		}, "tab_id", "chat_id", "title"), false, false, false, false),
		tool("workass_configure_chat", agenttext.Get("tools.workass_configure_chat"), mutationObject(map[string]any{
			"tab_id":            str(agenttext.Get("schema.agentMCPTools.guidance.09")),
			"chat_id":           str(agenttext.Get("schema.agentMCPTools.guidance.21")),
			"cwd":               str(agenttext.Get("schema.agentMCPTools.guidance.25")),
			"provider_id":       str(agenttext.Get("schema.agentMCPTools.guidance.26")),
			"model_id":          str(agenttext.Get("schema.agentMCPTools.guidance.27")),
			"effort":            str(agenttext.Get("schema.agentMCPTools.guidance.28")),
			"mode_id":           str(agenttext.Get("schema.agentMCPTools.guidance.29")),
			"permission_intent": enum(agenttext.Get("schema.agentMCPTools.guidance.30"), "read", "edit", "full"),
		}, "tab_id", "chat_id"), false, false, false, false),
		tool("workass_focus_chat", agenttext.Get("tools.workass_focus_chat"), mutationObject(map[string]any{
			"tab_id": str(agenttext.Get("schema.agentMCPTools.guidance.09")), "chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.21")),
		}, "tab_id", "chat_id"), false, false, true, false),
		tool("workass_delete_chat", agenttext.Get("tools.workass_delete_chat"), mutationObject(map[string]any{
			"tab_id": str(agenttext.Get("schema.agentMCPTools.guidance.09")), "chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.21")),
			"force": boolean(agenttext.Get("schema.agentMCPTools.guidance.35")),
		}, "tab_id", "chat_id"), false, true, false, false),
		tool("workass_send_chat_message", agenttext.Get("tools.workass_send_chat_message"), mutationObject(map[string]any{
			"tab_id":   str(agenttext.Get("schema.agentMCPTools.guidance.09")),
			"chat_id":  str(agenttext.Get("schema.agentMCPTools.guidance.21")),
			"message":  str(agenttext.Get("schema.agentMCPTools.guidance.38")),
			"delivery": enum(agenttext.Get("schema.agentMCPTools.guidance.39"), "auto", "queue", "steer"),
		}, "tab_id", "chat_id", "message"), false, false, false, true),
		tool("workass_cancel_chat_turn", agenttext.Get("tools.workass_cancel_chat_turn"), mutationObject(map[string]any{
			"tab_id": str(agenttext.Get("schema.agentMCPTools.guidance.09")), "chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.21")),
		}, "tab_id", "chat_id"), false, true, false, false),
		tool("workass_agent_catalog", agenttext.Get("tools.workass_agent_catalog"), object(map[string]any{}), true, false, true, false),
		tool("workass_host_artifact", agenttext.Get("tools.workass_host_artifact"), mutationObject(map[string]any{
			"source_path": str(agenttext.Get("schema.agentMCPTools.guidance.42")),
			"entry":       str(agenttext.Get("schema.agentMCPTools.guidance.43")),
			"name":        str(agenttext.Get("schema.agentMCPTools.guidance.44")),
		}, "source_path"), false, false, true, true),
		tool("workass_spawn_subagent", agenttext.Get("tools.workass_spawn_subagent"), map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"task", "operation_id"},
			"properties": map[string]any{
				"operation_id":      operationID(),
				"task":              str(agenttext.Get("schema.agentMCPTools.guidance.45")),
				"label":             str(agenttext.Get("schema.agentMCPTools.guidance.46")),
				"profile":           enum(agenttext.Get("schema.agentMCPTools.guidance.47"), "smart", "tasteful", "budget", "balanced", "independent-review"),
				"provider_id":       str(agenttext.Get("schema.agentMCPTools.guidance.48")),
				"model_id":          str(agenttext.Get("schema.agentMCPTools.guidance.49")),
				"effort":            str(agenttext.Get("schema.agentMCPTools.guidance.50")),
				"mode_id":           str(agenttext.Get("schema.agentMCPTools.guidance.51")),
				"permission_intent": enum(agenttext.Get("schema.agentMCPTools.guidance.52"), "inherit", "read", "edit", "full"),
				"cwd":               str(agenttext.Get("schema.agentMCPTools.guidance.53")),
			},
		}, false, true, false, true),
		tool("workass_list_subagents", agenttext.Get("tools.workass_list_subagents"), object(map[string]any{}), true, false, true, false),
		tool("workass_wait_subagent", agenttext.Get("tools.workass_wait_subagent"), mutationObject(map[string]any{
			"subagent_id": str(agenttext.Get("schema.agentMCPTools.guidance.54")),
			"timeout_ms":  subagentWaitTimeoutSchema(),
		}, "subagent_id"), false, false, false, false),
		tool("workass_wait_subagents", agenttext.Get("tools.workass_wait_subagents"), mutationObject(map[string]any{
			"subagent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 8},
			"return_when":  enum(agenttext.Get("schema.agentMCPTools.guidance.55"), "first", "all"),
			"timeout_ms":   subagentWaitTimeoutSchema(),
		}, "subagent_ids"), false, false, false, false),
		tool("workass_message_subagent", agenttext.Get("tools.workass_message_subagent"), mutationObject(map[string]any{
			"subagent_id": str(agenttext.Get("schema.agentMCPTools.guidance.56")),
			"message":     str(agenttext.Get("schema.agentMCPTools.guidance.57")),
		}, "subagent_id", "message"), false, false, false, true),
		tool("workass_retry_subagent", agenttext.Get("tools.workass_retry_subagent"), mutationObject(map[string]any{
			"subagent_id": str(agenttext.Get("schema.agentMCPTools.guidance.58")),
			"message":     str(agenttext.Get("schema.agentMCPTools.guidance.59")),
		}, "subagent_id"), false, true, false, true),
		tool("workass_list_subagent_receipts", agenttext.Get("tools.workass_list_subagent_receipts"), object(map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 256, "description": agenttext.Get("schema.agentMCPTools.05")},
		}), true, false, true, false),
		tool("workass_list_spawned_work", agenttext.Get("tools.workass_list_spawned_work"), object(map[string]any{
			"tab_id":     str(agenttext.Get("schema.agentMCPTools.guidance.09")),
			"chat_id":    str(agenttext.Get("schema.agentMCPTools.guidance.21")),
			"tail_chars": map[string]any{"type": "integer", "minimum": 0, "maximum": 12000, "description": agenttext.Get("schema.agentMCPTools.06")},
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_register_external_work", agenttext.Get("tools.workass_register_external_work"), mutationObject(map[string]any{
			"label":       str(agenttext.Get("schema.agentMCPTools.guidance.62")),
			"role":        enum(agenttext.Get("schema.agentMCPTools.guidance.63"), "work", "service"),
			"pid":         integer(agenttext.Get("schema.agentMCPTools.guidance.64"), 2),
			"output_file": str(agenttext.Get("schema.agentMCPTools.guidance.65")),
			"done_file":   str(agenttext.Get("schema.agentMCPTools.guidance.66")),
			"tab_id":      str(agenttext.Get("schema.agentMCPTools.guidance.67")),
			"chat_id":     str(agenttext.Get("schema.agentMCPTools.guidance.68")),
		}, "label"), false, false, false, true),
		tool("workass_settle_external_work", agenttext.Get("tools.workass_settle_external_work"), mutationObject(map[string]any{
			"work_id":   str(agenttext.Get("schema.agentMCPTools.guidance.69")),
			"status":    enum(agenttext.Get("schema.agentMCPTools.guidance.70"), "exited", "failed"),
			"exit_code": integer(agenttext.Get("schema.agentMCPTools.guidance.71"), 0),
			"summary":   str(agenttext.Get("schema.agentMCPTools.guidance.72")),
			"tab_id":    str(agenttext.Get("schema.agentMCPTools.guidance.67")),
			"chat_id":   str(agenttext.Get("schema.agentMCPTools.guidance.68")),
		}, "work_id", "status"), false, false, true, true),
		tool("workass_list_spawned_work_receipts", agenttext.Get("tools.workass_list_spawned_work_receipts"), object(map[string]any{
			"tab_id":  str(agenttext.Get("schema.agentMCPTools.guidance.09")),
			"chat_id": str(agenttext.Get("schema.agentMCPTools.guidance.21")),
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 256, "description": agenttext.Get("schema.agentMCPTools.05")},
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_cancel_subagent", agenttext.Get("tools.workass_cancel_subagent"), mutationObject(map[string]any{
			"subagent_id": str(agenttext.Get("schema.agentMCPTools.guidance.54")),
		}, "subagent_id"), false, true, false, false),
		tool("workass_decide_subagent_permission", agenttext.Get("tools.workass_decide_subagent_permission"), mutationObject(map[string]any{
			"subagent_id": str(agenttext.Get("schema.agentMCPTools.guidance.78")),
			"decision":    enum(agenttext.Get("schema.agentMCPTools.guidance.79"), "allow", "deny"),
		}, "subagent_id", "decision"), false, true, false, false),
		tool("workass_ask_user_question", agenttext.Get("tools.workass_ask_user_question"), mutationObject(map[string]any{
			"question_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$", "description": agenttext.Get("schema.agentMCPTools.question.id")},
			"question":    map[string]any{"type": "string", "minLength": 1, "maxLength": 400, "description": agenttext.Get("schema.agentMCPTools.question.prompt")},
			"header":      map[string]any{"type": "string", "maxLength": 40, "description": agenttext.Get("schema.agentMCPTools.question.header")},
			"options": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 4, "description": agenttext.Get("schema.agentMCPTools.question.options"),
				"items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"id", "label"},
					"properties": map[string]any{
						"id":          map[string]any{"type": "string", "minLength": 1, "maxLength": 64, "pattern": "^[A-Za-z0-9_-]+$"},
						"label":       map[string]any{"type": "string", "minLength": 1, "maxLength": 120},
						"description": map[string]any{"type": "string", "maxLength": 240},
					},
				},
			},
			"multi_select":    map[string]any{"type": "boolean", "description": agenttext.Get("schema.agentMCPTools.question.multi_select")},
			"allow_free_text": map[string]any{"type": "boolean", "description": agenttext.Get("schema.agentMCPTools.question.allow_free_text")},
			"timeout_ms":      map[string]any{"type": "integer", "minimum": 1000, "maximum": 3600000, "description": agenttext.Get("schema.agentMCPTools.question.timeout")},
		}, "question_id", "question", "options"), false, false, true, false),
	}
}

func callAgentMCPTool(request *http.Request, call browserMCPCallParams, options agentMCPOptions, control *agentControlHandler) (any, error) {
	method := ""
	params := copyAnyMap(call.Arguments)
	operationID, operationErr := requiredToolOperationID(agentToolKind, call)
	if operationErr != nil {
		return agentMCPErrorResult(operationErr.Error()), nil
	}
	if operationID != "" {
		params["operation_id"] = string(operationID)
	}
	if _, exists := params["operationId"]; exists {
		return agentMCPErrorResult("Workass tools use operation_id; operationId is not accepted"), nil
	}
	switch call.Name {
	case "workass_get_chat_diagnostics":
		method = "chat.diagnostics"
	case "workass_list_chats":
		method = "chat.list"
	case "workass_list_update_targets":
		method = "update.targets"
	case "workass_get_update_status":
		method = "update.status"
	case "workass_apply_update":
		method = "update.apply"
	case "workass_read_chat":
		method = "chat.read"
	case "workass_create_chat":
		method = "chat.create"
	case "workass_rename_chat":
		method = "chat.rename"
	case "workass_configure_chat":
		method = "chat.configure"
	case "workass_focus_chat":
		method = "chat.focus"
	case "workass_delete_chat":
		method = "chat.delete"
	case "workass_send_chat_message":
		method = "chat.send"
	case "workass_cancel_chat_turn":
		method = "chat.cancel"
	case "workass_agent_catalog":
		method = "agent.catalog"
	case "workass_host_artifact":
		method = "artifact.host"
	case "workass_spawn_subagent":
		method = "agent.spawn"
		params["prompt"] = params["task"]
		delete(params, "task")
		if _, exists := params["permission_mode"]; exists {
			return agentMCPErrorResult("Workass tools use mode_id; permission_mode is not accepted"), nil
		}
	case "workass_wait_subagents":
		method = "agent.wait_many"
		params["ids"] = params["subagent_ids"]
		delete(params, "subagent_ids")
	case "workass_message_subagent":
		method = "agent.message"
		params["id"] = params["subagent_id"]
		delete(params, "subagent_id")
	case "workass_retry_subagent":
		method = "agent.retry"
		params["id"] = params["subagent_id"]
		delete(params, "subagent_id")
	case "workass_list_subagent_receipts":
		method = "agent.receipts"
	case "workass_list_spawned_work":
		method = "spawned_work.list"
	case "workass_register_external_work":
		method = "external.register"
	case "workass_settle_external_work":
		method = "external.settle"
	case "workass_list_spawned_work_receipts":
		method = "spawned_work.receipts"
	case "workass_list_subagents":
		method = "agent.list"
	case "workass_wait_subagent":
		method = "agent.wait"
		params["id"] = params["subagent_id"]
		delete(params, "subagent_id")
	case "workass_cancel_subagent":
		method = "agent.cancel"
		params["id"] = params["subagent_id"]
		delete(params, "subagent_id")
	case "workass_decide_subagent_permission":
		method = "agent.decide_permission"
	case "workass_ask_user_question":
		method = "agent.question"
	default:
		return agentMCPErrorResult("unknown agent tool: " + call.Name), nil
	}
	params["parent_chat_id"] = options.ChatID
	params["parent_tab_id"] = options.TabID
	params["owner_key"] = options.OwnerKey
	if operationID != "" {
		params["operation_id"] = string(operationID)
	} else {
		delete(params, "operation_id")
	}
	result, err := control.call(request, agentControlRequest{Method: method, Params: params})
	if err != nil {
		return agentMCPErrorResult(err.Error()), nil
	}
	encoded, _ := json.Marshal(redactValue(result))
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}}, nil
}

func agentMCPErrorResult(message string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]any{"type": "text", "text": acp.RedactSensitiveText(message)}},
	}
}
