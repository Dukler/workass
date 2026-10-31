# Claude updater "exit -1", and a Full access child hung on a card

Before = installed v0.1.249. After = dev build of this commit. Same rig, own state
dirs. Logs beside this file, harness in `harness/`. The updater rig (`rig.zsh`) runs a
stub `claude` under its own HOME against the real npm and native release pointers.
The permission rig (`rig-perm.zsh`) uses a real Claude child for A and a mock child for B.

## Causes, from the real daemon

- `claude update` exited 0 and printed "Claude Code is up to date (2.1.314)". Workass
  had compared against npm `latest` (2.1.316); a native install follows
  `downloads.claude.ai/claude-code-releases/latest` (2.1.314). Verification then failed
  and wrote a made-up exit -1. Nothing was killed. The run started at 02:27:24Z.
- `wa-subagent-1793414418910-1` was stopped by a `spawned-work:stop` from the controller
  (durable actor operation, result `cancelling`), 1.7 s after the operator's message. Its
  receipt says `cancelled`; the agent tools mapped the row's `exited` to `done`.
- The 66 minute hang: native record `reasonCode: safetyCheck, circuitBreaker:
  dangerousRemoval`. Claude Code raises that check in bypassPermissions, the host
  turned it into a card, and a local child's wait reached its owner only by reading.

## Results

| Case | Before | After |
|---|---|---|
| Installed = native channel, npm ahead | offered, `claude: error (exit -1)`, Reintentar same | no offer, no card, `update-no-pending` |
| Real pending update installs | fails (target is npm's) | done, card gone |
| Updater exits 3, then Reintentar with it working | `exit 3`, no reason; retry fails on npm target | `exit 3` + reason; retry done, card gone |
| Updater killed | `exit -1`, no reason | `exit -1` + `signal: killed` |
| Exits 0 without installing | `exit -1`, no reason | `claude: error` + the real sentence, wire exit 0 |
| Running child across a CLI swap | still running | still running, then done with result |
| Child stopped from the rail, agent tool status | `done` | `cancelled` |
| A. Real Claude, Full access, unresolvable `rm` target | hangs on `Permission required: Bash` | no card, `FINISHED`, native `safetyCheck` accepted |
| B. Child waits on a permission, owner idle | 0 notices at +6 s and +20 s | 1 notice at +6 s, still 1 at +20 s |

In `before.log` case 6 the child stays running after release only because that earlier
rig run set the wrong hold variable; `after.log` has the corrected rig.
