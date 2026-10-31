# Jobs across a real macOS update, orphan notices, status with no window, cancelled rows

Rig: a packaged app at ~/Applications/WorkassUpdateRig.app, built from the real
0.1.253, 0.1.254 and 0.1.255 artifacts with a rig profile sealed into each bundle and
re-signed. The worker strips profile overrides from the environment on purpose, so
overriding by environment rolls back with "installed runtime profile does not match
the prepared update transaction": that is the rig's fault, not the updater's. A second
dev daemon with a hidden real renderer linked over the LAN starts jobs by process.start.

- 0.1.253 -> 0.1.254 and 0.1.254 -> 0.1.255, each by clicking the update card with a
  Haiku turn, a subagent and jobs running: no rollback, about 11 s click to healthy.
- `workass run` from the installed binary (by CLI and by the agent's own shell) and a
  job started by process.start kept running and each gave one notice (t1, t2 logs).
  A job that ended while the daemon was down gave one notice when it came back.
- A subagent running through the update: row orphaned, rendered failed, one notice. A
  further restart did not repeat it.
- Status of a packaged machine with its app closed: reproduced the exact error by
  closing the rig's app; 0.1.255 answers version, jobs and last receipt.
- A cancelled subagent, local job and remote job: gone from spawned-work:list and the
  rail DOM within 2 s on 0.1.255; old-after-cancel.png is 0.1.254 showing the dead row.

Trap: the screenshot endpoint can return a stale frame while the window is occluded.
Read the DOM over CDP, or the list, before believing a capture.
Trap: a rig renderer discovers real fleet machines by beacon. Forget them at once.
Trap: `workass fleet key` on a live daemon's state is not seen by that daemon; use the
renderer's fleetMint, and do not leave a stale key in the dialling daemon before 7d3f6120.
