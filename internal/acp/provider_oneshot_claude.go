package acp

// claudeOneShotStrategy asks Claude Code once with `-p`: no tools, no setting
// sources (so no hooks or CLAUDE.md), no MCP servers, no slash commands and no
// saved session. Model aliases keep the newest model of each family without a
// list to update.
type claudeOneShotStrategy struct{}

func (claudeOneShotStrategy) Models() []OneShotModel {
	return []OneShotModel{{ID: "haiku", Name: "Haiku"}, {ID: "sonnet", Name: "Sonnet"}, {ID: "opus", Name: "Opus"}}
}

func (claudeOneShotStrategy) Args(command, model, system string) []string {
	return []string{command, "-p", "--model", model, "--no-session-persistence", "--tools", "",
		"--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands",
		"--output-format", "text", "--system-prompt", system}
}
