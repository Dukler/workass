package acp

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

// claudeMCPConfigStrategy reads the MCP servers Claude Code loads in a
// Workass chat, which runs it with the user, project and local setting
// sources: user servers in the global config, a project's .mcp.json, and the
// per-project ("local") servers the global config keeps under that project.
// A .mcp.json server also carries its approval: rejected, approved, or not yet
// decided. Only names and that approval are read back.
type claudeMCPConfigStrategy struct{}

type claudeMCPApproval struct {
	EnableAll *bool    `json:"enableAllProjectMcpServers"`
	Enabled   []string `json:"enabledMcpjsonServers"`
	Disabled  []string `json:"disabledMcpjsonServers"`
}

type claudeMCPGlobalConfig struct {
	MCPServers map[string]json.RawMessage `json:"mcpServers"`
	Projects   map[string]struct {
		claudeMCPApproval
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	} `json:"projects"`
}

// claudeMCPConfigDir is CLAUDE_CONFIG_DIR, or ~/.claude.
func claudeMCPConfigDir(scope mcpConfigScope) string {
	if dir := scope.Env("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	if scope.Home == "" {
		return ""
	}
	return filepath.Join(scope.Home, ".claude")
}

// claudeMCPGlobalFile is the global config: inside CLAUDE_CONFIG_DIR when that
// is set, else ~/.claude.json.
func claudeMCPGlobalFile(scope mcpConfigScope) string {
	if dir := scope.Env("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, ".claude.json")
	}
	if scope.Home == "" {
		return ""
	}
	return filepath.Join(scope.Home, ".claude.json")
}

func (claudeMCPConfigStrategy) ConfiguredServers(scope mcpConfigScope) ([]MCPServerEntry, error) {
	var servers []MCPServerEntry
	var failures []error
	read := func(path string, into any) bool {
		if path == "" {
			return false
		}
		data, exists, err := readMCPConfigFile(path)
		if err != nil {
			failures = append(failures, err)
			return false
		}
		if !exists {
			return false
		}
		if json.Unmarshal(data, into) != nil {
			failures = append(failures, errors.New(filepath.Base(path)+" is not valid JSON"))
			return false
		}
		return true
	}

	globalFile := claudeMCPGlobalFile(scope)
	var global claudeMCPGlobalConfig
	read(globalFile, &global)
	for _, name := range sortedNames(global.MCPServers) {
		servers = append(servers, MCPServerEntry{Name: name, Scope: MCPScopeUser, File: globalFile})
	}
	var userSettings claudeMCPApproval
	if dir := claudeMCPConfigDir(scope); dir != "" {
		read(filepath.Join(dir, "settings.json"), &userSettings)
	}

	for _, project := range scope.Projects {
		entry, known := global.Projects[project]
		if !known {
			for key, value := range global.Projects {
				if filepath.Clean(key) == project {
					entry, known = value, true
					break
				}
			}
		}
		if known {
			for _, name := range sortedNames(entry.MCPServers) {
				servers = append(servers, MCPServerEntry{Name: name, Scope: MCPScopeLocal, Project: project, File: globalFile})
			}
		}
		mcpFile := filepath.Join(project, ".mcp.json")
		var shared struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if !read(mcpFile, &shared) || len(shared.MCPServers) == 0 {
			continue
		}
		var projectSettings, localSettings claudeMCPApproval
		read(filepath.Join(project, ".claude", "settings.json"), &projectSettings)
		read(filepath.Join(project, ".claude", "settings.local.json"), &localSettings)
		approvals := []claudeMCPApproval{entry.claudeMCPApproval, localSettings, projectSettings, userSettings}
		for _, name := range sortedNames(shared.MCPServers) {
			servers = append(servers, MCPServerEntry{
				Name: name, Scope: MCPScopeProject, Project: project, File: mcpFile,
				Status: claudeMCPProjectStatus(name, approvals),
			})
		}
	}
	return servers, errors.Join(failures...)
}

// claudeMCPProjectStatus is a .mcp.json server's approval: a rejection
// anywhere wins, then approval of all project servers or of this one, else it
// has not been decided.
func claudeMCPProjectStatus(name string, approvals []claudeMCPApproval) string {
	contains := func(list []string) bool {
		for _, item := range list {
			if strings.TrimSpace(item) == name {
				return true
			}
		}
		return false
	}
	for _, approval := range approvals {
		if contains(approval.Disabled) {
			return MCPStatusOff
		}
	}
	for _, approval := range approvals {
		if (approval.EnableAll != nil && *approval.EnableAll) || contains(approval.Enabled) {
			return ""
		}
	}
	return MCPStatusPending
}
