# Full access refuses Claude Code's own safety checks instead of auto-approving

v0.1.250 made `canUseTool` auto-approve, in `bypassPermissions`, the only kind of ask
that still reaches it there: Claude Code's own safety check (circuit breakers like
`dangerousRemoval`). That let a risky command through untouched and, separately, a
card raised for the same check could hang a background agent (66 min, 2026-10-31,
`docs/evidence/claude-update-20261030/RESULTS.md`). Changed `scripts/claude-native-host.mjs`'s
`bypassPermissions` branch in `canUseTool` to `deny` with the reason instead of
`allow`, with one stderr line and no card. Other modes are untouched — they already
go through `session/request_permission` unchanged.

Harness: `docs/evidence/claude-update-20261030/harness/rig-perm.zsh` (reused as-is,
own state dir + own HOME, real `claude` CLI, mock-provider renderer run via a hidden
Electron). Case A asks a real Claude Haiku child, in Full access, to run
`rm -f $WA_RIG_UNSET/*.wa-rig-nothing` — an `rm` whose target Claude Code's own
`dangerousRemoval` check cannot statically resolve.

## Before (v0.1.250, `docs/evidence/claude-update-20261030/after-perm.log`)
```
native {"decision": "accept", "source": "user_temporary", "grantScope": "once", "reasonCode": "safetyCheck", "circuitBreaker": "dangerousRemoval"}
```
Full access auto-approved the check; the native transcript records an `accept`.

## After (this change, `after.log` in this directory)
```
real-removal | done | The Bash tool's safety check blocked the command because it | Completed
0 attention notice(s)
native {"decision": "reject", "source": "user_reject", "reasonCode": "safetyCheck", "circuitBreaker": "dangerousRemoval"}
```
Daemon stderr for that run:
```
[workass] Full access refused a Claude Code safety check for Bash: Dangerous rm operation on possibly-empty variable path: $WA_RIG_UNSET/*.wa-rig-nothing in `rm -f $WA_RIG_UNSET/*.wa-rig-nothing` (rewrite it as "${WA_RIG_UNSET:?}"/*.wa-rig-nothing or use a literal path)
```
The native transcript records a `reject`; the child read the refusal, never raised a
card (0 attention notices), and finished (`done`/`Completed`) instead of hanging.
Reproduced three times; the model declined to even attempt the command once without
exercising the check (no stderr line that run, expected — `canUseTool` never fired).

Case B (a mock child waiting on an unrelated permission while its owner is idle) is
unaffected: 1 notice at +6s and +20s, same as before.

## Not proven here
- Only Claude Haiku was exercised as the real child (the rig's own model pick); other
  Claude models were not separately run through this case.
- Only the `dangerousRemoval` circuit breaker was exercised live. Other Claude Code
  safety checks (e.g. `isolatePeerMachines`, `outsideReadsBlocked`) share the same
  `bypassPermissions` code path and therefore the same fix, but were not individually
  reproduced with a real child.

## Gate and publish
`scripts/gate.sh`: `GATE_PASS` (go build/vet, renderer + Go + shell + script test
suites, including the rewritten
`official Claude SDK host refuses a Claude Code safety check in Full access` case in
`scripts/tests/claude-native-host.test.mjs`).

Published `scripts/stage-macos-local-update.sh --version 0.1.251` to the local update
feed. Not installed; no real daemon restarted; no real state touched (rig ran under
its own `--state-dir` and its own `HOME`).
