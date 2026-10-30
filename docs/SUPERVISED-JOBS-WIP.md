# Supervised jobs — WIP design note (SUPERSEDED, kept for the reasoning)

> **Superseded on 2026-10-30.** This note is a snapshot of the investigation
> that preceded the implementation, and it is kept only because its seven
> open questions are the reasoning behind what shipped — several of them were
> answered differently from the lean recorded here. The authority is
> `docs/specs/fleet-runtime.md` §13 P4 and §14c (reporting is a call, not a
> file) plus `docs/specs/delegation-fabric.md` §5. In particular: problem (1)'s
> owner-side polling is NOT what shipped — the home offers the monitor and
> pushes over the mesh — and the spool this note designs around has been
> replaced by a SQLite transactional outbox that is a write-ahead log behind a
> direct, acknowledged API call. Every status line below is false as of
> 2026-10-30; read it as history.

Status on 2026-10-29: **investigation complete, zero lines of code written.**
Working tree clean, nothing committed, nothing built, nothing published,
nothing touched on duklerver. Paused at the operator's instruction for a
design review before implementation.

This note is the handoff into that review: what the task is, what the
codebase already gives us, the design I would have built, and the seven
places where I think the design is genuinely undecided rather than merely
unwritten.

## 1. What was asked for

First-class **supervised jobs**, remote-capable, so an agent never polls:
`workass_run_job` / `list_jobs` / `read_job` / `wait_job(s)` / `cancel_job`,
with heartbeats, resource locks, `depends_on` pipelines, graceful stop,
terminal states that always durably wake the owning chat, a per-machine jobs
UI, tests, docs, and a release including the Linux artifact.

The failure history to design against (all real, last 48 h on duklerver):
detached `nohup` + marker files + agent-side polling watchers; watchers died
with the agent session or at the harness 2 h cap; a job finished but never
wrote its DONE marker; a remote `pgrep -f` matched its own ssh command and
reported "running" for 5 h; a job crashed silently and nothing woke the owning
chat; a success-only wait that never fires when output never comes.

Every one of those is a *liveness-evidence* failure or a *notification*
failure. That framing drives the design below: liveness must never be a text
match, and the end of a job must be news the daemon delivers, not a file the
job remembers to write.

## 2. What the codebase already gives us (verified, with locations)

- **Tool surface.** `agentMCPTools()` in `cmd/workass/agent_mcp.go:28` declares
  schemas; `callAgentMCPTool` (`:342`) maps each tool name to one control
  method. Adding tools is additive and mechanical.
- **One routing authority.** `remoteAgentMethodKinds` in
  `cmd/workass/agent_remote_parity.go:41` is the single table deciding which
  machine a call reaches, with four classes: local-only, chat-scoped,
  machine-scoped, fork-scoped.
- **Outbound remote path.** `agentControlHandler.call`
  (`cmd/workass/agent_control.go:107`) consults that table and forwards via
  `rendererAgentRouter.Call` (`cmd/workass/remote_agent_router.go:59`), which
  broadcasts `agent:route-request` to the **local renderer**
  (`hub.BroadcastLocalShell`, `:88`). The renderer owns the machine
  credentials; the daemon never sees them.
- **Inbound remote path.** The destination daemon answers on its `agent:control`
  wire channel (`cmd/workass/agent_control_remote.go:40`) →
  `callAsController` (`:46`), which re-applies the actor fence and mints a
  short-lived owner key. Remote validation is the same code as local.
- **Existing background-work plumbing.** `internal/acp/spawned_work.go` (2549
  lines) already has the per-chat projection `SpawnedWorkItem` (`:80`),
  durable receipts, role work-vs-service classification, PID liveness probes
  (`spawned_work_probe_unix.go`), and the wake-on-settle integration the rail
  and obligation surfaces read.
- **duklerver already runs under `systemd --user` with linger enabled**
  (`docs/LINUX-HEADLESS.md:69`), restarting itself via `Restart=on-failure`
  (`:82`). So the user bus a transient unit needs is already present there.

## 3. The design I would have built

**Execution.** On Linux, each job is a `systemd-run --user` transient unit:
`RuntimeMaxSec` gives us `timed_out` for free, the journal gives us log tails,
and exit status comes from `systemctl --user show -p ExecMainStatus,Result`.
Critically, a transient unit is **not** in the daemon's own cgroup, so the
daemon can restart (or self-update) without killing its jobs, and re-attaches
purely by unit name. On macOS/Windows: a daemon-supervised child with
persisted state, reconciled on restart, and a vanished process marked `lost`
— never `running`.

**Supervision is by the parent, never by markers.** No DONE files. Liveness is
a unit state or a `waitpid`, never a `pgrep -f` pattern that can match the
query itself.

**Cross-machine ownership.** The fleet link is renderer-initiated only, and
duklerver is headless, so duklerver **can never push** a terminal state to a
chat on the Mac. Therefore the *owning chat's* daemon is the supervisor of
record: it stores a proxy record (owner chat + remote machine + remote job id)
and runs a **daemon-side reconciler** polling remote `job.read`. Daemon
polling is not agent polling — it survives the agent session, the harness cap,
and daemon restarts. See problem (1) for what this costs.

**Scheduling.** Resource locks are FIFO per named resource, per executing
machine (resources are physical). `depends_on` is a dependency DAG the daemon
runs, each step notified.

## 4. The hardest design problems (my own view)

**(1) The reverse-direction wake is architecturally missing.** This is the big
one. A headless daemon has no renderer and cannot initiate a fleet call, so a
job ending on duklerver cannot push to the Mac. Owner-side polling works but
means: *if the Mac daemon is down when a remote job ends, the wake is late* —
delivered on reconcile at next start. That may be fine (it is still durable,
still never silent). The alternative is to let a headless daemon initiate
routes, which is a real new capability at the edge of the frozen wire
contract. **I think this is the decision the review should settle first**,
because everything else hangs off it.

**(2) Supervised jobs deliberately break the external-work ownership law.**
`external.register` and `external.settle` are `remoteAgentLocalOnly` on
purpose, with the reason "external work tracks a process on this machine and
must stay in the exact owning Workass chat"
(`cmd/workass/agent_remote_parity.go:76`). A job whose process is on
duklerver and whose chat is on the Mac violates exactly that. The one-authority
routing table has **no class that expresses machine-scoped execution with
chat-scoped ownership**. Either add a fifth kind, or split each job tool into a
local half and a remote half. I lean fifth kind, but it widens the table's
contract and deserves the gate's attention rather than my own judgement.

**(3) One record or two?** Jobs could be a new `kind` of `SpawnedWorkItem`,
inheriting rail, obligation, receipts and wake for free — but inheriting the
same-chat-same-machine law from (2). Or a separate `internal/jobs` store that
must re-earn every one of those integrations. My lean: authoritative record in
a new store, **projected** as a spawned-work row so every existing surface
shows it. But that is a dual write, and this repo has already been burned by
mirrored state (the two-bridge mirror law). The projection must be
derive-only, one direction, no write-back.

**(4) `stalled` is not terminal, yet must still be news.** The spec keeps a
stalled job running by default. So it needs a one-shot non-terminal notice,
distinct from the "every terminal state wakes the chat" contract. Without that
distinction a stall either goes silent or spams the chat on every heartbeat
check.

**(5) macOS cannot honestly survive a daemon restart.** A child reparented to
launchd after the daemon dies has an exit status nobody can recover, so it is
genuinely `lost`. A status-writing shim would fix it — and a shim's output file
is *a marker file*, the very thing that failed us. It is only safe because the
absence of a status is itself treated as evidence (process gone + no status =
`lost`), unlike the old DONE-marker design where absence meant "still
running". Worth stating explicitly in the spec so nobody "fixes" it back.

**(6) Scheduler semantics need pinning before code.** Does queue time count
against `max_runtime`? (My answer: no — the timer starts at exec.) Does a
cancelled or failed dependency *fail* its dependents, or leave them pending
forever? (It must be an explicit terminal `cancelled`/`blocked`, else the
"never-starting dependency" test is an indefinite hang — one of the exact
failures we are designing against.) Is the resource queue per machine? (Yes.)
Each of these is a one-line decision and a test; getting one wrong recreates a
hang.

**(7) `wait_job` across machines cannot block.** Remote waits are clamped to a
2 s poll (`clampControllerWait`, `cmd/workass/agent_control_remote.go:126`)
because invokes share one ordered lane per client and a blocking wait would
stall every other operation against that machine. So a cross-machine
`wait_job` is inherently a convenience, not a guarantee. **The wake is the
contract; wait is sugar.** The tool descriptions have to say so, or agents
will lean on the one mechanism that cannot carry the weight — which is how we
got here.

## 5. What is explicitly not done

No code, no schema, no tests, no docs page, no `docs/WORKASS-TOOLS.md` recipe,
no commit, no build, no release, no activation. The only artifact of this lane
so far is this note.
