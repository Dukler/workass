# A running child its own parent could not reach

Reported on installed v0.1.259: `workass_list_subagents` showed a child running, and
`workass_message_subagent` to it from the same chat answered "subagent not found for this owner",
while its siblings took messages in the same minute. The child had reported effort `high`, its
siblings `xhigh`.

## Cause

Listing reads the chat actor's rows. Messaging, wait, retry and cancel go through the manager's
`addressSubagentLocked`, which accepts a child if the running turn spawned it or if it was adopted by
its chat. Adoption happens when a turn ends, but only in the worker for human turns,
`runAppChatJob`. A turn Claude starts by itself, as when a background task finishes, ends in
`endAdoptedHarnessTurn` or `abandonAdoptedHarnessTurn`, and neither adopted. Its children stayed bound
to a job that no longer existed, for as long as they ran.

The persisted rows of the reported chat match: the unreachable child's origin turn is a
`harness-turn-*` id, and the four siblings' are `app-chat-*`.

The effort clue has the same origin. The self-started turn's job was built without the chat's
selection, so a child inherited nothing. Effort fell to the default `high`, a spawn naming no model
would take the first catalog model, and a spawn naming no cwd was refused.

## Fix

Both self-started end paths adopt running children, inside the lock that removes the job. The job now
carries the live session's cwd, model and mode.

## Live proof

`harness/rig.mjs`: scratch daemon, loopback only (checked for non-loopback sockets), real Claude Haiku.
The parent runs on `haiku[low]`. A human turn spawns `human-child`; a real self-started turn, woken by
a background shell ending, spawns `wake-child`. Both name the model and leave effort unset. Unfixed is
base 130690c, fixed is the same base plus this change.

| Step on `wake-child` | Unfixed | Fixed |
| --- | --- | --- |
| Spawn with no cwd | refused, "subagent cwd is required" | ok |
| Effort, with `human-child` at low | high | low |
| Message, no turn running | "subagent not found for this owner" | delivered live |
| Message, during a later human turn | "subagent not found for this owner" | delivered live |
| Wait, 1 s | "subagent not found for this owner" | timed out, still running |
| Cancel | "subagent is no longer cancellable" | ok |

`human-child` took every message and the cancel on both builds, as the siblings did in the report.

Run it with `RIG_SDK=<path to the Agent SDK sdk.mjs> node harness/rig.mjs <daemon binary> <label>`.
Rig trap: Claude Code refuses a foreground `sleep`, and a child that backgrounds it ends at once, so
the children hold on `ping`.

## Not run

Windows, the fleet rig (hosted children do not use this path), and other providers: the self-started
turn is the only job built this way, and only the Claude host reports one here.
