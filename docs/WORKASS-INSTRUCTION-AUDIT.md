# Workass instruction delivery audit

Date: 2026-09-16. Status: audit followed by the authorized implementation in
`NATIVE-WORKASS-INSTRUCTIONS.md`. Codex, Claude and OpenCode now use native
instruction delivery in source; Devin retains legacy delivery (see evidence below).

## Required behavior

Keep each provider's native harness, session lineage, permissions, native
subagents, and compaction. Workass supplies optional application tools. Ordinary
user messages must not accumulate copies of Workass instructions. Tool discovery
must remain available after native compaction and exact resume.

## Current implementation

`internal/acp/manager.go` adds the tool-context brief and runtime identity on
every ordinary turn. Its user-request wrapper adds language/browser/updater
instructions. The environment and initial history seed are separate first-input
behavior. `internal/acp/subagents.go` assembles another instruction wrapper.
`internal/acp/tool_context.go` generates session-owned CLI context files.

History seeding and missing-message imports have their own binding rules; removing
boilerplate must not remove those legitimate cross-provider history operations.
Existing native histories must not be rewritten to erase old boilerplate.

## Compatibility findings

| Registered provider | Evidence and candidate integration | Remaining verification |
| --- | --- | --- |
| Codex | Installed app-server schema exposes `developerInstructions` on thread start/resume, separately from `baseInstructions`. Workass already uses those native lifecycle calls. | Preserve pre-existing developer instructions; prove compaction and exact-resume behavior against the installed version. Field existence alone is not that proof. |
| Claude Code | Official SDK supports `systemPrompt: {type: 'preset', preset: 'claude_code', append: text}`. Workass already uses that preset. | Verify the pinned SDK's compaction and query-restart behavior with appended instructions. |
| OpenCode | Installed version reports 1.18.31. Its matching upstream prompt implementation reconstructs `instruction.system()` separately from conversation messages. `instructions` files combine with AGENTS.md. Workass already supplies process-scoped `OPENCODE_CONFIG_CONTENT`. | Test additive configuration merge, exact resume, compaction, and session-owned CLI discovery without changing the user's files. |
| Qwen Code | Installed version reports 0.24.0. `--append-system-prompt` flows from argv into config; the installed ACP implementation spreads the same argv when constructing a session. `getMainSessionSystemInstruction()` includes the append text; chat creation reads that method, and successful compaction restarts chat from compressed history. | Protocol canary for automatic/manual compaction and resume. Safe-mode and launch overrides must not silently disable discovery. |
| Oh My Pi | Installed 18.1.8 help advertises `--append-system-prompt`. Current upstream ACP session factory spreads its base session options; startup resolves append text into those options. | Installed-version ACP flag propagation and compaction are not yet verified. Upstream main is not proof of installed behavior. |
| Devin ACP | Official CLI documentation supports always-on project/global rules and plugin rules. | No verified session-scoped additive injection path found. Devin is not installed on this Mac. Do not modify user/global rules or assume compaction behavior. |
| LM Studio / Ollama / oMLX | All three registrations use Workass's `internal/agentserver/server.go`. The current prompt path sends stored user/assistant messages to the local model client. | This server has no observed shell-tool loop in that path; announcing a CLI cannot itself make tools executable. Persistent instructions and actual tool execution are separate capabilities. |
| Mock | Deterministic Workass-owned ACP fixture. | Extend it to test the selected contract; it cannot establish real vendor compaction semantics. |
| Custom ACP | Registration accepts an arbitrary ACP implementation. | ACP does not establish a persistent-instruction injection contract. Support requires a declared capability or another supported extension mechanism. |

No inference turns were run for this audit. No vendor credentials, session
transcripts, or Workass tool-context files were read.

## Architectural boundary

ACP's standard lifecycle requests attach external tools using `mcpServers`.
They do not define a general persistent system/developer-instruction field.
Native harnesses may have additional configuration mechanisms, but those are
provider integrations, not features implied by ACP compatibility.

The repository's 2026-09-09 law in `docs/PORT-SPEC.md` and
`docs/WORKASS-TOOLS.md` requires Workass-owned tools to use CLI/API and explicitly
removes Workass MCP endpoints. Therefore a universal MCP discovery interface
cannot be implemented as an unannounced fallback under the current spec.

Two reviewable choices remain:

1. Keep CLI-only: one shared bootstrap text and tool service, with native additive
   instruction integration per provider. Report unsupported integrations
   explicitly; do not silently pretend first-message text survives compaction.
2. Authorize a small MCP discovery/call interface over the existing tool service.
   Attach it through native harness tool configuration and ACP `mcpServers`.
   Keep schemas small and fetch detailed guidance on demand. Preserve existing
   ownership, mutation receipts, authorization, and native permissions. This
   still needs provider conformance tests and does not guarantee every arbitrary
   server correctly implements the standard.

Neither choice means instructions or tool schemas cost zero context. The goal
is to remove repeated history entries and keep context management native.

## Acceptance gates before runtime changes ship

- User input remains unmodified except required attachment/history framing.
- Native base instructions and user/project settings remain intact.
- Tools remain discoverable after native compaction, resume, and model switches.
- Session ownership survives spare adoption and host replacement without granting
  another chat's authority. No shared mutable project bootstrap files.
- Detailed tool instructions and runtime identity are available on demand.
- Native subagents remain the default; Workass orchestration is optional.
- Mock tests establish Workass transport behavior; vendor protocol canaries
  establish extension support, without treating model answer quality as a test.

## Evidence

- [ACP session setup](https://agentclientprotocol.com/protocol/v1/session-setup)
- [Claude SDK additive prompts](https://code.claude.com/docs/en/agent-sdk/modifying-system-prompts)
- [OpenCode rules](https://opencode.ai/docs/rules/)
- [OpenCode 1.18.31 prompt assembly](https://github.com/anomalyco/opencode/blob/v1.18.31/packages/opencode/src/session/prompt.ts)
- [OMP startup and ACP session factory](https://github.com/can1357/oh-my-pi/blob/main/packages/coding-agent/src/main.ts)
- [Devin CLI rules](https://docs.devin.ai/cli/extensibility/rules)
- [Devin CLI flags](https://docs.devin.ai/cli/reference/commands)

Local inspection: `internal/acp/provider_registration.go`, native host files,
`internal/agentserver/server.go`, installed Qwen 0.24.0 code under
`lib/chunks/acpAgent-EHXZXVUB.js`, `chunk-HWKWZSZY.js`, and
`chunk-SNPYTKXI.js`; installed Codex schema generated into a temporary directory.
Exact inspection commands and their outputs remain in the tool history.

## Implementation outcome (2026-09-16)

The selected implementation is CLI-only; earlier transport alternatives in this
investigation are not the implementation. No new MCP server is registered.
Codex uses app-server developerInstructions on start/resume, preserving the
configured developer instructions. Claude appends to its native preset on every
query creation. OpenCode receives an additive instructions file through its
process configuration. Ordinary messages on those bridges retain only current
input and required historical imports. The stable command/context environment
and local `tools guide` replace repeating discovery text and model identity.

Devin 3000.10.27's official binary was downloaded to /tmp only, verified against
the official manifest SHA-256, and inspected with --help and acp --help (no install,
setup, authentication or model request). --config explicitly overrides the
user config; ACP exposes no additive instruction/hook flag. The documented
SessionStart/PostCompaction hooks do not establish a safe process-local config
attachment. No Devin user/global/project settings were altered, and no unsupported
integration is advertised. Sources:
https://docs.devin.ai/cli/extensibility/hooks/lifecycle-hooks
https://docs.devin.ai/cli/reference/configuration/global-vs-local
https://docs.devin.ai/cli/reference/commands

## Final fallback decision

The user chose the existing per-prompt instruction delivery for Devin and other
providers without verified native delivery. Keep Codex, Claude and OpenCode on
the implemented native paths. No global Devin hooks or configuration replacement.
This routing was already implemented; the follow-up records the decision without
changing runtime code or activating a build.
