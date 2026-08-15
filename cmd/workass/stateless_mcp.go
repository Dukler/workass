package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
	"workass/internal/mcpprotocol"
	providercontract "workass/internal/provider"
)

const (
	statelessMCPProtocolVersion = mcpprotocol.ModernVersion
	agentMCPPath                = "/workass/mcp/agent"
	browserMCPPath              = "/workass/mcp/browser"
	mcpResultTTLMS              = 5 * 60 * 1000
)

type statelessMCPKind string

const (
	agentMCPKind   statelessMCPKind = "agent"
	browserMCPKind statelessMCPKind = "browser"
)

type statelessMCPHandler struct {
	kind               statelessMCPKind
	path               string
	name               string
	manager            *acp.Manager
	agentControl       *agentControlHandler
	providerChats      *providerChatRuntime
	validateOwner      func(ownerKey, chatID, tabID string) bool
	browserControlFile string
	browserClient      *http.Client
}

type statelessMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type statelessMCPRevision uint8

const (
	statelessMCPRevisionUnsupported statelessMCPRevision = iota
	statelessMCPRevisionLegacy
	statelessMCPRevisionModern
)

// statelessMCPToolMutates is the ingress manifest for the logical-operation
// boundary. It is intentionally separate from the JSON-RPC transport id: a
// caller may retry one logical mutation with any number of transport ids.
func statelessMCPToolMutates(kind statelessMCPKind, name string) bool {
	if kind == browserMCPKind {
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
		"workass_host_html", "workass_spawn_subagent", "workass_wait_subagent", "workass_wait_subagents",
		"workass_message_subagent", "workass_retry_subagent", "workass_register_external_work",
		"workass_settle_external_work", "workass_cancel_subagent", "workass_decide_subagent_permission":
		return true
	default:
		return false
	}
}

func requiredStatelessMCPOperationID(kind statelessMCPKind, call browserMCPCallParams) (providercontract.OperationID, error) {
	if !statelessMCPToolMutates(kind, call.Name) {
		return "", nil
	}
	if call.Arguments == nil {
		return "", errors.New("mutating MCP tool requires a caller-stable operation_id")
	}
	var operationID providercontract.OperationID
	seen := false
	for _, key := range []string{"operation_id", "operationId"} {
		raw, present := call.Arguments[key]
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", errors.New("MCP operation_id must be a string")
		}
		validated, err := providercontract.ValidateOperationID(value)
		if err != nil {
			return "", err
		}
		if seen && operationID != validated {
			return "", errors.New("operation_id and operationId must identify the same operation")
		}
		operationID, seen = validated, true
	}
	if !seen {
		return "", errors.New("mutating MCP tool requires a caller-stable operation_id")
	}
	return operationID, nil
}

func newAgentStatelessMCPHandler(manager *acp.Manager, control *agentControlHandler) http.Handler {
	var providerChats *providerChatRuntime
	if control != nil && control.chats != nil {
		providerChats = control.chats.providerChats
	}
	return &statelessMCPHandler{
		kind: agentMCPKind, path: agentMCPPath, name: "workass-agent",
		manager: manager, agentControl: control, providerChats: providerChats,
	}
}

func newBrowserStatelessMCPHandler(manager *acp.Manager, controlFile string, providerChats *providerChatRuntime) http.Handler {
	return &statelessMCPHandler{
		kind: browserMCPKind, path: browserMCPPath, name: "workass-browser",
		manager: manager, providerChats: providerChats, browserControlFile: controlFile,
		browserClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (h *statelessMCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Path != h.path {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.writeError(w, http.StatusMethodNotAllowed, nil, -32600, "MCP endpoint accepts POST only", nil)
		return
	}
	if r.TLS == nil {
		h.writeError(w, http.StatusUpgradeRequired, nil, -32600, "TLS is required", nil)
		return
	}
	if !localRemoteAddr(r.RemoteAddr) {
		h.writeError(w, http.StatusForbidden, nil, -32600, "forbidden", nil)
		return
	}
	// Workass MCP is not a browser API. Reject every browser-originated request
	// so a hostile page cannot use DNS rebinding to reach the loopback daemon.
	if strings.TrimSpace(r.Header.Get("Origin")) != "" {
		h.writeError(w, http.StatusForbidden, nil, -32600, "browser origins are not permitted", nil)
		return
	}
	ownerKey, ok := bearerCredential(r.Header.Get("Authorization"))
	chatID := strings.TrimSpace(r.Header.Get("X-Workass-Chat-ID"))
	tabID := strings.TrimSpace(r.Header.Get("X-Workass-Tab-ID"))
	if !ok {
		h.writeError(w, http.StatusUnauthorized, nil, -32000, "unauthorized", nil)
		return
	}
	if h.manager == nil || h.providerChats == nil {
		h.writeError(w, http.StatusServiceUnavailable, nil, -32001, "authoritative chat state is unavailable", nil)
		return
	}
	// The durable actor is the authority for whether this exact attachment is
	// still alive. Only after it fences the pair may the short-lived Manager
	// owner capability participate in authorization; a deleted chat must not
	// remain usable merely because its provider session is still live.
	if _, _, err := h.providerChats.exactActor(tabID, chatID); err != nil {
		h.writeError(w, http.StatusUnauthorized, nil, -32000, "unauthorized", nil)
		return
	}
	if !h.ownerAuthorized(ownerKey, chatID, tabID) {
		h.writeError(w, http.StatusUnauthorized, nil, -32000, "unauthorized", nil)
		return
	}
	accept := strings.ToLower(r.Header.Get("Accept"))
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		h.writeError(w, http.StatusBadRequest, nil, -32600, "Accept must include application/json and text/event-stream", nil)
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		h.writeError(w, http.StatusBadRequest, nil, -32600, "Content-Type must be application/json", nil)
		return
	}

	var request statelessMCPRequest
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4*1024*1024))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		h.writeError(w, http.StatusBadRequest, nil, -32700, "invalid JSON", nil)
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		h.writeError(w, http.StatusBadRequest, request.ID, -32700, "request body must contain one JSON-RPC message", nil)
		return
	}
	if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" {
		h.writeError(w, http.StatusBadRequest, request.ID, -32600, "invalid JSON-RPC request", nil)
		return
	}
	revision, version := classifyStatelessMCPRequest(r, request)
	if revision == statelessMCPRevisionUnsupported {
		h.writeError(w, http.StatusBadRequest, request.ID, -32022, "unsupported MCP protocol version", map[string]any{
			"requested": version, "supported": mcpprotocol.AllVersions(),
		})
		return
	}
	if request.ID == nil {
		if revision == statelessMCPRevisionLegacy && request.Method == "notifications/initialized" {
			if err := validateLegacyStatelessMCPHeaders(r, request, version, nil); err != nil {
				h.writeError(w, http.StatusBadRequest, nil, -32020, err.Error(), nil)
				return
			}
			h.writeAccepted(w)
			return
		}
		h.writeError(w, http.StatusBadRequest, nil, -32600, "invalid JSON-RPC request", nil)
		return
	}
	if revision == statelessMCPRevisionLegacy {
		h.serveLegacyMCP(w, r, request, version, ownerKey, chatID, tabID)
		return
	}
	h.serveModernMCP(w, r, request, ownerKey, chatID, tabID)
}

func (h *statelessMCPHandler) serveModernMCP(
	w http.ResponseWriter,
	r *http.Request,
	request statelessMCPRequest,
	ownerKey, chatID, tabID string,
) {
	params, meta, err := decodeStatelessMCPParams(request.Params)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, request.ID, -32602, err.Error(), nil)
		return
	}
	if err := validateStatelessMCPHeaders(r, request, params, meta); err != nil {
		h.writeError(w, http.StatusBadRequest, request.ID, -32020, err.Error(), nil)
		return
	}
	version := strings.TrimSpace(mcpString(meta["io.modelcontextprotocol/protocolVersion"]))
	if version != statelessMCPProtocolVersion {
		h.writeError(w, http.StatusBadRequest, request.ID, -32022, "unsupported MCP protocol version", map[string]any{
			"requested": version, "supported": []string{statelessMCPProtocolVersion},
		})
		return
	}
	if _, ok := meta["io.modelcontextprotocol/clientCapabilities"].(map[string]any); !ok {
		h.writeError(w, http.StatusBadRequest, request.ID, -32602, "request _meta must include client capabilities", nil)
		return
	}

	switch request.Method {
	case "server/discover":
		h.writeResult(w, request.ID, map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{statelessMCPProtocolVersion},
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":      h.instructions(),
			"ttlMs":             mcpResultTTLMS,
			"cacheScope":        "private",
			"_meta":             h.resultMeta(),
		})
	case "tools/list":
		h.writeResult(w, request.ID, map[string]any{
			"resultType": "complete",
			"tools":      h.tools(),
			"ttlMs":      mcpResultTTLMS,
			"cacheScope": "private",
			"_meta":      h.resultMeta(),
		})
	case "tools/call":
		var call browserMCPCallParams
		if err := json.Unmarshal(request.Params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
			h.writeError(w, http.StatusBadRequest, request.ID, -32602, "invalid tools/call parameters", nil)
			return
		}
		result, err := h.callTool(r, call, ownerKey, chatID, tabID)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, request.ID, -32603, acp.RedactSensitiveText(err.Error()), nil)
			return
		}
		resultMap := mapFromAnyMain(result)
		resultMap["resultType"] = "complete"
		resultMap["_meta"] = h.resultMeta()
		h.writeResult(w, request.ID, resultMap)
	default:
		h.writeError(w, http.StatusNotFound, request.ID, -32601, "method not found", nil)
	}
}

func (h *statelessMCPHandler) serveLegacyMCP(
	w http.ResponseWriter,
	r *http.Request,
	request statelessMCPRequest,
	version, ownerKey, chatID, tabID string,
) {
	params, err := decodeLegacyStatelessMCPParams(request.Params)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, request.ID, -32602, err.Error(), nil)
		return
	}
	if err := validateLegacyStatelessMCPHeaders(r, request, version, params); err != nil {
		h.writeError(w, http.StatusBadRequest, request.ID, -32020, err.Error(), nil)
		return
	}

	switch request.Method {
	case "initialize":
		requested := strings.TrimSpace(mcpString(params["protocolVersion"]))
		if requested == "" {
			h.writeError(w, http.StatusBadRequest, request.ID, -32602, "initialize params must include protocolVersion", nil)
			return
		}
		h.writeResult(w, request.ID, map[string]any{
			"protocolVersion": mcpprotocol.NegotiateLegacy(requested),
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": h.name, "version": daemonVersion},
			"instructions":    h.instructions(),
		})
	case "ping":
		h.writeResult(w, request.ID, map[string]any{})
	case "tools/list":
		h.writeResult(w, request.ID, map[string]any{"tools": h.tools()})
	case "tools/call":
		var call browserMCPCallParams
		if err := json.Unmarshal(request.Params, &call); err != nil || strings.TrimSpace(call.Name) == "" {
			h.writeError(w, http.StatusBadRequest, request.ID, -32602, "invalid tools/call parameters", nil)
			return
		}
		result, err := h.callTool(r, call, ownerKey, chatID, tabID)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, request.ID, -32603, acp.RedactSensitiveText(err.Error()), nil)
			return
		}
		h.writeResult(w, request.ID, result)
	default:
		h.writeError(w, http.StatusNotFound, request.ID, -32601, "method not found", nil)
	}
}

func (h *statelessMCPHandler) ownerAuthorized(ownerKey, chatID, tabID string) bool {
	if h == nil {
		return false
	}
	if h.validateOwner != nil {
		return h.validateOwner(ownerKey, chatID, tabID)
	}
	return h.manager != nil && h.manager.ValidateAgentOwner(ownerKey, chatID, tabID)
}

func (h *statelessMCPHandler) tools() []map[string]any {
	if h.kind == browserMCPKind {
		return browserMCPTools()
	}
	return agentMCPTools()
}

func (h *statelessMCPHandler) instructions() string {
	if h.kind == browserMCPKind {
		return "Control only the Workass browser owned by the authenticated chat."
	}
	return "Use exact Workass tab and chat identifiers and the live catalog when orchestrating tracked subagents."
}

func (h *statelessMCPHandler) resultMeta() map[string]any {
	return map[string]any{
		"io.modelcontextprotocol/serverInfo": map[string]any{
			"name": h.name, "version": daemonVersion,
		},
	}
}

func (h *statelessMCPHandler) callTool(r *http.Request, call browserMCPCallParams, ownerKey, chatID, tabID string) (any, error) {
	if h.kind == browserMCPKind {
		if h.providerChats == nil {
			return nil, errors.New("authoritative chat state is unavailable")
		}
		prepared, err := prepareBrowserMCPCall(call)
		if err != nil {
			return browserMCPErrorResult(err.Error()), nil
		}
		operationID, err := requiredStatelessMCPOperationID(h.kind, call)
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
			defer actor.mu.Unlock()
			state := actor.engine.Snapshot()
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
		reply, err := h.providerChats.executeBrowserMutation(
			r.Context(), tabID, chatID, providercontract.OperationID(operationID), call.Name, prepared.Method, digest,
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
	operationID, err := requiredStatelessMCPOperationID(h.kind, call)
	if err != nil {
		return agentMCPErrorResult(err.Error()), nil
	}
	return callAgentMCPTool(r, call, agentMCPOptions{
		ChatID: chatID, TabID: tabID, OwnerKey: ownerKey, OperationID: string(operationID),
	}, h.agentControl)
}

func (h *statelessMCPHandler) writeResult(w http.ResponseWriter, id any, result any) {
	h.writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (h *statelessMCPHandler) writeError(w http.ResponseWriter, status int, id any, code int, message string, data any) {
	errorValue := map[string]any{"code": code, "message": acp.RedactSensitiveText(message)}
	if data != nil {
		errorValue["data"] = data
	}
	h.writeJSON(w, status, map[string]any{"jsonrpc": "2.0", "id": id, "error": errorValue})
}

func (h *statelessMCPHandler) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (h *statelessMCPHandler) writeAccepted(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusAccepted)
}

func classifyStatelessMCPRequest(r *http.Request, request statelessMCPRequest) (statelessMCPRevision, string) {
	headerVersion := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if request.Method == "initialize" {
		if headerVersion == "" {
			// The 2026 direct protocol does not have an initialize exchange. Do
			// not silently reinterpret a known-modern body as a legacy client
			// merely because its HTTP version header is absent.
			var params map[string]any
			if decoded, err := decodeLegacyStatelessMCPParams(request.Params); err == nil {
				params = decoded
			}
			bodyVersion := strings.TrimSpace(mcpString(params["protocolVersion"]))
			if bodyVersion == statelessMCPProtocolVersion {
				return statelessMCPRevisionUnsupported, bodyVersion
			}
			return statelessMCPRevisionLegacy, mcpprotocol.LatestLegacyVersion
		}
		if mcpprotocol.IsLegacy(headerVersion) {
			return statelessMCPRevisionLegacy, headerVersion
		}
		return statelessMCPRevisionUnsupported, headerVersion
	}
	if headerVersion == statelessMCPProtocolVersion {
		return statelessMCPRevisionModern, headerVersion
	}
	if mcpprotocol.IsLegacy(headerVersion) {
		return statelessMCPRevisionLegacy, headerVersion
	}
	if headerVersion != "" {
		return statelessMCPRevisionUnsupported, headerVersion
	}

	bodyVersion := statelessMCPBodyVersion(request.Params)
	if bodyVersion == statelessMCPProtocolVersion {
		return statelessMCPRevisionModern, bodyVersion
	}
	if mcpprotocol.IsLegacy(bodyVersion) {
		return statelessMCPRevisionLegacy, bodyVersion
	}
	if bodyVersion != "" {
		return statelessMCPRevisionUnsupported, bodyVersion
	}
	// The modern revision requires its routing headers. Treat an incomplete
	// modern-shaped request as modern so header validation rejects it instead
	// of silently downgrading it to the initialize-based protocol.
	if r.Header.Get("Mcp-Method") != "" || r.Header.Get("Mcp-Name") != "" || request.Method == "server/discover" {
		return statelessMCPRevisionModern, statelessMCPProtocolVersion
	}
	return statelessMCPRevisionLegacy, mcpprotocol.LatestLegacyVersion
}

func statelessMCPBodyVersion(raw json.RawMessage) string {
	params, err := decodeLegacyStatelessMCPParams(raw)
	if err != nil {
		return ""
	}
	meta, _ := params["_meta"].(map[string]any)
	return strings.TrimSpace(mcpString(meta["io.modelcontextprotocol/protocolVersion"]))
}

func decodeLegacyStatelessMCPParams(raw json.RawMessage) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]any{}, nil
	}
	var params map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil || params == nil {
		return nil, errors.New("request params must be an object")
	}
	return params, nil
}

func validateLegacyStatelessMCPHeaders(
	r *http.Request,
	request statelessMCPRequest,
	version string,
	params map[string]any,
) error {
	headerVersion := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if headerVersion != "" && !mcpprotocol.IsLegacy(headerVersion) {
		return errors.New("MCP-Protocol-Version header is not a supported legacy revision")
	}
	if params != nil {
		if request.Method == "initialize" {
			requestedVersion := strings.TrimSpace(mcpString(params["protocolVersion"]))
			if headerVersion != "" && requestedVersion != "" && headerVersion != requestedVersion {
				return errors.New("MCP-Protocol-Version header does not match initialize params")
			}
		}
		meta, _ := params["_meta"].(map[string]any)
		bodyVersion := strings.TrimSpace(mcpString(meta["io.modelcontextprotocol/protocolVersion"]))
		if bodyVersion != "" && (!mcpprotocol.IsLegacy(bodyVersion) || (headerVersion != "" && headerVersion != bodyVersion)) {
			return errors.New("MCP-Protocol-Version header does not match request _meta")
		}
	}
	if headerVersion != "" && version != "" && headerVersion != version {
		return errors.New("MCP-Protocol-Version header does not match negotiated revision")
	}
	if method := r.Header.Get("Mcp-Method"); method != "" && method != request.Method {
		return errors.New("Mcp-Method header does not match request method")
	}
	if request.Method == "tools/call" && r.Header.Get("Mcp-Name") != "" {
		bodyName := mcpString(params["name"])
		headerName, err := decodeMCPHeaderValue(r.Header.Get("Mcp-Name"))
		if err != nil || bodyName == "" || headerName != bodyName {
			return errors.New("Mcp-Name header does not match request tool name")
		}
	}
	return nil
}

func decodeStatelessMCPParams(raw json.RawMessage) (map[string]any, map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil, errors.New("request params are required")
	}
	var params map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&params); err != nil || params == nil {
		return nil, nil, errors.New("request params must be an object")
	}
	meta, ok := params["_meta"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("request params must include _meta")
	}
	return params, meta, nil
}

func validateStatelessMCPHeaders(r *http.Request, request statelessMCPRequest, params, meta map[string]any) error {
	headerVersion := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if headerVersion == "" {
		return errors.New("missing MCP-Protocol-Version header")
	}
	bodyVersion := strings.TrimSpace(mcpString(meta["io.modelcontextprotocol/protocolVersion"]))
	if bodyVersion == "" || headerVersion != bodyVersion {
		return errors.New("MCP-Protocol-Version header does not match request _meta")
	}
	if r.Header.Get("Mcp-Method") != request.Method {
		return errors.New("Mcp-Method header does not match request method")
	}
	if request.Method == "tools/call" {
		bodyName := mcpString(params["name"])
		headerName, err := decodeMCPHeaderValue(r.Header.Get("Mcp-Name"))
		if err != nil || bodyName == "" || headerName != bodyName {
			return errors.New("Mcp-Name header does not match request tool name")
		}
	}
	return nil
}

func decodeMCPHeaderValue(value string) (string, error) {
	if value == "" {
		return "", errors.New("header value is missing")
	}
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		encoded := strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?=")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", err
		}
		return string(decoded), nil
	}
	if strings.HasPrefix(value, "=?base64?") || strings.HasSuffix(value, "?=") || strings.TrimSpace(value) != value {
		return "", errors.New("invalid MCP header encoding")
	}
	for _, char := range []byte(value) {
		if char < 0x20 || char > 0x7e {
			return "", errors.New("invalid MCP header characters")
		}
	}
	return value, nil
}

func bearerCredential(value string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	credential := strings.TrimSpace(strings.TrimPrefix(value, prefix))
	if credential == "" || len(credential) != len(strings.TrimPrefix(value, prefix)) {
		return "", false
	}
	return credential, true
}

func mcpString(value any) string {
	text, _ := value.(string)
	return text
}
