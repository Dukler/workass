# Workass distribution road

Workass has two intentionally separate packaging lanes:

- **Dogfood install** updates the copy on the development Mac. It may use the
  private, persistent Workass development certificate and is never published.
- **Public release** produces an immutable, versioned artifact for another
  computer. It requires the platform owner's distribution identity and every
  platform verification gate.

An updater is downstream of this contract. It consumes a signed release; it
does not create identity, relax Gatekeeper, or overwrite live code.

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

## macOS milestone 2 — updater, not yet enabled

The release already emits a Squirrel-compatible update ZIP and release
metadata. Before Workass enables Electron's `autoUpdater`, the daemon handoff
must meet these acceptance gates:

1. Download only over HTTPS and validate the platform's signed update.
2. Never replace a running app or daemon in place.
3. Wait for foreground turns and tracked asynchronous work to become
   quiescent, or ask the user to defer the update.
4. Stop the LaunchAgent, atomically activate the new signed release, restart,
   and roll back on failed health/controller/catalog recovery.
5. Preserve the same bundle ID, Developer ID team, and designated requirement.

Until that exists, updates are manual release installs. That is safer than an
updater which terminates background agents.

## Windows road

The corresponding normal Windows artifact requires:

1. x64 app/daemon build plus the vendored adapters and pinned portable
   `node.exe` (the staging layout already exists).
2. A user-level service/task lifecycle equivalent to the macOS LaunchAgent.
3. Authenticode signing for every executable and the installer, preferably
   through Microsoft Azure Artifact Signing or an EV signing provider.
4. A signed Squirrel/MSIX/MSI installer and update feed, with the same
   quiescent daemon handoff contract.
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
This lane does not replace the signed-installer road; it trades the update feed
and Authenticode identity for zero-touch portability.

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
6. Publish the Mac DMG + ZIP, Windows ZIP, and their `SHA256SUMS` files under
   the same Git tag/release. Keep the release outputs immutable after checksums
   are generated.

## Linux road

Linux follows after Windows: x64/arm64 daemon/runtime bundles, a user systemd
unit, signed repository or AppImage/deb artifacts, and clean-VM installation
tests. The browser-served UI remains the portable fallback on every platform.
