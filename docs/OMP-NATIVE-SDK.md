# OMP native SDK integration

User authority (2026-09-16): replace OMP ACP transport with its native SDK, use
Luna subagents, append the same centralized Workass instructions as other native
providers. SDK and required native runtime packaging are authorized dependencies.
Retain OMP's auth/settings, built-in tools/extensions, native subagents, session
lineage and compaction; no new Workass MCP server. Fail explicitly for unsupported
controls or missing exact sessions, never silently create replacement context.

Root lane: this document; internal/acp/{native_omp.go,native_omp_test.go,
provider_registration.go,native_frontier_test.go,provider_boundary_contract_test.go};
desktop/acp/mock-omp-sdk.mjs; scripts/tests/omp-native-host.test.mjs;
existing OMP registration tests where old ACP defaults are asserted;
cmd/workass/main_test.go native fixture setup.
Host Luna lane: scripts/omp-native-host.mjs only.
Packaging Luna lane: scripts/vendor-frontier-hosts.sh plus explicitly declared
scripts/omp-sdk/package.json, scripts/omp-sdk/package-lock.json, and
scripts/tests/omp-sdk-smoke.mjs. No desktop/package.json,
desktop/vendor, or Windows launch-script edits; no npm steps on Windows.

No changes to generic chat, persistence, renderer wire or permissions policy.
Native registration owns provider-specific launch/delivery controls. Append
WORKASS_INSTRUCTIONS_FILE through the SDK's additive appendSystemPrompt option.
Use the existing central instruction catalog without new repeated user-message
boilerplate. Runtime verification is dev-only. Publication/activation not requested.

Acceptance: native SDK contract fixtures validate session/model/permission state,
streaming, cancellation, resume, compaction continuity and instruction append;
actual pinned SDK imports/initializes in an isolated profile without paid inference.
Packaging proves platform-specific runtime/dependency availability. Run affected
checks, repository gate and dev handoff; installed production PIDs unchanged.

Permission controls retain native names: Always ask = "always-ask",
Write = "write", Yolo = "yolo". Default restores native configuration;
Plan uses native plan state plus "always-ask".
Per-tool native policy remains authoritative. Default preserves native settings
and is never claimed to be read-only. Plan proposals use the SDK review handler;
leaving Plan requires the user's mode selection. No global OMP settings are saved.

Superseding user authority (2026-09-16): OMP must already be installed; no engine
or Bun is bundled. See INTEGRATION-PACKAGING.md for the installed executable's
public extension/SDK bridge. WORKASS_OMP selects the install and the launcher
passes WORKASS_OMP_EXECUTABLE to the small transport wrapper. SDK upgrades are
owned by that installed OMP, not Workass releases.
Native tools, local auth and extensions are discovered by the SDK. Extension
operations that replace the current native session fail explicitly because they
would invalidate Workass's exact thread binding. Terminal-only custom UI/text
input is unavailable in the existing Workass permission selector.
