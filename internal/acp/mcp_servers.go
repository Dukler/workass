package acp

// sessionMCPServers deliberately supplies no Workass-owned MCP servers. Vendor
// and user MCP integrations remain the provider's responsibility. Workass actions
// are available only through the packaged tools CLI.
func (b *Bridge) sessionMCPServers(session SessionOptions) ([]any, error) {
	return []any{}, nil
}
