// Package mcpstdio exposes Workass's daemon-owned MCP endpoints over the
// mandatory ACP stdio transport. It is provider-neutral: the ACP initialize
// handshake chooses HTTP or stdio, while both transports reach the same tools.
package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"workass/internal/mcpprotocol"
)

const (
	protocol2025 = mcpprotocol.LatestLegacyVersion
	protocol2026 = mcpprotocol.ModernVersion

	envEndpoint           = "WORKASS_MCP_ENDPOINT"
	envCACertFile         = "WORKASS_MCP_CA_FILE"
	envOwnerCredential    = "WORKASS_MCP_OWNER_CREDENTIAL"
	envChatID             = "WORKASS_MCP_CHAT_ID"
	envTabID              = "WORKASS_MCP_TAB_ID"
	maxMessageBytes       = 4 * 1024 * 1024
	defaultRequestTimeout = 30 * time.Second
)

type revision uint8

const (
	revisionUnset revision = iota
	revision2025
	revision2026
)

type config struct {
	endpoint      *url.URL
	authorization string
	chatID        string
	tabID         string
	client        *http.Client
}

type server struct {
	config
	revision           revision
	legacyVersion      string
	clientInfo         map[string]any
	clientCapabilities map[string]any
}

// ServeEnvironment validates the injected per-session configuration and serves
// newline-delimited MCP until stdin closes or the context is cancelled.
func ServeEnvironment(ctx context.Context, stdin io.Reader, stdout io.Writer, getenv func(string) string) error {
	if getenv == nil {
		return errors.New("environment reader is unavailable")
	}
	cfg, err := environmentConfig(getenv)
	if err != nil {
		return err
	}
	return (&server{config: cfg}).serve(ctx, stdin, stdout)
}

func environmentConfig(getenv func(string) string) (config, error) {
	endpoint, err := validatedEndpoint(getenv(envEndpoint))
	if err != nil {
		return config{}, err
	}
	authorization := strings.TrimSpace(getenv(envOwnerCredential))
	if !validBearerCredential(authorization) {
		return config{}, errors.New("MCP owner credential is missing or invalid")
	}
	chatID := strings.TrimSpace(getenv(envChatID))
	tabID := strings.TrimSpace(getenv(envTabID))
	if chatID == "" || tabID == "" {
		return config{}, errors.New("MCP chat attachment is incomplete")
	}
	client, err := pinnedHTTPClient(strings.TrimSpace(getenv(envCACertFile)))
	if err != nil {
		return config{}, err
	}
	return config{endpoint: endpoint, authorization: authorization, chatID: chatID, tabID: tabID, client: client}, nil
}

func validatedEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil {
		return nil, errors.New("MCP endpoint is invalid")
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "mcp.localhost") || parsed.Port() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("MCP endpoint must be the authenticated mcp.localhost HTTPS service")
	}
	if parsed.Path != "/workass/mcp/agent" && parsed.Path != "/workass/mcp/browser" {
		return nil, errors.New("MCP endpoint path is not supported")
	}
	return parsed, nil
}

func validBearerCredential(value string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	credential := strings.TrimPrefix(value, prefix)
	return credential != "" && credential == strings.TrimSpace(credential) && !strings.ContainsAny(credential, "\r\n")
}

func pinnedHTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return nil, errors.New("MCP CA certificate file is missing")
	}
	pemBytes, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read MCP CA certificate: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pemBytes) {
		return nil, errors.New("MCP CA certificate contains no trusted certificate")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: transport, Timeout: defaultRequestTimeout}, nil
}

func (s *server) serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	if s == nil || s.endpoint == nil || s.client == nil {
		return errors.New("MCP stdio server is not configured")
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), maxMessageBytes)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		response, emit := s.handle(ctx, line)
		if !emit {
			continue
		}
		if _, err := stdout.Write(append(response, '\n')); err != nil {
			return fmt.Errorf("write MCP response: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP request: %w", err)
	}
	return nil
}

func (s *server) handle(ctx context.Context, line []byte) ([]byte, bool) {
	message, err := decodeMessage(line)
	if err != nil {
		return rpcError(nil, -32700, "invalid JSON"), true
	}
	id, hasID := message["id"]
	method := rawString(message["method"])
	if rawString(message["jsonrpc"]) != "2.0" || method == "" {
		return rpcError(id, -32600, "invalid JSON-RPC message"), hasID
	}
	if !hasID || bytes.Equal(bytes.TrimSpace(id), []byte("null")) {
		// Neither supported Workass MCP revision needs client notifications. The
		// 2025 initialized notification is acknowledged by silence, as required.
		return nil, false
	}

	if method == "initialize" {
		if s.revision != revisionUnset {
			return rpcError(id, -32600, "MCP connection is already initialized"), true
		}
		return s.initialize2025(ctx, id, message["params"]), true
	}
	if s.revision == revisionUnset {
		if requestProtocol(message["params"]) == protocol2026 {
			s.revision = revision2026
		} else {
			return rpcError(id, -32002, "MCP connection is not initialized"), true
		}
	}
	if s.revision == revision2026 {
		response, err := s.forward(ctx, line, method, message["params"])
		if err != nil {
			return rpcError(id, -32000, "Workass MCP endpoint request failed"), true
		}
		return response, true
	}

	switch method {
	case "ping":
		return rpcResult(id, map[string]any{}), true
	case "tools/list", "tools/call":
		modern, err := s.modernRequest(message, method)
		if err != nil {
			return rpcError(id, -32602, err.Error()), true
		}
		response, err := s.forward(ctx, modern, method, json.RawMessage(messageParams(modern)))
		if err != nil {
			return rpcError(id, -32000, "Workass MCP endpoint request failed"), true
		}
		translated, err := responseFor2025(response, method)
		if err != nil {
			return rpcError(id, -32603, "Workass MCP endpoint returned an invalid response"), true
		}
		return translated, true
	default:
		return rpcError(id, -32601, "method not found"), true
	}
}

func (s *server) initialize2025(ctx context.Context, id json.RawMessage, rawParams json.RawMessage) []byte {
	params, err := objectFromRaw(rawParams)
	if err != nil {
		return rpcError(id, -32602, "initialize params must be an object")
	}
	requestedVersion, ok := params["protocolVersion"].(string)
	if !ok || strings.TrimSpace(requestedVersion) == "" {
		return rpcError(id, -32602, "initialize params must include protocolVersion")
	}
	requestedVersion = strings.TrimSpace(requestedVersion)
	// The 2026 direct protocol is self-describing and has no initialize
	// exchange. Reject a modern initialize instead of silently negotiating it
	// down to the latest legacy revision.
	if requestedVersion == protocol2026 {
		return rpcError(id, -32022, "unsupported MCP protocol version")
	}
	if info, ok := params["clientInfo"].(map[string]any); ok {
		s.clientInfo = info
	} else {
		s.clientInfo = map[string]any{}
	}
	if capabilities, ok := params["capabilities"].(map[string]any); ok {
		s.clientCapabilities = capabilities
	} else {
		s.clientCapabilities = map[string]any{}
	}
	s.revision = revision2025
	s.legacyVersion = mcpprotocol.NegotiateLegacy(requestedVersion)
	discover := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"method":  "server/discover",
		"params": map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    protocol2026,
			"io.modelcontextprotocol/clientInfo":         s.clientInfo,
			"io.modelcontextprotocol/clientCapabilities": s.clientCapabilities,
		}},
	}
	body, _ := json.Marshal(discover)
	response, err := s.forward(ctx, body, "server/discover", json.RawMessage(messageParams(body)))
	if err != nil {
		return rpcError(id, -32000, "Workass MCP endpoint request failed")
	}
	result, rpcErr, err := decodedResponse(response)
	if err != nil {
		return rpcError(id, -32603, "Workass MCP endpoint returned an invalid response")
	}
	if rpcErr != nil {
		return response
	}
	capabilities, ok := result["capabilities"]
	if !ok {
		return rpcError(id, -32603, "Workass MCP discovery omitted capabilities")
	}
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"]
	if !ok {
		return rpcError(id, -32603, "Workass MCP discovery omitted server identity")
	}
	translated := map[string]any{
		"protocolVersion": s.legacyVersion,
		"capabilities":    capabilities,
		"serverInfo":      serverInfo,
	}
	if instructions, ok := result["instructions"]; ok {
		translated["instructions"] = instructions
	}
	return rpcResult(id, translated)
}

func (s *server) modernRequest(message map[string]json.RawMessage, method string) ([]byte, error) {
	params, err := objectFromRaw(message["params"])
	if err != nil {
		return nil, errors.New("request params must be an object")
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
	}
	meta["io.modelcontextprotocol/protocolVersion"] = protocol2026
	meta["io.modelcontextprotocol/clientInfo"] = s.clientInfo
	meta["io.modelcontextprotocol/clientCapabilities"] = s.clientCapabilities
	params["_meta"] = meta
	encodedParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	message["params"] = encodedParams
	message["method"], _ = json.Marshal(method)
	return json.Marshal(message)
}

func (s *server) forward(ctx context.Context, body []byte, method string, rawParams json.RawMessage) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", s.authorization)
	request.Header.Set("X-Workass-Chat-ID", s.chatID)
	request.Header.Set("X-Workass-Tab-ID", s.tabID)
	request.Header.Set("MCP-Protocol-Version", requestProtocol(rawParams))
	request.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		params, err := objectFromRaw(rawParams)
		if err != nil {
			return nil, err
		}
		name, _ := params["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("tools/call name is missing")
		}
		request.Header.Set("Mcp-Name", encodedHeaderValue(name))
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxMessageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > maxMessageBytes || !json.Valid(data) {
		return nil, errors.New("MCP endpoint response is not one bounded JSON value")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}

func responseFor2025(response []byte, method string) ([]byte, error) {
	result, rpcErr, err := decodedResponse(response)
	if err != nil || rpcErr != nil {
		return response, err
	}
	translated := make(map[string]any)
	switch method {
	case "tools/list":
		tools, ok := result["tools"]
		if !ok {
			return nil, errors.New("tools/list omitted tools")
		}
		translated["tools"] = tools
		if cursor, ok := result["nextCursor"]; ok {
			translated["nextCursor"] = cursor
		}
	case "tools/call":
		for _, key := range []string{"content", "structuredContent", "isError"} {
			if value, ok := result[key]; ok {
				translated[key] = value
			}
		}
		if _, ok := translated["content"]; !ok {
			translated["content"] = []any{}
		}
	default:
		return nil, errors.New("unsupported response translation")
	}
	message, err := decodeMessage(response)
	if err != nil {
		return nil, err
	}
	return rpcResult(message["id"], translated), nil
}

func decodedResponse(response []byte) (map[string]any, map[string]any, error) {
	message, err := objectFromBytes(response)
	if err != nil {
		return nil, nil, err
	}
	if rawErr, ok := message["error"].(map[string]any); ok {
		return nil, rawErr, nil
	}
	result, ok := message["result"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("JSON-RPC response omitted result")
	}
	return result, nil, nil
}

func decodeMessage(data []byte) (map[string]json.RawMessage, error) {
	var message map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&message); err != nil || message == nil {
		return nil, errors.New("message must be an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("message must contain one JSON value")
	}
	return message, nil
}

func objectFromRaw(raw json.RawMessage) (map[string]any, error) {
	return objectFromBytes(raw)
}

func objectFromBytes(raw []byte) (map[string]any, error) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil || value == nil {
		return nil, errors.New("value must be an object")
	}
	return value, nil
}

func requestProtocol(rawParams json.RawMessage) string {
	params, err := objectFromRaw(rawParams)
	if err != nil {
		return ""
	}
	meta, _ := params["_meta"].(map[string]any)
	version, _ := meta["io.modelcontextprotocol/protocolVersion"].(string)
	return strings.TrimSpace(version)
}

func messageParams(message []byte) []byte {
	decoded, err := decodeMessage(message)
	if err != nil {
		return nil
	}
	return decoded["params"]
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return strings.TrimSpace(value)
}

func encodedHeaderValue(value string) string {
	if value != strings.TrimSpace(value) {
		return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
		}
	}
	return value
}

func rpcResult(id json.RawMessage, result any) []byte {
	return rpcMessage(id, "result", result)
}

func rpcError(id json.RawMessage, code int, message string) []byte {
	return rpcMessage(id, "error", map[string]any{"code": code, "message": message})
}

func rpcMessage(id json.RawMessage, field string, value any) []byte {
	if len(bytes.TrimSpace(id)) == 0 {
		id = json.RawMessage("null")
	}
	message := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), field: value}
	encoded, _ := json.Marshal(message)
	return encoded
}
