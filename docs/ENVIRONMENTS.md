# Workass runtime environments

Workass uses three explicit profiles. Mutable state is never shared between
them.

| Profile | Daemon | Renderer | Data root | App |
|---|---:|---:|---|---|
| `prod` | LAN bind, port `8788` | `127.0.0.1:8798` | `~/Library/Application Support/Workass` | `/Applications/Workass.app` |
| `dev` | `127.0.0.1:18788` | `127.0.0.1:8799` | `<repo>/.dev/profiles/default` | source Electron |
| `test` | OS-assigned | OS-assigned | a required temporary directory | headless |

Production on Windows has its own file, `config/environments/windows-prod.env`:
LAN bind, port `80`, data under `%LOCALAPPDATA%\Workass`. Linux hosts are headless;
see `docs/LINUX-HEADLESS.md`. The daemon serves TLS only, so its URLs are `https`
and `wss`.

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
runtime/     installed production daemon and native provider hosts
logs/        development/test logs (prod logs: ~/Library/Logs/Workass/prod)
```

## Test root

`WORKASS_TEST_ROOT` is the test profile's data root, and the daemon holds to it
without being told anything else:

- State is `<root>/state`; `app-config.json` and `providers.json` sit in `<root>`.
- `--state-dir` must be strictly inside the root, or the daemon refuses to start.
- `HOME` and the `XDG_*` directories move to `<root>/home`, because provider
  detection launches the real CLIs and they write under `HOME`. Those CLIs are not
  logged in there. A rig that needs logged-in providers uses the dev profile with
  a scratch `--state-dir`.
- `WORKASS_DATA_ROOT` and `WORKASS_LOG_ROOT` are overwritten, and inherited tool
  credentials outside the root are dropped, so a daemon started from an agent's
  shell does not hand its children the installed daemon's paths.
- A root that is a source checkout, holds the home directory, or overlaps the
  installed data is refused.
- `--install-service` is refused.

`workass fleet` and `workass run` under the test profile default to `<root>/state`.
`workass run` refuses a credential from outside the root unless it is `--unreported`.

The daemon also refuses a start that would adopt a `state/` it was not told about:
the dev profile with no `--state-dir`, a test root without the test profile, the
test profile with neither a root nor `--state-dir`, and any start with no
`--state-dir` in a source checkout. Only an unpacked release outside a checkout
keeps the `state` default. The test profile with an explicit `--state-dir` and no
root is not confined; `scripts/canary` runs that way to reach real providers.

## Development

`desktop/scripts/dev-launch-macos.sh` loads only the `dev` profile. It starts
the isolated development daemon when needed, then opens the source shell on
port 8799. It never reads or writes production state. The shell uses the exact
Electron version pinned in `config/macos/electron.version`, staged by
`scripts/vendor-electron-runtime.sh`; an arbitrary global npm Electron is not a
release input.

## Production package

Publishing a release to every platform is `scripts/release/ship.sh`; see
`docs/DISTRIBUTION.md`. It never installs or activates anything.

`scripts/package-workass-macos.sh` builds the renderer, runs the repository gate
and package contracts, signs `Workass.app` and the standalone daemon with one
persistent identity, installs the app through a stop/stage/swap/rollback transaction, and
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
daemon, native provider hosts, and pinned portable Node runtime. Opening the app starts
its bundled server directly if needed, without installing a login or restart
service. Killing the server leaves it stopped until the app is opened again or
explicit recovery is requested. Old daemon LaunchAgents are removed on startup. See
`docs/DISTRIBUTION.md`.

## Rebuilds

Rebuild commands accept an explicit profile:

```sh
scripts/rebuild-workass-macos.sh electron --profile dev
scripts/rebuild-workass-macos.sh daemon --profile prod
```

A production daemon handoff additionally targets the installed runtime. Daemon
replacement is a terminal operation because it terminates daemon-owned ACP
processes. Dev Electron rebuilds never signal the production daemon.
