package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"workass/internal/acp"
)

const (
	statelessMCPProtocolVersion = "2026-07-28"
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
	browserControlFile string
	browserClient      *http.Client
}

type statelessMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func newAgentStatelessMCPHandler(manager *acp.Manager, control *agentControlHandler) http.Handler {
	return &statelessMCPHandler{
		kind: agentMCPKind, path: agentMCPPath, name: "workass-agent",
		manager: manager, agentControl: control,
	}
}

func newBrowserStatelessMCPHandler(manager *acp.Manager, controlFile string) http.Handler {
	return &statelessMCPHandler{
		kind: browserMCPKind, path: browserMCPPath, name: "workass-browser",
		manager: manager, browserControlFile: controlFile,
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
	if !ok || h.manager == nil || !h.manager.ValidateAgentOwner(ownerKey, chatID, tabID) {
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
	if request.JSONRPC != "2.0" || strings.TrimSpace(request.Method) == "" || request.ID == nil {
		h.writeError(w, http.StatusBadRequest, request.ID, -32600, "invalid JSON-RPC request", nil)
		return
	}
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
		result, err := h.callTool(r, call, ownerKey, chatID, tabID, stableMCPRequestOperationID(request.ID, call.Name, tabID, chatID))
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

func stableMCPRequestOperationID(requestID any, toolName, tabID, chatID string) string {
	raw, _ := json.Marshal(requestID)
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"agent-mcp-v1", strings.TrimSpace(toolName), strings.TrimSpace(tabID), strings.TrimSpace(chatID), string(raw),
	}, "\x00")))
	return fmt.Sprintf("agent-mcp:%x", digest[:16])
}

func (h *statelessMCPHandler) callTool(r *http.Request, call browserMCPCallParams, ownerKey, chatID, tabID, operationID string) (any, error) {
	if h.kind == browserMCPKind {
		return callBrowserMCPTool(call, browserMCPOptions{
			ControlFile: h.browserControlFile, ChatID: chatID, HTTPClient: h.browserClient,
		}, h.browserClient)
	}
	if h.agentControl == nil {
		return nil, errors.New("Workass agent control is unavailable")
	}
	return callAgentMCPTool(r, call, agentMCPOptions{
		ChatID: chatID, TabID: tabID, OwnerKey: ownerKey, OperationID: operationID,
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
