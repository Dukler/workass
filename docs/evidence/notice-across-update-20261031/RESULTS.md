# A notice across a daemon stop: the owner answers it

The bug: a task notification queued as the daemon stopped failed its turn with "service-tier
session is not attached", so the owner agent never answered that job's or agent's ending.

## Cause

`Coordinator.Close` detached the lanes while its effect worker kept running. Closing a lane ends
its running turn, the failed terminal promotes the next queued input, and that input's StartTurn
meets a closed session. Its failure is recorded as terminal, and the operation id is spent, so
nothing sends it again. Three shapes, all reproduced on the unfixed build:

- The first one queued: "service-tier session is not attached".
- The next one: "provider lane attachment was already unavailable", from the lane reopen.
- In the window between update prepare and commit: StartJob's drain refusal, "Workass is preparing
  a verified app update", recorded as a failed turn. A person's queued turn failed the same way.

A notice whose own turn was running when the stop came was also lost: interrupted, never answered.

## Fix

- Close fences new claims first and lets an effect in flight finish. A failure caused by the
  teardown settles nothing: the start goes back to Pending and recovery sends it once.
- During an update handoff queued turns wait; calling the update off wakes them.
- A notice interrupted by the stop, a provider connection loss or a crash is queued again, up to
  three times. A person's turn is not repeated; a turn someone stopped is not redelivered.

## Results

Real update: the update card clicked in a sealed packaged app, real Haiku owner, real `workass run`
jobs ending from 1.5 s before the click to 3 s after it. Unfixed 0.1.254 to 0.1.255; fixed builds
stamped 0.1.900 to 0.1.901. Receipt healthy, no rollback, in every run.

| Rig | Owner | Build | Cycles passed | Notices answered |
|---|---|---|---|---|
| Real update | idle | unfixed | 0/2 | 2/12 |
| Real update | turn running | unfixed | 0/2 | 0/12 |
| Real update | idle | fixed | 5/5 | 30/30 (5 cycles) |
| Real update | turn running | fixed | 3/3 | 18/18 |
| Real update | subagents, one orphaned | fixed | 3/3 | 6/6 |
| Dev daemon, real stop path | turn running | unfixed | 0/2 | 28/32 |
| Dev daemon, real stop path | idle, slow turns | unfixed | 0/2 | 26/32 |
| Dev daemon, real stop path | idle, slow turns, short drain | fixed | 4/4 | 64/64 |
| Dev daemon, real stop path | turn running | fixed | 4/4 | 64/64 |
| Dev daemon, SIGKILL | idle, slow turns | fixed | 4/4 | 64/64 |
| Dev daemon, update called off | queued at prepare | unfixed | 0/3 | 2 of 4 failed per cycle |
| Dev daemon, update called off | queued at prepare | fixed | 3/3 | 4/4 per cycle |

Pass: one answered turn for each notice, none refused. The dev sweeps also restart once more and
check that nothing repeats. In the fixed runs 3 of 30 notices in the real idle cycles and 8 of 192
in the dev sweeps were redelivered. Each of those shows two notice rows: the interrupted attempt
and the answered one.

Fixed-build real cycles left out of the table, all rig faults found and corrected on the way: one
with the version stamp below, one whose owner had no tool permission, four where the interrupted
turn held a 400 s command that the agent ran again, so the wait ended first. Before the last
correction those four read 0/24, with the owner's reply running.

Not proven: the first update onto this fix is still stopped by the old daemon, so a notice caught
in that one stop can still fail. A notice that was consumed and then interrupted is answered once
but its text reaches the agent twice. A provider crash outside a daemon stop reports its turn as
failed without this redelivery unless the connection loss arrives first.

## Traps

- The app's version also lives in `Contents/Resources/app/package.json`. Stamp it or the worker
  fails with "incoming app version does not match the release manifest".
- Give the owner chat a permission mode that allows tools. Haiku reads the job's output file, and a
  permission card nobody answers looks exactly like a stuck turn.
- A long shell command in the interrupted turn may be run again by the agent when it next gets a
  turn, which holds the queue behind it. That is the agent working, not a stall.
- A queued notice has no row until its turn starts, so a missing row is not yet a lost notice.
