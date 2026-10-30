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

## Later correction: both items that were listed here

Same rig on 2026-10-30: dev daemon and real renderer on the Mac, scratch daemon on
duklerver. Real Codex children, owner turn the mock. Host Codex this time was a
0.165.0 copy inside `/data/workass-scratch`. Output is in `fix-baseline.log` (before)
and `fix-final.log` (after), commands in `harness/fix/`, labels below are the logs'.

The earlier line "stopping the parent turn does not stop a remote child, as it does
a local one" was wrong about local. `fix-baseline.log` (`p2`) shows a local child's
`sleep 45` running on through the parent's Stop and ending `PS-DONE`, exactly like the
remote one. That is the law, PORT-SPEC §3.4c: a parent turn ending, cancelled included,
never cancels a running child. So the remote child was not changed to die, and the
two now match. What differed from the rows was the card, for both of them.

| # | Case | Result | Evidence |
|---|------|--------|----------|
| B1 | Before: owner restart, child succeeds | Reproduced. Card `falló`, stored `failed`, after the end and after a reload. Row and notice right | `r2` in `fix-baseline.log`, `baseline-r2-*` |
| B2 | Before: parent Stop, local and remote | Both run on, no notice, both cards `cancelado` until a reload | `p2`, `baseline-p2-*` |
| F1 | Owner restart, child succeeds | PASS. Card `en curso` after the restart, `listo` at the end, 40s later, after a reload and after a second restart. Stored `completed`, one notice | `f5`, `fix1-*` |
| F1b | A new turn is running when it ends | PASS. One card, on the turn that started it; stored `completed` | `f1b`, `fix1b-end` |
| F1c | Child really fails after the restart | PASS. Card stays `falló`, stored `failed`, one notice with the error | `f6`, `fix1c-end` |
| F2 | Parent Stop, local and remote side by side | PASS. Both processes still alive after Stop, both cards `en curso`, both `listo` at the end and 40s later with no reload, stored `completed`, no notice for either | `f4`, `fix2-*` |
| F2b | Parent Stop, then owner restart, then the end | PASS. `listo`, stored `completed`, no notice | `f7`, `fix2b-end` |
| F3 | Children end before the parent's Stop | PASS. Local and remote alike: one notice each | `f3`, `fix3-end` |

Found only live: the first fix relied on the rail row, and the row leaves the client's
list a moment after settling (the summary keeps running rows only). The card then fell
back to `cancelado` with the stored value already right. A settled row now writes its
end onto the loaded card.

`ps` and `rs` in `fix-baseline.log` are from before the host Codex had its
`codex-code-mode-host` helper: the commands failed, the model replied anyway, and those
children prove nothing. `p2` onward had it.

## Still not proven

- A stale card written by an older build is not repaired unless its child ends after
  this one is running.
- Owner turn is still the mock, so neither a real Claude or Codex parent was stopped.

## A remote child's end that told nobody

Live on 2026-10-30, owner chat on the Mac: `wa-subagent-1793383430598-5` ended 18:04:11Z,
row settled, receipt written, no notice.

**Cause, from the production log and then reproduced:** the hourly notice cap.
`15:04:11 [fleet] held a exit notice for chat-81716b9c...: this chat is at its 6-notice
hourly cap`. Six successful remote ends had reached the chat in the previous hour
(17:24:47, 17:24:52, 17:33:02, 17:39:43, 17:59:31, 18:02:20), and the seventh was held.
The eighth at 18:14:21 was held too. By then the monitor's cursor had moved and it was
consumed, so nothing could redeliver it. The owner's turn had ended at 18:03:56, 15s
earlier, so the turn was not running, and timing was not the cause.

Rig: dev daemon and real renderer on the Mac, scratch daemon on duklerver, real Codex
children, the owner turn the mock, ended naturally. Baseline is unmodified 1339c91d.
Commands are in `harness/notice/`, output in `notice-*.log`.

Notices per child. The gap is the notice's stored time against the end of the parent turn.

| Child ends | Remote, before | Remote, after | Local, before | Local, after | Gaps after the fix (remote, local) |
|---|---|---|---|---|---|
| Mid-turn | 1 | 1 | 1 | 1 | 6.3s, 6.4s before the end |
| Last second of the turn | 1 | 1 | 1 | 1 | 0.19s, 0.27s before the end |
| Just after the turn | 1 | 1 | 1 | 1 | 1.5s, 0.7s after the end |
| No turn running | 1 | 1 | 1 | 1 | 16.8s, 17.8s after the end |
| 5th to 8th in one hour, burst | **2 of 4** | 4 of 4 | 4 of 4 | 4 of 4 | |
| 8 at once, rail open | **6 of 8**, twice | 8 of 8 | not run | not run | |
| Remote job past the old count | not run | 1 | | | |
| 8 local `workass run` jobs | | | **0 of 8** | **0 of 8** | see below |

Every timing case gave exactly one notice on both builds. Only the count separates them:
each baseline run lost exactly the seventh and eighth remote ends, each with a `held` line
(`notice-baseline.log`, `notice-baseline-visual.log`), and the fixed build logged none.
`notice-baseline-rail-settled` shows it plainly: eight rows finished, six in the queue.
`notice-final-rail-settled` is the same moment with the fix. The baseline
`l-none` first failed on a rig fault with no child at all, so `baseline-l-none2` is its
rerun. Baseline `baseline-v*` ran twice with the same labels, so its tally reads 12 for 16.

**Fix:** the cap is gone (spec §14h). It could only ever hold a successful end. The 60 s
window for the same event stays. Two unit tests fail on the old code with the cap's own
message and pass now.

## Found by this run, not fixed

- **A local `workass run` job gave no notice on either build**, 16 jobs of 16, with row and
  receipt correct and no `held` line. Its monitor is recorded on the home side only, and
  `IngestLocalEvent` looks in the watcher's registry, which has no row for it. Every monitor
  in the real Mac's `watched.json` targets the host, so nothing there contradicts this. A
  rig credential was used, so a job from a real agent shell has not been run this way.
- A failed `deliver` is logged and the cursor still moves, so a notice the actor refuses is
  lost the same way. Read from the code, not seen live.
- Notices already lost stay lost: `wa-subagent-1793383430598-5` and `-1793384027131-6` are
  consumed on the Mac and this build will not redeliver them.
- Rig mistake, cleaned up: `workass run` without `--state-dir` fell back to the installed
  state. Eight `sleep 2` jobs created `process-outbox.db` and 16 files under the real Mac
  state, and no job reached the real daemon. All 24 rows were verified as those jobs', no
  process had the file open, and those files were removed. `harness/notice/jobs.sh` now passes
  the flag.

Still unproven: a real Claude or Codex owner turn, and the same on Windows.

## Re-running

`harness/remote-setup.sh` on duklerver, `harness/run-mac-daemon.sh` on the Mac, then
`harness/rail-harness.cjs` under the pinned Electron; `harness/fix/session.sh SCRIPT` does the last two for one foreground command. `case1to2.sh`, `case5.sh` and the
commands recorded in `run.log` drive the cases. Paths in the scripts are this Mac's.
