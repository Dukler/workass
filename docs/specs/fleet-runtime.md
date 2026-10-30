# The fleet runtime — one engine, many homes, no local/remote split

Status: SPEC v4 — **ACCEPTED 2026-10-30** (operator: "go"). §13 P1 (ids and
homes) shipped in `bb517b99`; §13 P2 (the mesh) is built — `internal/fleetclient`
dials, enrols with no human, keeps one heartbeat per pair, and `apply` to a
non-self home travels over that link with the §6 envelope, the renderer relay
demoted to the fallback §12 reserves it for. §13 P3 (logs, subscriptions,
monitors) is built — `internal/fleetlog` is the per-entity append-only log and
the durable monitor registry at both ends, subscriptions are cursors over the
link with store-and-forward and dedup by seq, `home_down`/`home_up` are
synthesized by the watcher's own daemon from the heartbeat, and subagents are
the first client: a subagent spawned from a chat on one daemon and hosted on
another reports its completion and attention INTO THE SPAWNING CHAT. P4 onwards
is unbuilt.
All §16 decisions are DECIDED, and five lessons from T3 Code's peer links
are adopted (§4, §6, §11): per-link pairing that is individually revocable,
narrowing-only permission forwarding with an immutable origin stamp and no
chaining, an identity proof before any credential with ordered routes, one
command surface reused across links, and one idempotency key shared by "launch
there" and "record here".
Origin: operator, 2026-10-29 — "somehow workass knows everything locally, but
its lost on network, and this is wrong, it shouldnt have this split".
This spec replaces the premise of `docs/specs/delegation-fabric.md`: that
document's delegation model, `workass run` supervisor, notice rules and
failure analysis all stand, but they now describe **one layer inside this
runtime** rather than a fabric bolted beside it. Where the two disagree, this
file wins. `docs/SUPERVISED-JOBS-WIP.md` remains absorbed.

## Binding laws for this lane (restated — executors do not inherit memory)

- Wire contract `invoke/reply/event` is FROZEN: new channels and additive
  fields only; no existing field changes meaning.
- Identifier law: never rename an existing field, id, path or function. A
  global id **contains** today's local id; it does not replace it.
- One writer per entity; one engine per chat; stdout purity for ACP code.
- `RedactSensitiveText` before any free text is persisted, emitted or sent,
  and again on arrival from a peer. Secrets never travel in argv.
- A wake is never a user message (the 2026-10-16 law).
- Chrome needs an operator-approved mock before it exists.
- Receipts: every claim in a lane report needs the command and its output.

## 1. What Workass is today, and exactly where it stops

**Inside one machine it is a small distributed system that happens to share
memory.** One daemon process holds: a **wire hub** (`internal/wire`) that
speaks `invoke/reply/event` over a TLS websocket to clients; a **chat actor
per chat** (`cmd/workass/provider_chat_runtime.go:249`), each with a reducer,
a durable ledger in SQLite and a single-writer lock, so every change is an
`Apply(command)` with an operation id that makes retries no-ops; a
**coordinator** that is woken (`actor.coordinator.Wake()`) whenever work
arrives; and the **ACP manager** (`internal/acp`) which runs provider CLIs as
child processes and owns subagents, observed background work, forks and
scheduled messages.

Everything "just works" locally because all of that is one process. A subagent
finishing calls a Go function on the chat actor
(`cmd/workass/provider_chat_subagent_completion.go`); a process child is probed
by reading `/proc` and a file on the same disk; a notice becomes a ledger row
and the hub broadcasts the new projection to every connected client. There is
no protocol anywhere in that path — just calls, a mutex and a wake.

**At the machine boundary every one of those things is missing, and nothing
replaced them.** The daemon has no outbound client: `internal/wire` only
*accepts* websockets (`wire.go:881` is an upgrade handler), so the only way one
daemon reaches another is to broadcast a request to the **renderer** —
`rendererAgentRouter.Call` → `hub.BroadcastLocalShell`
(`remote_agent_router.go:88`, `wire.go:587`) — which holds the per-machine
credentials and forwards it over *its* socket to the other daemon's
`agent:control` channel. So: the Electron app is the fleet's only bridge; with
it closed there is no fleet. Children on another machine therefore report into
*that* machine's chat (`agent_control_remote.go:103`), never to the chat that
asked. Entity ids are local, machine-tagged only as they cross the edge
(`M~machine~local`). And because none of this is uniform, a table decides
per method what is even addressable (`remoteAgentMethodKinds`,
`agent_remote_parity.go:39`), while `workass_list_chats` fakes a fleet view by
merging another machine's answer into the local one at the tool layer
(`mergeRemoteAgentChatList`).

That is the split, stated precisely: **locality is in the semantics, and the
UI is the transport.** Nothing is wrong with the local engine — it is good. The
error is that it was never given a way to talk to a copy of itself.

## 2. The principle

Every daemon runs the same engine. Every entity — chat, turn, subagent,
process child, schedule, wait, fork — has a **global id** and exactly one
**home daemon** that owns its write lock and its log. Daemons form a mesh of
direct authenticated links. There is one API; `route(id)` compares the id's
home to self and either calls in-process or sends over a link. **The local API
*is* the fleet API with `home == self`.** No method is "local-only"; some
operations simply require the machine where the thing physically is, and since
that machine is the entity's home, routing already puts them there.

The renderer becomes a client of its own daemon and nothing else. It is never
a bridge, never holds another machine's credentials, and closing it changes
nothing about the fleet.

## 3. Ids and homes

```
wa:<home-machine-id>/<type>/<local-id>
e.g. wa:m-7f3ad2/chat/app-chat-1792187130561
     wa:m-7f3ad2/subagent/wa-subagent-1792187130561-8
     wa:m-91be04/process/xw-6f12aa
```

- `type ∈ {chat, turn, subagent, process, wait, fork, forkrun, peer}`.
- The **local id is today's id, unchanged** (identifier law). Existing ids are
  valid local parts forever.
- The home is embedded, so routing needs no directory service and no lookup
  round trip.
- A move leaves a `moved_to` forwarding record at the old home with an epoch
  bump (§7), so the string stays resolvable after migration.
- The legacy `M~machine~local` tag is accepted on input and emitted only to
  clients that predate global ids.

## 4. The mesh

Trust has **two layers**, deliberately, and this is the T3 Code lesson that
most changes v3: fleet membership says *who may ask*, and a per-link pairing
record says *which peer is actually linked to me right now*.

- **Layer 1 — membership** is the fleet key: one ceremony per daemon, never per
  pair (`internal/fleet`, D3). A key-holder may *request* a link with any peer
  it discovers (`fleet:challenge` / `fleet:enroll`, `wire.go:1660`, `:1701`);
  the key never crosses the wire and neither does the derived token.
- **Layer 2 — per-link pairing.** Each accepted link is its own record on the
  **receiving** side: name, peer machine id, routes, when it was approved,
  which fleet key let it in, and last use. It is **visible and revocable
  individually**, so cutting one machine out never means re-keying the fleet.
  This reuses the device store that already exists and already anticipates it:
  `lease.Device` carries `Owner` and `KeyID` precisely so that "retiring a key
  can be told from revoking a device" (`internal/lease/lease.go`), with
  revocation already on the wire (`handleRevoke`, `wire.go:1861`). A peer link
  is a device of kind `peer`.
  A link may be approved either by a **pairing code** shown on the receiving
  machine (the existing join-code/QR path, `internal/fleetqr`) or, for a
  key-holder, automatically — the difference is policy on the receiver, not two
  mechanisms.
- **Revocation is immediate and honest.** Revoking a link stops its
  subscriptions and monitors at once; entities keep existing at their homes,
  and every affected watcher is told `link_revoked{peer, at}` on the same
  monitor stream as `home_down` — never silence, and never a fake outcome.
- **Discovery** is what already exists: the multicast beacon and machine book
  (`internal/machinebook`), plus addresses a human typed.
- **Identity before credentials.** Before any token is presented, the dialer
  requires the address to prove it is the expected daemon: read the identity
  card at `/workass/health` and match `machineId` (and the pinned certificate),
  exactly as `internal/machinebook/probe.go` already does. A wrong id is not a
  retryable error — it is a refusal, logged with the address.
- **Ordered routes, learned.** A peer record holds a *list* of routes in
  preference order — LAN addresses first, then a stable overlay/VPN address,
  with a relay reserved for later (§15) — each with its last success, latency
  and a **cooldown** after failure so a dead route is not retried in a tight
  loop. New routes observed from a successful inbound connection are *learned*
  and appended, never promoted above a human-entered one without a success.
- **Links.** Each pair keeps **one duplex authenticated link**, TLS 1.3,
  authenticated by the derived device token exactly as every other client
  (`?deviceToken=`, `wire.go:1071`). Either side may dial; the lower machine id
  retries on a shorter interval so two daemons do not race forever.
  New: `internal/fleetclient` (probe identity, dial, pair, persist token,
  invoke, subscribe) — the one capability the daemon has never had.
- **Link state** is `connecting | up | down(since)`. It is a property of the
  *pair*, published locally as an event, and it is the only timer in the
  runtime (a heartbeat ≈1 min, threshold ≈10 min).
- **No leader, no quorum, no consensus.** The runtime has no global invariant
  that needs agreement: every fact has exactly one writer (§6).

## 5. Transport = replicated event logs

Each entity has an append-only, sequence-numbered **log** at its home, which is
the authoritative history (it is also what the local engine already does: a
ledger with revisions, `internal/chat/sqlread.go:234`, plus per-run sequences
like `SubagentRun.ProgressSequence`).

- A daemon that cares about an entity **subscribes** with a cursor:
  `subscribe {id, after_seq}`. The home streams events; the subscriber applies
  them in order, deduplicating by seq, and keeps the last seq it applied.
- Delivery is **store-and-forward with acknowledgements**: the home holds every
  event until the subscriber acks, so a disconnect postpones events, it never
  drops them. On link up, the subscriber states its cursor and the home sends
  the rest. Nothing polls.
- A subscriber may serve local readers from its replica (read-anywhere, §6),
  and may answer "am I current?" with the watermark.
- Logs are bounded: compaction records `first_seq` and appends
  `truncated{to_seq}`; a subscriber below the watermark resyncs from the head
  (state + outcome + receipt) and says so, rather than failing.
- **Subscription is on demand, not total replication**: a daemon subscribes to
  what it monitors, what it is showing, and its own entities. A forty-chat
  fleet does not copy itself into every machine.

## 6. Commands, reads, and the death of the locality table

- **Write** = `apply(id, command, operation_id)` routed to `home(id)` and
  applied there by the existing single-writer actor path. Idempotent by
  operation id (as today). Local and remote differ in one `if`.
- **Read** = either the home, or any daemon's replica plus its watermark. The
  caller chooses: `fresh` (ask the home) or `cached` (serve the replica, state
  the lag). The UI uses `cached`, which is why **any machine's UI shows every
  machine**.
- `remoteAgentMethodKinds` is replaced by a single per-type annotation on the
  entity: which capabilities its home must have (a process child needs the
  machine with the files and the GPU; a chat needs the provider credentials; a
  secret reveal needs a local client). "Local-only" stops being a property of a
  *method* and becomes a property of a *place*, which routing already respects.
  The legacy table stays until §10's cutover is complete, then it is deleted.

### One command surface, not two

A link does **not** carry a second API. It carries the command surface that
already exists — an agent-control method name and its params — which is exactly
what the controller path does today when it rebuilds the local tools request
and calls the ordinary handler (`callAsController`,
`cmd/workass/agent_control_remote.go:46`). New tools therefore reach the whole
fleet the day they exist locally, and there is only one place where
authorization is written.

The envelope around a forwarded command is small and fixed:

```
{ v, target_id, method, params,
  limit,        // the caller's own permission/mode ceiling
  origin,       // immutable: chat, turn, operation, originating machine
  operation_id, // one key for "launch there" and "record here"
  epoch }       // the target's home epoch (§8)
```

- **`limit` can only narrow.** The receiving home intersects the link's grant
  with the caller's own limit and applies the lesser. A forwarded command can
  never obtain a permission the caller did not hold, and never one the link was
  not given. Local precedent: a subagent's mode is already derived from its
  parent's rather than chosen freely (`effectiveSubagentParentMode`,
  `internal/acp/subagents.go:696`).
- **`origin` is immutable** and travels with everything derived from the
  command, so provenance survives restarts and re-homing. Precedent:
  `OriginLaneID` / `OriginOperationID` / `OriginTurnID` on `SpawnedWorkItem`.
- **`operation_id` is one key for both sides.** The caller mints it once and
  uses it for the remote `apply` *and* for its own local record of that work.
  A retry therefore cannot create a second child on the home or a second row at
  the caller — the failure mode that made "launch there, record here" two
  half-truths. Precedent: `scheduledSendOperationID`
  (`cmd/workass/scheduled_messages.go:21`).

## 7. Monitors and links (Erlang semantics, one stream)

- `monitor(watcher_id, target_id, kinds)` — durable at **both** ends: the
  watcher's home records it so it survives restarts, the target's home records
  it so it knows who is owed. Returns a monitor ref.
- The watcher receives, on one ordered stream per monitor:
  `exit{outcome}` (= `settled|failed|lost|timed_out|cancelled`),
  `attention{kind,title}`, `waiting{conditions}`, `resumed`, and opt-in
  `progress`.
- **`home_down{home, since, last_known}` arrives on that same stream** when the
  link to the target's home is down past the threshold — synthesized by the
  *watcher's own* daemon. This is Erlang's `{'DOWN', ref, _, _, noconnection}`:
  it means **unknown, not dead**. A watcher must never convert it into an
  outcome. `home_up` follows, then the backlog from the cursor. One stream, one
  vocabulary, local and remote identical — which is the whole point of the
  design.
- One notice per home, not per child: a dark machine with twenty children
  produces one `home_down` notice listing them.
- `link(a, b)` is the bidirectional form: either side's exit reaches the other,
  which is how supervision trees cancel downward
  (`policy.on_cancel ∈ {cancel_children, detach_children}`) and how orphans are
  handled upward (`policy.on_parent_gone ∈ {finish, detach, cancel}`, with
  terminal logs parked in the home's inbox when the parent is gone).
- `demonitor(ref)` on consume; monitors are re-established automatically on
  link up; a monitor whose watcher no longer exists is dropped by the target's
  home (no leak).
- **Monitors are the only notification mechanism.** A chat notice is a
  projection of monitor events through the actionable filter (settled, lost,
  timed_out, attention, declared stall, `home_down`), coalesced per chat per
  60 s, capped per hour with failures exempt, suppressed when a live waiter
  will consume the result, when the parent turn was stopped, or when the parent
  cancelled the child — delivered as a task-notification notice, never a user
  message (the 2026-10-16 law and the five `aa53975e` anti-regressions, carried
  over verbatim from `delegation-fabric.md` §7).

## 8. Consistency model

- **Single writer per entity, at its home.** No two daemons ever write the same
  entity, so there is no merge, no CRDT, no conflict resolution — ownership
  *is* the resolution.
- **Per-entity total order** by seq; **no cross-entity atomicity.** A parent's
  record and its child's record may be momentarily out of step, and the cursor
  says exactly how far. Anything needing one order must live in one entity.
- **Reads are causal**: a replica never shows seq N+1 before N, and never
  invents state the home did not publish.
- **Fencing on migration.** A home hands an entity over by stamping
  `moved_to{home, epoch+1}` and refusing writes afterwards; the new home
  accepts only commands carrying the new epoch. Two writers cannot exist even
  if the old home comes back from the dead.
- **Tombstones.** A deleted entity leaves a tombstone until every known monitor
  has acked it, so "deleted" is never confused with "unreachable".
- **A home that never returns** is an operator action, not an automatic
  failover: reassigning an entity to a new home with an epoch bump is explicit,
  because inventing an outcome for work nobody can see is the exact dishonesty
  this design exists to remove.

## 9. What stays deliberately local

- **Secrets.** The fleet key is readable only by a local client
  (`fleet:reveal`, `internal/wire/fleet_keys.go`); provider credentials,
  tokens and native session files never replicate. A remote reader gets the
  *effects* of a secret, never the secret.
- **Physical things.** Processes, the filesystem, GPUs, cameras. These are why
  a home exists at all.
- **Provider CLIs and their native sessions** — a resumed session must run
  where its session file is; that is a home capability, not a limitation.
- **Large blobs** (images, artifacts, logs) replicate by reference and are
  fetched on demand from their home, bounded and redacted.
- **UI-only state** — focus, scroll, drafts, which window is open. It belongs
  to the client, not the runtime.
- **Daemon-side spawning stays a non-goal**: a process child is started by the
  client that registered it (`workass run`, `delegation-fabric.md` §5), so the
  daemon never lends its environment to a child.

## 10. What collapses into this

| Today | Becomes |
|---|---|
| `SubagentRun` + its completion delivery | entity `subagent`, monitored by its parent turn |
| `SpawnedWorkItem`, external work, "jobs" | entity `process`, home = the machine that runs it, started by `workass run` |
| `scheduledmessage.Entry` | entity `wait`, a condition held at its home |
| `ForkPoint`/`ForkRun`/`ForkChild` | entities `fork`/`forkrun`, one monitor per child, aggregate notice |
| `remoteAgentMethodKinds` (4 locality classes) | one home-capability annotation per type (§6) |
| `rendererAgentRouter` + `BroadcastLocalShell` relay | deleted as a *bridge*; the renderer is a client of its own daemon |
| Per-machine credentials held in the renderer | per-link pairing records in the daemon's device store, individually revocable (§4) |
| `agent_control_remote.go` controller path | ordinary `apply` at the home, same authorization |
| `mergeRemoteAgentChatList` | a read over replicas (§5) |
| `clampControllerWait` (2 s poll for remote waits) | monitors; waiting is an event, not a poll |
| `M~machine~local` edge tags | global ids (§3), tags kept as a legacy alias |
| Per-child liveness leases, reconcile ticks | link state + `home_down` (§7) |

## 11. Security

- Membership: the fleet key; authentication: the derived device token; both
  already shipped and unchanged (§4).
- **Authorization is per entity, not per peer.** A link grants nothing by
  itself. Commands carry the same owner-key/actor fence the local path already
  applies (`ValidateAgentOwner`, `fenceActor`), evaluated **at the home**.
  Monitor registrations carry a capability naming the watcher, the target and
  the event kinds — nothing else can be read through a monitor.
- A **sandbox** travels with a delegated entity: allowed cwd roots, provider,
  resources, `max_runtime`, and whether it may delegate at all. Today a remote
  spawn silently inherits the host chat's defaults; here the parent states what
  the child is permitted to be, and the home enforces it.
- **Narrowing only, and no chaining.** A forwarded command's effective
  permission is `min(link grant, caller limit)` (§6). Work that arrives through
  a link **below full access** additionally may not:
  - use the receiving daemon's **own** links — no second hop, so a narrowed
    grant cannot be laundered into a wider one by passing through a third
    machine;
  - change settings, projects, schedules or updates on the receiving machine —
    concretely `chat.configure` beyond its own entity, `schedule.*`,
    workspace/project mutation and `update.*` are refused with the capability
    named.

  This **replaces `max_depth`**, which is now removed from the sandbox: a
  permission rule is strictly simpler than a counter, needs no bookkeeping, and
  bounds the thing that actually matters (escalation) rather than the thing that
  does not (depth). A full-access chain is the operator's own fleet acting on
  their behalf; its storm guards are the per-peer rate limits and the notice cap,
  not an arbitrary number.
- **Refusals name the missing capability.** A home that will not host a kind, or
  will not accept a narrowed command, answers immediately with what it lacks —
  never a silent queue and never a generic denial.
- Blast radius of a compromised daemon: it can lie about **its own** entities
  to the watchers that asked about them, and nothing else. It cannot write
  another home's entity, enumerate chats it hosts nothing for, or read a
  secret.
- Per-peer rate limits and abuse counters (precedent: failed enrolments,
  `wire.go:1728`). Audit lives in the logs: `grant_refused`,
  `cancel_requested{by}`, `resumed{by}`, `link_revoked` and every forwarded
  command's `origin` are events, so "who asked for this, through which link,
  under what limit" is answerable per entity after the fact.

## 12. Migration, without a big bang

- **M0 Homes for free.** Every existing entity is assigned `home = this
  daemon`, and its global id is built from its existing local id. Nothing
  moves, nothing is renamed, no data migration runs.
- **M1 The mesh is an extra transport.** `fleetclient` links ship alongside the
  renderer relay; routing prefers a direct link and falls back to the relay.
  Semantics do not change, so this phase is observable only as "remote calls
  now work with the app closed".
- **M2 One subsystem at a time** onto monitors — subagents first (they already
  own the notice path), then process children, then forks, then waits — each
  cutover gated by a **projection-parity test**: the chat's rows, cards and
  receipts must be byte-identical before and after.
- **M3 Replicas and read-anywhere UI**, which deletes the tool-layer chat merge.
- **M4 The locality table** is reduced to home-capability annotations and then
  removed; the relay bridge path is deleted last.
- **Mixed versions are a steady state**: a peer without mesh support is reached
  through the legacy relay and treated as a home that answers commands but
  publishes no logs and hosts no monitors. It keeps working, with yesterday's
  latency and no cross-machine UI.

## 13. Phases

**P1 — ids and homes (local only).** Global ids containing today's locals,
`home(id)`, `route(id)` with every call resolving to self. Tests: every
existing suite green; legacy `M~` input still resolves; a projection-parity
test over chats, subagents, work and forks.

**P2 — the mesh.** `internal/fleetclient`, duplex links, per-link pairing
records, identity-proof-before-credential, ordered routes with cooldowns, link
state and heartbeat, and `apply` routed to a non-self home carrying the §6
envelope. Tests: key-holder pairing and pairing-code pairing both produce one
revocable record; an address that answers as the wrong machine id is refused
before any token is sent; a revoked link stops subscriptions at once, emits
`link_revoked`, and leaves every entity intact; revoking one link leaves the
other links and the fleet key untouched; routes are tried in order, a failed
route cools down instead of spinning, and a learned route never outranks a
human-entered one without a success; forged/expired tokens refused; a command
to another home succeeds with **no renderer process anywhere**; dial races
converge on one link; a forwarded command with a wider `limit` than the link's
grant is narrowed, and one below full access is refused the second hop and the
settings/schedule/update surfaces with the capability named; one
`operation_id` replayed creates exactly one child and one local row; the relay
still serves an unlinked peer.

**P3 — logs, subscriptions, monitors.** `subscribe/ack`, store-and-forward with
dedup by seq, monitors/links at both ends, `home_down`/`home_up` on the same
stream, subagents as the first client. Tests: a four-day outage delivers the
backlog once, in order, with no poll running; a home restarted between append
and delivery still owes exactly those events; a watcher restarted between apply
and ack deduplicates; `home_down` never becomes an outcome; one notice per dark
home regardless of child count; a monitor survives both daemons restarting; a
dropped watcher leaks no registration.

**P4 — process children.** `workass run` (preflight, process group / scope /
Job object, report-and-acknowledge, exit code), `lost`, `timed_out`, one-shot
stall, refusal of evidence-less registration. Tests: child survives a daemon
restart *and* a self-update; killing the supervisor yields `lost`, never
`running`; the token never appears in argv. Plus §14b's `process.start`:
refused below full access and refused locally, both by name; the job's
environment contains nothing of the daemon's; one credential authorizes that
one job's reports and dies with its outcome; the supervisor's argv names the
asking chat as `--owner`, the daemon's state directory as `--state-dir`, and
the credential only as a path. Plus §14c's reporting gates: the daemon-up path
delivers over the Agent API with an acknowledgement and reads no file; a
daemon-down-then-up window replays the outbox in order exactly once; a crash
between the insert and the send replays once; an unreachable daemon is loud on
stderr AND in the receipt and keeps trying; a wrong or relative state
directory is refused before anything runs; concurrent supervisors share one
outbox without losing a seq; and **a remote start from a chat on P produces
exactly one notice in that chat on P for each of settled, refused, timed_out,
cancelled and lost.** Plus §14e's: a real `workass run` started on a host with no
chats at all reports every one of those outcomes to its owner.

**P5 — waits and forks collapse.** Declared `waiting{conditions}` resumes the
same native session; an undeclared wait raises `attention{ended-while-waiting}`
from the disposition tier. Tests: resume with context on both providers,
across engine recycle and daemon restart; fork runs notify once in aggregate.

**P6 — read-anywhere.** Replicas, watermarks, `fresh|cached` reads, one UI
showing every machine. Tests: the UI on a peer lists and reads chats hosted
elsewhere; a stale replica is labelled, never silently old; deleting the
tool-layer merge changes no output.

**P7 — migration and tombstones.** `moved_to` + epoch fencing, explicit
reassignment, tombstones acked by monitors. Tests: an old home that returns
after a handover cannot write; a reassigned chat keeps its id; a deleted entity
is never reported as unreachable.

**P8 — remove the scaffolding.** Delete the locality table and the relay
bridge; `docs/WORKASS-TOOLS.md` recipes for monitors; retire polling and mirror
doctrine from agent-facing docs. **Also update `CLAUDE.md`'s detached-lane
paragraph**: it still states that the daemon "never injects a synthetic chat
message or starts a turn on completion", which `f9a096f1` (2026-10-16) already
changed for subagents and which P4 extends to every entity. The law it should
state is the current one: a completion reaches a chat only as a
task-notification notice, never as a user message, and never for a
non-actionable event.

## 14. Failure table

| Failure | Handled by |
|---|---|
| App closed, fleet unreachable | the daemon has its own links (§4); the renderer is just a client |
| Child reports to the wrong chat | the parent is an entity id, not a machine-local chat (§3) |
| Delegator's session restarts | entities live at homes with durable logs, not in harness state |
| Child finishes while nobody watches | monitor `exit` → actionable notice (§7) |
| Child dies silently | its home sees a gone pid with no outcome ⇒ `lost` |
| Supervisor itself dies | pid gone, no `settled` ⇒ `lost` |
| Daemon restart or self-update mid-child | process children are not daemon children; events are queued in the outbox and replayed in order on reconnect |
| Supervisor cannot reach its daemon | loud on stderr and in the receipt, retried forever, and the row reads `not reporting` rather than healthy (§14c) |
| A job healthy and quiet vs a job whose reports are lost | the supervisor heartbeats on a declared cadence; three missed beats with a live pid is `not reporting`, which is news and never a verdict (§14c) |
| Supervisor dies with an outcome still queued | its home drains that child's outbox by id before reaching `lost` (§14c) |
| Home powered off, crashed, cut off | `home_down` on the monitor stream, one notice per home |
| Stale `running` after a home goes dark | `home_down` means unknown; the UI shows `last known` (§7) |
| Delivery interrupted | store-and-forward: unacked events stay owed (§5) |
| Same event delivered twice | dedup by seq |
| Partition for days | backlog from the cursor on link up; nothing polls |
| Two daemons writing one entity | impossible: single writer + `moved_to` epoch fencing (§8) |
| Home never returns | explicit operator reassignment with an epoch bump; no invented outcomes |
| Deleted vs unreachable confusion | tombstones acked by monitors (§8) |
| Version skew | structured refusal naming the supported range; relay fallback; mixed fleet is supported (§12) |
| Hostile or compromised daemon | per-entity authorization at the home; it can only lie about its own (§11) |
| Monitor leak / unbounded logs | monitors dropped with their watcher; logs compacted with `truncated` + resync |
| Secret exposure across machines | secrets never replicate; reveal needs a local client (§9) |
| Oversized blob or leaked text | by-reference blobs, byte caps, redaction on send and on arrival |
| Notification storms | actionable filter, 60 s coalescing, hourly cap, one notice per home |
| Escalation through a third machine | narrowed work may not use the receiving daemon's own links (§11) |
| A paired peer must be cut off | revoke that one link; fleet key and other links unaffected (§4) |
| An address impersonating a daemon | identity proof before any credential; a wrong id is a refusal, not a retry (§4) |
| A dead route retried in a loop | ordered routes with per-route cooldowns (§4) |
| "Launch there" succeeded, "record here" retried | one `operation_id` for both sides (§6) |
| Clock disagreement | ordering is by seq; expiries are read only by their issuer |
| UI showing stale fleet state | replicas carry watermarks; `fresh` reads are one call away |

## 14b. What the first live P1–P4 test changed (2026-10-30)

The operator drove duklerver from the Mac and reported five findings. Four were
bugs in this lane's own code and are fixed where they were; two of them change
what this document says, and both changes are recorded here rather than left as
a difference between the spec and the product.

**§6 gains a HOST TARGET on `agent.spawn`.** The spec already held that where an
entity runs and who owns it are two facts, but the Agent API expressed only one
of them: the single way to name a machine was the machine-tagged `tab_id` +
`chat_id` of a chat on it — which also changed the OWNER. A subagent of the
CALLING chat, hosted elsewhere, was therefore inexpressible, and the attempt
produced a child of the other machine's chat: a row under a conversation that
never asked for it, which the operator read, correctly, as "not a subagent, not
even a child chat". `workass_spawn_subagent` now takes `machine_id` as the host
while the owner stays the calling chat; the host registers no row and the asking
chat holds the row, the monitor and the notice. A hosted subsession is REFUSED
by name until P6 (a chat hosted elsewhere cannot be listed under its parent
here), and a hosted fork is refused by §9 (a native session runs where its file
is).

**§9 and §15's "daemon-side spawning" non-goal is AMENDED, narrowly.** The
non-goal's stated reason is that the daemon must never lend its environment to
a child, and that reason stands. What the test found is that the non-goal also
removed the only way to START supervised work on the machine that has the
files: `workass run --owner <global chat id>` already reports into a chat on
another machine, but registration is local by design, so somebody had to be
standing on that machine to run it — and `workass run` from a plain shell there
says so ("no Workass credential in this environment ... but no chat is told").
`process.start` closes that gap under five conditions, all enforced:

1. it is refused on a link below full access, naming the capability (§11);
2. it is refused for the local machine, where the caller has its own shell —
   which is where the non-goal's reason actually applies;
3. the job's environment is BUILT, not inherited: PATH, HOME, the data root,
   its own credential path, and the variables the caller named. Nothing of the
   daemon's environment reaches it, so no secret can;
4. the only credential it holds is a token good for exactly one method
   (`process.register`), one job and that job's one owner, revoked the moment
   that job is terminal — it lasts the job's life rather than one call,
   because §14c made reporting continuous rather than a closing poke. It names
   no chat, so it authenticates as nobody (§14e);
5. it is still `workass run` that supervises, and P4's `lost` verdict is unchanged.
   (This said "in its own process group", as if that kept it from the daemon's
   fate. Under a systemd service it did not: §14g.)

The three remaining findings needed no spec change: a self-addressed
`update.status`/`update.apply` lost the `machine_id` the updater is addressed by
all the way down (the strip that routes a self-addressed call locally was not
undone for the renderer half); the headless updater's operation-id alphabet was
stricter than the product-wide one and refused an id for its dots without
saying so; and `workass fleet links` read a REDACTED peer and then asked
"linked?", which is "has a token" — so every paired peer printed as unlinked
while the link was up and heartbeating. The same display also showed nothing
for the reverse direction, because a dialler never stated the port it listens
on, so the receiving daemon learned no route back and could not dial or
heartbeat the pair from its own side. All four are fixed with tests.

**The reverse-direction fix needed a second pass (2026-10-30, later).** Stating
the port inside `enroll` was not enough, because enrolment happens ONCE per
peer lifetime: a pair that linked before a port was ever stated reconnects with
its stored token, never re-enrols, and can never learn the route. The port
therefore rides the HEARTBEAT, joined by the receiver to the host its own
socket reports and never to anything the peer names — so every already-paired
pair self-heals on the next beat. Two consequences are part of the fix: a route
restated every minute must not be a disk write every minute, so `AddRoute`
returns without saving when nothing changed; and once both ends can dial, the
pair would hold two sockets, so a dialler asks whether the peer already has a
live inbound link before dialling and publishes `up` for an inbound-only pair.
One duplex link per pair, whichever side opened it, as §4 says.

## 14c. Reporting is a call, not a file (2026-10-30)

The first live two-machine run of `process.start` started four jobs on
duklerver from a chat on the Mac. All four returned `ok` with the right owner.
All four ran to completion and recorded their whole lifecycle. **Not one notice
ever reached the owning chat.**

The proximate cause was a path: `workass run` resolved its state directory from
a default that ended in a bare relative `state`, so with `cwd=/tmp` four jobs
wrote `/tmp/state/process-children/<id>/events.jsonl`, where no daemon would
ever look. But the operator's reading of it was the one that mattered — *"we
are sending events THROUGH FILES INSTEAD OF A TOOL?"* — and it is correct. The
spool had become the PRIMARY record, so a supervisor that never reached its
daemon was indistinguishable, from the outside, from one that had. A wrong
path was merely the first thing that could go wrong silently.

So the direction is inverted. This supersedes §5's "events are appended to a
spool directory; the daemon ingests the spool on start and on a tick" and the
P4 line's "spool-and-ingest".

**1. The primary path is the Agent API.** The supervisor reports every
transition — `created`, `queued`, `started`, `stalled`, `attention`,
`cancel_requested`, `settled`, and a `heartbeat` — to the daemon on the machine
it is running on, through `workass_register_process_child`: the same
authenticated tool surface an agent calls, with the credential minted for that
one job. Each report carries the registration facts and the batch of events the
daemon has not acknowledged; the daemon applies them in seq order, dedups by
seq, and answers `ackThrough`. It then publishes them as P3 defines — the
entity log, the monitors at both ends, the owning chat wherever it is. One
verb: a restart that lost the in-memory row rebuilds it from the same message
that delivers the backlog, and a job that ends while its first report is still
travelling needs no second call.

**2. The outbox is a write-ahead log, not a channel.** It is a SQLite
transactional outbox at `state/process-outbox.db` — WAL, `busy_timeout`,
`synchronous=FULL`, one small database per machine shared by every `workass
run` on it, opened through the same `internal/store` that opens `state.db` and
deliberately NOT `state.db` itself, which is the daemon's single-writer ledger
and holds an exclusive lock a supervisor could not wait out. Each event is
INSERTed with its per-child seq *inside the transaction that allocates it*
before the report goes out; the daemon's acknowledgement marks the row
delivered; anything undelivered is replayed in seq order on reconnect, exactly
once. SQLite rather than another JSONL file because a queue cannot afford a
torn last line, a reader that parses to find the end, a seq allocated outside
the write that records it, or a directory listing standing in for an index.

**The daemon never discovers a job by scanning anything.** It learns that a job
exists from its report. It reads the outbox for exactly ONE case — a child it
already holds a row for, whose supervisor has died with events still
undelivered — and it looks that up by id, because a supervisor that crashed
between recording its settle and delivering it is not `lost`, and reading the
queue it left is the difference between the truth and a guess. The outbox path
comes from the credential the daemon wrote or from `--state-dir`, never from
the job's working directory: a relative state directory is refused before
anything runs.

**3. Never silent.** Three rules, and they are the whole point of the rewrite:

- A supervisor unable to reach its daemon for longer than the grace says so on
  stderr, in the words "NOT REPORTING", naming the gap and stating that the
  work continues; it repeats that on an interval, not per retry; and it keeps
  trying for as long as it lives.
- Its receipt carries `reported`, `pendingEvents`, `reportGapMs` and
  `reportError`. A run that finished perfectly and could tell nobody is a
  different outcome from one that finished perfectly, and the receipt says
  which. A finished supervisor keeps trying to deliver its outcome until a
  deadline and, if it fails, says `THIS OUTCOME WAS NOT REPORTED`.
- The owner-side row cannot look healthy about a supervisor nothing has heard
  from. The supervisor heartbeats on a declared cadence (≈1 min, the link
  heartbeat's, not a second knob); three missed beats with a live pid marks the
  row `not reporting` and raises ONE `attention{not_reporting}`. It settles
  nothing — the work may be going perfectly — and the next report clears it.
  The gap the supervisor reports on reconnect is kept on the row, so one that
  healed still shows that it happened.
- A run with no credential is REFUSED rather than run quietly. `--unreported`
  is the explicit opt-in for the supervision without a chat, and it says so.
- A daemon that accepts a report without acknowledging any event is treated as
  a failure, not an ack: it is older than this contract, and reading its
  silence as agreement would retire the only copy of something nobody has been
  told.

**4. A refusal is news.** Preflight used to refuse synchronously and record
nothing, on the reasoning that nothing started so no chat needed a row. That
holds only when the caller is standing there reading the receipt. A job started
from another machine has a chat waiting, and a refusal it never hears about is
indistinguishable from a job that vanished. A refusal is now reported like any
other outcome.

**5. Three silent drops on the owner's side, found by the test this section
required.** Each was its own instance of the same disease and each is fixed:
a monitor offered for a chat on another machine carried no tab id, and the
notice admission fence compares the tab exactly and dropped the mismatch
without a word — the owner's own machine now fills its tab in, because it is
the only machine that knows it. The host exported its OWN lane and turn ids as
the owner's, and admission refuses a lane it cannot find — a remote owner now
carries none of the host's local ids, and an absent lane falls through to the
chat's desired lane exactly as a local completion does. And the event payload
reader accepted only `float64` and `int`, while the wire encodes numbers as
strings, so every exit code of every job reported over a link was dropped: the
one fact a finished job is judged by, absent, with nothing saying so.

## 14d. The second live run: an empty string, and a chat with nobody in it (2026-10-30)

The reporting rewrite of §14c was published as v0.1.239 and tested live on the
same pair. Four jobs again, again all `ok` with the right owner, again every
event correct in the outbox — and `delivered_at` NULL on every row. The daemon
had acknowledged nothing. The repo's two-daemon end-to-end test passed
throughout.

**Two refusals in series.**

The first was one empty string. `workass run` stated `role: ""` on every
report, and `role` is an enum on the tool it calls (`work | service`), so that
is not "no role" — it is a value outside the enum, and the daemon's own
argument validator refused the WHOLE report before the registration was
reached. Instantly, identically, every time. The supervisor read the refusal as
"the daemon is unreachable", waited out its grace, and said so on stderr.

The second was behind it. A registration resolves the owning chat's provider,
and it asked only this daemon's LIVE session bindings — which a supervised
job's host chat does not have. That chat is a record holder: nobody prompts in
it, so after a restart nothing is bound to it, and the registration answered
"external work registration is unavailable for this Workass ACP provider". The
chat's DURABLE provider is now the fallback behind the live binding. (§14e
removed the host chat itself; the fallback stays for a local `workass run`.)

**And the thing that hid both.** `process.start` gave the supervisor it
launched no stdout and no stderr, and passed `--quiet`. So "NOT REPORTING" —
the one sentence §14c promises is never missing — was written by a process with
nowhere to write it, and the closing receipt went the same way. Three fixes,
and they are the general lesson rather than this bug's detail:

1. **The supervisor's own stream is captured.** `<output>.supervisor.log`, next
   to the job's output, holding the diagnostics and the final receipt. Not
   `--quiet`: the one caller that cannot read a receipt is the one that must
   keep it.
2. **The row exists from the LAUNCH, not from the first report.** Every way
   this product notices a job that cannot report — the silence notice, the
   `lost` verdict, the drain of a dead supervisor's outbox — hangs off a row,
   so a supervisor whose every report was refused left nothing behind at all
   and the job looked, from the owner's side, like one that had never been
   asked for. The launch row loses every race it can lose: a receipt for that
   work id means the job has already finished and nothing is resurrected.
3. **Silence is measured against one beat for a job this daemon started.** A
   supervisor reports as it starts, so a minute of nothing is already news; the
   notice says it has never been heard from and names the log to read.

**The test that would have caught it.** Every test built the report payload by
hand, so they all spoke a dialect the daemon accepts while the supervisor spoke
one it refuses. There is now one constructor — `procchild.ReportArguments` —
and the daemon's own test asserts that ITS output passes the validator that
refused it, for a declared role and for none.

**And the owner's own list.** `workass_list_process_children` listed only rows
homed on the machine it was called on, so a chat whose every job runs elsewhere
— the case `workass_start_process_child` exists for — asked the one tool meant
for watching them and was told it had none. It now folds in the jobs this chat
owns on other machines, enumerated from the monitors it already holds (so the
set is this chat's own work and nobody else's), each marked with the
`host_machine` it runs on, with a home that cannot be asked NAMED rather than
dropped. Forwarded numbers are normalised on arrival, because the transport
hands some of them back as decimal strings and a list whose local rows carry
`0` and whose remote rows carry `"0"` is one nobody can write a comparison
against.

## 14e. No chat on the host (2026-10-30)

§14b's `process.start` required `host_tab_id` + `host_chat_id`, a real chat on
the running machine to hold the job's row, called a record holder because
nobody prompts in it. What the operator saw was an empty chat on duklerver that
had nothing to do with the work, and the ruling was "it shouldnt have this
split". The split was a contradiction of §6 and of §14b's own first finding:
hosted subagents already keep where it runs and who owns it as two facts.

**Starting a remote job needs a machine and a job.** Both fields are gone from
the schema and the wire method. On the host:

- **The row is keyed by the job's id and names its owner**, with no chat. No
  actor fences it, no rail shows it, no provider is looked up for it, and no
  session binding can refuse it. It lives in the same registry as every process
  child, so list, read, cancel, silence and `lost` are the code that exists. A
  row whose chat fields are empty is by definition hosted.
- **Its durable record is `state/hosted-process-children.json`**, because the
  per-tab snapshots and receipts that keep a chat's row are what it does not
  have. A settled row leaves the list as a chat's settled row does, but stays
  readable by id, the most recent 64 of them, so a replayed start finds the job.
- **The credential is bound to the job id, the owner and the operation id**
  (§14b condition 4). It is narrower than the one it replaces, not wider: it
  resolves to no chat, so no chat-authorized path can accept it, and the tool
  endpoint admits it for `workass_register_process_child` alone, by POST, with
  no chat stated; it is not even a catalog reader. A report naming another job,
  another owner or any chat is refused.

The owner side did not change, because it never depended on any of this: the
monitor is offered to the owner by global id, the notice comes through it, and
`workass_list_process_children` folds the job in from that monitor with its
`host_machine`. The asking chat holds the monitor and the notice. It does not
yet hold a rail row as a hosted subagent's chat does, which is a surface change
and belongs with the UI work.

**Mixed versions, both directions, because machines update one at a time.**

- *New owner, old host.* The old host answers with guidance about two fields
  this tool no longer has, before it launches anything. The owner refuses by
  name instead: the machine runs an older Workass, nothing was started, update
  it. That reads a fixed sentence out of builds that have already shipped,
  because a link carries no capability list to ask.
- *Old owner, new host.* The old owner still sends the two fields and the new
  host does not read them, so the job runs and its chat is not involved.
- *A job already running when its host updates.* It holds a token bound to a
  chat on the host, its row is in that chat, and its supervisor reports with
  what it has. Those records keep working, with exactly the old checks, until
  the job ends; none is ever minted again. This is durable state belonging to
  work in flight rather than a compatibility surface: cutting it off would be
  the silent job §14c exists to prevent.

**A job's wait is not its run.** A notice reported elapsed 5h45m35s beside a
result saying "Finished with exit 0 after 2h47m22s", and the list showed a job
at queue position 2 as `running` with a `started_at` from when it was queued.
Three facts now stay apart: `queued_at`, when it was asked for; `started_at`,
when its command started, absent until then; and its run time, which the
supervisor measures from exec and states when it settles. The list reads
`status: "queued"` until exec. That word is for readers: the row's own status
stays the closed lifecycle in which anything but `running` has ended, which is
what a supervisor's report is answered with and what spends its credential.

A notice carries `<run-time>`, plus `<queued>` when the wait rounds to at least
a second, and a job that never ran carries no run time. A receipt from a host
that predates the split keeps `<elapsed>`, because that number does include the
queue and calling it a run time would be false. A job that straddles the update
still gets both numbers right, since its supervisor always measured from exec.

`stale_after` cannot call a wait a stall: the stall clock starts at exec, and
`max_hold` is read only from a holder. Nothing in the daemon infers a stall on
its own. A supervisor keeps heartbeating while it waits, so a queued job is not
`not reporting` either.

The same split holds for a local `workass run` in the surfaces that read a chat's
actor: `workass_list_spawned_work` and `workass_list_spawned_work_receipts` state
the same facts in their own camelCase, and read `queued` the same way. The rail's
wire is frozen and an older renderer reads any status but `running` as finished,
so `spawned-work:list` keeps `status: "running"` and `startedAt` and adds
`queuedAt`, `execStartedAt` and `runMs`. The renderer derives the word from those:
the row says `en cola <wait>` in its elapsed slot with no running pulse, and a
finished row's duration is its run alone. The sidebar's clock is how long the chat
has been busy, not a run time, and still counts a wait.

**A retried closing report is a success.** A job's credential used to be deleted
the moment its job was terminal, so if the reply to the closing report was lost,
the retry was refused `unauthorized` and the supervisor logged THIS OUTCOME WAS NOT
REPORTED about a job whose owner had been told. It now closes instead, and answers
for ten minutes, several times the two a supervisor keeps trying. A closed
credential resolves to no chat and applies nothing: it can only hear its own job's
final status, `replayed: true`, with the events it carried acknowledged. A local
run reporting on the chat's own credential met the other half: its settled row may
already be compacted, and the retry brought a finished job back and told its owner
twice. A closing report with no row and a receipt now gets the same answer.

**One operation id is one job, now actually.** A replayed start used to launch a
second supervisor over the first one's id, output file and stop file. It now
answers with the job that exists, `replayed: true`, and an id already owned by
another chat is refused.

## 14f. Work on another machine is a row like any other (2026-10-30)

§14e said the asking chat "does not yet hold a rail row". That was a regression,
not a deferred surface: removing the host chat removed the only place the app
showed a remote job, and the remote subagent's row said only "Running on
<machine>". The operator's ruling: remote jobs and remote subagents show
normally, the same row a local one has.

**The row is the ordinary one, and the host states what it says.** The owner
holds a row with `hostMachine` set; only the machine named there can say anything
about its life, so nothing here probes its pid, opens its path, or orphans it on
a restart. A hosted subagent uses the `subagent` row and a hosted job the
`external` row, so avatar, label, time, status, model, activity and stop are the
code that exists. The machine is shown beside the activity and never replaces it.

**`fleet:state` is a value, not an event.** The host states the latest phase,
activity, model and queue facts, and each tool call of a child. It is not written
to the entity log, cannot become a notice and is not acknowledged: progress must
not grow a log or wake anybody (§5). A statement lost to a dropped link is stated
again in the answer to the sync that link's return causes, and a job restates
itself on every report, heartbeats included. The log still carries the outcome.
A child's calls go into the parent turn as the same events a local child emits.

**The statement creates the row.** That covers a job this chat started, a job
started on the other machine with `--owner` naming this chat, and a job that was
already running when this daemon updated. The monitor is the authority, as it is
for the log (§11), and a statement crossing the job's own end brings nothing back.

**Stop and output go to the host.** The stop reaches the host's `process.cancel`,
or `agent.cancel_hosted` for a child, which only the machine the child runs for
may call. The row ends when the host says the work has, including a cancel, which
notifies nobody. A stop that cannot reach the host answers with the reason and
can be retried. It deliberately bypasses the chat actor's stable operation id,
which would otherwise replay the first failure forever.

**A push reaches a peer over either direction of the pair.** A home used to push
only to a peer that had dialled it, so for a pair linked from the host's side
every push was dropped and an outcome waited for the next sync. It now falls back
to the link it dialled itself, through `fleet:push`, without ever dialling to
deliver one.

## 14g. A process group does not escape a cgroup (2026-10-30)

A 3-hour training job started with `process.start` died at update 10,464 of 12,000
when its machine updated. Its row said `isolation: process_group`, and this spec said
that was what made a job survive a restart. On Linux under a systemd service it is
not: when the daemon exits, the stock `KillMode=control-group` kills everything left
in the unit's cgroup, and the supervisor was in it. Nothing in the product measured
that, and the owner found out from a late `lost`.

**The supervisor moves itself.** On Linux, inside a service's cgroup, `workass run`
re-execs through `systemd-run --user --scope`, before it has done anything. That asks
the user manager to adopt the calling process into a new cgroup and then execs the
command, so it is the same pid throughout. It needs no root and no change to the unit.
A machine with no reachable user manager cannot do this: the supervisor says NOT
ISOLATED and runs anyway. On a unix without cgroups a run with no terminal leaves the
caller's process group, which is all launchd and the app updater reach for. On Windows
the updater stops every process running from the install's executable, so a supervisor
the daemon starts runs from a private copy. The Windows half has been compiled, never
run.

**Survival is measured, never assumed.** `procchild.SurvivalOf` reads the real pid when
asked: its cgroup against the daemon's and the unit's KillMode on Linux, its process
group elsewhere, its image path on Windows. So the answer is also true of a supervisor
from an older build. The job row carries `survives_restart` and `survival_note`, and
`process.start` answers with them and a warning when a job is exposed.

**The updater states it and does not block.** `update.status` gains `runningJobs`: what
is protected, what would die, by name, and one sentence saying so. A person decides.

**A `lost` reaches the owner in seconds.** The live delay was not the verdict. The
owner's link supervisor looked only on its next tick, and a push went only to a peer
that had dialled the host. A link now retries within a second of its socket dying and
dials at once on boot, a push also uses a link this side dialled, and the grace before
`lost` is one reconcile tick, because the outbox is drained before the verdict. A job
whose supervisor died says so if its process is still running.

**Exactly one notice, and no silent drop.** The exit goes to the log before the row is
recorded, and the log refuses a second exit for one job. A monitor adopted from a sync
gets its tab from this machine, because the admission fence drops a notice with no tab
without a word.

**§5's "nothing polls" is amended, narrowly.** While a monitor is still owed its outcome,
a heartbeat asks the home for what it holds, at most every two minutes, and a new monitor
asks once. A push can be lost in several ways nobody hears about, and "nothing polls" had
no answer to that: an exit stayed in its home's log for as long as the link stayed up. It
rides the existing heartbeat and costs nothing once every monitor has its outcome.

**A row with no monitor adopts one.** Every remote subagent spawned before this had none,
because the spawn path read its own answer wrongly. Registering returns the log from the
start, so an exit already there arrives once. A home with neither the child nor an exit
for it says so, and the row ends failed rather than running forever.

**Run time.** A row that predates §14e reads when its command started from the outbox. With
nothing there it claims no run time and reports `<elapsed>`, because a figure that includes
the queue is not one. A chat's settled row is compacted into its receipt, and a read by id
now answers from that receipt rather than "not tracked".

**A child on a stopping host fails, with that reason.** It does not survive, and nobody
asked for it to end, so it is neither Cancelled nor suppressed.

Live proof: `docs/evidence/remote-rail-20261030/RESULTS.md`, which also says what is unproven.

## 15. Non-goals and deferred

Non-goals: consensus, leader election or automatic failover; a global
transaction across entities; a DAG engine; replicating secrets or large blobs
by value; a cloud relay for peers that cannot see each other.

Amended 2026-10-30 (§14b): **daemon-side spawning** was a non-goal and is now
one narrow, guarded capability — `process.start`, which starts the HOST's own
`workass run` for a chat on another machine, under the five conditions §14b
lists. It stays a non-goal for the local machine, where a caller has a shell of
its own, and the rule it was protecting — the daemon never lends its
environment to a child — is kept literally rather than relaxed.

Removed by decision: `sandbox.max_depth` — replaced by narrowing-only
permissions plus the no-chaining rule (§11), which is simpler and bounds
escalation instead of depth.

Deferred, named: a **relay route** for peers that cannot see each other
directly (the route list is ordered from day one so adding it later changes no
semantics); automatic home migration (manual, chats only, in v1); total
replication (subscribe-on-demand first); priorities and preemption in resource
queues (FIFO + visible queue + `max_hold` as a report); mTLS between peers (the
device token is derived and never transmitted); content-asserting push
(`trust_push`); a cross-peer tree UI beyond P6's read-only view.

## 16. Decisions — DECIDED 2026-10-30

The operator accepted v4 on **2026-10-30** ("go"). These are no longer open
questions; a builder implements them as written.

1. **Replication scope — on demand.** A daemon subscribes to what it monitors,
   what it is showing and what it owns. A fleet never copies itself into every
   machine.
2. **Home migration — chats only, manual, fenced.** No automatic failover and
   no migration of other entity types in v1. Handover stamps
   `moved_to{home, epoch+1}` and the old home refuses writes afterwards (§8).
3. **Numbers — three, fixed.**
   (a) A machine silent for **10 minutes** produces **one** `home_down` notice
   per host, listing its affected children (the heartbeat itself is a fixed
   ≈1 min, derived from this, not a separate knob).
   (b) Notices are **coalesced per chat per 60 s**, at most **6 per chat per
   hour**, and **failures are always exempt** from the cap.
   (c) Logs are retained **7 days or 1,000 events per entity**, whichever comes
   first; terminal logs survive the cap until acked, and eviction is logged.
4. **A home may refuse a kind**, immediately and structurally, **naming the
   capability it lacks** — never a silent queue, never a generic denial.
5. **Fan-out notifies once in aggregate.** A fork run produces one notice, not
   one per child.
6. **`on_parent_gone` per kind** — `process` **finishes** (the work is usually
   the point), `chat`, `wait` and `fork` **cancel**. Orphan terminal logs park
   in the home's inbox either way.
7. **`max_hold` on a resource is report-only.** A holder past its declared hold
   raises an `attention` naming it; a host never kills a holder to be fair.
8. **Adopted from T3 Code's peer links (2026-10-30):** per-link pairing records,
   individually revocable (§4); narrowing-only `limit` with an immutable
   `origin` stamp and **no chaining** for below-full-access work, replacing
   `max_depth` (§6, §11); identity proof before any credential, with ordered
   routes, learned routes and per-route cooldowns (§4); one command surface
   reused across links instead of a second API (§6); one `operation_id` shared
   by "launch there" and "record here" (§6).
