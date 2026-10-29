# The fleet runtime — one engine, many homes, no local/remote split

Status: SPEC v4 — **ACCEPTED 2026-10-30** (operator: "go"). §13 P1 (ids and
homes) shipped in `bb517b99`; §13 P2 (the mesh) is built — `internal/fleetclient`
dials, enrols with no human, keeps one heartbeat per pair, and `apply` to a
non-self home travels over that link with the §6 envelope, the renderer relay
demoted to the fallback §12 reserves it for. P3 onwards is unbuilt.
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
Job object, spool-and-ingest, exit code), `lost`, `timed_out`, one-shot stall,
refusal of evidence-less registration. Tests: child survives a daemon restart
*and* a self-update; killing the supervisor yields `lost`, never `running`; the
token never appears in argv.

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
| Daemon restart or self-update mid-child | process children are not daemon children; events spool and are ingested |
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

## 15. Non-goals and deferred

Non-goals: consensus, leader election or automatic failover; a global
transaction across entities; daemon-side spawning; a DAG engine; replicating
secrets or large blobs by value; a cloud relay for peers that cannot see each
other.

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
