# Windows ZIP updater — user correction, 2026-09-16

The Windows update follows download, graceful shutdown, replacement, launch.
No automatic rollback, mutable-state copy, post-launch health/catalog/controller
gate, PowerShell/taskkill cleanup, robocopy mirror, or separate progress owner.
The native `workass-daemon.exe install-update` command from the incoming release
runs outside the installed tree. It waits for one explicit commit and the old
shell's process handle, then for application files to become writable. A locked
file fails before replacement; it never kills processes to unlock files.

Download remains HTTPS/size/SHA-256 checked. Complete ZIPs are stored once in
`updates/downloads/<sha256>.zip`, independently of installation transactions.
Each explicit attempt checks this cache first, including after app restart or
staging/install failure. Matching ZIPs retained in older transaction directories
are imported too. Only missing, incomplete or mismatched artifacts download again;
cache identity is exact size and checksum, not a release name or version alone.
Cache reads do not resume old transactions or grant update authorization.
ZIP paths and installation identity are checked before deletion.
Only inventoried application files are replaced. The first update from a release
without an inventory replaces incoming release paths and preserves unknown paths.
Later updates also remove obsolete files from the previous release inventory.
Unknown files and mutable data remain untouched, including on extraction failure.
An interrupted installation retains its ZIP and records the failed step. No
startup code resumes it. The next explicit update reclaims completed downloads.

Success means files installed and launch requested (`installed`), not that a
provider or renderer passed a health check. The existing updater UI renders that
terminal result. macOS keeps its existing installer. For the first update from an
older shell, the incoming JS compatibility entrypoint hands off to the native
installer; that outgoing shell still owns its already-shipped startup machinery.

Implementation lane: `internal/appinstall/`, `cmd/workass/main.go`,
`desktop/shell/update-manager.js`, `desktop/shell/update-worker.js`,
`desktop/shell/update-progress.js`, their tests, `desktop/renderer2/src/app-updater.ts`,
`desktop/renderer2/src/components/Sidebar.tsx`, the updater renderer tests,
the generated `cmd/workass/embedded/dist/` renderer snapshot, and
this document plus the superseding law in `docs/PORT-SPEC.md`.

Acceptance: deterministic ZIP replacement and failure fixtures preserve data;
blocked files/invalid ZIP paths cannot start deletion; explicit commit and exact
targets are required; native receipt rehydration never resumes/retries; Windows
cross-compilation; affected shell/renderer tests; canonical isolated dev rebuild.
Live CrowdStrike acceptance requires the affected Windows machine and its alert;
Mac tests cannot prove an endpoint policy will permit the new execution path.
