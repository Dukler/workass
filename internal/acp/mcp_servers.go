package acp

import (
	"errors"
	"path/filepath"
	"strings"
)

type mcpServerTransport uint8

const (
	mcpServerStdio mcpServerTransport = iota
	mcpServerHTTP
)

type workassMCPServer struct {
	name string
	path string
}

func (b *Bridge) negotiatedMCPTransport() mcpServerTransport {
	if b == nil {
		return mcpServerStdio
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	capabilities := mapFromAny(b.agentCaps["mcpCapabilities"])
	if supported, ok := capabilities["http"].(bool); ok && supported {
		return mcpServerHTTP
	}
	// ACP v1 requires every agent to support stdio. HTTP is optional and may
	// only be selected after an affirmative initialize capability.
	return mcpServerStdio
}

func (b *Bridge) sessionMCPServers(session SessionOptions) ([]any, error) {
	if b == nil {
		return nil, errors.New("ACP bridge is unavailable")
	}
	return sessionMCPServers(b.opts, session, b.negotiatedMCPTransport())
}

func browserMCPServers(options Options, session SessionOptions, transport mcpServerTransport) ([]any, error) {
	if session.Ephemeral || session.Spare {
		return []any{}, nil
	}
	// A delegated child does not drive the user's browser: it was spawned to do
	// a job and report back. Attaching the server anyway costs every one of its
	// requests the whole browser tool list for a child that cannot use it.
	if strings.HasPrefix(strings.TrimSpace(session.ChatID), subagentChatIDPrefix) {
		return []any{}, nil
	}
	return describeWorkassMCPServers(options, session, transport, []workassMCPServer{{
		name: "workass-browser", path: "/workass/mcp/browser",
	}})
}

func agentMCPServers(options Options, session SessionOptions, transport mcpServerTransport) ([]any, error) {
	if session.Ephemeral {
		return []any{}, nil
	}
	return describeWorkassMCPServers(options, session, transport, []workassMCPServer{{
		name: "workass-agent", path: "/workass/mcp/agent",
	}})
}

func sessionMCPServers(options Options, session SessionOptions, transport mcpServerTransport) ([]any, error) {
	browser, err := browserMCPServers(options, session, transport)
	if err != nil {
		return nil, err
	}
	agent, err := agentMCPServers(options, session, transport)
	if err != nil {
		return nil, err
	}
	return append(browser, agent...), nil
}

func describeWorkassMCPServers(options Options, session SessionOptions, transport mcpServerTransport, servers []workassMCPServer) ([]any, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(options.WorkassMCPBaseURL), "/")
	ownerKey := strings.TrimSpace(session.AgentOwnerKey)
	if !strings.HasPrefix(baseURL, "https://") || ownerKey == "" {
		return []any{}, nil
	}
	result := make([]any, 0, len(servers))
	for _, server := range servers {
		endpoint := baseURL + server.path
		switch transport {
		case mcpServerHTTP:
			result = append(result, map[string]any{
				"name": server.name,
				"type": "http",
				"url":  endpoint,
				"headers": []any{
					map[string]any{"name": "Authorization", "value": "Bearer " + ownerKey},
					map[string]any{"name": "X-Workass-Chat-ID", "value": strings.TrimSpace(session.ChatID)},
					map[string]any{"name": "X-Workass-Tab-ID", "value": strings.TrimSpace(session.TabID)},
				},
			})
		case mcpServerStdio:
			command := strings.TrimSpace(options.WorkassMCPStdioCommand)
			caFile := strings.TrimSpace(options.WorkassMCPCACertFile)
			if command == "" || !filepath.IsAbs(command) {
				return nil, errors.New("ACP stdio MCP transport requires an absolute Workass daemon command")
			}
			if caFile == "" || !filepath.IsAbs(caFile) {
				return nil, errors.New("ACP stdio MCP transport requires an absolute Workass MCP CA certificate path")
			}
			result = append(result, map[string]any{
				"name":    server.name,
				"command": command,
				"args":    []string{"mcp-stdio"},
				"env": []any{
					map[string]any{"name": "WORKASS_MCP_ENDPOINT", "value": endpoint},
					map[string]any{"name": "WORKASS_MCP_CA_FILE", "value": caFile},
					map[string]any{"name": "WORKASS_MCP_OWNER_CREDENTIAL", "value": "Bearer " + ownerKey},
					map[string]any{"name": "WORKASS_MCP_CHAT_ID", "value": strings.TrimSpace(session.ChatID)},
					map[string]any{"name": "WORKASS_MCP_TAB_ID", "value": strings.TrimSpace(session.TabID)},
				},
			})
		default:
			return nil, errors.New("ACP MCP transport is invalid")
		}
	}
	return result, nil
}
