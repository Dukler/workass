package acp

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBrowserMCPServerIsInjectedForEveryConfiguredACPSession(t *testing.T) {
	command := filepath.Join(t.TempDir(), "workass")
	servers := browserMCPServers(Options{
		BrowserMCPCommand:     command,
		BrowserMCPControlFile: "/tmp/workass-browser.json",
	}, SessionOptions{ChatID: "chat-any-provider"})
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	server := mapFromAny(servers[0])
	if server["name"] != "workass-browser" || server["command"] != command {
		t.Fatalf("server = %#v", server)
	}
	args, ok := server["args"].([]string)
	if !ok {
		t.Fatalf("args type = %T (%#v)", server["args"], server["args"])
	}
	want := []string{"browser-mcp", "--control-file", "/tmp/workass-browser.json", "--chat-id", "chat-any-provider"}
	if len(args) != len(want) {
		t.Fatalf("args = %#v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
	manager := NewManager(Options{BrowserMCPCommand: command, BrowserMCPControlFile: "/tmp/workass-browser.json"})
	t.Cleanup(func() { manager.Reset() })
	if brief := manager.buildEnvironmentBrief(false); !strings.Contains(brief, "workass-browser MCP server") {
		t.Fatalf("browser availability missing from environment brief:\n%s", brief)
	}
}

func TestBrowserMCPInjectionRejectsRelativeExecutable(t *testing.T) {
	if got := browserMCPServers(Options{BrowserMCPCommand: "workass"}, SessionOptions{}); len(got) != 0 {
		t.Fatalf("relative command should not be injected: %#v", got)
	}
}

func TestBrowserMCPInjectionSkipsProbeAndSpareSessions(t *testing.T) {
	options := Options{BrowserMCPCommand: filepath.Join(t.TempDir(), "workass")}
	if got := browserMCPServers(options, SessionOptions{Ephemeral: true}); len(got) != 0 {
		t.Fatalf("ephemeral session received browser MCP: %#v", got)
	}
	if got := browserMCPServers(options, SessionOptions{Spare: true}); len(got) != 0 {
		t.Fatalf("spare session received browser MCP: %#v", got)
	}
}

// A child is told about the browser only if it has one. Getting this wrong is
// not merely wasteful: on claude it costs a fruitless ToolSearch, and on codex
// the model can call a tool that is not attached.
func TestSubagentBriefOmitsTheBrowserItDoesNotHave(t *testing.T) {
	manager := NewManager(Options{BrowserMCPCommand: "/usr/local/bin/workass"})
	t.Cleanup(func() { manager.Reset() })
	if brief := manager.buildEnvironmentBrief(true); strings.Contains(brief, "workass-browser MCP server") {
		t.Fatalf("subagent brief advertises a browser server it is not given: %q", brief)
	}
	if servers := browserMCPServers(Options{BrowserMCPCommand: "/usr/local/bin/workass"},
		SessionOptions{ChatID: subagentChatIDPrefix + "abc"}); len(servers) != 0 {
		t.Fatalf("subagent session was given browser servers: %#v", servers)
	}
}
