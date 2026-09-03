package main

import (
	"encoding/json"
	"net/http"

	"workass/internal/acp"
)

type agentMCPOptions struct {
	ChatID      string
	TabID       string
	OwnerKey    string
	OperationID string
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
		return str("Caller-stable logical operation id, distinct from the JSON-RPC transport id. Reuse it only for the same immutable request.")
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
		tool("workass_list_chats", "List local chats and chats mounted from connected Workass machines with exact tab_id/chat_id targets, machine identity, provider/model/effort/permission state, and queue status. Remote targets use the returned machine-tagged ids. Use before controlling a chat; never infer a target from title or position.", object(map[string]any{}), true, false, true, false),
		tool("workass_read_chat", "Read a byte-bounded canonical transcript and current controls for one exact local or mounted remote Workass chat without focusing it. Event-heavy reads preserve the newest complete event suffix and report eventCount, includedEventCount, and eventsTruncated instead of failing the MCP transport.", object(map[string]any{
			"tab_id":         str("Tab id from workass_list_chats."),
			"chat_id":        str("Exact conversation id paired with tab_id by workass_list_chats."),
			"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Newest messages to return; defaults to 40."},
			"include_events": boolean("Include persisted tool/plan event records; false by default. Oversized event history is explicitly tail-truncated to the MCP response budget."),
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_create_chat", "Create a durable Workass chat. Selection and cwd inherit the calling chat unless explicitly supplied and validated against the live catalog. Does not focus the UI unless focus=true.", mutationObject(map[string]any{
			"title":             str("Chat title; defaults to Nuevo chat."),
			"cwd":               str("Absolute server directory, or inherit."),
			"provider_id":       str("Provider id from workass_agent_catalog."),
			"model_id":          str("Base model id from workass_agent_catalog."),
			"effort":            str("Exact effort exposed by the selected model."),
			"mode_id":           str("Exact provider-native permission mode."),
			"permission_intent": enum("Provider-neutral permission intent; mutually exclusive with mode_id.", "read", "edit", "full"),
			"focus":             boolean("Select the created chat in the user's UI."),
		}), false, false, false, false),
		tool("workass_rename_chat", "Rename one exact Workass chat without changing the active tab or any other chat state.", mutationObject(map[string]any{
			"tab_id": str("Tab id from workass_list_chats."), "chat_id": str("Paired conversation id."), "title": str("New title."),
		}, "tab_id", "chat_id", "title"), false, false, false, false),
		tool("workass_configure_chat", "Set cwd/provider/model/effort/permission for one exact chat. Every selection is validated against the live provider catalog and the resolved native ids are returned.", mutationObject(map[string]any{
			"tab_id":            str("Tab id from workass_list_chats."),
			"chat_id":           str("Paired conversation id."),
			"cwd":               str("Absolute server directory; omit to preserve."),
			"provider_id":       str("Provider id from workass_agent_catalog; omit to preserve."),
			"model_id":          str("Base model id from workass_agent_catalog; omit to preserve."),
			"effort":            str("Exact effort exposed by the selected model; omit to use the provider default."),
			"mode_id":           str("Exact provider-native permission mode; mutually exclusive with permission_intent."),
			"permission_intent": enum("Provider-neutral permission intent.", "read", "edit", "full"),
		}, "tab_id", "chat_id"), false, false, false, false),
		tool("workass_focus_chat", "Focus one exact Workass chat in the user's UI. This is the only chat tool that changes the active tab by default.", mutationObject(map[string]any{
			"tab_id": str("Tab id from workass_list_chats."), "chat_id": str("Paired conversation id."),
		}, "tab_id", "chat_id"), false, false, true, false),
		tool("workass_delete_chat", "Delete one exact Workass chat and its native binding. Refuses a running turn unless force=true explicitly authorizes cancellation and deletion.", mutationObject(map[string]any{
			"tab_id": str("Tab id from workass_list_chats."), "chat_id": str("Paired conversation id."),
			"force": boolean("Cancel a running turn before deleting."),
		}, "tab_id", "chat_id"), false, true, false, false),
		tool("workass_send_chat_message", "Send text to one exact local or mounted remote chat without focusing it. auto sends at the next idle boundary and queue preserves FIFO order. Local chats also support steer; mounted remote chats reject steer and require auto or queue so a lost route reply cannot duplicate live input.", mutationObject(map[string]any{
			"tab_id":   str("Tab id from workass_list_chats."),
			"chat_id":  str("Paired conversation id."),
			"message":  str("Message to send."),
			"delivery": enum("Delivery behavior; defaults to auto.", "auto", "queue", "steer"),
		}, "tab_id", "chat_id", "message"), false, false, false, true),
		tool("workass_cancel_chat_turn", "Cancel the currently running turn in one exact chat. Does not delete queued follow-ups.", mutationObject(map[string]any{
			"tab_id": str("Tab id from workass_list_chats."), "chat_id": str("Paired conversation id."),
		}, "tab_id", "chat_id"), false, true, false, false),
		tool("workass_agent_catalog", "List the actual Workass providers, models, reasoning effort levels, and permission/mode ids available for tracked subagents. Call this instead of guessing model ids from config files.", object(map[string]any{}), true, false, true, false),
		tool("workass_host_artifact", "Host a file or static directory from the calling agent's Workass cwd. Returns a stable URL and ready-to-use markdown; put that markdown in your response rather than a local path. Any file type. A directory defaults to index.html, else pass entry.", mutationObject(map[string]any{
			"source_path": str("Supported artifact file or static directory, absolute or relative to the calling agent's Workass cwd."),
			"entry":       str("Relative entry artifact for a directory; defaults to index.html when present and is ignored for a file."),
			"name":        str("Optional short human label used in the stable hosted id."),
		}, "source_path"), false, false, true, true),
		tool("workass_spawn_subagent", "Start a tracked subagent and return immediately. Omitted selection/cwd inherits the current turn. Explicit ids must come from workass_agent_catalog. Available even when the owning chat has no running turn.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"task", "operation_id"},
			"properties": map[string]any{
				"operation_id":      operationID(),
				"task":              str("Complete task for the child agent."),
				"label":             str("Short name shown in the Workass Turnos rail."),
				"profile":           enum("Optional user-scored selection profile.", "smart", "tasteful", "budget", "balanced", "independent-review"),
				"provider_id":       str("Optional provider id from workass_agent_catalog; defaults to the profile recommendation or current provider."),
				"model_id":          str("Optional base model id from workass_agent_catalog; defaults to the profile recommendation or current model."),
				"effort":            str("Optional exact effort from the selected catalog model; defaults to profile/current/high."),
				"mode_id":           str("Optional provider-native permission mode from workass_agent_catalog."),
				"permission_intent": enum("Provider-neutral permission intent used when mode_id is omitted. DEFAULTS TO inherit, which gives the child this chat's own effective mode — usually what you want. read and edit are NARROWER than inherit, not safer defaults: they select modes that stop and ask, so the child can park on a request only a human can answer. Reach for them when you mean to restrict the child, and be ready to answer with workass_decide_subagent_permission.", "inherit", "read", "edit", "full"),
				"cwd":               str("Absolute or Workass-root-relative working directory; omit or use inherit for the parent cwd."),
			},
		}, false, true, false, true),
		tool("workass_list_subagents", "List tracked subagents owned by the current Workass turn, including selection, status, and bounded result/error text. Adopted runs from the same chat remain listed across turns and while the chat is idle.", object(map[string]any{}), true, false, true, false),
		tool("workass_wait_subagent", "Wait for one tracked subagent and durably record the wait observation. A child permission request forcibly ends the wait with needsAttention=true, which you must read before continuing; otherwise returns the final result or error.", mutationObject(map[string]any{
			"subagent_id": str("Subagent id returned by spawn/list."),
			"timeout_ms":  map[string]any{"type": "integer", "minimum": 1000, "maximum": 3600000, "description": "Wait timeout in milliseconds; defaults to 10 minutes."},
		}, "subagent_id"), false, false, false, false),
		tool("workass_wait_subagents", "Wait for the first or all selected subagents and durably record the wait observation; returns completed plus still-running snapshots. A child permission request ends the wait with a latched attention list to read. Timeout is a normal result, not an error.", mutationObject(map[string]any{
			"subagent_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 8},
			"return_when":  enum("Return after the first completion or after all complete.", "first", "all"),
			"timeout_ms":   map[string]any{"type": "integer", "minimum": 1000, "maximum": 3600000, "description": "Wait timeout in milliseconds; defaults to 10 minutes."},
		}, "subagent_ids"), false, false, false, false),
		tool("workass_message_subagent", "Send coordinator feedback to a running subagent. It persists one immediate follow-up before attempting acknowledged live steering; unsupported or rejected steering leaves that same follow-up queued without interrupting the child.", mutationObject(map[string]any{
			"subagent_id": str("Running subagent id."),
			"message":     str("Correction, clarification, or additional direction."),
		}, "subagent_id", "message"), false, false, false, true),
		tool("workass_retry_subagent", "Retry a completed/failed/cancelled subagent with the same resolved selection and optional additional guidance.", mutationObject(map[string]any{
			"subagent_id": str("Settled subagent id to retry."),
			"message":     str("Optional retry guidance."),
		}, "subagent_id"), false, true, false, true),
		tool("workass_list_subagent_receipts", "List bounded durable subagent result receipts from this chat, including previous turns.", object(map[string]any{
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 256, "description": "Newest receipts to return; defaults to 32."},
		}), true, false, true, false),
		tool("workass_list_spawned_work", "List background Bash, Agent, and Workflow work passively observed for one exact Workass chat, including live status and an optional bounded output tail.", object(map[string]any{
			"tab_id":     str("Tab id from workass_list_chats."),
			"chat_id":    str("Paired conversation id."),
			"tail_chars": map[string]any{"type": "integer", "minimum": 0, "maximum": 12000, "description": "Optional redacted output tail characters per item; 0 omits tails."},
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_register_external_work", "Register a detached lane that will finish outside the ACP engine. For every ACP provider, work that must outlive the engine must be registered in the same turn; prefer workass_spawn_subagent for delegated agents. Returns the output and done-marker paths to use.", mutationObject(map[string]any{
			"label":       str("Short label for the lane; required."),
			"role":        enum("Lifecycle of the lane. Use work (default) when it finishes and its completion is the answer; use service for a process expected to keep running, such as a dev server, so it does not report this chat as working.", "work", "service"),
			"pid":         integer("Detached process id, if known.", 2),
			"output_file": str("Optional absolute output file path under an allowed temp or Workass external-work directory."),
			"done_file":   str("Optional absolute done marker path; defaults to output_file.done."),
			"tab_id":      str("Owning tab id; omit for the calling chat."),
			"chat_id":     str("Owning chat id; omit for the calling chat."),
		}, "label"), false, false, false, true),
		tool("workass_settle_external_work", "Mark a registered external lane finished. Repeating the same settle is idempotent.", mutationObject(map[string]any{
			"work_id":   str("workId returned by workass_register_external_work."),
			"status":    enum("Terminal status.", "exited", "failed"),
			"exit_code": integer("Process exit code, if known.", 0),
			"summary":   str("Optional short completion summary; Workass redacts secret-shaped text."),
			"tab_id":    str("Owning tab id; omit for the calling chat."),
			"chat_id":   str("Owning chat id; omit for the calling chat."),
		}, "work_id", "status"), false, false, true, true),
		tool("workass_list_spawned_work_receipts", "List bounded durable completion receipts for passively observed background work in one exact Workass chat.", object(map[string]any{
			"tab_id":  str("Tab id from workass_list_chats."),
			"chat_id": str("Paired conversation id."),
			"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 256, "description": "Newest receipts to return; defaults to 32."},
		}, "tab_id", "chat_id"), true, false, true, false),
		tool("workass_cancel_subagent", "Cancel one tracked subagent owned by the current Workass turn.", mutationObject(map[string]any{
			"subagent_id": str("Subagent id returned by spawn/list."),
		}, "subagent_id"), false, true, false, false),
		tool("workass_decide_subagent_permission", "Answer a permission request from one of your tracked subagents rather than leaving it parked on a human. You may always deny; you may allow only what this chat's own mode already does without asking.", mutationObject(map[string]any{
			"subagent_id": str("Subagent id reported with needsAttention by wait/list."),
			"decision":    enum("Allow or deny the action the subagent asked to take.", "allow", "deny"),
		}, "subagent_id", "decision"), false, true, false, false),
	}
}

func callAgentMCPTool(request *http.Request, call browserMCPCallParams, options agentMCPOptions, control *agentControlHandler) (any, error) {
	method := ""
	params := copyAnyMap(call.Arguments)
	operationID, operationErr := requiredStatelessMCPOperationID(agentMCPKind, call)
	if operationErr != nil {
		return agentMCPErrorResult(operationErr.Error()), nil
	}
	if operationID != "" {
		params["operation_id"] = string(operationID)
	}
	if _, exists := params["operationId"]; exists {
		return agentMCPErrorResult("MCP uses operation_id; operationId is not accepted"), nil
	}
	switch call.Name {
	case "workass_list_chats":
		method = "chat.list"
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
			return agentMCPErrorResult("MCP uses mode_id; permission_mode is not accepted"), nil
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
