# Central agent instruction text

User-authorized scope: all Workass-authored prompt/guide blocks, history and
attachment framing, delegation wrappers, and tool/schema guidance have one
editable source: internal/agenttext/catalog.json. Go embeds and selects named
entries; existing assembly, authority, session and delivery behavior remains.
Browser wording advertises availability without directing exclusive use.
Native delivery remains Codex/Claude/OpenCode; Devin/custom ACP retain fallback.
Ordinary UI labels, diagnostic errors, protocol identifiers, user content and
provider-owned prompts are not Workass instruction text.

Lane manifest: this document; internal/agenttext/*;
internal/acp/{manager.go,subagents.go,tool_context.go,native_instructions.go,model_scores.go};
cmd/workass/{agent_mcp.go,browser_mcp.go}; affected prompt contract tests if needed.
Dev validation and activation only. No publication or production activation.
