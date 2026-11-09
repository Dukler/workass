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
- direct launch of the bundled server when the app opens, so a clean Mac needs
  no clone, npm, Homebrew, terminal setup, or automatic background service.

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

1. verifies the manifest's offline signature against the keys pinned in the
   installed app, on every platform, before it offers or stages anything (see
   "Release manifest signature" below); then downloads only over HTTPS and
   enforces the manifest's exact size and SHA-256;
2. inspects archive paths before extraction and stages beside the installed app;
3. verifies the incoming app's platform signature, version, bundled daemon,
   runtime manifest, and mutual designated requirement with the installed app;
4. asks the daemon for a user-clicked handoff. The
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

Steps 5–7 above are Windows (its native installer) and the first update
driven by a macOS manager of 0.1.274 or older. On macOS from the release after
it (owner, 2026-11-09), everything that proves a release works happens while
Workass stays open, and the close is only a restart:

- **Staged in the background.** As soon as discovery finds a new release
  (`stageInBackground`), steps 1–3 run in the app; the staged app is then
  sealed read-only (owner write taken away: cheap hygiene, since anyone able to
  undo it could change the installed app as well, and macOS checks the
  signature at launch).
- **Preflight** (`runUpdatePreflight`, `desktop/shell/update-manager.js`). The
  staged app itself is started, as it will be after the swap, against an APFS
  clone of the live state (copied again until the database did not move during
  the copy) in a temporary directory, on two free loopback ports: its shell
  hidden as an accessory app (no window shown, no Dock icon, no menu bar, never
  the focus, audio muted) with its own profile, starting its own daemon, which
  runs any state migration on the copy. With `WORKASS_UPDATE_PREFLIGHT=1` that
  daemon serves loopback only, closes admission before any chat actor exists
  (no turn, subagent, job or agent update), fires no schedule, and neither
  announces itself nor links to the fleet. It must pass the gates the old
  post-launch check had: daemon version, shell version, the renderer rendered,
  controller authority, and a model catalog with ready models, within 120 s.
  Then its daemon and shell are shut down and the temporary data deleted.
- **A failed preflight** never installs. The release is removed, the failure
  is written as a `failed` receipt with `step: "preflight"` (shown by the
  update icon and Ajustes · Actualizaciones, also after a restart), the log of
  the staged app is kept in `updates/preflight-last.log`, and the installed
  app is untouched. A newer release is staged and preflighted on its own.
- **The close.** The click (or "Instalar solo") hands the worker a `closeOnly`
  transaction: wait for the shell to exit, stop the daemon, put the old app
  aside and the preflighted one in its place (two renames in the same folder;
  a failed rename puts back what it moved and restarts the old app), record
  the update as done, start the new app, then delete the old files and make
  the new app owner-writable again. No progress window, no verification, no
  state snapshot, no health gate and no rollback run after the restart: the
  restart is the end of the update. If the worker dies after the swap, the new
  app finishes the transaction when it starts.
- **The first update from 0.1.274.** That manager still starts the incoming
  release's progress process and refuses its handoff without a fresh
  `windowVisible: true` heartbeat. The new release answers it from an accessory
  process with no window (`runWindowlessCompatibilityOwner`, the receipt also
  says `windowless: true`), and its worker runs the full flow for transactions
  that are not `closeOnly`. Both are removable once 0.1.274 is no longer the
  installed version.

Windows is unchanged: steps 1–3 run after the click, its native installer
replaces the files, and the relaunched app shows its own recovery window
(`createUpdateBootstrapWindow`) while it starts, the only update UI there. It
never used the update progress process.

A staged Mac release survives a restart. `staged.json` in its transaction
directory names it; on launch the shell keeps it out of startup cleanup and
believes nothing in that file as written: the ids and paths must be this
installation's, the manifest must verify against the pinned keys and still be
newer than the running version, and the staged app must still be there. Then
the platform verification staging ran (signature, mutual designated
requirement, version, bundled daemon) runs again off the main thread, and the
worker runtime is copied again from that app and self-tested. Only then is it
ready, and on macOS it is preflighted again against the state as it is now;
anything that fails is removed and the next check stages again. A directory an
install began from (it has `transaction.json`) is left to recovery. While a release waits staged, discovery keeps running, and a newer
release replaces it (staged again) rather than the older one installing.

The UI exposes this through the left panel's update icon and Ajustes ·
Actualizaciones, both driving the same updater with the same single
"Actualizar". A user click updates immediately regardless of running work.
Otherwise Workass installs nothing on its own unless the owner turns on
"Instalar solo" (settings key `autoInstallUpdates`, off by default, Mac only).
Then a staged release installs by itself only while nothing would be cut: the
shell first reads `POST /workass/update/work` (loopback, changes nothing) and
asks for the handoff only when it is quiet, and backs off again unless the
handoff's own counts, taken with admission closed, are all zero. It never
installs over a turn, subagent, background task, job, agent update or starting
operation; it waits and asks again a minute later. An unattended install
relaunches without taking focus. A release adopted
after a restart waits for one discovery first, so a newer one can replace it.
`workass_apply_update` takes the exact machine, the expected versions and a
stable operation id; Ajustes · Actualizaciones uses the same exact-machine
route for "Actualizar <máquina>".

### Release manifest signature

HTTPS, the size and the SHA-256 all come from the same GitHub release as the bytes
they describe, so a compromised account or asset satisfies every one of them. The
check that does not is an ed25519 signature over each platform's manifest, made at
publish time with an offline key and verified against keys pinned in the shipped
binaries.

- **Format.** One additive field in the manifest,
  `"signature": { "algorithm": "ed25519", "keyId": "...", "value": "<base64>" }`,
  so a client that predates it reads the same manifest it always read. The signed
  bytes are the canonical form built from the manifest's fields, in one fixed order
  and length-prefixed: schema version, product, version, build, platform, arch,
  `portable`, `headless`, designated requirement, notes, and the update artifact's
  name, URL, SHA-256 and size.
- **Where it is verified.** `verifyReleaseSignature` in
  `desktop/shell/update-manager.js` for macOS, Windows and the Mac local feed, at
  discovery and again before the first byte is staged, and `Check` in
  `internal/headlessupdate` on Linux. `verify-candidate` and `verify-publication`
  make the same check, and `publish-windows.sh` makes it before it can reach GitHub.
- **Failing closed.** A missing, malformed, wrong or tampered signature leaves the
  update card in its failed state with a plain sentence, and nothing is downloaded.
  There is no environment override and no way for a release to name its own key.
- **Replay.** A real signature on an old release still never installs: a release that
  is not newer than the installed version is not offered and cannot be applied.
- **Windows** needs no Authenticode. This signature is the independent check there.
- **Older installed clients** ignore the field and keep updating, with no more
  protection than they had. The protection starts with the first installed build that
  carries the pinned keys.

The private key is at
`$HOME/Library/Application Support/Workass/signing/release-manifest-ed25519.key`
(base64 of the 64-byte ed25519 private key, mode 0600), outside the repository. The
`stage-*` scripts read it at publish time only, and stop before the build if it is
missing, readable by others, or not one the updaters pin. `WORKASS_RELEASE_MANIFEST_KEY`
moves the path for tests.

**If the key is lost:** no new release can be signed, and installed clients will
refuse any release that is not. Nothing already installed breaks, and those machines
simply stop being offered updates. Recovery is one manual install per machine: make a
new key, pin it, build, and replace the app or daemon by hand on each machine, as a
first install does. After that, in-app updates work again. Keep an offline backup of
the key so this never has to happen.

**If the key is exposed:** treat it as lost, and do it quickly. A published release
cannot withdraw trust from the old key, only a release that removes it from the list
can, so anything still running a build that pins it trusts it until updated.

**To rotate while the old key still works:**

1. Generate the new key at a new path outside the repository, mode 0600.
2. Add its public half to both lists, `pinnedReleaseManifestKeys` in
   `internal/headlessupdate/signature.go` and `PINNED_RELEASE_MANIFEST_KEYS` in
   `desktop/shell/release-manifest-signature.js`. Keep the old key. A test holds the
   two lists equal.
3. Publish a release signed with the old key, and let every machine take it.
4. Point the lane at the new key and publish. Machines on step 3's build accept it;
   a machine still older than that refuses it and needs step 3's release first.
5. Remove the old key from both lists in a later release.

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
not normal publication entrypoints. The local feed is signed and verified the
same way as the public one, so a dogfood build needs the signing key on this Mac.

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

The resulting zip contains `Workass.exe`, `workass-daemon.exe`, `workass-tools.exe`, the renderer,
the checksum-pinned portable `node.exe`, and the vendored Claude/Codex native
hosts in one extracted directory. Launching `Workass.exe` starts the sibling
daemon with `--headless` when its health endpoint is unavailable; otherwise the
Electron shell connects to the already-running daemon. The same sibling-daemon
handoff is used by the packaged macOS shell.

For daemon-only operation, run the sibling executable directly:

```powershell
workass-daemon.exe --prod --headless --install-service
```

This creates a least-privilege per-user Scheduled Task on Windows. macOS uses
direct server startup when the app opens and rejects service installation;
manual daemon-only operation uses `workass --headless`. Signed upstream binaries
are left byte-for-byte intact.
The bundle contains the transactional updater and emits
`workass-windows-amd64-release.json`. Portable Windows builds update through
GitHub without requiring Authenticode: Workass verifies the manifest's offline
signature (see "Release manifest signature"), then the latest stable release
manifest over HTTPS, the archive's exact size and SHA-256, safe ZIP paths, the
embedded platform/version metadata, and the required PE32+ x86-64 runtime
files before the quiescent swap. A failed health/controller/catalog
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
   must contain `Workass.exe`, `workass-daemon.exe`, `workass-tools.exe`, `resources/app`,
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
