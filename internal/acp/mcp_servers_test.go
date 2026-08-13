package acp

import "testing"

func mcpDescriptorValues(raw any) map[string]string {
	values := make(map[string]string)
	for _, item := range anySlice(raw) {
		entry := mapFromAny(item)
		name, _ := entry["name"].(string)
		value, _ := entry["value"].(string)
		values[name] = value
	}
	return values
}

func TestMCPDescriptorTransportFollowsInitializeCapability(t *testing.T) {
	options := Options{
		WorkassMCPBaseURL:      "https://mcp.localhost:8788",
		WorkassMCPCACertFile:   "/workass/daemon-cert.pem",
		WorkassMCPStdioCommand: "/workass/workass-daemon",
	}
	session := SessionOptions{ChatID: "chat-1", TabID: "tab-1", AgentOwnerKey: "owner-1"}

	stdioBridge := &Bridge{opts: options, agentCaps: map[string]any{
		"mcpCapabilities": map[string]any{"http": false},
	}}
	stdio, err := stdioBridge.sessionMCPServers(session)
	if err != nil {
		t.Fatal(err)
	}
	if len(stdio) != 2 {
		t.Fatalf("stdio descriptors = %#v", stdio)
	}
	server := mapFromAny(stdio[0])
	if server["command"] != "/workass/workass-daemon" || server["type"] != nil || server["url"] != nil {
		t.Fatalf("stdio descriptor = %#v", server)
	}
	if args, _ := server["args"].([]string); len(args) != 1 || args[0] != "mcp-stdio" {
		t.Fatalf("stdio args = %#v", server["args"])
	}
	env := mcpDescriptorValues(server["env"])
	if env["WORKASS_MCP_ENDPOINT"] != "https://mcp.localhost:8788/workass/mcp/browser" ||
		env["WORKASS_MCP_OWNER_CREDENTIAL"] != "Bearer owner-1" {
		t.Fatalf("stdio env = %#v", env)
	}

	httpBridge := &Bridge{opts: options, agentCaps: map[string]any{
		"mcpCapabilities": map[string]any{"http": true},
	}}
	httpServers, err := httpBridge.sessionMCPServers(session)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := mapFromAny(httpServers[0])
	if httpServer["type"] != "http" || httpServer["command"] != nil {
		t.Fatalf("HTTP descriptor = %#v", httpServer)
	}
	if headers := mcpDescriptorValues(httpServer["headers"]); headers["Authorization"] != "Bearer owner-1" {
		t.Fatalf("HTTP headers = %#v", headers)
	}
}

func TestStdioMCPDescriptorRequiresPackagedDaemonAndCA(t *testing.T) {
	_, err := sessionMCPServers(Options{WorkassMCPBaseURL: "https://mcp.localhost:8788"},
		SessionOptions{ChatID: "chat-1", TabID: "tab-1", AgentOwnerKey: "owner-1"}, mcpServerStdio)
	if err == nil {
		t.Fatal("stdio descriptor accepted without its packaged daemon and CA")
	}
}
