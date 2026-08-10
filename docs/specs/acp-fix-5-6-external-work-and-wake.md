# Fix 5+6 — tracked external lanes (wake-on-settle removed)

Status: Fix 6 remains built. Fix 5 was superseded by user law 2026-08-10.
Scope: Go daemon (`internal/acp`,
`cmd/workass`) + one new shell script. NO renderer changes (external kind
renders via the existing generic fallback in `SpawnedWorkCard.tsx`).

## Problem (receipts in chat 2026-07-18)

A. **Untracked spawns.** Detached kill-isolated lanes (setsid/start_new_session
   double-forks) emit no Claude task wire events. Only the seconds-long
   *launcher* Bash is tracked; the hour-long lane is invisible to the registry,
   the rail, receipts, and lifecycle decisions.
B. **Historical behavior, now removed.** Workass briefly woke an idle chat on
   completion by injecting a synthetic user message. The user rejected that
   behavior. Terminal records now remain internal receipts until explicitly
   read.

## Binding laws (restate: executors do not inherit memory)

- Wire contract `invoke/reply/event` is FROZEN. Only additive JSON fields.
- One engine per chat; serialized session/prompt per bridge; stdout purity.
- Secrets: `RedactSensitiveText` before any tail/summary is persisted or
  emitted. Never persist raw prompts or native session ids in receipts.
- Identifier law: never rename existing fields/functions; add.
- Dev daemon only; prod untouched. Blessed rebuild scripts only.
- Receipts: every claim in the lane log needs command + output.

## Fix 6 — external work registration

### New registry kind: `external`

External records live in the same per-chat registry, persist in the same
snapshots, produce the same receipts, and emit the same
`spawned-work:changed` events. `maxSpawnedWorkPerChat` etc. apply.

### MCP tools (cmd/workass/agent_mcp.go) + HTTP methods (agent_control.go)

Both surfaces share semantics, mirroring `agent.spawn`'s owner model
(`ValidateAgentOwner`; same "no running turn" error text style).

1. `workass_register_external_work` / `external.register`
   - params: `label` (required, ≤240 after compactText), `pid` (optional
     int > 1), `output_file` (optional absolute path), `done_file`
     (optional absolute path), `tab_id`+`chat_id` (optional pair; default =
     caller's bound chat; if given must pass the same owner authorization
     used by chat-control tools).
   - If `output_file` omitted: daemon designates
     `<state>/external-work/<workId>.output` (dir 0700, file created empty
     0600) and returns it. If `done_file` omitted it defaults to
     `<output_file>.done` (whether designated or caller-provided).
   - Returns `{ok, workId, taskId, outputFile, doneFile}`. `taskId` ==
     `workId`, prefixed `xw` + random suffix via the existing id helper style.
   - Creates a running registry record: kind `external`, ToolCallID empty,
     SessionID empty. Provider gate: same
     `acceptsClaudeSpawnedWorkProvider` rule as observation (claude, or mock
     outside production).
   - Path validation for CALLER-PROVIDED paths (new `validateExternalWorkPath`,
     do NOT touch `validateClaudeTaskOutputPath`): absolute, `filepath.Clean`
     equal to itself, under one of: `os.TempDir()`, `/tmp`, `/private/tmp`,
     or `<state>/external-work`. Reject symlinks at read time (Lstat, regular
     file). Reject if basename contains path separators post-clean (defense
     in depth).

2. `workass_settle_external_work` / `external.settle`
   - params: `work_id` (required), `status` (`exited`|`failed`, required),
     `exit_code` (optional int), `summary` (optional, ≤1000 compactText,
     redacted). Owner-validated the same way; only settles records of kind
     `external` belonging to the authorized chat; idempotent (settling a
     settled record returns `{ok:true, already:true}` without mutation).

3. `workass_list_spawned_work` / `spawned_work.list` gains NOTHING — external
   items appear there automatically. Do not add parameters.

### Probe integration (reconcileSpawnedWork)

External records get their own settle rules, evaluated in this order each
reconcile tick:

1. `done_file` exists → read ≤64 bytes; first line parsed as int = exit code
   (parse failure → nil). Settle `exited` (code 0/nil) or `failed`
   (code != 0). Summary: `"Done marker written (exit N)"`.
2. Else if `pid` was provided and the process is gone (new
   `externalPIDAlive(pid int) bool` in `spawned_work_probe_unix.go`:
   `syscall.Kill(pid, 0)`; ESRCH → dead; EPERM → alive; windows stub in
   `spawned_work_probe_other.go` returns true=unknown → never settles by
   pid there) → start/continue a missing grace of 2 consecutive ticks
   (reuse `MissingSince` + a distinct external grace constant
   `externalWorkMissingGrace = 10 * time.Second`) → settle `exited`,
   summary `"Process exited without a done marker"`.
3. Else keep running. Records with neither pid nor files never auto-settle;
   they are settled by `external.settle` or remain until the registry cap
   evicts them — acceptable, documented.

Receipt tails for external records read from `output_file` via a new
`readExternalWorkTail` using the SAME byte caps as `readSpawnedWorkTail`, the
new path validator, and `RedactSensitiveText` applied to the tail before it
is persisted or returned anywhere.

### Lifecycle interaction (critical, contrast with fix 1)

- Running `external` records DO NOT pin hibernation:
  `hasRunningSpawnedWorkForChat` gains a kind filter excluding `external`
  (in-process kinds bash/agent/workflow/background keep pinning exactly as
  fix 1 shipped). Rationale: external lanes survive engine death by
  construction; their durable receipt and explicit status card close the loop.
- `orphanInProcessSpawnedWorkForChat` remains workflow/agent only — external
  records are never orphaned by engine exit.
- `touchSpawnedWorkBridgeActivity` on external changes is fine (harmless
  activity refresh on register/settle).

## Fix 5 — removed: no automatic wake or synthetic message

Settling external work, observed background work, or a tracked subagent writes
its bounded durable receipt and emits the ordinary spawned-work state update.
It does not enqueue text, start or resume a chat turn, or retain a pending wake
flag. Older snapshot `wake` fields are ignored during JSON decoding. The
background-work card and owner-authenticated receipt tools remain the only
completion surfaces.

## Wrapper script (new): `scripts/spawn-tracked-lane.sh`

POSIX sh, macOS-first. Usage:

```
scripts/spawn-tracked-lane.sh --label "Lane G rebuild" [--output FILE] -- cmd args...
```

- Designates `--output` default under `${TMPDIR:-/tmp}/workass-lanes/` with a
  timestamp+random name; `DONE_FILE="$OUTPUT.done"`.
- Detaches: `nohup setsid sh -c '...; echo $? > "$DONE_FILE"' >>"$OUTPUT" 2>&1 &`
  a second fork is not required beyond setsid; must survive parent and parent's
  session death (verify in test with kill of parent).
- Prints exactly one machine-readable line:
  `LANE pid=<pid> output=<path> done=<path> label=<label>` — the launching
  agent MUST then call `workass_register_external_work` with those values.
- No network calls, no tokens, no daemon coupling inside the script.

## Tests (regression; mock provider is the oracle; NO model-quality oracles)

In `internal/acp` unless noted. Each new test FAILS on pre-fix code.

- T1 register: designated output path lands under `<state>/external-work/`,
  record running, kind external, list shows it, snapshot persists it.
- T2 no-pin: running external work does NOT block idle-TTL hibernation
  (mirror of fix-1's blocking test, inverted); in-process bash still blocks.
- T3 done-file settle: exit 0 → exited; exit 3 → failed, ExitCode 3; receipt
  written with redacted tail from output file.
- T4 pid settle: dead pid + grace → exited with the no-marker summary.
- T5 terminal delivery: settled work exposes no `wake` field, creates no
  synthetic message, and does not count as live park evidence.
- T6 migration: an older snapshot containing `wake:"pending"` loads as an
  ordinary terminal receipt and cannot reactivate the removed mechanism.
- T7 explicit settle: idempotent second call; owner validation rejects a
  non-owning key (HTTP-level test in cmd/workass mirroring existing
  agent_control tests).
- T8 (cmd/workass): a queue that exhausts its bounded start retries parks the
  original row without inserting a synthetic user message.
- T9 path validation: rejects relative, symlinked, out-of-root and
  traversal paths; accepts tmp + state roots.
- Script test `scripts/tests/spawn-tracked-lane.test.sh`: lane survives
  parent kill; done file holds exit code; output captured.

## Gates (in order, receipts required)

1. `go build ./...`, `go vet ./internal/acp/ ./cmd/workass/`.
2. Full `go test ./internal/acp/... ./cmd/workass/...`.
3. Script test.
4. Dev-profile daemon rebuild via `scripts/rebuild-workass-macos.sh daemon
   --profile dev` (build skill rules; prod PIDs unchanged — record them).

## Non-goals (do not build)

- Daemon-side process spawning (env/secret capture landmines).
- Codex-provider registry support (observer gate unchanged).
- Renderer changes of any kind.
- Enforcement that agents remember to register — that is doctrine, not code.
