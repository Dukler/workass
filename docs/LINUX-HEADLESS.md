# Workass on Linux: the headless daemon

A Linux fleet machine runs no window. There is no Electron shell, no renderer
process, and nobody sitting in front of it to click "install update". That one
fact decides everything below: the release artifact is a service install rather
than an app, and the activation mechanics live in the daemon (Go,
`internal/headlessupdate`) instead of in the Electron updater
(`desktop/shell/update-manager.js`) that owns macOS and Windows.

What is deliberately *not* different: the wire and MCP contract. The same
`update.targets` / `update.status` / `update.apply` methods, the same
`app:update-status` / `app:update-apply` channels, the same
`update <machineId> from <a> to <b>` authorization sentence, and the same
`operation_id` replay receipts. A controller on another machine cannot tell
which half of the fleet answered, and does not need to.

## The artifact

`scripts/stage-linux-headless.sh` produces, per release:

```
Workass-<version>-linux-amd64.tar.gz
  Workass-<version>-linux-amd64/
    workass                        the daemon (run with -headless)
    workass-tools                  tools client
    workass-agent                  LM Studio ACP server
    node/linux-amd64/bin/node      pinned portable Node, SHA-256 verified
    frontier-hosts/linux-amd64/    claude / codex / pi / omp hosts + Agent SDK
    manifest.json
workass-linux-amd64-release.json   the update feed for this platform
SHA256SUMS-linux-amd64
```

All three are assets on the same GitHub release the Windows client reads, so
one tag carries every non-Mac target and each client resolves its own manifest
through `/releases/latest/download/`.

The archive layout **is** the install layout: every entry sits directly in the
install root (`/data/workass/runtime` on duklerver). That is what lets the
daemon's own updater rename the new tree into place without a translation
table.

`headless: true` in the feed is to Linux what `portable: true` is to Windows:
it states the artifact's shape, so a daemon never accepts an app bundle and an
app never accepts a daemon tree. Platform and arch are checked just as
strictly.

Only `linux-amd64` is published today, because that is the only Linux machine
in the fleet. `--arch arm64` already works in the staging script and
`vendor-node-runtime.sh`; publishing it is one more phase in
`scripts/release/stage-updates.sh` and one more pair of input paths in
`scripts/release/lib/release-input.mjs`.

## Installing from the release alone

```sh
install_root=/data/workass/runtime
mkdir -p "$install_root" /data/workass/state
chmod 700 /data/workass/state
curl -fLo /tmp/workass-linux.tar.gz \
  https://github.com/Dukler/workass/releases/latest/download/Workass-<version>-linux-amd64.tar.gz
curl -fLo /tmp/SHA256SUMS-linux-amd64 \
  https://github.com/Dukler/workass/releases/latest/download/SHA256SUMS-linux-amd64
(cd /tmp && sha256sum -c SHA256SUMS-linux-amd64)   # must pass before extracting
tar -xzf /tmp/workass-linux.tar.gz -C /tmp
cp -a /tmp/Workass-<version>-linux-amd64/. "$install_root"/
```

Then a `systemd --user` unit, no root beyond `loginctl enable-linger`:

```ini
[Service]
Type=simple
WorkingDirectory=/data/workass
ExecStart=/data/workass/runtime/workass -headless -bind lan -port 8788 -state-dir /data/workass/state
Environment=PATH=/data/workass/runtime/node/linux-amd64/bin:/usr/local/bin:/usr/bin:/bin
Environment=WORKASS_NODE=/data/workass/runtime/node/linux-amd64/bin/node
Restart=on-failure
RestartSec=5
```

`Restart=` is not optional. It is the activation mechanism (below), and a unit
with `Restart=no` reports `supported: false` with that exact reason rather than
updating and leaving the machine down.

## How activation works

`Restart=on-failure` plus a daemon that replaces its own files and then exits
is the whole mechanism. No root, no `systemctl restart` from inside the unit's
own cgroup (which `KillMode=control-group` would kill mid-swap), no detached
helper to lose track of.

One `workass_apply_update` for this exact machine runs:

1. **Discover.** A background loop (hourly) reads the feed. `update.status` is
   a pure read of that cached state: the read tools promise they never check
   for an update, and the promise is kept literally.
2. **Download and verify.** Exact size and SHA-256 from the manifest.
3. **Extract.** Absolute paths, `..`, symlinks, hard links and device nodes are
   rejected rather than sanitized.
4. **Preflight.** `<staged>/workass version` must print the version the
   manifest binds. A wrong architecture, a truncated extract or a mislabelled
   release all fail here, where failing costs nothing.
5. **Swap.** Same-filesystem renames into `.workass-update/backup-<previous>/`
   and then into place. Replacing a running executable is safe on Linux: the
   running image holds the old inode.
6. **Receipt, then exit.** A durable `activating` receipt, then the ordinary
   graceful shutdown and exit `75`, which the unit restarts on. Engines settle
   exactly as on any other stop. The reply reaches the caller first.
7. **Arbitrate, then confirm.** The next boot's arbiter runs before any port is
   opened. If this is the new version it becomes `activated`; once the daemon is
   actually *serving* it becomes `healthy`. A binary that boots but never
   serves is rolled back from the backup on the third attempt.

What the updater replaces: `workass`, `workass-tools`, `workass-agent`,
`frontier-hosts/<platform>-<arch>/`, `manifest.json`.

What it installs only when absent: `node/<platform>-<arch>/`. A
hand-provisioned host may hold a fuller Node than the release prunes to — the
one on duklerver keeps `npm`, which is how Pi and Codex were installed beside
it — and an update is not a licence to take an operator's tool away. The
release still carries Node so a first install from the release alone is
complete.

What it never touches: anything outside the install root. `/data/workass/pi`,
`/data/workass/codex`, `/data/workass/state` and the unit file are not the
release's to own.

## Reading and driving it from another machine

From the Mac, with duklerver mounted and paired:

```jsonc
// workass_list_update_targets  -> duklerver's exact machineId
// workass_get_update_status    { "machine_id": "<id>" }
//   -> { headless: true, diagnostics: { strategy: "linux-systemd-user",
//        state: { supported, currentVersion, availableVersion, phase }, receipt } }
// workass_apply_update         { "machine_id": "<id>",
//        "expected_current_version": "<current>", "expected_target_version": "<target>",
//        "authorization": "update <id> from <current> to <target>",
//        "operation_id": "<caller-stable id>" }
```

`workass_apply_update` is authorized by a current human request that names that
exact machine and orders the update now. Nothing else authorizes it: not a
build, not a publish, not availability, not an older message, not another
agent. Replaying the same `operation_id` reads its durable receipt back and
cannot start a second activation.

## Diagnosing

```sh
journalctl --user -u workass.service -n 100
cat /data/workass/state/headless-update/receipt.json
ls /data/workass/runtime/.workass-update/
```

The receipt is the record that survives the restart it causes: the process that
writes `activating` is never the process that reports `healthy`.
