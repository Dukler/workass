package acp

import "regexp"

var (
	rawMCPDockerRunRE   = regexp.MustCompile(`(?i)(?:^|[\\\s"])(?:docker|docker\.exe)["\s]+run\b`)
	rawMCPDockerImageRE = regexp.MustCompile(`(?i)(?:^|\s)(?:mcp/|ghcr\.io/mgcrea/mcp-|docker\.io/acuvity/mcp-)`)
)

func isRawMCPDockerCommandLine(command string) bool {
	return rawMCPDockerRunRE.MatchString(command) && rawMCPDockerImageRE.MatchString(command)
}
