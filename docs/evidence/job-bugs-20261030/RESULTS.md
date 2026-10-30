# Three job bugs: live proof, 2026-10-30

Owner: dev daemon on the Mac with its own state and port 18990, and the real renderer in a
hidden Electron window, stamped as the shell so it answers agent routes. Host: scratch daemon
on duklerver (`workass-scratch.service`, stock `KillMode=control-group`, port 18788, state under
`/data/workass-scratch`). Every job passed `--state-dir`. Nothing touched the real daemons,
`/data/workass`, `/data/ml` or the GPU, and no update was applied.

"Before" is unmodified 6bbb68a2, which is v0.1.246. Output is in `baseline.log` and `final.log`,
commands in `harness/`. No fault was injected anywhere.

## Causes

1. **Local job, no notice.** `WatchProcess` writes the monitor to the home's registry
   (`held.json`) and `IngestLocalEvent` looks in the watcher's (`watched.json`), which had no row.
2. **Refused notice lost.** `emit` logged the error and returned nothing, so `applySlice` moved
   the cursor and marked the monitor consumed. A retry alone would also have been eaten: the 60 s
   duplicate window had already recorded the notice.
3. **Status said none.** Asked about another machine, the tool path replaced the top-level
   `runningJobs` with the asking machine's own jobs. The host's correct answer was nested under
   `diagnostics.runningJobs` the whole time. The first fix only removed the overwrite; the rig
   then showed "unknown", which the unit test had passed, and that found the nesting.

## Results

| # | Case | Before | After | Evidence |
|---|------|--------|-------|----------|
| 1a | Real Claude (Haiku) shell, job in the foreground | 0 notices, `held` row only | 1 | `rb-claude-fg`, `rf-claude-fg` |
| 1b | Real Claude shell, job outlives the turn | 0 | 1 | `rb-claude-bg`, `rf-claude-bg` |
| 1c | Real Codex (gpt-6-luna) shell, foreground | 0 | 1 | `rb-codex-fg`, `rf-codex-fg` |
| 1d | Real Codex shell, outlives the turn | 0 | 1 | `rb-codex-bg`, `rf-codex-bg` |
| 1e | Exit 3 / max-runtime / preflight refused / ended at once | not run | 1 each: `failed`, `timed_out`, `failed`, `done` | `rf-fail`, `rf-timeout`, `rf-refused`, `rf-instant` |
| 1f | 8 and 12 at once | not run | 8 of 8, 12 of 12 | `rf-burst*`, `nb-burst*` |
| 1g | Job ends while the owner is down | not run | 1 | `rh-down` |
| 1h | Job cancelled / parent turn stopped | not run | 0, by law (§7, PORT-SPEC 3.4c) | `rh-cancel`, `rh-stopped` |
| 1i | Fixed owner replaces the old one while a job runs | | 1 for the running job, 0 for the one that had ended | `up-running`, `up-ended` |
| 1j | Jobs that ended before the fix | | 0: retired, not replayed | `rb-*` after every fixed boot |
| 1k | In the real renderer | | Queued in the chat: `sv-ok terminó`, `sv-fail falló`, `sv-remote terminó` | `screens/job-notices-final.png` |
| 2a | Refused delivery, remote job | Logged, `applied 1, consumed True`, 0 notices, not offered after a restart | Refused at 5 s, 10 s, 20 s; `applied None`, not consumed, not acked | `rb2b-remote`, `rd-remote` |
| 2b | Refused delivery, local job | not reachable: bug 1 | Same | `rd-local` |
| 2c | Refusal survives an owner restart | | Still owed, still retried | `final.log`, 20:42 |
| 2d | The chat becomes able to admit it | | Both in the chat 17 s later, no restart: 1 each, consumed, local one acked | `final.log`, 20:43 |
| 2e | Owner restart after delivery | | Still 1 each | `final.log`, 20:43:42 |
| 3a | Status from the Mac, 3 hosted jobs | `total: 0`, "no supervised job is running" with 2 running | `total: 3`, 2 protected, names the 1 that would die with why | `rb3-*`, `f3-*` |
| 3b | Same with the host on the old build | | Lists its job from the nested answer | `mx-job` |
| 3c | After they end | | `total: 0` | `final.log` |

The refusal is real: the monitor names a lane the chat does not have, set by editing `watched.json`
with the owner stopped, and the actor's own admission refuses it. Recovery in 2d is the chat really
gaining that lane: `future_lane` computes the id with the production formula, checked against the
chat's existing lane first.

## Found, not fixed

- **A job's row can read `orphaned` with the outcome right.** 1 of 12 on the old build, 1 of 8 and
  0 of 16 on the fixed one, only in bursts. In the case inspected, the receipt, the log and the
  notice all said it finished. The row's cause was not established.
- A status does not count a hosted subagent, which does not survive an update either. It names
  supervised jobs only.

## Not proven

- Windows, and the Mac as a job host.
- A status asked of a packaged app from another machine, or of the rig's own Mac: the rig has no
  `workassUpdater`, so only the headless host path ran live. The self-addressed branch has a unit test.
- `home_down` re-announced after a refusal, and a synthesized end retried: unit test or code only.
  A synthesized end is held in memory, so an owner restart during its refusal loses that notice.
- A real parent turn being stopped: the Stop case was the mock.
- Notices to an owner whose monitor names a lane that never appears retry every 5 minutes forever
  and never arrive. Nothing in the product produced such a lane; the rig had to write one.
