// Package mcpprotocol defines the MCP protocol revisions used by Workass's
// provider boundary. The initialized revision is the current external MCP
// handshake used by Codex; the direct revision is Workass's authenticated
// stateless endpoint dialect.
package mcpprotocol

const (
	// CurrentInitializedVersion is the initialized MCP revision emitted by the
	// current external client shipped with the Workass development runtime.
	CurrentInitializedVersion = "2025-06-18"
	InitializedVersion        = "2025-11-25"
	ModernVersion             = "2026-07-28"
)

func IsInitializedVersion(version string) bool {
	return version == CurrentInitializedVersion || version == InitializedVersion
}

func InitializedVersions() []string {
	return []string{CurrentInitializedVersion, InitializedVersion}
}
