# A Stop right behind the prompt: the turn ran anyway

The flaky test and the lost Stop turned out to be separate things. The test was wrong twice. Testing
the worry behind it found a real product bug in three hosts and in the Go agent.

## The flaky test (test and fixture wrong, not the product)

`TestARemoteChildOfAStoppedTurnEndsLikeALocalOne`. Unfixed: 0/20 alone, 21/200 under load (25 at
once), all 21 at "the cards never happened", before the test presses Stop.

- It registered children as soon as the actor showed a native id. The manager shows a running job
  later, and a row registered in that gap has no turn to put a card on. No real child exists there:
  children come from a turn the provider is running. Forcing the gap open (300 ms sleep, scratch
  worktree): old test 3/3 fail with the same message, corrected test 10/10 pass.
- The mock's `[mock:active-without-terminal]` parked after a cancel read while streaming, so nothing
  ever answered. Forcing it (mock delay 150 ms): old mock 2/3 fail with "the stopped turn never
  happened", corrected mock 6/6 pass. This is the "lost Stop" the flake looked like. It was the
  fixture's; no harness behaves that way.

Fixed test and mock: 0/60 alone, 0/200 under load.

## The product bug

The daemon is right: a Stop during preparation sends no prompt, and one that races the write sends
the cancel after it (seen in the rig as `preAdmission`, no prompt written). The host is where it was
lost, when the cancel is read together with the prompt, as a busy host reads them.

- Claude: `interrupt()` returned silently with no query. There is none on a new chat whose options
  invalidated it, and none after any earlier Stop, which retires it. The prompt then went in. The
  daemon marked the job cancelled and the model ran on, with its tools.
- Pi and OMP: `abort()` only reaches a run that exists, so before `agent_start` it does nothing. The
  run started anyway, ran to the end, and answered "cancelled", which hid it.
- Go agent (`workass-agent`): prompts run on goroutines and the cancel is handled inline, so the
  cancel usually arrived before the prompt had a context to cancel.

Fixes: Claude settles a prompt the harness does not hold yet, and pushes nothing; opens are now shared
so the next prompt cannot start a second query on the id. Pi and OMP repeat the abort at `agent_start`
and report a throw from it as cancelled. The agent records the Stop count in stdin order at the read
loop and does not start a prompt that a later Stop passed.

## Rigs

Harness in `harness/`. `seal.sh` makes a copy of the published 0.1.257 bundle with a rig profile
sealed in and re-signs it; "after" is that bundle plus the head daemon and hosts. The daemon runs
headless with `env`-limited scratch state, `--bind localhost --beacon=false`, and aborts if it holds a
non-loopback socket. `stdin-hold.mjs` (a preload, no product change) makes a host read `session/prompt`
together with what follows; the Stop is sent when the host reports it is holding the prompt.

| Rig | Provider | Before | After |
| --- | --- | --- | --- |
| Sealed daemon, Stop on the held prompt, new chat each | real Claude Haiku | 6/6 model answered after an accepted Stop | 0/12, follow-up alive 12/12 |
| Sealed daemon, same, one chat | real Claude Haiku | 0/20 (query reopened by mode control) | 0/12 |
| Sealed daemon, Stop 0 to 600 ms after start, no hold | real Claude Haiku | 0/12 | 0/24 |
| Real host, prompt + cancel in one write | real Claude Haiku | 3/6 | 0/12 |
| Real host, one write | real Pi SDK 1.0.0, loopback fake model | 8/8 answer finished | 0/8 |
| Real host, 1 to 20 ms gap | real Pi SDK, same | 0/16 | 0/24 |
| Real host, one write | real OMP 18.1.8, loopback fake model | 8/8 | 0/8 |
| Real host, 5 and 20 ms gap | real OMP, same | 2/16 | 0/16 |
| Real host, one write | Codex fixture | replied cancelled, 20/20 | unchanged |
| `workass-agent`, one write, 20 rounds | loopback fake model | end_turn in round 0 | model asked 0 times |

Each regression test fails on the unfixed host or agent and passes on the fixed one.

Trap: `--bind localhost` does not stop `--beacon`. A rig daemon with an empty state probes private
LAN hosts on TCP 80 the moment it starts; it did so for the few seconds the first attempt here ran,
before `--beacon=false`. Trap: a mode control in preparation reopens the Claude query, so the timing
rig in a warm chat cannot reach the bug; use a new chat.

## Not proven

Real Codex: the host handles a Stop before `turn/start` answers, tested on the fixture only. Real LM
Studio behind `workass-agent`: not running here. Windows. Real Pi and OMP models: a loopback fake
stands in for the model. How often a real chat hits it: the rig forces the read, so these are not field
rates. The first `scripts/gate.sh` failed once on `TestNodeProbeAgainstGoRun` (5 s initialize behind
`go run`, in the package just changed, so probably a cold compile under load, not proven); the rerun
with a warm cache passed.
