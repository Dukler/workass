package acp

import "testing"

func TestRawMCPDockerCommandLineMatchesOnlyBlockedDirectImages(t *testing.T) {
	tests := []struct {
		command string
		want    bool
	}{
		{`docker.exe run --rm mcp/playwright`, true},
		{`"C:\Program Files\Docker\docker.exe" run ghcr.io/mgcrea/mcp-web`, true},
		{`docker run docker.io/acuvity/mcp-github`, true},
		{`docker ps`, false},
		{`docker run ubuntu:latest`, false},
		{`node workass agent-mcp`, false},
	}
	for _, test := range tests {
		if got := isRawMCPDockerCommandLine(test.command); got != test.want {
			t.Errorf("isRawMCPDockerCommandLine(%q)=%v want %v", test.command, got, test.want)
		}
	}
}
