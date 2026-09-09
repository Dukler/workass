package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
	providercontract "workass/internal/provider"
	"workass/internal/toolcli"
)

const toolsPath = "/workass/tools"

type toolKind string

const (
	agentToolKind   toolKind = "agent"
	browserToolKind toolKind = "browser"
)

// workassToolHandler serves ordinary authenticated JSON requests. It has no
// MCP handshake, protocol negotiation, discovery session, or event stream.
type workassToolHandler struct {
	kind               toolKind
	manager            *acp.Manager
	agentControl       *agentControlHandler
	providerChats      *providerChatRuntime
	validateOwner      func(ownerKey, chatID, tabID string) bool
	browserControlFile string
	browserClient      *http.Client
}

func newWorkassToolHandler(manager *acp.Manager, control *agentControlHandler, controlFile string, chats *providerChatRuntime) *workassToolHandler {
	return &workassToolHandler{manager: manager, agentControl: control, providerChats: chats,
		browserControlFile: controlFile, browserClient: &http.Client{Timeout: 30 * time.Second}}
}

func (h *workassToolHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fail := func(status int, message string) {
		writeToolJSON(w, status, toolcli.Response{Error: acp.RedactSensitiveText(message)})
	}
	if r.URL == nil || r.URL.Path != toolsPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		fail(405, "use GET for the catalog or POST for a tool call")
		return
	}
	if r.TLS == nil {
		fail(426, "TLS is required")
		return
	}
	if !localRemoteAddr(r.RemoteAddr) || strings.TrimSpace(r.Header.Get("Origin")) != "" {
		fail(403, "forbidden")
		return
	}
	owner, ok := bearerCredential(r.Header.Get("Authorization"))
	chatID, tabID := strings.TrimSpace(r.Header.Get("X-Workass-Chat-ID")), strings.TrimSpace(r.Header.Get("X-Workass-Tab-ID"))
	if !ok {
		fail(401, "unauthorized")
		return
	}
	if h.manager == nil || h.providerChats == nil {
		fail(503, "authoritative chat state is unavailable")
		return
	}
	child := chatID == tabID && strings.HasPrefix(chatID, subagentTabPrefix) && strings.TrimPrefix(chatID, subagentTabPrefix) != ""
	if !child {
		if _, _, err := h.providerChats.exactActor(tabID, chatID); err != nil {
			fail(401, "unauthorized")
			return
		}
	}
	if !h.ownerAuthorized(owner, chatID, tabID) {
		fail(401, "unauthorized")
		return
	}
	catalog := append(agentMCPTools(), browserMCPTools()...)
	if child {
		catalog = agentMCPTools()
	}
	if r.Method == http.MethodGet {
		name := r.URL.Query().Get("name")
		if name != "" {
			for _, t := range catalog {
				if t["name"] == name {
					writeToolJSON(w, 200, toolcli.Response{Result: t})
					return
				}
			}
			fail(404, "unknown Workass tool")
			return
		}
		writeToolJSON(w, 200, toolcli.Response{Result: map[string]any{"tools": catalog}})
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		fail(400, "Content-Type must be application/json")
		return
	}
	var call browserMCPCallParams
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&call); err != nil {
		fail(400, "invalid tool call JSON")
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		fail(400, "request must contain one JSON object")
		return
	}
	known := false
	for _, t := range catalog {
		if t["name"] == call.Name {
			known = true
			break
		}
	}
	if !known {
		fail(404, "unknown Workass tool")
		return
	}
	// The per-request dispatcher copy avoids mutating shared routing state.
	dispatcher := *h
	dispatcher.kind = agentToolKind
	if strings.HasPrefix(call.Name, "workass_browser_") {
		dispatcher.kind = browserToolKind
	}
	value, err := dispatcher.callTool(r, call, owner, chatID, tabID)
	if err != nil {
		fail(500, err.Error())
		return
	}
	response := toolResponse(value)
	writeToolJSON(w, 200, response)
}

// Business handlers retain their content representation internally; only this
// transport boundary projects text/JSON and binary attachments onto the CLI API.
func toolResponse(value any) toolcli.Response {
	response := toolcli.Response{}
	item := mapFromAnyMain(value)
	content, _ := item["content"].([]any)
	var texts []any
	for _, raw := range content {
		part := mapFromAnyMain(raw)
		switch part["type"] {
		case "text":
			text, _ := part["text"].(string)
			if failed, _ := item["isError"].(bool); failed {
				response.Error = acp.RedactSensitiveText(text)
				continue
			}
			var decoded any
			if json.Unmarshal([]byte(text), &decoded) == nil {
				texts = append(texts, decoded)
			} else {
				texts = append(texts, text)
			}
		case "image":
			data, _ := part["data"].(string)
			mimeType, _ := part["mimeType"].(string)
			response.Images = append(response.Images, toolcli.Image{Data: data, MIMEType: mimeType})
		}
	}
	if len(texts) == 1 {
		response.Result = texts[0]
	} else if len(texts) > 1 {
		response.Result = texts
	} else if len(content) == 0 {
		response.Result = redactValue(value)
	}
	return response
}

func writeToolJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func workassToolMutates(kind toolKind, name string) bool {
	if kind == browserToolKind {
		switch name {
		case "workass_browser_open", "workass_browser_navigate", "workass_browser_click",
			"workass_browser_type", "workass_browser_scroll", "workass_browser_key",
			"workass_browser_batch", "workass_browser_history":
			return true
		default:
			return false
		}
	}
	switch name {
	case "workass_create_chat", "workass_rename_chat", "workass_configure_chat", "workass_focus_chat",
		"workass_delete_chat", "workass_send_chat_message", "workass_cancel_chat_turn", "workass_host_artifact",
		"workass_apply_update",
		"workass_spawn_subagent", "workass_wait_subagent", "workass_wait_subagents",
		"workass_message_subagent", "workass_retry_subagent", "workass_register_external_work",
		"workass_settle_external_work", "workass_cancel_subagent", "workass_decide_subagent_permission":
		return true
	default:
		return false
	}
}

func requiredToolOperationID(kind toolKind, call browserMCPCallParams) (providercontract.OperationID, error) {
	if !workassToolMutates(kind, call.Name) {
		return "", nil
	}
	if call.Arguments == nil {
		return "", errors.New("mutating Workass tool requires a caller-stable operation_id")
	}
	raw, present := call.Arguments["operation_id"]
	if !present {
		return "", errors.New("mutating Workass tool requires a caller-stable operation_id")
	}
	value, ok := raw.(string)
	if !ok {
		return "", errors.New("Workass operation_id must be a string")
	}
	return providercontract.ValidateOperationID(value)
}

func (h *workassToolHandler) ownerAuthorized(ownerKey, chatID, tabID string) bool {
	if h == nil {
		return false
	}
	if h.validateOwner != nil {
		return h.validateOwner(ownerKey, chatID, tabID)
	}
	return h.manager != nil && h.manager.ValidateAgentOwner(ownerKey, chatID, tabID)
}

func (h *workassToolHandler) callTool(r *http.Request, call browserMCPCallParams, ownerKey, chatID, tabID string) (any, error) {
	if h.kind == browserToolKind {
		if h.providerChats == nil {
			return nil, errors.New("authoritative chat state is unavailable")
		}
		prepared, err := prepareBrowserMCPCall(call)
		if err != nil {
			return browserMCPErrorResult(err.Error()), nil
		}
		operationID, err := requiredToolOperationID(h.kind, call)
		if err != nil {
			return browserMCPErrorResult(err.Error()), nil
		}
		params := prepared.Params
		params["chatId"] = chatID
		if !prepared.Mutating {
			actor, _, err := h.providerChats.exactActor(tabID, chatID)
			if err != nil {
				return browserMCPErrorResult("browser chat attachment is stale"), nil
			}
			actor.mu.Lock()
			state := actor.engine.Snapshot()
			actor.mu.Unlock()
			if state.Deleted || !state.Initialized || strings.TrimSpace(state.Presentation.TabID) != strings.TrimSpace(tabID) {
				return browserMCPErrorResult("browser chat attachment is stale"), nil
			}
			result, err := invokeBrowserControl(h.browserControlFile, prepared.Method, params, h.browserClient)
			if err != nil {
				return browserMCPErrorResult(err.Error()), nil
			}
			return formatBrowserMCPResult(call, prepared, result)
		}
		digest := browserMCPRequestDigest(prepared.Method, params)
		reply, err := h.providerChats.executeBrowserMutationWithAdmission(
			r.Context(), tabID, chatID, providercontract.OperationID(operationID), call.Name, prepared.Method, digest,
			// Prove a live, controller-owning shell before the first durable
			// Record+Claim. The executor runs this inside the per-actor external
			// mutation lock, but never while holding the actor state lock. Existing
			// terminal operations skip admission; dispatched/ambiguous operations
			// remain pure receipt readbacks.
			func() error {
				return probeBrowserControl(h.browserControlFile, h.browserClient)
			},
			func() (browserControlReply, error) {
				return invokeBrowserControlMutation(h.browserControlFile, prepared.Method, params, string(operationID), digest, h.browserClient)
			},
			func() (browserControlReply, error) {
				return invokeBrowserControlReceipt(h.browserControlFile, string(operationID), digest, h.browserClient)
			},
		)
		if err != nil {
			return browserMCPErrorResult(acp.RedactSensitiveText(err.Error())), nil
		}
		return formatBrowserMCPReply(call, prepared, reply)
	}
	if h.agentControl == nil {
		return nil, errors.New("Workass agent control is unavailable")
	}
	operationID, err := requiredToolOperationID(h.kind, call)
	if err != nil {
		return agentMCPErrorResult(err.Error()), nil
	}
	return callAgentMCPTool(r, call, agentMCPOptions{
		ChatID: chatID, TabID: tabID, OwnerKey: ownerKey, OperationID: string(operationID),
	}, h.agentControl)
}
