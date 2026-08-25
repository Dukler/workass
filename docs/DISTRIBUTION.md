# Workass distribution road

Workass has two intentionally separate packaging lanes:

- **Dogfood install** updates the copy on the development Mac. It may use the
  private, persistent Workass development certificate and is never published.
- **Public release** produces an immutable, versioned artifact for another
  computer. It requires the platform owner's distribution identity and every
  platform verification gate.

An updater is downstream of this contract. It consumes a signed release; it
does not create identity, relax Gatekeeper, overwrite live code, or run a
compiler/test suite during activation.

For local Mac dogfood, build and install are deliberately separate:

```sh
scripts/package-workass-macos.sh \
  --artifact-only "$PWD/.dev/package/candidates/Workass.app" \
  --version X.Y.Z

scripts/install-workass-macos.sh \
  --candidate "$PWD/.dev/package/candidates/Workass.app"
```

The first command owns the repository gate, renderer build, runtime staging,
and signing. The second command consumes only those immutable app bytes. It
does not call Go, Node package tools, or the repository gate, so an activation
or rollback retry never rebuilds the candidate. It verifies the stable signing
identity and shell/daemon version match before stopping the old shell, then
requires the new TLS daemon, Electron version, controller, catalog, and browser
control to recover before deleting the previous app.

## macOS milestone 1 — implemented

`scripts/release-workass-macos.sh` builds an Apple-silicon release containing:

- the Electron shell and renderer, from the exact version pinned in
  `config/macos/electron.version` and verified against Electron's published
  SHA-256;
- the Go daemon;
  - Workass-owned Claude/Codex native hosts plus Anthropic's checksum-pinned
    official Claude Agent SDK (never vendor CLI binaries or credentials);
- pinned Node.js LTS runtime bytes, fetched at build time from nodejs.org and
  checked against a committed SHA-256;
- a first-launch user LaunchAgent bootstrap, so a clean Mac needs no clone,
  npm, Homebrew, or terminal setup.

The release command requires an Apple `Developer ID Application` identity,
signs nested Mach-O code from the inside out, enables the hardened runtime with
the minimum Electron JIT entitlement, submits the app and DMG to Apple's notary
service, staples both tickets, and runs Gatekeeper assessment. It emits:

```text
dist-release/macos/X.Y.Z/
  Workass-X.Y.Z-darwin-arm64.dmg
  Workass-X.Y.Z-darwin-arm64.zip
  RELEASES.json
  workass-darwin-arm64-release.json
  release.json
  SHA256SUMS
  notary-app.json
  notary-dmg.json
```

Every release is staged outside `dist-bin` and outside the installed production
runtime. The Go daemon is rebuilt from the gated source, and the exact supported
adapter and Electron versions are installed into that isolated staging
directory. A release build therefore cannot wipe or replace the shell/provider
runtime serving a live Workass session. Development consumes the same pinned
Electron runtime without adding an npm dependency to `desktop/package.json`.

Setup is deliberately outside the build script:

1. Enroll in the Apple Developer Program.
2. Install a `Developer ID Application` certificate in the signing keychain.
3. Store notary credentials in Keychain using `xcrun notarytool
   store-credentials`; never put an Apple password or API key in this repo.
4. Run the release:

```sh
WORKASS_CODESIGN_IDENTITY=<Developer-ID-certificate-SHA1> \
WORKASS_NOTARY_PROFILE=workass-notary \
  scripts/release-workass-macos.sh \
    --version 1.0.0 \
    --build-number 1 \
    --base-url https://releases.example.com/workass/darwin/arm64
```

This Mac currently has no Developer ID identity, so the real Apple submission
cannot be exercised until those account credentials exist. The release command
fails before producing public artifacts when they are absent.

The verified runtime staging tree is currently about 719 MB uncompressed before
the Electron shell is added. That is functional but not a 1.0 size target.
Before broad distribution, prune only adapter files proven unnecessary by the
ACP initialize/turn canaries and record a compressed DMG/ZIP budget; do not
remove dependency files by guesswork.

## App-owned transactional updater — implemented

Packaged Workass includes its own updater manager and an independent update
worker. It consumes `workass-darwin-arm64-release.json` from the GitHub release
feed and treats Electron, the renderer, Go daemon, pinned Node runtime, and
native agent hosts as one indivisible release. There is no supported
shell-only or daemon-only automatic update path.

The updater:

1. downloads only over HTTPS and enforces the manifest's exact size and SHA-256;
2. inspects archive paths before extraction and stages beside the installed app;
3. verifies the incoming app's platform signature, version, bundled daemon,
   runtime manifest, and mutual designated requirement with the installed app;
4. asks the daemon for a user-clicked handoff (user law 2026-08-25). The
   daemon atomically blocks new turns, subagents, tracked external work, and
   provider updates for the handoff window; active work never blocks the
   click and is recorded in the durable receipt instead;
5. starts an updater worker outside the code being replaced, stops the daemon,
   atomically swaps the complete release, and launches the new Workass;
6. keeps the previous release until daemon health, controller authority,
   non-empty provider catalog, and shell version all match the target; and
7. automatically restores and health-checks the previous release if any
   activation gate fails. A durable receipt under the Workass state directory
   records `healthy`, `rollback_healthy`, or `failed`.

The UI exposes this through the same compact sidebar update card used for
provider updates; it does not add a separate Settings section. Updates are
user-clicked only: Workass never enters or schedules an update on its own,
and a user click updates immediately regardless of running work.

### macOS local update channel

Dogfood builds on the development Mac do not require GitHub, Apple Developer
ID, or notarization. A locally packaged app carries
`WORKASS_UPDATE_CHANNEL=local` and reads its platform manifest from:

```text
~/Library/Application Support/Workass/update-feed/
  workass-darwin-arm64-release.json
  Workass-X.Y.Z-darwin-arm64.zip
  SHA256SUMS
```

The normal private lane prepares both platform updates from one immutable
commit. After dev acceptance, materialize the generated renderer before the
single reviewed commit:

```sh
scripts/release/prepare-source.sh
```

After that exact commit is clean and pushed, publication is one command:

```sh
scripts/release/ship.sh
```

`ship.sh` discovers the next version from the installed app, local feed, and
stable GitHub release, derives one retry-stable build number from the exact
commit, and owns the gate, parallel Mac/Windows staging, publication, and
readback. It emits one final `publication=` receipt and keeps complete output
in its `log=` artifact; callers do not repeat its internal checks. The
installed Mac app reads and copies those local bytes directly; it never starts
an HTTP server and never weakens the transactional
daemon/controller/catalog/browser health gates. Its local signature preserves
bundle identity and macOS privacy grants without an Apple account.
`scripts/stage-updates.sh` and the platform packagers remain internal lanes,
not normal publication entrypoints.

The packaged shell checks once shortly after launch and then polls this local
manifest every 30 seconds. Publishing a dogfood build therefore makes the
existing production app's update card appear without restarting Electron.
Public GitHub builds use the same state machine with an hourly check instead.

`scripts/release-workass-macos.sh` remains the public lane. Its
`--release-signing` package automatically carries
`WORKASS_UPDATE_CHANNEL=github`, so after Developer ID and notarization are
configured, public builds resume the platform-specific GitHub feed without a
source change.

## Windows road

The corresponding normal Windows artifact requires:

1. x64 app/daemon build plus the vendored adapters and pinned portable
   `node.exe` (the staging layout already exists).
2. A user-level service/task lifecycle equivalent to the macOS LaunchAgent.
3. Authenticode signing for every executable and the installer, preferably
   through Microsoft Azure Artifact Signing or an EV signing provider.
4. A signed Squirrel/MSIX/MSI installer and update feed, with the same
   user-clicked daemon handoff contract.
5. A clean Windows VM acceptance test with no Node, npm, source checkout, or
   developer credentials installed.

Existing protected Windows rebuild scripts remain untouched.

### Portable Electron bundle — implemented

For endpoint-managed Windows machines, a separate portable lane deliberately
avoids the full installer above: no MSI, no registry hives, and no admin. The
Mac build host runs:

```sh
scripts/stage-windows-portable.sh --version X.Y.Z
```

The resulting zip contains `Workass.exe`, `workass-daemon.exe`, the renderer,
the checksum-pinned portable `node.exe`, and the vendored Claude/Codex native
hosts in one extracted directory. Launching `Workass.exe` starts the sibling
daemon with `--headless` when its health endpoint is unavailable; otherwise the
Electron shell connects to the already-running daemon. The same sibling-daemon
handoff is used by the packaged macOS shell.

For daemon-only operation, run the sibling executable directly:

```powershell
workass-daemon.exe --prod --headless --install-service
```

This creates a least-privilege per-user Scheduled Task on Windows or a user
LaunchAgent on macOS. Signed upstream binaries are left byte-for-byte intact.
The bundle contains the transactional updater and emits
`workass-windows-amd64-release.json`. Portable Windows builds update through
GitHub without requiring Authenticode: Workass verifies the latest stable
release manifest over HTTPS, the archive's exact size and SHA-256, safe ZIP
paths, the embedded platform/version metadata, and the required PE32+ x86-64
runtime files before the quiescent swap. A failed health/controller/catalog
recovery rolls back to the previous extracted directory. Authenticode remains
recommended for reducing Windows reputation warnings, but it is not an update
gate for this private portable lane. Builds that predate this capability need
one manual portable replacement before future in-app updates can work.

## Release checklist — Mac + Windows portable

Use one version for both artifacts. Build from a clean, committed `main`; do
not publish a ZIP from `dist-bin` or a loose renderer directory. The release
must be self-contained at extraction time.

1. Run the guarded dev recovery check. In Workass press `⌘,`, then Enter on
   **Reiniciar daemon y reconectar**. It must replace the dev daemon process
   and return with controller authority and a populated catalog. Recovery
   preserves malformed startup files under `state/recovery/`; if it has to
   replace the TLS certificate, remote machines must pair again.
2. Build and notarize the Mac release using
   `scripts/release-workass-macos.sh --version X.Y.Z --build-number N
   --notary-profile PROFILE`. This produces the DMG and update ZIP.
3. Build the Windows portable release on the Mac with
   `scripts/stage-windows-portable.sh --version X.Y.Z --output-root
   "$PWD/dist-release/windows"`.
4. Inspect the Windows ZIP before publishing. Its top-level extracted folder
   must contain `Workass.exe`, `workass-daemon.exe`, `resources/app`,
   `resources/renderer`, `node/windows-amd64/node.exe`, and
   `frontier-hosts/windows-amd64/`. If any is absent, the artifact is rejected.
5. On a clean Windows test folder, launch only `Workass.exe`. It must start
   the sibling daemon when none is healthy, connect when one is already
   healthy, and survive **Reiniciar daemon y reconectar**. No Windows-side
   npm, source checkout, PowerShell bootstrap, or installer script is part of
   that test.
6. Publish the Mac DMG + ZIP, Windows ZIP, both platform feed manifests, and
   their `SHA256SUMS` files under the same Git tag/release. A Windows-only
   portable release is also valid, but it must be a stable GitHub Release so
   GitHub's `latest/download` URL resolves its fixed manifest asset name. Keep
   all release outputs immutable after checksums are generated.

## Linux road

Linux follows after Windows: x64/arm64 daemon/runtime bundles, a user systemd
unit, signed repository or AppImage/deb artifacts, and clean-VM installation
tests. The browser-served UI remains the portable fallback on every platform.
