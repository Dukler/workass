package acp

import (
	"strings"
	"testing"
)

func TestBrowserMCPServerIsInjectedForEveryConfiguredACPSession(t *testing.T) {
	servers, err := browserMCPServers(Options{
		WorkassMCPBaseURL: "https://localhost:8788",
	}, SessionOptions{ChatID: "chat-any-provider", TabID: "tab-any-provider", AgentOwnerKey: "owner-any-provider"}, mcpServerHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	server := mapFromAny(servers[0])
	if server["name"] != "workass-browser" || server["type"] != "http" ||
		server["url"] != "https://localhost:8788/workass/mcp/browser" || server["command"] != nil {
		t.Fatalf("server = %#v", server)
	}
	headers := mcpDescriptorValues(server["headers"])
	if headers["Authorization"] != "Bearer owner-any-provider" || headers["X-Workass-Chat-ID"] != "chat-any-provider" ||
		headers["X-Workass-Tab-ID"] != "tab-any-provider" {
		t.Fatalf("headers = %#v", headers)
	}
	manager := NewManager(Options{WorkassMCPBaseURL: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	if brief := manager.buildEnvironmentBrief(false); !strings.Contains(brief, "workass-browser MCP server") {
		t.Fatalf("browser availability missing from environment brief:\n%s", brief)
	}
}

func TestConfiguredBrowserPromptIsAdjacentToEveryTopLevelTurn(t *testing.T) {
	manager := NewManager(Options{WorkassMCPBaseURL: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })

	for _, request := range []string{"inspect the visible page", "continue with the same page"} {
		prompt := manager.buildUserRequestBlock(request, true)
		ruleAt := strings.LastIndex(prompt, "Browser tools: this top-level Workass chat")
		requestAt := strings.LastIndex(prompt, "User request:\n"+request)
		if ruleAt < 0 || requestAt < 0 || ruleAt > requestAt {
			t.Fatalf("per-turn browser rule is missing or misplaced: rule=%d request=%d prompt=%q", ruleAt, requestAt, prompt)
		}
		for _, want := range []string{
			"workass-browser MCP server",
			"deferred from the initial tool list",
			"search or discover the available tool catalog",
			"workass_browser_list",
			"workass_browser_snapshot",
			"before concluding they are unavailable",
			"report the exact Workass tool error",
			"do not ask the user to remind you to use Workass",
		} {
			if !strings.Contains(prompt[ruleAt:requestAt], want) {
				t.Fatalf("per-turn browser rule missing %q: %q", want, prompt[ruleAt:requestAt])
			}
		}
	}
}

func TestUnconfiguredBrowserIsNotAdvertisedPerTurn(t *testing.T) {
	manager := NewManager(Options{})
	t.Cleanup(func() { manager.Reset() })
	if prompt := manager.buildUserRequestBlock("inspect the page", true); strings.Contains(prompt, "workass_browser_") {
		t.Fatalf("unconfigured browser was advertised: %q", prompt)
	}
}

func TestBrowserMCPInjectionRejectsPlaintextURL(t *testing.T) {
	got, err := browserMCPServers(Options{WorkassMCPBaseURL: "http://127.0.0.1:8788"}, SessionOptions{AgentOwnerKey: "owner"}, mcpServerHTTP)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("plaintext MCP URL should not be injected: %#v", got)
	}
}

func TestBrowserMCPInjectionSkipsProbeAndSpareSessions(t *testing.T) {
	options := Options{WorkassMCPBaseURL: "https://localhost:8788"}
	if got, err := browserMCPServers(options, SessionOptions{Ephemeral: true}, mcpServerHTTP); err != nil || len(got) != 0 {
		t.Fatalf("ephemeral session received browser MCP: %#v", got)
	}
	if got, err := browserMCPServers(options, SessionOptions{Spare: true}, mcpServerHTTP); err != nil || len(got) != 0 {
		t.Fatalf("spare session received browser MCP: %#v", got)
	}
}

// A child is told about the browser only if it has one. Getting this wrong is
// not merely wasteful: on claude it costs a fruitless ToolSearch, and on codex
// the model can call a tool that is not attached.
func TestSubagentBriefOmitsTheBrowserItDoesNotHave(t *testing.T) {
	manager := NewManager(Options{WorkassMCPBaseURL: "https://localhost:8788"})
	t.Cleanup(func() { manager.Reset() })
	if brief := manager.buildEnvironmentBrief(true); strings.Contains(brief, "workass-browser MCP server") {
		t.Fatalf("subagent brief advertises a browser server it is not given: %q", brief)
	}
	if servers, err := browserMCPServers(Options{WorkassMCPBaseURL: "https://localhost:8788"},
		SessionOptions{ChatID: subagentChatIDPrefix + "abc", AgentOwnerKey: "owner"}, mcpServerHTTP); err != nil || len(servers) != 0 {
		t.Fatalf("subagent session was given browser servers: %#v", servers)
	}
}
