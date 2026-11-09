package acp

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MCP servers an agent's own CLI is configured with, for Ajustes >
// Herramientas. Each provider adapter that knows where its CLI keeps them reads
// its own files (providerMCPConfigStrategy); this file only gathers the
// answers. Nothing is started, so there is no tool count and no connection
// state, and no command, URL, header or environment value leaves the adapter:
// that is where secrets live.

const (
	MCPScopeUser    = "user"    // every project of this person
	MCPScopeProject = "project" // a file in the project, shared with whoever has it
	MCPScopeLocal   = "local"   // this person only, in one project

	MCPStatusOff     = "off"     // the config switches it off or rejected it
	MCPStatusPending = "pending" // a project server nobody has approved yet
)

// MCPServerEntry is one configured server, as its config files describe it.
type MCPServerEntry struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"`
	Project string `json:"project,omitempty"`
	File    string `json:"file"`
	Status  string `json:"status,omitempty"`
}

// AgentMCPServers is one agent's configured servers. Error says a config file
// exists and could not be read, so an empty list is never mistaken for none.
type AgentMCPServers struct {
	ProviderID string           `json:"providerId"`
	Name       string           `json:"name"`
	Servers    []MCPServerEntry `json:"servers"`
	Error      string           `json:"error,omitempty"`
}

// mcpConfigScope is where an adapter looks: the person's home, the provider's
// environment (its own config-directory variables), and the projects Workass
// knows.
type mcpConfigScope struct {
	Home     string
	Env      func(string) string
	Projects []string
}

type providerMCPConfigStrategy interface {
	ConfiguredServers(scope mcpConfigScope) ([]MCPServerEntry, error)
}

// ConfiguredMCPServers reads, for every provider whose adapter knows its CLI's
// MCP configuration, the servers configured for this person and for projects.
func (m *Manager) ConfiguredMCPServers(projects []string) []AgentMCPServers {
	home, _ := os.UserHomeDir()
	m.mu.Lock()
	records := m.providerRecordsLocked()
	production := m.isProductionRuntime()
	m.mu.Unlock()
	return configuredMCPServers(records, production, home, os.Getenv, projects)
}

func configuredMCPServers(records []ProviderConfig, production bool, home string, getenv func(string) string, projects []string) []AgentMCPServers {
	projects = cleanMCPProjects(projects)
	out := []AgentMCPServers{}
	for _, record := range records {
		if production && providerIsFixture(record.ID) {
			continue
		}
		strategy := providerAdapterForID(record.ID).mcpConfig
		if strategy == nil {
			continue
		}
		env := record.Env
		lookup := func(key string) string {
			if value := strings.TrimSpace(env[key]); value != "" {
				return value
			}
			return strings.TrimSpace(getenv(key))
		}
		servers, err := strategy.ConfiguredServers(mcpConfigScope{Home: home, Env: lookup, Projects: projects})
		if servers == nil {
			servers = []MCPServerEntry{}
		}
		for index := range servers {
			servers[index].File = homeRelative(servers[index].File, home)
		}
		sortMCPServers(servers)
		agent := AgentMCPServers{ProviderID: record.ID, Name: firstNonEmpty(record.Name, record.ID), Servers: servers}
		if err != nil {
			agent.Error = redactSensitiveText(err.Error())
		}
		out = append(out, agent)
	}
	return out
}

// homeRelative writes a path under the home folder as ~/..., the way people
// name their config files.
func homeRelative(path, home string) string {
	if home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return path
	}
	return "~" + string(filepath.Separator) + rel
}

func cleanMCPProjects(projects []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(projects))
	for _, project := range projects {
		project = strings.TrimSpace(project)
		if project == "" || !filepath.IsAbs(project) {
			continue
		}
		project = filepath.Clean(project)
		if seen[project] {
			continue
		}
		seen[project] = true
		out = append(out, project)
	}
	return out
}

func sortMCPServers(servers []MCPServerEntry) {
	rank := map[string]int{MCPScopeUser: 0, MCPScopeLocal: 1, MCPScopeProject: 2}
	sort.SliceStable(servers, func(i, j int) bool {
		a, b := servers[i], servers[j]
		if rank[a.Scope] != rank[b.Scope] {
			return rank[a.Scope] < rank[b.Scope]
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
}

// mcpConfigMaxBytes bounds one config file. A CLI's own state file also holds
// per-project history, so it is allowed to be large.
const mcpConfigMaxBytes = 32 * 1024 * 1024

// readMCPConfigFile reads a config file that may not exist. A missing file is
// no servers, never an error.
func readMCPConfigFile(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, errors.New("cannot read " + filepath.Base(path))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, true, errors.New("cannot read " + filepath.Base(path))
	}
	data, err := io.ReadAll(io.LimitReader(file, mcpConfigMaxBytes+1))
	if err != nil {
		return nil, true, errors.New("cannot read " + filepath.Base(path))
	}
	if len(data) > mcpConfigMaxBytes {
		return nil, true, errors.New(filepath.Base(path) + " is too large to read")
	}
	return data, true, nil
}

// sortedNames is a map's keys in order, for deterministic rows.
func sortedNames[V any](values map[string]V) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
