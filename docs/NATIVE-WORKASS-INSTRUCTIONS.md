# Native Workass instruction delivery

Authorized 2026-09-16: use native instruction mechanisms for Codex, Claude,
OpenCode. Subsequent user decision: keep the existing per-prompt fallback for
Devin and other providers without verified native instruction delivery.
CLI tools only. No MCP registration, native history rewrite, replacement harness,
permission changes, global/project configuration mutation, or production activation.

Implementation lane: this document; docs/WORKASS-INSTRUCTION-AUDIT.md;
internal/acp/{native_instructions.go,native_instructions_test.go,provider_adapter.go,
provider_registration.go,bridge.go,lifecycle.go,manager.go,session_attachment.go,subagents.go};
cmd/workass/{tools_cli.go,tools_cli_test.go,agent_mcp.go};
scripts/{codex-native-host.mjs,claude-native-host.mjs};
scripts/tests/{codex-native-host.test.mjs,claude-native-host.test.mjs};
desktop/acp/{mock-codex-app-server.mjs,mock-claude-agent-sdk.mjs}.

Codex app-server thread start/resume: append Workass to effective configured
 developerInstructions, leave baseInstructions untouched. Claude SDK: append to
native claude_code system preset, including resumed/restarted queries. OpenCode
ACP: merge an absolute instructions file into process OPENCODE_CONFIG_CONTENT.
Use a private per-process stable context file, refreshed from admitted ownership,
with command/context paths inherited through environment. Detailed guidance is a
local CLI guide. Runtime identity is available through workass_agent_catalog.
Preserve initial historical seed and exact-resume deltas; remove recurring Workass
wrappers only for bridges with native delivery configured. Generic ACP unchanged.

Devin docs expose SessionStart and PostCompaction hooks, but their output-format
table does not explicitly include PostCompaction additionalContext. --config is
not documented as an additive overlay. Do not replace user configuration or
claim verified native integration until these contracts are established.

Acceptance: deterministic native-host fixtures verify instruction append and
resume; Go tests verify isolation, stable binding refresh, prompt history and
config merge; CLI tests verify environment discovery. Provider-native compaction
semantics come from native instruction facilities; mock tests do not prove vendor
model behavior. Run the repository gate and isolated dev handoff; production PIDs
must remain unchanged.

Devin CLI 3000.10.27 was downloaded to an isolated temporary directory from the
official distribution manifest and checked against its published SHA-256;
no installer/setup ran. Its own --help explicitly says --config overrides the
default user config. acp --help has no append-instruction/rules/hooks argument.
Therefore the scoped additive mechanism is not available through verified flags;
Devin retains the existing per-prompt delivery as the explicitly chosen policy.
Do not install global Devin hooks or replace its configuration. ACP transport
alone does not select the fallback: OpenCode keeps its verified native path.

## Verification receipt

- `go test ./internal/acp ./cmd/workass`: passed.
- `node --test scripts/tests/codex-native-host.test.mjs scripts/tests/claude-native-host.test.mjs`:
  52 passed, 1 existing real-Claude credential-dependent test skipped.
- `scripts/rebuild-workass-macos.sh daemon --profile dev`: repository GATE_PASS,
  candidate preflight passed, detached handoff reached phase=healthy.
- Dev daemon changed from PID 49888 to 90079. Dev shell reconnected as controller
  with 18 ready models. Production daemon 60527 and shell 60621 unchanged.
- Handoff receipt: `.dev/rebuild/daemon-handoff-20260916T185310Z-87595.status`;
  handoff log: `.dev/rebuild/daemon-handoff-20260916T185310Z-87595.log`.
- Browser status retained its pre-existing `persistent=true`, `agentControl=true`,
  `cdpAttached=false`; CDP attachment is not claimed verified.
- Native-host tests are deterministic protocol fixtures. No real-model automatic
  compaction experiment or live Windows provider test was performed.
- Devin intentionally uses the per-prompt fallback under the subsequent user decision.
- Follow-up policy check: existing runtime routing already implements this choice;
  only documentation changed, so no rebuild or activation is needed.
