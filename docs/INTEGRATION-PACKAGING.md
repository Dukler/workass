# Installed harness integrations

User authority (2026-09-16): every supported harness, including OMP, must be
installed on the target machine before Workass can use it. Ship connection code,
not another copy of a provider's engine or its dependency graph.

Packaging lane: this document; scripts/vendor-frontier-hosts.sh;
scripts/vendor-node-runtime.sh; scripts/tests/integration-packaging.test.mjs;
internal/acp/native_omp{,_test}.go; scripts/omp-{native-host,installed-host,sdk-extension}.mjs;
desktop/acp/mock-omp-cli.mjs; scripts/tests/omp-{native-host.test,sdk-smoke,installed-smoke}.mjs;
docs/OMP-NATIVE-SDK.md; scripts/omp-sdk/package{,-lock}.json;
scripts/release/prepare-input.sh; scripts/package-workass-macos.sh;
scripts/stage-windows-portable.sh.

Codex, Claude, OpenCode, Devin, Qwen, OMP and custom providers use their existing
user-owned installs. Keep the small official Claude client SDK (it launches the
installed claude executable), Workass's connection hosts and one shared Node
runtime. Do not ship OMP's SDK engine/dependency tree or Bun. No npm, npx,
corepack or dependency installer is shipped inside the shared Node runtime.
Preserve licenses. No runtime downloads or automatic provider installations.

OMP launch sets WORKASS_OMP_EXECUTABLE to the installed executable (WORKASS_OMP
remains the discovery override). A small Node wrapper loads an OMP extension
through its supported --extension option. The session_start hook supplies the
public api.pi SDK exports to the existing Workass SDK host over a one-use,
authenticated loopback connection. The outer CLI is idle with --no-session and
--no-tools; it receives no prompts and owns no persisted Workass conversation.
The SDK sessions retain ordinary user tools, extension discovery, permissions,
compaction and exact native journals. No stdio takeover, extension guard bypass,
RPC translation, downloaded source, bundled SDK, or bundled Bun is needed.
Bootstrap credentials are deleted before SDK tools/subagents are created.
The native OMP version is reported from the installed SDK rather than hardcoded.

Acceptance: packaging fixtures and actual Mac/Windows host staging contain no
OMP engine/Bun/npm trees; all existing provider host checks pass; isolated dev
rebuild. No publication or production activation requested.

Final runtime inventory: Claude's server client entrypoint plus package metadata
and licenses; Workass connection scripts; one Node executable plus license;
Electron's target-platform runtime, locale/ICU/media/GPU resources and licenses;
Workass daemon, shell and renderer assets. Codex/OpenCode/Devin/Qwen/custom
harnesses are never copied into this tree. Preserve both renderer deliveries:
the daemon serves remote clients and Electron serves its local view.

Keep standalone Node: it runs the native hosts and the currently shipped
Windows update worker after Electron closes; old updaters also require it in
incoming releases. Removing it would break the supported update path. Do not
replace it with a copied Electron executable lacking its shared libraries.

Release Go binaries omit linker symbol tables and DWARF debug sections (-s -w),
while retaining runtime stack traces and Go build/version metadata. Development
builds retain their ordinary debugging information. Browser locale resources
and third-party licenses remain required; no locale or browser-feature pruning.
