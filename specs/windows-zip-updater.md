# Windows ZIP updater

This lane implements the Windows updater law in `docs/PORT-SPEC.md`.

Owned implementation: `internal/appinstall/`, the native install-update entry in
`cmd/workass/main.go`, and the Windows handoff in `desktop/shell/update-manager.js`.
This repair changes only `internal/appinstall/` and this acceptance document.

An authorized update uses the verified incoming native installer. It waits for
one explicit commit, waits for the outgoing shell to exit, validates release-owned
paths, checks replacement access before deletion, replaces owned files, and
launches Workass once. Preserve user state and unrelated files. Do not introduce
rollback, process sweeps, automatic retries, or health/progress readiness gates.

The native installer owns a small Windows progress window independent of
Electron. Closing that window hides progress without interrupting replacement.
An installation failure writes its durable receipt before displaying a native
error dialog. The dialog reports the actual failing stage/file/error. Dismissing
it does not retry the update. The downloaded ZIP remains available.

Directory validation reuses checks for shared parent directories within one
preflight, but never treats a cached directory as a valid file. File-lock retries
retry the blocked path rather than rescanning earlier files. The 30-second lock
retry budget starts at the first blocked file; filesystem call duration is still
controlled by Windows, not by an artificial cancellation of filesystem work.

Acceptance:
- Existing appinstall fixture tests preserve state, unrelated files and ZIPs.
- A lock failure reports waiting_for_files and the relative filename/OS error.
- A permanently blocked path consumes the bounded retry budget without rescans.
- Native Windows tests accept a reader sharing replacement rights and reject a
  non-sharing reader and a running executable.
- On Windows, observe the native progress window across Electron shutdown and
  a persistent error dialog on failure; no heartbeat gates installation.
- Cross-compilation alone does not prove Windows execution or visual acceptance.
