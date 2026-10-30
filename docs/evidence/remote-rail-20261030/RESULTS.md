# Remote rows and job survival: live proof, 2026-10-30

Owner: a dev build of the daemon on the Mac with its own state dir and port 18990,
and the real renderer it serves in a hidden Electron window. Host: a scratch daemon
on duklerver (`workass-scratch.service`, stock `KillMode=control-group`, state under
`/data/workass-scratch`, port 18788) linked over the LAN. Host providers were the
real Codex and every job a real `workass run`. The owner chat's turn was the mock
provider held open, which supplied the tool credential.

Exact output is in `run.log`, screenshots in `screens/`, commands in `harness/`.

| # | Case | Result | Evidence |
|---|------|--------|----------|
| 1 | Remote subagent succeeds | PASS. Same row as a local one beside it: real model, activity moving through the real tool calls, duklerver beside the label, then done. One notice, result readable | `case1-t1..3`, `case1-end` |
| 2a | Unsupported model named | PASS. Refused at spawn with the reason; no row | `run.log` |
| 2b | Provider killed on the host | PASS. Row failed 1s later with `ACP server exited`; one notice with the error | `case2b-running`, `case2b-failed` |
| 2c | Exit while the owner was down | PASS. Exit logged on the host while the owner was stopped; row settled within 1s of the owner returning; exactly one notice | `case2c-running`, `case2c-caught-up` |
| 3 | No model named | PASS. Resolved from the host's catalog (GPT-6-Luna-low) and ran | `case1-t1` |
| 4 | Stop on a remote subagent row | PASS. Click ended the process on the host and the row went to Cancelled. No notice, by design | `case4-after-stop` |
| 4b | Stop and output on a remote job row | PASS. Output read from the host; click ended the host process within 2s | `case4b-*` |
| 5 | Remote jobs | PASS. Row from the start; queued then running; one notice each for ok, refused, timed_out, lost. Run 20s / 5s with queued 20s / 4s / 14s, wait kept out of the run | `case5-final-*` |
| 6 | Real update under a stock unit | PASS. Exit 75 and restart to 0.1.901 with a job and a subagent running. Job ran on to exit 0 at 1m10s and its row stayed correct by itself. Subagent, which cannot survive, failed with the real reason and one notice. A deliberately unprotected job was named by the update status beforehand, warned at start, and got one `lost` about 6s after the exit | `case6-*` |
| 7 | Owner restart | PASS. Rows came back running and queued, then ended by themselves, one notice each, queued 45s kept apart | `case7-*` |

## Found by this run, and only by it

Unit tests passed through all of these:

- A host-targeted spawn never had a monitor: the path read the returned struct as a
  map, found no id and returned silently. **This is why a remote subagent's exit
  never reached its owner, and v0.1.243 did not fix it.**
- A row already running with no monitor could never catch up. It now adopts one, and
  an exit already in the host's log arrives once. Seen live on a pre-final build
  (`wa-subagent-1793372747089-1`); not repeated on the final one.
- `process.start` never recorded the monitor in its reply, so a job's row never moved
  when the host could not dial back.
- A monitor adopted by a sync had no tab, and every notice through it was dropped
  without a word. The row settled and nobody was told.
- A job that ended at once left its row running, because the exit beat the row.
- `spawned-work:list` dropped the machine, so a reload lost it.
- A subagent ended by its host's restart read Cancelled with no notice.

## Not proven, stated plainly

- **Windows:** compiled only, never run. By the code, the updater stops every process
  running from the install's executable, so supervisors now run from a private copy.
  The copy, the image-path check and how endpoint protection treats it are unproven.
- **Mac as a job host:** the real app update was not run. Proven: a child in its own
  process group survives its launchd job stopping, and one in the job's group dies.
- **A model the catalog offers and the session refuses:** the fallback to the session's
  own model has not run, because the rig could not reproduce that disagreement.
- **`fleet:push`** (a host pushing over a link it dialled) has a unit test only. In this
  rig the Mac always dialled the host.
- **Real Claude or Codex as the owner's agent:** the owner turn was the mock.

## Known and left as they are

- After an owner restart interrupts the parent turn, the transcript card for a child
  still running can read "falló", and it is not corrected when the child succeeds. The
  rail row and the notice are right.
- Stopping the parent turn does not stop a remote child, as it does a local one. The
  child runs to its end, its row settles (`case1c-parent-ended`) and, as for any child
  of a stopped turn, no notice is sent.

## Re-running

`harness/remote-setup.sh` on duklerver, `harness/run-mac-daemon.sh` on the Mac, then
`harness/rail-harness.cjs` under the pinned Electron. `case1to2.sh`, `case5.sh` and the
commands recorded in `run.log` drive the cases. Paths in the scripts are this Mac's.
