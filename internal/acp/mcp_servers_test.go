package acp

import "testing"

func TestNoWorkassMCPDescriptorsForAnySession(t *testing.T) {
	for _, http := range []bool{false, true} {
		b := &Bridge{opts: Options{WorkassToolsOrigin: "https://tools.localhost:8788"}, agentCaps: map[string]any{"mcpCapabilities": map[string]any{"http": http}}}
		for _, session := range []SessionOptions{{ChatID: "chat", TabID: "tab", AgentOwnerKey: "owner"}, {Spare: true}, {Ephemeral: true}, {ChatID: subagentChatIDPrefix + "child"}} {
			servers, err := b.sessionMCPServers(session)
			if err != nil || len(servers) != 0 {
				t.Fatal("Workass tools must never be injected as MCP servers")
			}
		}
	}
}
