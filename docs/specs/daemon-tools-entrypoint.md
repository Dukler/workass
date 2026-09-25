# Daemon tools entrypoint

Authority: the user's 2026-09-22 request to implement the final change in the
San-laptop conversation `cloudstrike`, as narrowed by the 2026-09-25 Windows
endpoint-protection rule. Windows production uses one signed bundled Node
shim; legacy layouts keep in-process daemon dispatch.

Dispatch `workass tools ...` before daemon flag parsing and startup, using the
existing authenticated HTTPS tool client and redacted JSON errors. On
production Windows layouts containing the signed bundled Node runtime, the
packaged `workass-tools.cmd` shim and `workass-tools.mjs` client are the sole
exception: providers receive that shim as `WORKASS_TOOLS_COMMAND`. The shim
uses bundled signed `node.exe` and preserves the existing client security and
tool behavior. Windows layouts without this new shim, including legacy
layouts, use the in-process daemon entrypoint. Other production layouts receive
the current daemon executable. Development may explicitly override the
absolute executable path. Never discover, launch, or retry another sibling
tools helper.

Keep `workass-tools.exe` bundled in this compatibility release because the
installed 0.1.173 updater requires it in an incoming archive. The new native
installer, staging validator, and independent updater worker accept its
absence, but validate its PE32+ x86-64 format when present. The
signed Node shim and its client are the only new Windows-layout exception;
stage and validate both with the signed bundled Node runtime. Required daemon,
shell, identity, archive, and other runtime checks remain in force. Preserve
the frozen LAN protocol, tool capabilities, and security checks. Do not use
tasklist or PowerShell process-query loops for RSS sampling or the raw-MCP
guard, and do not sweep shortcuts through PowerShell, WScript, or ie4uinit.
Do not build or run unsigned Go executables/tests on the San-laptop Windows
development machine; use Mac or Windows CI. These rules do not guarantee
CrowdStrike approval. LAN peer discovery and updater behavior are not changed;
their design options remain outstanding.

## Lane manifest

- `cmd/workass/main.go`, `cmd/workass/tools_command.go`
- `cmd/workass/tools_command_test.go`, `cmd/workass/tools_entrypoint_test.go`
- `internal/appinstall/install.go`, `internal/appinstall/install_test.go`
- `internal/toolcommand/run.go` (package comment)
- `desktop/shell/update-manager.js`, `desktop/shell/update-manager.test.js`
- `desktop/shell/update-worker.js`, `desktop/shell/update-worker.test.js`
- `docs/WORKASS-TOOLS.md`, this spec

Acceptance covers early dispatch, selection of the signed Node shim only for a
complete new Windows layout, legacy in-process fallback, production override
rejection, development override validation, optional helper acceptance,
malformed helper rejection, and preservation of existing installation/user
files. Existing packaging tests continue requiring the compatibility helper.
Validate the isolated dev runtime and publish through the canonical paired
release command. Windows endpoint-protection acceptance requires Windows
observation and is not established by Mac fixture tests or cross-compilation;
no CrowdStrike approval is guaranteed.
