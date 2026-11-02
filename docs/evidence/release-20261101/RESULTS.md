# Integration proof, merged build, 2026-11-01

The six fix branches merged, then the top behaviours run on that build. Real daemons on scratch
state, real cheap providers (Claude Haiku, Codex gpt-5.6-luna), real `workass run`. Nothing touched
an installed app, its daemon, or another machine. Output is in `logs/`, with paths, machine ids and
the host name replaced. Commands are in `harness/`.

## Remote rig (`harness/rrig.mjs`)

Both ends on one Mac: an owner daemon on loopback, and a host daemon with its own state and identity
reached only over the LAN address through the fleet mesh. The parent is real Haiku on the owner. Real
Linux was not needed, so no other machine was used. Every background task is a script that writes a
heartbeat and records any signal.

| Case, child hosted on the other daemon | Parent told | Work finished | Child read the result |
| --- | --- | --- | --- |
| Claude Haiku, 40 s background shell | one `completed`, after 57 s | yes, no signal | yes, by its own wake |
| Codex Luna, 40 s background command | one `completed`, after 61 s | yes, no signal | yes, by the daemon's prompt |
| Claude Haiku, host engine SIGKILLed while waiting | one `failed`, work named | no, SIGTERM | no |
| Claude Haiku, owner daemon down while the work ended | one `completed`: row settled 5 s after the owner returned, notice followed in the chat's queue | yes | yes |

Each read `running` / `waiting_background` until the work ended, on the owner's list and on its rail
row. Log: `logs/remote-hosted-children.log`.

Rail, in the owner's real renderer in a hidden window (`logs/rail.log`): the row reads `rig claude`,
the host, the time, the model and `Trabajando`. With the row expanded, the old label and the daemon's
waiting text are not on the page.

## Hold clock, on the host daemon (same log, `[hold]`)

Holder 40 s on one resource; behind it a waiter with `max_hold` 15 s and an over-holder with
`max_hold` 6 s. The waiter queued 32 s and ran 8 s: no stall flag, one `done` notice with run time and
queue time apart. The over-holder's stall flag rose about 9 s after it got the resource, so a real stall
still reports. Its `stalled` notice was read in the rig's first run (`logs/hold-first-run.log`, same
result); the final run read notices before that one arrived.

## Local (`logs/`)

| Proof | Result |
| --- | --- |
| Local child, Haiku, 40 s shell (`bg-claude.log`) | `completed` only after the work ended; child read the result |
| Self-started-turn child, real Haiku (`owner.log`) | spawn with no cwd ok at the chat's effort; message `live` with no turn and in a later turn; cancel ok |
| Hosted mp4 with an audio track (`video.log`) | real link click in the real shell; plays, time advances, seeks; picture confirmed in a capture; no toast. Chromium `mute-audio`, `video.muted` true, `audible` false on every page (the shell unmutes the on-screen page itself, as designed, so the switch is what holds) |

## Found, not changed

- `workass_message_subagent` to a hosted child refuses with "subagent not found for this owner",
  though the list shows it. Nothing routes that message to the host, so this is an unbuilt path rather
  than a regression. It reads like the local bug fixed here. The owner can see and stop such a child.
- `machines:add` writes the book but does not read it into the mesh; that happens at boot and after a
  network scan. The rig restarts the owner after adding the host.

## Not run

Windows and real Linux. Real Pi, OMP and Devin.
