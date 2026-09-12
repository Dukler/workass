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
| `omp` | `omp acp` | User-owned Oh My Pi profile over standard ACP |
| `custom` | `acp.command` plus `acp.args` | Future Go agent or another ACP server |

Selecting another provider chooses another lane inside the same Workass chat.
Returning to a provider resumes that lane's exact native thread. Cross-provider
history enters a provider lane that has never consumed input once, as a bounded
semantic seed immediately before its first real user request. After that first
input, later cross-provider gaps move only through the versioned, non-sampling
context-import contract; missing import support blocks the switch rather than
replaying or replacing an established thread.

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
  `userMessage.clientId` as both stronger "applied" feedback and the semantic
  transcript boundary. Only a genuinely unresolved live row remains in the
  composer-adjacent Steering tray. The provider acknowledgement moves that same
  row into the transcript immediately and exactly once; while consumption is
  pending, only its reserved assistant continuation stays hidden. The receipt
  reveals that continuation after the already-visible row without moving or
  copying it. A terminal turn settles any still-unresolved owner out of the live
  tray and never replays it. The provider boundary still commits after the
  current sampling step, immediately before the next model/tool step. A cached turn-id
  mismatch is resynchronized and retried once, matching the official Codex TUI.
  An already-finished or non-steerable turn rejects the explicit direction back
  to the composer without interrupting the active Workass turn or creating FIFO
  work. Transport-uncertain input keeps its single durable owner and is never
  replayed automatically.
- The native Claude host advertises its live steering receipt extension and
  injects the direction into the running SDK query. Workass waits for the
  accepted prompt UUID; a definite rejection returns the direction to the
  composer, while a timeout remains uncertain and is never resent.
- Mock/custom ACP agents retain the `_session/steer` extension when advertised;
  agents without that capability reject explicit steering back to the composer.
  Only a separate ordinary queue intent creates durable FIFO work.

The official Devin CLI bundle `3000.10.21` was probed in isolation on 2026-09-11.
Its `initialize` response advertises neither `sessionSteer` nor
`steerNotification`; live steering is therefore unavailable through that ACP
contract. A second `session/prompt` or cancel-and-restart is not a live-steer
implementation. This is independent of exact-session attachment failures.

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

## Codex speed controls

The native host projects Fast only when the selected model's official catalog
advertises it. Modern `serviceTiers` supplies the native request id; the older
`additionalSpeedTiers` field is used only when modern tier metadata is absent.
An explicitly empty modern catalog never inherits a legacy Fast flag.

The composer remembers Standard/Fast per chat, provider, and base model, separately
from reasoning effort and permissions. Submission freezes that choice in the
durable turn input, including FIFO work. The host sends the official
`turn/start.serviceTierForTurn` override; Standard explicitly sends `default`.
Changing a control does not steer or restart a running turn, and provider rejection
never silently downgrades Fast. The UI notes Fast's higher usage without hardcoding
a multiplier. See [OpenAI speed documentation](https://learn.chatgpt.com/docs/agent-configuration/speed).

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

## Provider terminal ownership

The ACP harness owns turn completion. Workass sends `session/prompt` once and
uses that request's ordinary events and result; it has no terminal polling,
custom turn-readback method, bridge-recycle watchdog, or prompt replay. If the
transport closes first, Workass ends the visible row as interrupted, keeps any
partial output, and retains the exact native session id for the next distinct
prompt to resume. Explicit Stop is forwarded once and is not replayed after a
daemon restart.

### Codex upstream WebSocket disconnects

The native host uses stdio JSON-RPC to the installed official app-server.
`responseStreamDisconnected` with `websocket closed by server before
response.completed` describes Codex's upstream Responses connection, not the
Workass renderer socket. Native `error.willRetry` notices leave terminal
authority with `turn/completed`; a failed completion preserves the native cause
and partial output. Workass does not replay a prompt, poll the turn, or replace
its exact thread to recover it.

On official Codex 0.154.0, `codex features list` reports
`responses_websockets` and `responses_websockets_v2` as removed. The documented
`model_providers.<id>.supports_websockets` setting applies to configurable
providers; overriding the reserved built-in `openai` provider is rejected by
this installation. Do not ship either as a built-in transport workaround or
substitute a custom provider to bypass the rejection. See the
[official configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).
`codex doctor --json` provides vendor-owned, redacted HTTP/WebSocket diagnostics
without sending a model prompt. A successful handshake does not prove a long
response stream will complete; no supported built-in transport override has
been verified for this version.

The direct-host regression includes successful native recovery after retry
notices, both terminal error payloads and a native
terminal notification whose cause was supplied by the preceding `error` event:
`node --test --test-name-pattern='WebSocket disconnect' scripts/tests/codex-native-host.test.mjs`.
Its fixture records only RPC method names and an exact-thread boolean, proving
two distinct user requests cause exactly two native turn admissions and no
replacement thread, terminal polling, or failed-input replay.

### Large Codex thread resume

The host requests `thread/resume` with `excludeTurns: true`: Workass already
owns display history and does not consume native `thread.turns` in the reply.
The installed 0.154.0 schema supports this field; it omits returned turn items
while Codex still loads the exact saved thread's inference context. It does not
truncate that context, compact it, or reduce the next inference request's size.
See the [official 0.154.0 resume regression](https://github.com/openai/codex/blob/6b9826e3aa83b1a5947db50f4332cb9c65f1b340/codex-rs/app-server/tests/suite/v2/thread_resume.rs#L2446).

`node --test --test-name-pattern='large exact resume' scripts/tests/codex-native-host.test.mjs`
uses 512 synthetic historical turns (over 4 MiB), verifies a small metadata
reply across two exact attachments, retained native history, and one intact
current input with no history override, readback, or replacement. This removes
unnecessary resume serialization and parsing. It is not evidence that large
history caused `responseStreamDisconnected`, nor a fix for that socket close.
The native error identifies a Responses close before completion; it does not
identify why the peer closed or establish an upstream service defect.

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
- `[mock:lost-terminal]` streams a complete response and then leaves the original `session/prompt` pending until explicit cancellation, proving Workass does not invent completion or poll a second terminal source.
- `[mock:lost-terminal-unreleased]` is the same permanently pending harness boundary under the older fixture name; Workass neither recycles the bridge nor resends the prompt.
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

## Oh My Pi

Install the user-owned OMP CLI with one of its official installers. On macOS with Homebrew:

```sh
brew install can1357/tap/omp
```

OMP owns its profile, provider credentials, model catalog, sessions, rules, and extensions. Workass
only detects the executable and launches its standard ACP entry point:

```sh
node desktop/scripts/probe-acp.mjs omp acp
```

On Windows, detection covers OMP's native `%LOCALAPPDATA%\omp\omp.exe` install plus the common
Bun and npm global-bin locations. When the resolved executable is an `omp.cmd` shim, Workass runs
that shim through the hidden managed `cmd.exe` boundary; it never asks `CreateProcess` to execute
the text launcher directly.

Expected result: ACP protocol version `1` and agent name `oh-my-pi`. If OMP has no usable model,
run `omp` and use `/login`; Workass never reads or stores OMP credentials.

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

For cancellation tests, `WORKASS_MOCK_ACP_CONTROL_GATE=/absolute/fixture/path`
makes `session/set_config_option` create that marker and wait for
`/absolute/fixture/path.release` before replying. This lets tests prove Stop
finishes pre-prompt preparation without waiting for a blocked control response.

- Never write logs or diagnostics to ACP stdout; stdout must contain JSON-RPC messages only.
- Send diagnostics to stderr.
- Keep `shell: false` for normal executables. Workass only enables a shell for Windows npm `.cmd`
  shims such as `qwen.cmd`.
- Keep secrets inside `acp.env` or environment variables. Workass redacts secret-looking config
  keys when configuration is exposed to clients or agents.
- Do not treat a successful handshake as a successful inference test. Detection and prompting are
  intentionally separate checks.

## Codex native goals

The native host advertises `/goal` through the attached lane's command catalog
when the official app-server reports the enabled `goals` feature. Workass enables
that feature for its child process without editing the user's Codex config.

- `/goal <objective>` creates a native persistent goal and starts it.
- `/goal` reads its native status, objective, usage, and budget, if present.
- `/goal pause` stops automatic continuation after the current native turn.
- `/goal resume` resumes the existing objective in the exact native thread.
- `/goal clear` removes the objective. Clear an unfinished goal before replacing it.
- Workass Stop pauses the native goal and interrupts its current turn.

While running, send inspect/pause/clear through the normal live-steer surface.
Starting or resuming a goal requires an idle foreground. Commands accept text;
attachments must first be sent as an ordinary message. State remains owned by
Codex and is read again on exact resume; Workass does not restart goals after a
transport failure or maintain a second continuation scheduler.

Before `thread/goal/set` can start inference, the host uses
`thread/settings/update` for the selected model, effort, service tier, and
permissions, then `thread/inject_items` for that input's actor-authored context.
Inspect/clear inputs also preserve their context without sampling. Explicit
command-intent metadata keeps seeded or quoted text from becoming a command.
One Workass prompt remains owned across native continuation turns until the goal
stops and its current turn completes. Native goal notifications update the
existing tool card; the renderer does not interpret vendor goal state.

Deterministic coverage: `node --test scripts/tests/codex-native-host.test.mjs`
and `go test -race ./cmd/workass -run TestCodexNativeGoal`.

## Native subagent observation

Codex owns spawning, steering, stopping, and completing its native child agents.
Workass passively projects native collaboration/child-thread events into the
same expandable subagent row used for Workass-managed agents. Live and completed
rows retain provider branding, observed model, elapsed time, calls, and available
results; a durable child record overrides foreground settlement without rewriting
transcript events. Explicit tool identity joins the two observations, avoiding a
second generic background row. Earlier live children remain visible after the
parent turn ends; historical results remain inspectable in the background fold.

Native agent rows are read-only. They have no Workass Stop button, and the daemon
rejects a native-agent stop even when a record includes PID/output metadata.
Only Workass-managed subagent records use Workass's subagent cancellation path.
Inspecting a row does not spawn, steer, resume, or interrupt any native agent.

## Codex private runtime diagnostics

The native host emits a private `session/update` only while an admitted prompt
has a nonempty Workass client input id:

```json
{"sessionUpdate":"_workass_diagnostic","schemaVersion":1,"clientUserMessageId":"<current Workass input id>","event":{"kind":"turn","phase":"started"}}
```

This is diagnostic metadata for the daemon's private storage/read boundary.
The frozen renderer protocol and native terminal, cancellation, context, and
checkpoint authority remain unchanged. No native ids, paths, URLs, transcript
text, error messages, or additional error details enter `event`.

- `input`: `inputBytes` is the UTF-8 JSON size of the current native input array;
  `textBytes` counts its text, `imageCount` counts images, and `imageDataBytes`
  counts encoded base64 payload bytes without data URL prefixes. Goal commands
  measure the current native injected message content array. These are current
  input measurements, **not the total upstream model request**. `resumed`,
  `resumeReplyBytes`, and `resumeElapsedMs` describe this host attachment's
  `thread/start` or exact `thread/resume` reply. Reply bytes include the actual
  received JSON-RPC envelope, excluding its line terminator; elapsed time uses
  a monotonic clock and rounded milliseconds. An optional request observer
  records these metrics synchronously and cannot fail the RPC. `historyMode`
  accepts native `paginated`/`legacy`, otherwise `unknown`. `hostInstanceId` is
  one fresh random UUID per host process, with no vendor identity. Observed
  `effort` and resolved native `serviceTier` use only their declared enums.
- `usage`: only observed safe nonnegative integer `used`, `size`, `input`,
  `cachedInput`, `output`, and `reasoningOutput` counts are included. The next
  prompt may receive its last exact-turn observation with `prior:true`.
  Missing or malformed counts are omitted, never coerced into zero.
- `error`: `willRetry`, neutral `category` and `reason`, and
  `closeDetailsAvailable:false`. Numeric `httpStatus` is included only when
  native error metadata supplies a valid status; `retryAttempt`/`retryLimit`
  are parsed only from an explicit native reconnect/retry count. There are no
  invented close codes or upstream request sizes. Retry notices retain native
  success/failure authority. Socket reason classification examines the native
  message and additional details transiently; only the neutral enum is retained.
- `fallback`: `transport:"https", scope:"thread"` only after an explicit native
  fallback warning targeting the exact thread during an active Workass prompt.
  Both `HTTP(S)` and `HTTPS` transport spellings are recognized. Official
  `WarningNotification` has a message and optional thread id, **no turn id**:
  this records a thread-scoped observation, not native turn attribution. A
  delayed same-thread warning cannot be distinguished from a current warning.
  Idle, wrong-thread, and untargeted warnings are discarded. Retry counts never
  imply a fallback.
- `compaction`: `phase:"started"|"completed"` from current native
  `contextCompaction` item events or legacy `thread/compacted`. Duplicate item
  completions and the modern-completion/legacy-checkpoint pair are suppressed;
  the existing semantic checkpoint is still emitted independently. The installed
  deprecated `ContextCompactedNotification` schema requires a turn id; if an
  older producer omits it, no compaction diagnostic is emitted or turn guessed.
- `turn`: native `started`, `completed`, `failed`, or `interrupted`. Native
  goal continuations retain the same owning Workass input id.

Turn-scoped diagnostics have their own exact thread/turn fence. Before a normal start reply
establishes its turn id, only bounded sanitized metadata is buffered; mismatched
entries are discarded. Retired, missing-turn, wrong-thread, child-thread, and
idle notifications cannot populate the next input's turn diagnostics or prior
usage. Thread-scoped fallback observations follow the explicit exception above.
The input consumption receipt stays deduplicated while its client id remains
available until prompt settlement. Resume history produces no diagnostics and
is never fetched or replayed for this feature.

`workass_get_chat_diagnostics` reads these observations for an exact tab/chat
pair, including daemon-side startup timing, prepared input sizes, and separate
host transport failures. It returns at most 20 turns from a global retention
limit of 256. Each turn keeps at most 32 chronological events, counters, and the
latest usage snapshot. All payloads pass a fixed field/type allowlist; prompt
contents, native ids, and raw errors are excluded.

The manager persists bounded snapshots in `turn-diagnostics.json` under its
state directory. The canonical file and its single pending staging file are
each limited to 4 MiB. A manager-owned writer coalesces failure checkpoints with
a trailing flush, at most once per second; turn completion and shutdown force
a flush. Provider reads and diagnostic reads never wait for disk I/O. Storage
failure is reported in diagnostic persistence status without failing the turn.
After restart, retained observations are historical and inactive; an unfinished
checkpoint does not become a fabricated completion or a resumed live job.
These measurements cannot expose the full upstream request or WebSocket close
details that the native client does not report.

Verification: `node --test scripts/tests/codex-native-host.test.mjs` covers
retries that recover or exhaust, adversarial error details, malformed usage,
actual resume envelope bytes, modern and legacy compaction, exact-turn event
ordering, thread-only fallback warnings, missing-turn legacy compaction, goal
continuation, cancellation, and the existing native behaviors.
