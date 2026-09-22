# Daemon tools entrypoint

Authority: the user's 2026-09-22 request to implement the final change in the
San-laptop conversation `cloudstrike`. This supersedes the separate-executable
runtime selection introduced before release 0.1.173.

Dispatch `workass tools ...` before daemon flag parsing and startup, using the
existing authenticated HTTPS tool client and redacted JSON errors. Production
provider processes receive the current daemon executable as
`WORKASS_TOOLS_COMMAND`; development may explicitly override that absolute
executable path. Never discover, launch, or retry a sibling tools helper.

Keep `workass-tools.exe` bundled in this compatibility release because the
installed 0.1.173 updater requires it in an incoming archive. The new native
installer, staging validator, and independent updater worker accept its
absence, but validate its PE32+ x86-64 format when present. Required daemon,
shell, identity, archive, and other runtime checks remain in force. No signing,
endpoint protection, quarantine, MCP, or production activation changes.

## Lane manifest

- `cmd/workass/main.go`, `cmd/workass/tools_command.go`
- `cmd/workass/tools_command_test.go`, `cmd/workass/tools_entrypoint_test.go`
- `internal/appinstall/install.go`, `internal/appinstall/install_test.go`
- `internal/toolcommand/run.go` (package comment)
- `desktop/shell/update-manager.js`, `desktop/shell/update-manager.test.js`
- `desktop/shell/update-worker.js`, `desktop/shell/update-worker.test.js`
- `docs/WORKASS-TOOLS.md`, this spec

Acceptance covers early subprocess dispatch, missing or present sibling
selection, production override rejection, development override validation,
optional helper acceptance, malformed helper rejection, and preservation of
existing installation/user files. Existing packaging tests continue requiring
the compatibility helper. Validate the isolated dev runtime and publish through
the canonical paired release command. Live Falcon acceptance requires Windows
observation and is not established by Mac fixture tests or cross-compilation.
