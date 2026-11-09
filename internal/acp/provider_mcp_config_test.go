package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeMCPFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func noEnv(string) string { return "" }

// Every file is a fixture under a temporary home: the real one is never read.
func TestConfiguredMCPServersReadsEachAgentsOwnFilesWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "proyecto")
	other := filepath.Join(t.TempDir(), "otro")
	writeMCPFixture(t, filepath.Join(home, ".claude.json"), `{
		"mcpServers": {"figma": {"type": "stdio", "command": "npx", "args": ["figma-mcp", "--token=sk-fixture-secret"], "env": {"FIGMA_TOKEN": "fixture-env-secret"}}},
		"projects": {
			"`+project+`": {
				"mcpServers": {"local-db": {"command": "db-mcp", "env": {"DB_PASSWORD": "fixture-db-secret"}}},
				"enabledMcpjsonServers": ["approved"],
				"disabledMcpjsonServers": ["rejected"]
			},
			"`+other+`": {"mcpServers": {"not-a-workass-project": {"command": "x"}}}
		}
	}`)
	writeMCPFixture(t, filepath.Join(project, ".mcp.json"), `{"mcpServers": {
		"approved": {"type": "http", "url": "https://example.invalid/mcp?key=fixture-url-secret", "headers": {"Authorization": "Bearer fixture-header-secret"}},
		"rejected": {"command": "r"},
		"pending": {"command": "p"},
		"by-settings": {"command": "s"}
	}}`)
	writeMCPFixture(t, filepath.Join(project, ".claude", "settings.local.json"), `{"enabledMcpjsonServers": ["by-settings"]}`)
	writeMCPFixture(t, filepath.Join(home, ".codex", "config.toml"), `
model = "x"
[mcp_servers.repl]
command = "node"
args = ["--secret=fixture-arg-secret"]
[mcp_servers.repl.env]
API_KEY = "fixture-toml-secret"
enabled = false
[mcp_servers."computer use"]
url = "https://example.invalid"
enabled = false # switched off
`)
	records := []ProviderConfig{
		{ID: "mock", Name: "Mock"},
		{ID: "claude", Name: "Claude Code"},
		{ID: "codex", Name: "Codex"},
	}
	agents := configuredMCPServers(records, false, home, noEnv, []string{project, project + "/", "relative/ignored"})
	if len(agents) != 2 || agents[0].ProviderID != "claude" || agents[1].ProviderID != "codex" {
		t.Fatalf("agents = %#v", agents)
	}
	type row struct{ name, scope, project, file, status string }
	rows := func(agent AgentMCPServers) []row {
		out := []row{}
		for _, server := range agent.Servers {
			out = append(out, row{server.Name, server.Scope, server.Project, server.File, server.Status})
		}
		return out
	}
	mcpFile := filepath.Join(project, ".mcp.json")
	globalFile := "~" + string(filepath.Separator) + ".claude.json"
	if got, want := rows(agents[0]), []row{
		{"figma", MCPScopeUser, "", globalFile, ""},
		{"local-db", MCPScopeLocal, project, globalFile, ""},
		{"approved", MCPScopeProject, project, mcpFile, ""},
		{"by-settings", MCPScopeProject, project, mcpFile, ""},
		{"pending", MCPScopeProject, project, mcpFile, MCPStatusPending},
		{"rejected", MCPScopeProject, project, mcpFile, MCPStatusOff},
	}; !reflect.DeepEqual(got, want) || agents[0].Error != "" {
		t.Fatalf("claude rows = %#v error %q\nwant %#v", got, agents[0].Error, want)
	}
	codexFile := filepath.Join("~", ".codex", "config.toml")
	if got, want := rows(agents[1]), []row{
		{"computer use", MCPScopeUser, "", codexFile, MCPStatusOff},
		{"repl", MCPScopeUser, "", codexFile, ""},
	}; !reflect.DeepEqual(got, want) || agents[1].Error != "" {
		t.Fatalf("codex rows = %#v error %q\nwant %#v", got, agents[1].Error, want)
	}
	encoded, _ := json.Marshal(agents)
	for _, secret := range []string{"secret", "npx", "db-mcp", "example.invalid", "Bearer", "not-a-workass-project"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the reply carries %q: %s", secret, encoded)
		}
	}
}

func TestConfiguredMCPServersFollowsConfigDirectoriesAndApproveAll(t *testing.T) {
	home := t.TempDir()
	claudeDir := filepath.Join(t.TempDir(), "claude-config")
	codexDir := filepath.Join(t.TempDir(), "codex-home")
	project := filepath.Join(t.TempDir(), "p")
	writeMCPFixture(t, filepath.Join(home, ".claude.json"), `{"mcpServers": {"ignored-home": {}}}`)
	writeMCPFixture(t, filepath.Join(claudeDir, ".claude.json"), `{"mcpServers": {"from-dir": {}}}`)
	writeMCPFixture(t, filepath.Join(claudeDir, "settings.json"), `{"enableAllProjectMcpServers": true}`)
	writeMCPFixture(t, filepath.Join(project, ".mcp.json"), `{"mcpServers": {"shared": {}}}`)
	writeMCPFixture(t, filepath.Join(codexDir, "config.toml"), "[mcp_servers.from-codex-home]\ncommand = \"x\"\n")
	records := []ProviderConfig{
		{ID: "claude", Name: "Claude Code", Env: map[string]string{"CLAUDE_CONFIG_DIR": claudeDir}},
		{ID: "codex", Name: "Codex"},
	}
	getenv := func(key string) string {
		if key == "CODEX_HOME" {
			return codexDir
		}
		return ""
	}
	agents := configuredMCPServers(records, false, home, getenv, []string{project})
	names := func(agent AgentMCPServers) []string {
		var out []string
		for _, server := range agent.Servers {
			out = append(out, server.Name+"/"+server.Status)
		}
		return out
	}
	if got := names(agents[0]); !reflect.DeepEqual(got, []string{"from-dir/", "shared/"}) {
		t.Fatalf("claude = %v", got)
	}
	if got := names(agents[1]); !reflect.DeepEqual(got, []string{"from-codex-home/"}) {
		t.Fatalf("codex = %v", got)
	}
}

func TestConfiguredMCPServersSaysWhenAFileCannotBeRead(t *testing.T) {
	home := t.TempDir()
	writeMCPFixture(t, filepath.Join(home, ".claude.json"), `{not json`)
	agents := configuredMCPServers([]ProviderConfig{{ID: "claude", Name: "Claude Code"}, {ID: "codex", Name: "Codex"}}, false, home, noEnv, nil)
	if len(agents) != 2 || agents[0].Error == "" || len(agents[0].Servers) != 0 {
		t.Fatalf("an unreadable config read as no servers: %#v", agents)
	}
	if agents[1].Error != "" || agents[1].Servers == nil {
		t.Fatalf("a missing config is no servers, not an error: %#v", agents[1])
	}
}

// The TOML reader finds servers in every form Codex accepts and is not fooled
// by text inside values.
func TestCodexMCPServersFromTOMLForms(t *testing.T) {
	names, off := codexMCPServersFromTOML(`
# [mcp_servers.commented]
mcp_servers.dotted.command = "x"
mcp_servers.dotted.enabled = false
notes = """
[mcp_servers.inside_string]
"""
args = [
  "[mcp_servers.inside_array]",
  ["nested"],
]
[[profiles]]
name = "p"
[mcp_servers]
inline = { command = "y", enabled = false, env = { A = "b" } }
also = { command = "z" }
[mcp_servers.'single quoted']
command = "w"
[mcp_servers.tabled]
command = "v"
args = [
  "--flag",
]
enabled = true
[other]
enabled = false
`)
	if want := []string{"dotted", "inline", "also", "single quoted", "tabled"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %#v, want %#v", names, want)
	}
	if want := map[string]bool{"dotted": true, "inline": true}; !reflect.DeepEqual(off, want) {
		t.Fatalf("off = %#v, want %#v", off, want)
	}
}
