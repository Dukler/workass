# A subagent's background work died when its turn ended, and the parent read "completed"

Live case, 2026-11-01: `wa-subagent-1793526013686-10` ("feet: cheap causal fill") ended its turn with
"the sweep is still running, and the monitor will report when the sweep and sheets finish". Its receipt
reads `done`, `end_turn`, after 560 s. The sweep process was gone minutes later with an empty output
file, and no report ever came.

## What happens to background tasks at turn end

Nothing happens at turn end. The end of the session is what kills them.

| Experiment (real provider, cheapest model) | Result |
| --- | --- |
| Claude Haiku, top-level chat, engine stays open | Shell finishes; Claude Code starts a turn by itself about 2 s later and reads the result |
| Claude Haiku, tracked subagent, before the fix | Parent told `completed`; shell gets SIGTERM about 1 s after the turn ends |
| Codex Luna, bare `codex app-server`, thread stays open | Command outlives the turn, its item stays `inProgress` past `turn/completed` and completes when the process exits; no turn ever starts by itself |
| Codex Luna, tracked subagent, before the fix | Parent told `completed`; process killed when the thread closes |

`runSubagent` settled the run and closed its session on the first `end_turn`. A child owns a whole
engine, unlike a chat, whose engine stays open, so its background work died with that close.

## Fix

Options weighed: keep it alive and wake it, or report the loss. The fix does both, because each covers a
case the other cannot.

- A live engine waits. The run stays `running` in phase `waiting_background`, naming the work, and stays
  cancellable and messageable. Claude Code wakes the child; Codex cannot, so the daemon sends one prompt
  10 s after the work ends. Work owed is what the session would kill: launched into the background, alive,
  not a service. Scheduled one-shot wakes count too. Bounded at 24 h.
- No close is silent. `finishSubagent` names whatever is still running on the receipt
  (`backgroundStopped`) and in the notice (`<background-stopped>`). Work it owed makes the receipt `failed`,
  with the child's own last report kept. A service alone leaves it `done`, named.
- Codex host: a command still open at a completed `turn/completed` is published before the prompt reply,
  and again when it ends. Tracked for subagents only: in a chat it would be a new rail row, and nothing
  classifies it as a service, so a dev server would pin the engine for good.

`workass run` and registered lanes from a child belong to the parent chat, because a child's tools run
under the parent's owner. They outlive the session and report themselves, so they hold nothing open.

## Proof

`harness/bgrig.mjs`: real daemon, scratch state, loopback only, `--beacon=false`, aborts on a non-loopback
socket. Real parent chat and real child, with explicit cheap models. The background task is a script that
writes a heartbeat and records any signal it receives. `after` runs were built from the commit of the fix.

| Run | Parent told | Work finished | Child read the result |
| --- | --- | --- | --- |
| before, Claude Haiku, 120 s shell | `completed` | no, SIGTERM at the close | no |
| before, Codex Luna, 120 s command | `completed` | no, killed at the close | no |
| after, Claude Haiku, 120 s shell | `completed`, after 131 s | yes | yes |
| after, Claude Haiku, 40 s Monitor (build before the last small edits) | `completed`, after 52 s | yes | yes |
| after, Codex Luna, 120 s command | `completed`, after 140 s | yes | yes, by the daemon's prompt |
| after, Claude Haiku, host SIGKILLed while waiting | `failed`, work named, last report kept | no | no |
| after, Claude Haiku, daemon restarted while waiting | `failed`, "Orphaned after daemon restart" | no | no |

Timelines are in `timelines/`. Mock tests in `internal/acp/subagent_background_test.go` cover the wake,
the prompt, the ceiling, cancel, a coordinator message while waiting, engine death, and close over each kind
of work. With the wait, the close report and the host signal disabled, 7 Go tests and the host test fail;
with them, all pass, also under `-race`.

## Traps

A rig that names its run from `Date.now()` alone collides when two start in the same millisecond: shared
state directory, "no health", and a process check that reads the other run's sweep. The stamp now includes
the pid.

## Not proven

Real Pi, OMP, Devin and others: their adapters report no background work, so a child there behaves as before
and any loss stays invisible. The 24 h ceiling ran only on the mock, with a short ceiling. Windows.

Hosted children and the rail row in a real window were run later: `../release-20261101/RESULTS.md`.
