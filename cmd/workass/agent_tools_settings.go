package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"workass/internal/acp"
	"workass/internal/wire"
)

// agentMCPProjectLimit bounds how many project folders one MCP read looks in.
const agentMCPProjectLimit = 64

// registerAgentToolsHandlers serves Ajustes > Herramientas:
//   - agent-tools:get   the Workass tool groups, on or off, and their cost;
//   - agent-tools:set   {group, on} switches one group, for new turns;
//   - agent-mcp:list    the MCP servers each agent's own CLI is configured
//     with, read from its files and never started.
//
// It also points the manager at the setting, so guides follow it.
func registerAgentToolsHandlers(hub *wire.Hub, state *daemonState, manager *acp.Manager, sessions *sessionStore, chats *providerChatRuntime) int {
	settings := state.setting
	if manager != nil {
		manager.SetToolGroupsSource(func() acp.ToolGroups { return toolGroupsOffFromSettings(settings.get()) })
	}
	hub.Register("agent-tools:get", func(args []any) (any, error) {
		return agentToolsView(manager, toolGroupsOffFromSettings(settings.get())), nil
	})
	hub.Register("agent-tools:set", func(args []any) (any, error) {
		arg := firstMapArg(args)
		on, given := boolField(arg, "on")
		if !given {
			return nil, errors.New("agent-tools:set requires {group, on}")
		}
		saved, err := setAgentToolGroup(settings, fieldString(arg, "group"), on)
		if err != nil {
			return nil, err
		}
		hub.Broadcast("settings:changed", saved)
		return agentToolsView(manager, toolGroupsOffFromSettings(saved)), nil
	})
	hub.Register("agent-mcp:list", func(args []any) (any, error) {
		if manager == nil {
			return map[string]any{"agents": []any{}}, nil
		}
		return map[string]any{"agents": manager.ConfiguredMCPServers(agentMCPProjects(sessions, chats))}, nil
	})
	return 3
}

// agentMCPProjects is every project folder this daemon's sidebar shows: the
// workspaces it keeps and the folders its chats work in, less the ones removed.
func agentMCPProjects(sessions *sessionStore, chats *providerChatRuntime) []string {
	root := sessions.GlobalSnapshot()
	removed := map[string]bool{}
	for _, raw := range anySlice(root["removedWorkspaces"]) {
		if path, ok := raw.(string); ok {
			removed[filepath.Clean(strings.TrimSpace(path))] = true
		}
	}
	seen := map[string]bool{}
	var projects []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || !filepath.IsAbs(path) || len(projects) >= agentMCPProjectLimit {
			return
		}
		path = filepath.Clean(path)
		if seen[path] || removed[path] {
			return
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return
		}
		seen[path] = true
		projects = append(projects, path)
	}
	for _, raw := range anySlice(root["workspaces"]) {
		switch value := raw.(type) {
		case string:
			add(value)
		case map[string]any:
			add(fieldString(value, "path"))
		}
	}
	if chats != nil {
		if listed, err := chats.ListChats(); err == nil {
			for _, chat := range listed {
				add(fieldString(chat, "cwd"))
			}
		}
	}
	return projects
}
