# Fix 1 — idle hibernation must not kill live spawned work

Status: SPEC APPROVED FOR BUILD (dev-daemon lane; prod untouched until gates pass)
Owner: Fable (spec + gate review). Builder: Sol/codex lane.
Incident receipts: engine for tab-1784314720441-27 SIGKILLed by `idle-ttl`
hibernation at 16:45, 17:49 and 19:46:23 on 2026-07-17 — the last exactly
20 min after final foreground activity while workflow `wf_df09026f-059`
(task w0ddbfb2b) plus two wait-tasks were live in-process. The in-process
workflow supervisor died leaving "no completion record"; only detached
kill-isolated chunks survived.

## Binding laws for this lane (restated verbatim — the builder does not inherit them)
- Never rename referenced files/ids/paths; add aliases if a new name is needed.
- Do NOT commit. Do NOT touch files outside the scope below (the tree has
  extensive unrelated uncommitted work — leave it exactly as found).
- stdout purity for any ACP server code: JSON-RPC only on stdout, diagnostics
  to stderr (not expected to be touched here, but binding).
- Wire contract (`invoke/reply/event`, desktop/lan-server.js) is frozen; new
  event payload FIELDS are additive-only.
- Every claim in your final report needs a receipt: the command you ran and
  its outcome.

## Root cause (confirmed by code read)
`internal/acp/lifecycle.go` hibernates engines via SIGKILL
(`hibernateBridgeIfEligible`, kill at lifecycle.go:186). Eligibility is
checked twice — in `hibernateCandidate` (lifecycle.go:73
`pinned := b.pinned || b.hasRunningJobLocked()`) and again in the locked
recheck (lifecycle.go:118) — but both consult ONLY foreground turn state.
The spawned-work registry (`m.spawnedWork`, manager.go:75; persisted to
`state/spawned-work/<tab>.json`) in the same Manager is never consulted, so
"agent ended its turn while a background workflow/agent/bash task runs" —
the normal Workflow pattern — reads as idle. `hibernate-ttl` defaults to
20m (cmd/workass/main.go:59). The same gap applies to the recycle-at-idle
paths (age/RSS marks, lifecycle.go:95-97 and SampleRSS lifecycle.go:236).

## Required behavior changes (all in `internal/acp/`)

### C1 — spawned work pins the engine
Add `func (m *Manager) hasRunningSpawnedWorkForChat(tabID, chatID string) bool`
(under `m.spawnedWorkMu`): true iff any `m.spawnedWork` record has
`Item.Status == "running"` and matching (TabID, ChatID). ALL kinds pin
("workflow", "agent", "bash", "background") — blocking hibernation is cheap
(RAM); killing a supervisor loses completion records. Consult it:
- in `hibernateCandidate` alongside the `pinned` computation, and
- in the final recheck inside `hibernateBridgeIfEligible`, for BOTH the
  idle-ttl and recycle reasons (spare bridges have no chat — skip them).

The bridge's (tabID, chatID) are the `b.tabID`/`b.chatID` fields; snapshot
them under `b.mu` but call the registry check while NOT holding `b.mu`
(lock-ordering: never hold `b.mu` and `spawnedWorkMu` simultaneously —
check registry first, then take `b.mu` for the recheck). This is race-safe
because new spawned-work records are only created by tool events of a
RUNNING turn, which `hasRunningJobLocked()` already blocks on; with no
running job the set can only shrink.

When the check blocks hibernation, log via the existing
"acp hibernate aborted" line with an added `"spawnedWork": true` field.

### C2 — background activity refreshes the idle clock
In `commitSpawnedWorkChange` (spawned_work.go:492), after persisting, if the
change belongs to a bridge (match tabID/chatID), update that bridge's
`lastActivity` to now. Effect: when the last item settles, the idle TTL
counts from the settle time, not from the last foreground turn.

### C3 — forced kills settle spawned work as orphaned
When an engine dies WITH running spawned work for its chat — the seams are
(a) `hibernateBridgeIfEligible` after the kill (should now be unreachable
with running work, but keep as backstop), (b) `Bridge.Close(force, …)` /
engine-crash observation (recovery path), and (c) daemon shutdown — settle
the chat's running records of kind `"workflow"` and `"agent"` (in-process;
they die with the engine) with NEW status `"orphaned"`:
- extend `settleSpawnedWorkLocked` (spawned_work.go:377) to pass
  `"orphaned"` through instead of coercing to `"exited"`; add it to
  `spawnedWorkStatus` normalization if needed.
- set `Item.Summary` to
  `"Orphaned: the ACP engine exited while this ran in-process (reason: <reason>)"`.
- receipts flow through the existing `persistNewSpawnedWorkReceipts` path
  unchanged (verify a receipt is written).
Kind `"bash"`/`"background"` records are NOT force-settled — they may be
detached and alive; the existing PID-probing reconciler
(`reconcileSpawnedWork`) remains their authority.

## Regression tests (Go, `internal/acp`, alongside existing spawned_work_test.go / lifecycle coverage)
- T1: bridge idle past HibernateTTL, running spawned record (kind workflow)
  for its chat → `SweepLifecycle` does NOT hibernate; abort logged.
- T2: same bridge after the record settles → next sweep hibernates.
- T3: forced close with running workflow + agent + bash records → workflow
  and agent records become status `"orphaned"` with FinishedAt + Summary +
  receipt written; the bash record stays `"running"`.
- T4: `commitSpawnedWorkChange` advances the owning bridge's lastActivity.
- T5: recycle-at-idle (age/RSS mark) is also blocked by running spawned work.
Use short TTLs via `ManagerOptions` (hibernate-ttl accepts sub-second
durations); no sleeps beyond what existing lifecycle tests use.

## Verification gates (dev daemon ONLY — never the prod daemon on port 8788)
1. `go build ./... && go vet ./internal/acp/ && go test ./internal/acp/...`
2. Dev-daemon manual gate (see docs/DAEMON-DEV.md; dev profile
   `.dev/profiles/default`, port 18788): run with `-hibernate-ttl 90s`,
   start a chat turn that spawns a background task and ends its turn;
   confirm the engine survives past the TTL while the task runs, then
   hibernates after settle+TTL. Capture the log lines as receipts.
