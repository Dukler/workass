# Test flakes, 2026-11-01

Each flake reproduced with parallel copies of the test, each with a scratch `HOME` and `TMPDIR`, `GOMAXPROCS=1`
as the suite sets it, on a machine already busy with other agents (load 50-240). Rates are failed/total runs.

## Causes that are the product's

- `lsof` takes about 1.1 s alone on this Mac against the 1.5 s it has, and every copy misses at 10 at once. A
  Stop refused and a lane's process stayed unknown. The kernel call behind libproc gave the same answer in 12-17 ms
  (`internal/acp/spawned_work_probe_darwin.go`). Linux still uses lsof.
- A Codex session failed when an MCP server's ready status landed right behind a list asked before it was ready.
- The late end of a Stop-settled Codex turn settled the next prompt, whose real turn ran on with no owner.

## Proving flake 1's cause

The agent stamped the clock in its first init, on a Mac at load 45-50.

| Launches at once | Fresh executable, slowest | Same executable again, slowest | Fresh over 5 s |
| --- | --- | --- | --- |
| 1 | 642 ms | 14 ms | 0 |
| 10 | 5.8 s | 150 ms | 2 |
| 30 | 15.9 s | 188 ms | 22 |
| 60 | 33.3 s | 308 ms | 52 |

All of it before Go's main; after main both reply in about a millisecond. New executables queue one at a time
across the machine.

## Proving flake 3's cause

Mean gap between frames, and time in the durable commit, per frame:

| Copies | Disk | Commit | Mean gap | Failed |
| --- | --- | --- | --- | --- |
| 1 | real | 4 ms | 20 ms | 0/3 |
| 30 | real | 36 ms | 54 ms | 150/150 |
| 30 | RAM, HFS, load 125 | | 17-24 ms | 0/90 |
| 30 | RAM, APFS, load 120-134 | | 19-25 ms | 0/90 |

The mock's output didn't move (1.4 ms between chunks, alone or at 30 copies).

With every frame rewriting the actor (the stall the test exists for), the new test still fails: 2/2 at 1 copy,
16/16 at 8 and 60/60 at 30.

Found on the way, not changed: on SQLite, the store a Mac uses in production, the same burst over a 25 MB actor
sends everything in 1-4 frames at 30 copies. Its first commit holds the stream, with no per-frame sync involved.

## Hosted message and machine add

Two scratch daemons on this Mac, the host reached over the LAN address only. Hosted children and the parent on real
Claude Haiku. `harness/rrig-message.mjs` is release-20261101's rig with one more scenario, and `harness/add-links.mjs`
is new. Logs have paths, machine ids and the host name replaced.

| Proof | Before | After |
| --- | --- | --- |
| `machines:add`, no restart (`logs/add-links-*.log`) | not linked after 45 s | linked in under a second |
| Message, child mid-turn (`logs/hosted-message-*.log`) | refused, not found for this owner | `live`, token in its final reply |
| Message, child parked on background work (same) | refused, not found for this owner | `followup`, token in its final message |

Not proven on a real link: a below-full limit refusing a hosted message. That has a test over `agent:control` with a
real hosted child, no second daemon. Retrying a hosted child isn't built, so its finished answer doesn't offer it.
