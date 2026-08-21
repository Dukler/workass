# Workass runtime environments

Workass uses three explicit profiles. Mutable state is never shared between
them.

| Profile | Daemon | Renderer | Data root | App |
|---|---:|---:|---|---|
| `prod` | `127.0.0.1:8788` | `127.0.0.1:8798` | `~/Library/Application Support/Workass` | `/Applications/Workass.app` |
| `dev` | `127.0.0.1:18788` | `127.0.0.1:8799` | `<repo>/.dev/profiles/default` | source Electron |
| `test` | OS-assigned | OS-assigned | a required temporary directory | headless |

The checked-in, non-secret source of truth is `config/environments/*.env`.
Machine-local overrides may be placed in
`config/environments/local/<profile>.local.env`; those files are ignored by
Git and accept only the same whitelisted assignments. Frontier-provider login
credentials remain owned by their official CLIs and never belong in a Workass
profile.

Derived paths under each data root are:

```text
state/       canonical chats, archives, checkpoints, native sessions
electron/    profile-specific Chromium/Electron data
run/         browser-control and runtime descriptors
runtime/     installed production daemon and ACP adapters
logs/        development/test logs when applicable
```

## Development

`desktop/scripts/dev-launch-macos.sh` loads only the `dev` profile. It starts
the isolated development daemon when needed, then opens the source shell on
port 8799. It never reads or writes production state. The shell uses the exact
Electron version pinned in `config/macos/electron.version`, staged by
`scripts/vendor-electron-runtime.sh`; an arbitrary global npm Electron is not a
release input.

## Production package

`scripts/package-workass-macos.sh` builds the renderer, runs shell/profile
tests, signs `Workass.app` and the standalone daemon with one persistent
identity, installs the app through a stop/stage/swap/rollback transaction, and
requires controller authority plus a non-empty provider catalog. It rejects
ad-hoc/CDHash-only code and rejects any ordinary update whose designated
requirement is incompatible with the installed release. The downloaded Workass
artwork is stored at `desktop/assets/workass-macos.png` and converted into the
macOS `.icns` during packaging.

Private builds on one Mac can use the opt-in local identity bootstrap; public
distribution requires Developer ID signing and notarization. See
`docs/MACOS-SIGNING.md`.

`scripts/release-workass-macos.sh` is the separate public path. It does not
install or relaunch the local production profile. It produces a versioned,
self-contained, Developer ID-signed and notarized DMG plus update ZIP. The app
is built from a checksum-verified pinned Electron runtime and contains its
daemon, ACP adapters, and pinned portable Node runtime; on a clean Mac, first
launch installs the daemon as a user LaunchAgent. See
`docs/DISTRIBUTION.md`.

## Rebuild law

Rebuild commands accept an explicit profile:

```sh
scripts/rebuild-workass-macos.sh electron --profile dev
scripts/rebuild-workass-macos.sh daemon --profile prod
```

A production daemon handoff additionally targets the installed runtime. Daemon
replacement is a terminal operation because it terminates daemon-owned ACP
processes. Dev Electron rebuilds never signal the production daemon.
