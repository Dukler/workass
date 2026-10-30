# `orphaned` over a finished job, and subagents in an update status: live proof, 2026-10-30

Same rig as `RESULTS.md`: dev daemon on the Mac (own state, port 18990), real renderer in a hidden
Electron window, scratch daemon on duklerver (`workass-scratch.service`, stock unit, port 18788,
state under `/data/workass-scratch`). Every job passed `--state-dir`. No real daemon, `/data/workass`,
`/data/ml` or GPU was touched and no update was applied. "Before" is unmodified e3dbfeb3, which is
v0.1.247. Output is in `baseline-14j.log` and `final-14j.log`, commands in `harness/14j/`.

## Cause of the row

`commitSpawnedWorkChange` lists the rows, gives them to the actor, then drops what has settled. In a
burst, job A's report can settle its row between another commit's list and its drop:

1. The commit for B lists A as running, and the actor accepts it as running.
2. A settles in the registry.
3. That same commit writes A's receipt and, seeing A settled with its receipt written, drops the row.
4. A's own commit lists no A. The actor treats a snapshot as complete, so it marks A `orphaned`.

The receipt was written from the settled row and the notice comes from the report, so both stayed
right. The row keeps its original summary (`sleep 2`), which is how every mismatch below reads. A row
now goes only when the actor accepted it settled. `TestSpawnedWorkCommitKeepsARowThatSettledAfterItsSnapshot`
drives that order and fails without the fix.

## Results

| # | Case | Before (v0.1.247) | After |
|---|------|-------------------|-------|
| 1a | Local `workass run`, 24 ending together, 7 rounds before and 8 after | **6 of 167** rows `orphaned` over an `exited` receipt | **0 of 192** mismatches |
| 1b | Hosted jobs on duklerver, bursts of 12 | 0 of 60: never reproduced | 0 of 24 |
| 1c | One notice per job, last round of 24 | not counted | 24 of 24, all `done` |
| 2a | Status of duklerver asked from the Mac: 1 hosted subagent, 1 job | `total: 1`, "would survive", subagent absent | `total: 2`, `WOULD KILL 1`, names the subagent, 1 protected |
| 2b | Status of the Mac asked on the Mac: real Codex subagent | `total: 0`, "no supervised job" | `total: 1`, `wouldDie` |
| 2c | Same, a native Claude Agent still busy | not run busy | `total: 1`, `wouldDie`; duklerver asked at the same moment says 0 |
| 2d | Nothing running | "no supervised job" | "no job or subagent" |
| 2e | Is it true? Scratch daemon restarted | | Hosted subagent `failed`, its `sleep` gone; job's scope stayed active and it ended exit 0 at 2m |
| 2f | Is it true? Owner restarted | | Codex subagent and native agent `orphaned` |

At the before rate, 0 of 192 by chance is about 0.1%.

The rig's self-addressed status uses a stubbed `window.workassUpdater`, since a dev daemon has no
shell. The daemon's own path that adds `runningJobs` is real; the stub supplies only the updater half.

## Found, not fixed

- A burst's first jobs on a fresh machine can race creating the outbox: one of the first 24 was refused
  with "state database migration history does not match this build: recorded migration 1, want 1 of 0" and never ran (`bl1` has 23 rows).
  It reads as a refusal on that job's stderr, not as a wrong row.
- A native agent that ends while a background shell it started runs on is not listed: its row has ended.
  That shell also dies in an update. A status counts jobs and subagents, not provider background shells.
- Rows already `orphaned` by an earlier build stay as they are; their receipts are right.

## Not proven

- Hosted-row race: same commit path, but the bug never fired on a hosted row here, before or after.
- A status asked on a headless host of itself: the rig has no chat on the host (a hosted child's
  credential answers "durable chat actor does not exist"). Asked from another machine it is the host's
  own handler, which is what 2a ran.
- A packaged app asked from another machine, Windows, and the Mac as a job host.
- A real update: 2e and 2f restart a daemon; none applied an update.
