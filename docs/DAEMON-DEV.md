# Workass Daemon Development

This phase includes the P1b mock ACP bridge. The Go daemon serves the existing renderer, accepts the frozen LAN WebSocket protocol, registers daemon-side channel handlers, and can run app-chat sessions against the deterministic mock ACP provider.

## Build

```sh
go build ./...
```

The production binary embeds `desktop/renderer2/dist` via a synced copy under
`cmd/workass/embedded/dist`:

```sh
scripts/sync-renderer2.sh
go build -trimpath -o dist-bin/workass-darwin-arm64 ./cmd/workass
```

`go generate ./cmd/workass` runs the same sync script before builds that need a fresh renderer
bundle. The sync expects `desktop/renderer2/dist/index.html` to already exist; Windows production
never runs npm.

Cross-compile the daemon bundle from the Mac:

```sh
scripts/build-daemon.sh
```

This writes `dist-bin/workass-darwin-arm64`, `dist-bin/workass-windows-amd64.exe`, and
`dist-bin/workass-linux-amd64` with `-trimpath`.

The darwin artifacts are then signed with the persistent local identity
(`com.workass.dev.daemon`, `com.workass.dev.agent`). macOS binds privacy grants to a code-signing
designated requirement, and a Go binary's ad-hoc identity is its CDHash, so an unsigned rebuild is a
new application that must be authorized again. `scripts/macos/workass-signing-status.sh` reports
every Workass identity; see `docs/MACOS-SIGNING.md`.

## Run

```sh
go run ./cmd/workass
```

Defaults:

- `--port 8788`
- `--bind localhost`
- `--renderer-dir ""` (serve embedded `renderer2`; pass a directory to override from disk)
- `--acp-command node`
- `--acp-args '["desktop/acp/mock-server.mjs"]'`
- `--state-dir state`
- `--trust-localhost true`
- `--hibernate-ttl 20m`
- `--rss-sample-interval 30s`
- `--engine-max-age 12h`
- `--engine-max-rss-kb 4194304`
- `--spare-sessions 0`
- `--spare-ttl 5m`

Daemon logs are written to stderr. Stdout is unused.

To run explicitly against the mock:

```sh
go run ./cmd/workass \
  --acp-command node \
  --acp-args '["desktop/acp/mock-server.mjs"]'
```

To serve a renderer directory from disk during development:

```sh
go run ./cmd/workass --renderer-dir desktop/renderer
```

Check the mock oracle directly before debugging daemon behavior:

```sh
node desktop/scripts/probe-acp.mjs node desktop/acp/mock-server.mjs
```

Expected result: `ok: true`, protocol version `1`, and agent name `Workass Mock ACP`.

## LAN Access Approval and Controller Lease

Device tokens are presented on WebSocket connect as a query parameter:

```text
ws://<host>:8788/?deviceToken=<64-hex-token>&deviceName=<name>
```

The daemon-served `/lan-bridge.js` stores `deviceToken`, `deviceId`, and device name in browser
`localStorage` under `workass.lan.*` keys and adds `deviceToken` to every reconnect URL. Tokens are
random 32-byte hex strings. The daemon persists only `sha256:<hash>` values in
`state/devices.json`; plaintext tokens are returned only to the approved waiting client and must not be
logged except as a redacted prefix.

Approval flow:

1. A WebSocket connection without a valid token is upgraded and parked in pending state.
2. The pending client receives:

   ```json
   { "t":"event", "channel":"lan:access-state", "payload":{ "state":"waiting", "requestId":"..." } }
   ```

3. The daemon emits this only to the current controller connection(s):

   ```json
   { "t":"event", "channel":"lan:access-request", "payload":{ "requestId":"...", "ip":"192.168.1.50", "deviceName":"phone", "userAgent":"...", "requestedAt":"..." } }
   ```

4. The controller approves or denies:

   ```json
   { "t":"invoke", "channel":"lan:access-decide", "args":[{ "requestId":"...", "allow":true }] }
   ```

5. Allow issues `{ state:"approved", deviceId, deviceToken, name, controller }` to the waiting
   client only. Deny or 120-second timeout sends `lan:access-state` with `state:"denied"` or
   `state:"timeout"` and closes the pending socket.

`lan:pairing-info` remains registered for identifier compatibility but is deprecated and returns
`{ deprecated:true }` plus any current access status. It no longer accepts or validates PINs.

Localhost dev ergonomics: when `--trust-localhost=true`, localhost WebSocket clients without a
valid token are auto-approved and receive a token without host approval. Tests can force the real
approval path on loopback with `--trust-localhost=false`.

Exactly one approved device owns the controller lease. The first approved device to connect after
daemon start gets it. Another approved device can call `lan:take-control`; takeover is immediate and
broadcasts:

```json
{ "t":"event", "channel":"lan:controller-changed", "payload":{ "deviceId":"...", "name":"..." } }
```

The lease is runtime-only; after restart, first approved connect wins again. Approved devices that are
not the controller can still use read invokes, but controller-gated mutating invokes are rejected
with `reply.error` containing JSON code `lan:not-controller`.

Approved devices can be listed and revoked:

```json
{ "t":"invoke", "channel":"lan:devices", "args":[] }
```

returns `{ devices:[{ deviceId, name, ip, lastSeen, controller }] }`.

```json
{ "t":"invoke", "channel":"lan:revoke", "args":[{ "deviceId":"..." }] }
```

removes the token from the allow list. A revoked token is rejected on the next WebSocket connect.

Controller-gated mutating invokes:

```text
lan:access-decide
lan:revoke
settings:set
config:set
session:save
chat:archive-append
activity:clear
teams:refresh
jira:sync
deploy:auth
job:start
job:cancel
chat:kill-terminal
chat:kill-command
app-chat:steer
app-chat:reset
app-chat:detect-acp
app-chat:new-session
app-chat:close-session
app-chat:set-model
app-chat:set-mode
proc:kill
proc:kill-all
agent-proc:kill
code:unlock
code:lock
chat:permission-decide
draft:save
status:set
clipboard:write
notify
teams:share-link
external:open
review:open
browser:set-active
```

Controller-only events: `chat:permission-request`, and future `notify`/`show` event channels, are
sent only to the current controller's active WebSocket connection(s). Timeout and fallback behavior
remain owned by the ACP manager.

LAN discovery probes can use unauthenticated identity only:

```text
GET /workass/health
```

returns:

```json
{ "app":"workass", "version":"0.0.1-dev", "name":"hostname" }
```

The development default remains `--port 8788`. Production Windows binding is prepared/documented
for `--port 80` because a managed machine is reachable only on port 80. On Windows, `--prod` or
`WORKASS_PROD=1` changes only unspecified defaults to `--port 80 --bind lan`; explicit `--port` or
`--bind` flags still win.

## Service Wrappers

Windows installs alongside the blessed Electron rebuild flow and does not replace or edit it:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts\windows\Install-WorkassService.ps1 -ExePath "C:\Program Files\Workass\workass.exe" -StartNow
```

The wrapper uses `schtasks.exe /SC ONSTART /RU SYSTEM`, not third-party service hosts. Keep the
validated executable name and path stable for endpoint tooling; copy a new binary over the same
path when upgrading.

macOS user launchd install:

```sh
scripts/macos/install-workass-launchd.sh /absolute/path/to/workass
```

The template is `scripts/macos/workass.launchd.plist`; the install script writes
`~/Library/LaunchAgents/com.workass.daemon.plist` and logs to `~/Library/Logs/Workass`.

## Test

```sh
go test ./...
```

## Frontier ACP Adapters

The daemon auto-detects Claude Code and Codex through ACP adapters, not through API keys. Login stays
inside each vendor CLI:

- Claude Code: run `claude` and use `/login`.
- Codex: run `codex login`.

Claude and Codex detection resolves only their explicit provider/env override or the official
`claude` / `codex` executable on PATH. Workass does not search for, download, or launch Zed ACP
compatibility adapters.

The normal build remains hermetic:

```sh
scripts/build-daemon.sh
```

To stage the direct-provider hosts and checksum-pinned official Claude Agent SDK for packaged
builds from the Mac, opt in explicitly:

```sh
scripts/build-daemon.sh --with-frontier-hosts
```

That runs `scripts/vendor-frontier-hosts.sh`, which stages Workass's Claude/Codex native hosts and
the exact official `@anthropic-ai/claude-agent-sdk` package under
`dist-bin/frontier-hosts/darwin-arm64/`. Codex uses the installed official `codex app-server`;
Claude uses the staged SDK with the installed official `claude` executable. Runtime installs never
run `npm install`.

## B1 Soak Harness

The B1 soak harness is a standalone program, intentionally outside the unit-test suite:

```sh
SOAK_MINUTES=3 go run ./scripts/soak
```

Unset `SOAK_MINUTES` to run the default 30-minute gate:

```sh
go run ./scripts/soak
```

It builds a fresh temporary daemon binary from `./cmd/workass` and launches that real process. Set
`SOAK_USE_DIST=1` to force `dist-bin/workass-darwin-arm64`, or `SOAK_DAEMON_PATH=/path/to/workass`
to test a specific binary. The harness uses a private temp state directory, the deterministic mock
provider, raw RFC 6455 WebSocket clients, ten fixture chat workspaces with nested git repos,
`--hibernate-ttl 5s`, `--rss-sample-interval 1s`, and a low compaction threshold.

The scenario mix runs continuously for the configured duration: normal mock turns, slow cancel,
slow steer, local queue flush, permission approve/reject, forced compaction plus a post-compaction
turn, crash-marker recovery plus a post-recovery turn, hibernation/resurrection replay-once checks,
checkpoint/diff/rewind checks, one forked chat turn per cycle, and a second localhost client taking
and returning the controller lease while permission routing is asserted.

The final report enforces the B1 pass criteria: no unexpected `reply.error`, intentional
`lan:not-controller` rejections observed, all cancels succeed, compaction count is at least the
completed cycle count, crash recoveries are counted, replay seeds do not double-apply according to
the mock trace file, daemon RSS at the end is within 25% of the 5-minute baseline for full runs, and
event/reply/proc RSS counts are printed.

## Current Surface

- HTTP static serving from the renderer directory.
- `/` maps to `index.html`.
- `index.html` receives `<script src="/lan-bridge.js"></script>` before `</head>`.
- `/lan-bridge.js` serves the LAN browser bridge.
- WebSocket upgrade and RFC 6455 text framing are handled in Go with no third-party dependencies.
- The invoke/reply/event JSON protocol is active.
- Broadcast events fan out to connected clients.
- `app:meta` returns daemon metadata.
- `state:get` is a frozen compatibility method that returns an empty work queue: `{ "runAt": null, "items": [] }`.
- `app-chat:new-session` launches one mock ACP subprocess per chat key and returns ACP session info.
- `job:start` supports `kind: "app-chat"` only. Other job kinds return `not implemented until P2`.
- ACP `session/update` notifications emit `job:event` payloads for start/data/end, tools, thinking, plan, and usage.
- Assistant and thought chunks are coalesced before broadcast.
- `job:cancel` sends `session/cancel` for app-chat jobs and resolves outstanding permission requests.
- `chat:permission-request` and `chat:permission-decide` implement the ACP permission round-trip. No default deadline: a card waits for the user and is settled by a decision, a cancelled turn, or a closed session. `Options.PermissionTimeout` is opt-in and never expires a question.
- `app-chat:set-model` and `app-chat:set-mode` call mock `session/set_config_option`.
- `app-chat:close-session` closes the ACP session, and `app-chat:reset` closes all mock ACP bridges.
- Access approval, persisted device tokens, the single-controller runtime lease, takeover broadcast, and
  controller-only permission routing are active.
- ACP bridges now track lifecycle states `warm`, `active`, `idle`, and `hibernated`.
- In-flight app-chat prompts mark the bridge `active` and pinned; pinned bridges are never hibernated.
- When a prompt ends, the bridge becomes `idle` and stamps `lastActivity`.
- Idle chat bridges past `--hibernate-ttl` are hibernated by killing the ACP child while retaining the session record and disk transcript. The next `job:start` for that chat creates a fresh ACP session, emits `chat:session-replaced`, and uses the replay-once seed path to restore context.
- The hibernation reaper rechecks `lastActivity` and pinned state under the bridge lock immediately before killing the child, so a concurrent prompt wins and aborts the reap.
- ACP engine RSS is sampled with `ps -o rss= -p <pid>` on macOS/Linux and `tasklist /FI "PID eq N" /FO CSV /NH` on Windows at `--rss-sample-interval`.
- `proc:changed` and `proc:list` include the existing process fields plus additive `state` and `rssKb` fields for ACP engines.
- Engines crossing `--engine-max-age` or `--engine-max-rss-kb` are marked for recycle and hibernate only at the next idle moment.
- `--spare-sessions` keeps isolated pre-warmed ACP sessions ready; spares use `--spare-ttl` and are not treated as idle chats.
- Other non-shell-local wire handlers remain P2 stubs returning documented empty/default result shapes.

Not included in this phase: real Devin engines, headless non-chat jobs, full non-engine process management beyond the existing stubs, and Electron shell-local actions.

## Native OpenAI-Compatible ACP Agent

`cmd/workass-agent` is a standalone ACP stdio server for local OpenAI-compatible
servers such as LM Studio and Ollama. Provider detection auto-registers a
native local provider for each responding server and launches this agent with
`OPENAI_BASE_URL`, `OPENAI_API_KEY=local`, and the first `/v1/models` id as
`OPENAI_MODEL`.

The daemon resolves the agent binary in this order:

1. `WORKASS_AGENT_BIN` override.
2. A sibling of the running daemon executable, including the `dist-bin` names
   emitted by `scripts/build-daemon.sh` such as `workass-agent-darwin-arm64`.
3. `go run ./cmd/workass-agent` only when `WORKASS_AGENT_DEV_GO_RUN=1` and the
   daemon is running from the repository root. This fallback is for Mac dev
   runs only; packaged deploys must ship the sibling agent binary.

If none resolves, the native local provider reports a status error explaining
how to provide `workass-agent`.

Run it directly:

```sh
OPENAI_BASE_URL=http://127.0.0.1:1234/v1 \
OPENAI_API_KEY=lm-studio \
OPENAI_MODEL=workass-dev \
go run ./cmd/workass-agent
```

The command also accepts `serve-acp` as a no-op subcommand for custom-provider
configs:

```sh
go run ./cmd/workass-agent serve-acp
```

Probe mode writes only to stderr and exits:

```sh
OPENAI_BASE_URL=http://127.0.0.1:1234/v1 OPENAI_MODEL=workass-dev go run ./cmd/workass-agent --probe
```

ACP stdout remains JSON-RPC only. Diagnostics, probe JSON, and HTTP/model
catalog errors go to stderr.
