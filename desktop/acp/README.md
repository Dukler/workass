# Workass ACP Development

This directory contains the local ACP development infrastructure for Workass. It gives agents two
separate test paths:

1. A deterministic mock ACP server for protocol and UI development.
2. A tiny local model behind Qwen Code for real ACP plus inference smoke tests.

Do not use model quality as the test oracle. The mock is the source of truth for ACP event handling;
the local model only proves that the complete external process and inference path is connected.

Mock and fixed smoke-test models are dev/test-only. Production does not
advertise or accept them through model or tracked-subagent surfaces, and hides
fixture-only receipts from the transcript and Turnos rail.

## Architecture

Workass keeps one durable provider-neutral actor per immutable chat. A chat may
own several provider lanes; each lane retains one exact provider-native thread,
while its ACP/native-host process attachment is disposable. Ordinary ACP agents
speak newline-delimited JSON-RPC on stdin/stdout. Claude and Codex instead use
Workass-owned native hosts so their official transports satisfy the same lane
contract without leaking vendor behavior into chat code.

The launcher in `desktop/main.js` supports these providers through `app-config.json`:

| Provider | Command | Purpose |
|---|---|---|
| `devin` | Existing Devin ACP command | Production/default behavior |
| `mock` | `node desktop/acp/mock-server.mjs` | Deterministic development |
| `qwen` | `qwen --acp` | Real local inference canary |
| `claude` | Official Agent SDK + installed `claude` | Native Claude Code session |
| `codex` | Installed `codex app-server` | Native Codex session |
| `custom` | `acp.command` plus `acp.args` | Future Go agent or another ACP server |

Selecting another provider chooses another lane inside the same Workass chat.
Returning to a provider resumes that lane's exact native thread. Cross-provider
history moves only through the versioned, non-sampling context-import contract;
missing import support blocks the switch rather than replaying the transcript.

## Deterministic Mock

Select the mock provider:

```json
{
  "acp": {
    "provider": "mock"
  }
}
```

The mock supports:

- ACP `initialize`
- `session/new`, `session/prompt`, `session/set_config_option`, and `session/close`
- deterministic exact `session/resume` and same-id `session/load` when
  `WORKASS_MOCK_ACP_SESSION_STORE` points to a durable fixture file; select
  `resume`, `load`, `both`, or `none` with
  `WORKASS_MOCK_ACP_SESSION_CAPABILITY`
- plan updates
- thought chunks
- tool-call start and completion updates
- streamed assistant message chunks
- usage updates
- `session/cancel`

## Provider steering semantics

Workass deliberately does not model steering as one universal cancel action:

- The native Codex host advertises `_meta.workassCodexSteerRequest` and accepts the
  `_workass/codex/steer` request. The host calls the
  official app-server `turn/steer` RPC; Workass only reports success after the
  app-server returns the active `turnId`, and correlates the later canonical
  `userMessage.clientId` as both stronger "applied" feedback and the transcript
  boundary: the pending preview is committed after the current sampling step,
  immediately before the next model/tool step, so click time never cuts a
  streaming sentence in half. A cached turn-id
  mismatch is resynchronized and retried once, matching the official Codex TUI;
  an already-finished turn becomes the immediate persisted follow-up, while
  review/manual-compaction rejection waits in FIFO without cancelling that
  operation. Transport-uncertain input keeps its single durable owner and is
  never replayed automatically. A rejecting native host falls back to
  interrupt + immediate persisted follow-up, never a detached prompt.
- The native Claude host advertises its steering receipt extension. Workass
  persists the follow-up first, interrupts the active turn, and drains that FIFO
  as soon as cancellation completes; it does not wait for natural completion.
- Mock/custom ACP agents retain the `_session/steer` extension when advertised;
  agents without any steering capability use the same durable FIFO fallback.

Real native-provider sessions are canaries for these protocol shapes only. The
deterministic mock and direct-host fixtures remain the correctness oracle.

## Tracked subagent permission attention

Tracked subagents expose permission waits as a latched
`phase: "waiting_permission"` event with `needsAttention: true` and a bounded,
redacted activity summary. Both single-child and group wait tool calls are
forcibly completed with that model-visible notification. A quick decision does
not erase an unread event, and unresolved permission remains attention-worthy
on subsequent waits. The current Workass controller still owns the decision.

## Provider context compaction

Every provider owns context compaction inside its exact native thread. Workass
never sends a summary prompt, closes the session to create another thread,
loads another thread, replays transcript text, or emits a synthetic zero-usage
reset. Native usage and compaction events are authoritative, and manual
`/compact` continues to route to the selected provider. A provider without
verified in-place compaction reaches a visible context-limit state; it does not
receive a Workass fallback.

Provider-authored context `used/size` readings are separate from compaction and
subscription plan limits. Workass stores the latest reading per exact
tab/chat/provider in daemon-owned session state, rehydrates it without a model
turn after reconnect/restart, and reflects it in the existing composer context
ring. Exact percentages and token counts remain in the ring's popover; no
permanent text or numeric badge is added beside it. Switching providers selects
that provider's last known reading; it never relabels the previous provider's
context as the new model's.

## Provider plan-limit extensions

The stable Workass bridge contract does not standardize subscription-window
utilization, so the native frontier hosts expose narrow, version-gated requests:

- Codex advertises `_meta.workassCodexRateLimitsRequest`; Workass calls
  `_workass/codex/rate-limits`, which delegates to the official app-server
  `account/rateLimits/read` RPC and returns primary/secondary utilization,
  reset timestamps, window durations, and any earned reset-credit snapshot.
- Codex separately advertises `_meta.workassCodexRateLimitResetRequest`;
  controller action `_workass/codex/rate-limit-reset/consume` delegates to
  official `account/rateLimitResetCredit/consume`, preserving the caller's
  idempotency key. The host immediately refetches `account/rateLimits/read`
  and returns that snapshot with the redemption outcome. Workass never spends
  an earned reset automatically.
- Claude advertises `_meta.workassClaudeUsageRequest`; Workass calls
  `_workass/claude/usage` for an existing session, which delegates to the
  installed Agent SDK structured usage control and returns five-hour, weekly,
  model-scoped utilization and reset timestamps.

`scripts/vendor-frontier-hosts.sh` checksum-pins the official Claude Agent SDK;
Codex uses the user's installed official app-server directly. Workass never
reads vendor OAuth files, never sends credentials through its bridge, and treats
either extension as optional: failure keeps the last transient snapshot and
does not block session startup or prompting.

## Provider terminal reconciliation

The native Codex host advertises `_meta.workassTurnReconcileRequest` and
implements `_workass/turn/reconcile`. After a quiet interval during an
outstanding `session/prompt`, Workass asks the host to read the authoritative
Codex thread. A completed/interrupted native turn reconciles a lost
`turn/completed` notification and releases the original ACP prompt. Silence by
itself never completes a turn; an explicitly active native turn keeps running.
Repeated liveness failures or a terminal turn whose ACP prompt still cannot
return recycle the wedged bridge, so the UI cannot remain in AI-turn mode
forever.

## Tool-result images

ACP tool updates may return structured raster image blocks alongside text.
Workass preserves bounded inline PNG/JPEG/WebP/GIF data on the tool timeline
event and renders the images below the folded tool row with click-to-zoom.
Remote image URLs and SVG are not accepted. Durable session/archive state owns
the bytes; agent chat-control reads return image metadata only.

Special prompt markers:

- `[mock:slow]` slows the turn so cancellation can be tested reliably.
- `[mock:steer]` is used with `[mock:slow]` in steer tests. The mock advertises
  `_meta.sessionSteer` / `_meta.steerNotification`, accepts `_session/steer`
  notifications during a running turn, and appends `Steer input: ...` to the
  deterministic assistant output.
- `[mock:error]` returns ACP error `-32001` deterministically.
- `[mock:permission]` emits a deterministic `session/request_permission` with `allow-once` and `reject` options, waits for the client response, and streams the selected/cancelled outcome in the assistant text.
- `[mock:tool-image]` returns a valid tiny PNG beside the deterministic tool's text result, proving that structured tool media survives the bridge, durable mirror, folded tool row, and lightbox path.
- `[mock:assistant-image]` writes a valid tiny PNG inside the mock session cwd and returns the ordinary `[Open](path)` plus `![Preview](path)` Markdown used naturally by ACP agents. It proves the provider-neutral terminal importer, durable assistant attachment, collapse of the redundant Open link into the clickable image, and reload path without teaching the fixture a Workass-specific media syntax.
- `[mock:bigusage]` completes normally but reports `used:85, size:100` so the
  visible context-limit path can be tested without mutating or replacing the
  provider lane.
- `[mock:burst]` emits 4,096 deterministic 128-byte answer chunks with a zero-delay event-loop yield between them. It stress-tests ACP ingestion, 16 ms daemon coalescing, WebSocket delivery, and renderer streaming without using model quality as an oracle. Override the volume with `WORKASS_MOCK_ACP_BURST_CHUNKS` and `WORKASS_MOCK_ACP_BURST_CHUNK_BYTES`.
- `[mock:phases]` emits one `commentary` assistant chunk followed by one
  `final_answer` chunk through the provider-neutral
  `_meta.workassAssistantPhase`. It proves that Workass preserves a provider's
  explicitly typed final result through coalescing, persistence, archive
  recovery, and ordinary assistant-Markdown rendering without dedicated result
  chrome, a provider/model allowlist, or guesses from
  terminal results, headings, or prose. Providers that omit this metadata keep
  the ordinary single assistant-content path. Codex's native
  `_meta.codex.phase` remains supported for compatibility.
- `[mock:spawned-work]` emits a Claude-shaped background Bash tool/result,
  typed task start + live-set events, writes a real temp output file, then emits
  a terminal notification. It deterministically proves passive discovery,
  per-chat state, bounded tail reads, and durable completion receipts without a
  model turn. Its fallback result uses Claude's real sentence shape (`ID: task.`)
  so trailing punctuation cannot create a phantom task. `[mock:spawned-work-running]` keeps the output file descriptor
  open and omits the terminal notification so PID/output-file reconciliation
  remains the only running-state authority.
- `[mock:crash]` exits the mock process mid-turn after a thought update, exercising daemon crash recovery.
- `[mock:lost-terminal]` streams a complete response, settles its tool/plan, and withholds the original `session/prompt` response until `_workass/turn/reconcile` confirms it.
- `[mock:lost-terminal-unreleased]` reports a terminal provider turn but deliberately refuses to release the ACP prompt, exercising the bounded bridge-recycle fallback.
- `[mock:active-without-terminal]` goes quiet while still reporting an authoritative active turn, proving that silence is never guessed to mean completion.

Run the handshake probe from the repository root:

```sh
node desktop/scripts/probe-acp.mjs node desktop/acp/mock-server.mjs
```

Or from `desktop/`:

```sh
npm run acp:probe:mock
```

Expected result: `ok: true`, protocol version `1`, and agent name `Workass Mock ACP`.

## Real Local Canary

The tested lightweight model is Qwen3.5-2B MLX 4-bit. It is deliberately small and fast. It is
adequate for proving ACP negotiation, authentication, session creation, streaming, and local
inference, but it is not reliable enough to validate agent decisions or tool selection.

Install and load it in LM Studio:

```sh
lms get qwen/qwen3.5-2b --mlx -y
lms server start
lms load qwen/qwen3.5-2b -c 32768 --gpu max --identifier workass-dev -y
```

The 32K context is required because Qwen Code's built-in agent instructions do not fit in 8K.

Install Qwen Code outside the project:

```sh
npm install -g @qwen-code/qwen-code@latest
```

Configure Workass:

```json
{
  "acp": {
    "provider": "qwen",
    "env": {
      "OPENAI_BASE_URL": "http://127.0.0.1:1234/v1",
      "OPENAI_API_KEY": "lm-studio",
      "OPENAI_MODEL": "workass-dev"
    }
  }
}
```

Probe Qwen Code directly:

```sh
OPENAI_BASE_URL=http://127.0.0.1:1234/v1 \
OPENAI_API_KEY=lm-studio \
OPENAI_MODEL=workass-dev \
node desktop/scripts/probe-acp.mjs qwen --acp
```

The verified setup used Qwen Code `0.19.8`, negotiated ACP version `1`, and completed a real
prompt with `stopReason: end_turn`. A file-tool test was not reliable with the 2B model; that is an
expected model limitation, not an ACP transport failure.

## ACP Detection

The reusable probe is `desktop/acp/probe.js`. It launches a candidate, sends `initialize`, records
latency and capabilities, and terminates the candidate.

Workass exposes detection through both IPC and its file-based Agent API:

```json
{ "type": "detect-acp" }
```

With no target, it probes the mock, Qwen Code, and Devin. A specific provider or command can be
tested instead:

```json
{ "type": "detect-acp", "provider": "mock" }
```

```json
{
  "type": "detect-acp",
  "command": "/path/to/acp-agent",
  "args": ["--acp"],
  "timeoutMs": 5000
}
```

Environment overrides are available for isolated launches:

- `ASSISTANT_ACP_PROVIDER`
- `ASSISTANT_ACP_COMMAND`
- `ASSISTANT_ACP_ARGS` as a JSON array
- `ASSISTANT_ACP_API_KEY`
- `ASSISTANT_ACP_PROTOCOL_VERSION`

## Launching Workass

### macOS development

The original Workass launch and build scripts are Windows-specific because the other environment
cannot download Electron from npm. Do not replace or modify that flow for macOS development.

On macOS, Workass stages the exact Electron version pinned in
`config/macos/electron.version` under `.dev/runtime/electron/`. It is downloaded
from Electron's official release, verified against the checked-in SHA-256, and
does not modify `desktop/package.json` or the Windows build:

```sh
scripts/vendor-electron-runtime.sh
```

The isolated development profile is then launched with:

```sh
desktop/scripts/dev-launch-macos.sh
```

The launcher uses isolated state under `.dev/profiles/default`, starts the dev
daemon on `127.0.0.1:18788` when needed, leaves project dependencies unchanged,
and serves the renderer at:

```text
http://localhost:8799/
```

Production is a separate `/Applications/Workass.app` process on renderer port
8798 with state under `~/Library/Application Support/Workass`; it never occupies
the development renderer port or development data root. See
`docs/ENVIRONMENTS.md`.

### Windows development and production

The portable Windows package is staged on the Mac build host; Windows only
extracts and launches the finished tree:

```sh
scripts/stage-windows-portable.sh --version X.Y.Z
```

Launch `Workass.exe` from the extracted directory. It finds
`workass-daemon.exe` beside itself, starts it with `--headless` if the daemon
health endpoint is unavailable, and connects to an already-running daemon when
one exists. The package includes the pinned Electron runtime, renderer,
portable Node, and native provider hosts, so Windows does not run npm.

The endpoint-specific scripts remain available only for existing workflows:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File desktop\scripts\Dev-Launch.ps1
```

Only use the existing blessed rebuild script for packaged production:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File desktop\scripts\Rebuild-Relaunch.ps1
```

Do not add Electron to `desktop/package.json`; the restricted Windows environment depends on its
vendored runtime. For daemon-only operation, use
`workass-daemon.exe --prod --headless --install-service`; this installs a
per-user Scheduled Task on Windows and a user LaunchAgent on macOS.

## Future Go Split

The Go implementation should enter through the existing `custom` provider instead of changing the
Electron ACP client first:

```json
{
  "acp": {
    "provider": "custom",
    "command": "/path/to/workass-agent",
    "args": ["serve-acp"],
    "env": {
      "LMSTUDIO_BASE_URL": "http://127.0.0.1:1234/v1",
      "LMSTUDIO_MODEL": "workass-dev"
    }
  }
}
```

Recommended boundaries:

- `cmd/workass-agent`: ACP server executable using stdio NDJSON
- `internal/acp`: protocol types, JSON-RPC routing, sessions, and capabilities
- `internal/lmstudio`: OpenAI-compatible inference client
- `internal/agent`: turn loop and tool orchestration

ACP currently has no official Go SDK. Generate or maintain Go types from the official ACP JSON
schemas and keep transport tests against `mock-server.mjs` until the Go fixture can replace it.

## Safety Notes

- Never write logs or diagnostics to ACP stdout; stdout must contain JSON-RPC messages only.
- Send diagnostics to stderr.
- Keep `shell: false` for normal executables. Workass only enables a shell for Windows npm `.cmd`
  shims such as `qwen.cmd`.
- Keep secrets inside `acp.env` or environment variables. Workass redacts secret-looking config
  keys when configuration is exposed to clients or agents.
- Do not treat a successful handshake as a successful inference test. Detection and prompting are
  intentionally separate checks.
