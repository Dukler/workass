# A top-level Codex chat's background command and idle hibernation

`harness/rig.mjs`: scratch daemon with `--state-dir`, scratch `HOME` and `CODEX_HOME` (only `auth.json` is
linked), real Codex 0.166.0 on `gpt-5.6-luna`, a 20 s idle TTL instead of 20 min. A top-level chat runs a
75 s command that appends a line per second to a file, and ends its turn while the command runs.

| | Before | After |
| --- | --- | --- |
| Heartbeats, of 75 | 25 | 75 |
| Engine | hibernated at the idle TTL, killing the command | held 11 times, hibernated right after the command ended |

Run with `RIG_MODEL=gpt-5.6-luna node harness/rig.mjs BIN REPO LABEL`.

## Bound

The hold ends after `SubagentBackgroundCeiling` (24 h), because nothing can tell a build from a dev server.
At that point the engine hibernates and the command dies, as before. The ceiling is covered by
`TestACommandThatOutlivedItsTurnHoldsTheEngineForABoundedTime`, not by a 24 h live run.

## Not changed

No rail row and no notice: either would be new UI. Codex also starts no turn when the command ends, so the
agent isn't told; it has to ask, or use `workass run`.
