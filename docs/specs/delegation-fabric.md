# The delegation fabric — one relation, one log, local or across the fleet

> **Superseded in framing (2026-10-29):** this document now describes the
> **delegation layer inside** `docs/specs/fleet-runtime.md`, which replaces its
> premise — entities have global ids and home daemons, daemons form a direct
> mesh, and there is no local/remote split to bridge. The delegation model,
> `workass run`, the notice rules and the failure analysis below all stand;
> §6's transport is restated there as replicated event logs plus monitors, and
> where the two disagree, `fleet-runtime.md` wins.

Status: SPEC v3 — design under review, **no code written**. v3 keeps v2's model
(one delegation relation, one event log per child, a shipped supervisor binary,
waiting as a state, capability placement, host-side resource queues,
two-directional grants) and replaces its liveness machinery with **two rules**
(§6) after the operator's 2026-10-29 instruction: "i don't understand the
liveness and the recheck why is this so complicated". Per-child leases,
freshness scores and the periodic reconcile tick are gone; §15 records every cut
and deferral.

Supersedes the cross-machine parts of
`acp-fix-5-6-external-work-and-wake.md` (Fix 6 stays; Fix 5's removal is
honoured by §7) and absorbs the paused `docs/SUPERVISED-JOBS-WIP.md` (jobs
become children of kind `process`).

Origin: operator, 2026-10-29 — "it should be an universal solution between
daemons, so the mac becomes actual 'parent chat daemon' communicating with
'subagent chat daemon'". Nothing here names a machine, an OS, or a
desktop-versus-headless role: those are properties a peer advertises, never
assumptions the design makes.

## Binding laws for this lane (restated — executors do not inherit memory)

- Wire contract `invoke/reply/event` is FROZEN. New channels and additive
  payload fields only; no existing field changes meaning.
- Identifier law: never rename an existing field, id, path or function. Add.
- One engine per chat; serialized session/prompt per bridge; stdout purity for
  ACP code (JSON-RPC on stdout, diagnostics on stderr).
- `RedactSensitiveText` before any free text is persisted, emitted or sent,
  and again on receipt from a peer. Never persist raw prompts or
  provider-native session ids.
- A wake is never a user message. Completion reaches a chat only as a
  task-notification notice (the 2026-10-16 law, §7).
- Secrets never travel in argv (`cmd/workass/fleet_cli.go`: "The key is never
  taken from argv" — argv is world-readable through `ps` and lands in shell
  history).
- User-visible chrome needs an operator-approved mock before it exists.
- Receipts: every claim in a lane report needs the command and its output.

## 1. Why the current shape cannot be fixed in place

Workass already delegates work five ways, each with its own record, lifecycle,
notification story and locality rule:

| Today | Record | Reports to its parent? | Crosses peers? |
|---|---|---|---|
| Subagent (`agent.spawn`) | `SubagentRun` (`internal/acp/subagents.go:150`) | yes, task-notification (`cmd/workass/provider_chat_subagent_completion.go`) | commands yes, **reports no** |
| Observed background work | `SpawnedWorkItem` (`internal/acp/spawned_work.go:80`) | no | no |
| External work | same registry, kind `external` | no | refused (`cmd/workass/agent_remote_parity.go:75`) |
| Fork run children | `ForkRun`/`ForkChild` (`internal/acp/forks.go:131`) | aggregate only | fork-scoped routes |
| Scheduled message | `scheduledmessage.Entry` | yes, as its own chat input | refused (`agent_remote_parity.go:72`) |

1. **Reports only travel inward.** A parent reaches a child on another peer
   (`rendererAgentRouter.Call`, `cmd/workass/remote_agent_router.go:59`), but a
   child can never reach its parent: the only outbound path broadcasts to an
   approved local Electron client (`hub.BroadcastLocalShell`,
   `internal/wire/wire.go:587`) and no Go client exists. A subagent spawned on
   a peer reports into *that peer's* chat
   (`cmd/workass/agent_control_remote.go:103`), not to the chat that asked.
2. **Locality leaks into semantics.** `remoteAgentMethodKinds` decides per
   method whether a thing is addressable at all, so "where it runs" and "what
   it means" are one table.
3. **Every new child kind re-earns everything** — record, notification,
   routing class, UI (WIP §4.2-§4.4).

## 2. The model

### Entities

- **Peer** — one daemon: machine identity (`internal/machineid`), fleet
  membership (`internal/fleet`), address and liveness
  (`internal/machinebook`), and a **capability card** (§9). Peers are
  symmetric; nothing distinguishes a desktop daemon from a headless one, or a
  "controller" from a "worker".
- **Chat** — a durable actor owned by exactly one peer. Unchanged.
- **Turn** — the only thing that may create a delegation.
- **Delegation** — the relation and the unit of authorization:
  `{delegation_id, parent{peer,tab,chat,lane,operation,turn}, host_peer,
  child_kind, child_ref, intent, policy, sandbox, grants, created_at}`.
  Parent coordinates are immutable, exactly as `OriginLaneID`,
  `OriginOperationID`, `OriginTurnID` already are on `SpawnedWorkItem`.
- **Child** — what was delegated: `chat` (a subagent session), `process` (any
  supervised process; every "job" is one), `wait` (a condition that resumes
  the delegator), `fork` (one child of a fan-out). Closed set in v1.
- **Event log** — per delegation, append-only, sequence-numbered, on the host.
  **The log is the only history**; receipts, cards, rails and notices are
  projections of it (§3).
- **Head** — the log's mutable summary: `{state, first_seq, last_seq,
  acked_through, output_path, output_size, outcome?}`. Derivable by replaying
  the log plus a filesystem probe, so it is a cache, never a second truth.
- **Cursor** — an integer the *parent* keeps per delegation: the last seq it
  has consumed; `acked_through` is the host's copy of the same number. A
  delegation has exactly one parent, so that is one integer each — not a
  subscriber registry, nothing to leak or clean up. Precedent in today's code:
  `SubagentRun.ProgressSequence` and `attentionDeliveredSequence`
  (`internal/acp/subagents.go:180`, `:212`), and the actor store's `Revision`
  checks (`internal/chat/sqlread.go:234`).
- **Receipt** — the bounded, redacted terminal projection of a log. Still the
  only *content* object that crosses peers, and still produced by the existing
  types (`SubagentReceipt`, `SpawnedWorkReceipt`).
- **Grant** — a capability minted by one peer for one delegation in one
  direction (§8).

### Relations

`parent-of` (a delegation, **one hop**), `hosted-by` (child → peer; the host is
authoritative for child state), `owns` (peer → chat).

Nesting: a child on H may delegate to G. Each delegation knows only its own
parent, so G's events reach H's chat and H decides — by *its* delegation's
policy — whether anything rises to P. No global tree, no transitive grants, no
peer that must know the fabric's shape. Fan-out is N siblings sharing a parent.
Recursive delegation is bounded by permission, not by a counter: work that
arrived under a narrowed grant may not use its host's own links at all
(`fleet-runtime.md` §11). `sandbox.max_depth` is removed.

### One protocol, local == remote

One service interface. Locality is resolved **once**, at the edge, by the
authority that already resolves it (`remoteAgentMethodKinds`): in-process call,
or `delegation.*` invoke over a peer link. The service never learns which it
was. A local delegation writes the same log, uses the same cursor, and passes
the same authorization (owner key + actor fence), so "local" can never become
the cheaper, looser path — the usual way this class of design rots.

## 3. The log is the protocol

### Event kinds (v1, closed; additive by spec)

`created`, `queued{resource,position}`, `started{pid?}`, `waiting{conditions}`,
`resumed{by}`, `attention{kind,title}`, `attention_resolved`,
`stalled{evidence}`, `cancel_requested{by}`, `grant_refused{reason}`,
`settled{outcome}`, `lost{evidence}`, `truncated{to_seq}`.

Every event: `{seq, at, kind, payload}` — bounded, redacted, and *a state
transition or a decision*, never a slice of output.

**Output is not events.** A child's stdout/stderr is a file; a reader asks for
a bounded tail by byte offset. Same cursor idea, applied to bytes, with no
copying and no log growth. Progress is therefore free, and never wakes anybody
(§6).

**Heartbeats are not per-child events.** Liveness is one heartbeat per machine
(§6, rule 2), not a lease per child: a 30-second heartbeat appended to one
child's log for three days is 8,640 events describing nothing that changed, and
a hundred children on one host would say the same thing a hundred times.

### Retention and resync

A log is bounded (per-delegation event cap and a retention window). Compaction
drops the oldest events and records `first_seq`; a `truncated{to_seq}` event is
appended so a reader learns it happened rather than inferring it from a gap. A
parent whose cursor is below `first_seq` **resyncs from the head** (state +
outcome + receipt) instead of failing, and says so in its mirror. Terminal logs
are retained until the parent acks (§6) or the cap evicts, and eviction is
logged, never silent.

### Messages (frozen v1, additive-only)

Parent → host (needs a **command grant**, or locality):

1. `delegation.create {delegation_id, parent, child_kind, intent, policy,
   sandbox, report_grant}` → `{child_ref, head}`. `delegation_id` is
   caller-chosen; a repeat returns the same child.
2. `delegation.command {delegation_id, verb ∈ message|resume|steer|cancel|
   configure, payload, operation_id}`.
3. `delegation.events {delegation_id, after_seq, limit}` → `{events[], head}`.
   The one read primitive; `read {receipt|tail}` is sugar over it.
4. `delegation.list {filter}` → heads, for reconciliation.
5. `delegation.ack {delegation_id, through_seq}` — lets the host retire a
   terminal log.

Host → parent (needs a **report grant**, or locality):

6. `delegation.push {report_grant, delegation_id, events[], last_seq}` — the
   host hands over the log events the parent has not acked, and the parent
   replies `{through_seq}` (§6, rule 1). Held by the host until acknowledged.
7. `heartbeat {peer, at}` — once per machine, not per child (§6, rule 2).

Reserved, default off: a push that asserts a *state* instead of handing over
log events (`trust_push`). A pushed event is still just a log entry the parent
applies by seq and can re-read from the host at any time; it is never a peer
telling a parent what to believe about a child it cannot check.

Exactly-once, replay after partition, days-offline catch-up and version
tolerance are all cursor semantics; none of them needs a mechanism of its own.

## 4. What a parent knows (two facts, nothing else)

A parent holds exactly two facts about a child, and they never mix:

1. **What the child is** — `requested → accepted → (queued) → running →
   settled{outcome} | lost | cancelled`, with `attention{open|resolved}`,
   `waiting{conditions}` and a one-shot `stalled` as flags on `running`, never
   states. `outcome = {status ∈ done|failed|timed_out|lost|cancelled,
   exit_code?, summary, elapsed_ms, receipt_id}`. **The host decides this.** It
   is the only party that can: it holds the log, the pid and the output file.
   A dead process with no `settled` event is `lost` — a verdict, not a guess.
2. **Whether its host is reachable** — one fact per *machine*, not per child
   (§6, rule 2). Reachable, or `unreachable since T`.

So a parent either knows the child's state from a reachable host, or it says
`host unreachable since 18:40 (last known: running, 41 min of output)`. It never
shows a bare `running` it cannot stand behind, and it never invents a state the
host did not report. "The child died" and "I cannot see the host" are different
sentences with different fixes; conflating them is what let a finished run sit
unnoticed for five hours.

There is no per-child liveness lease, no freshness score, and no third state in
between. A reachable host is authoritative about its own children; an
unreachable host produces exactly one sentence per machine.

### Cancellation is a wish, not a message

`cancel` is stored as desired state on the delegation and recorded in the log.
A cancel issued while the host is unreachable is not lost: it is applied when
the host comes back, and propagation down a tree is the same wish repeated at
each hop.

### Grant

`minted → active → expired | revoked`. Revoked on settle-and-ack, on chat
deletion or `chat.settle`, or by the operator. Re-minted whenever a parent
connects to the host, so expiry is invisible while things work and self-healing
when they do not.

## 5. `workass run` — the supervisor that ships with the product

A new subcommand of the existing binary, so every peer has it by construction:

```
workass run --delegation-token-file F --label L [--resource gpu0]
            [--max-runtime 3h] [--stale-after 40m]
            [--inputs PATH,...] [--min-free-disk 50G]
            -- cmd args...
```

What it does, in order: resolve and check `--inputs` (exists, readable,
symlink targets resolve) and the tool on PATH and free disk; acquire the
resource lease (§10, blocking with a visible queue position); start the child in
its own process group (POSIX `setsid` / a systemd scope where present /
a Windows Job object) so it is **not** a daemon child; append `started{pid}`;
append `settled{outcome}` with the real exit code; release the resource hold;
exit with the child's code. It reports nothing about being alive: it is a child
of nothing, the host can see its pid, and a pid that is gone with no `settled`
event is `lost`.

Properties that matter:

- **The daemon still never spawns work.** `workass run` is invoked by the agent,
  with the agent's environment, which keeps the `acp-fix-5-6` non-goal intact
  (no environment or secret capture inside the daemon).
- **It survives daemon restarts and self-updates**, because the daemon is not
  its parent — the case that kills in-process subagents today.
- **It REPORTS, and it works while the daemon is down.** Superseded 2026-10-30
  by `fleet-runtime.md` §14c: every transition is reported to the local daemon
  over the authenticated Agent API and acknowledged by seq, and the on-disk
  copy is a SQLite transactional outbox (`state/process-outbox.db`) that is a
  write-ahead log for exactly the window when the daemon is unreachable —
  appended before the call, marked delivered by the ack, replayed in seq order
  on reconnect. Facts on disk, as before, because a supervisor that needs a
  live daemon would fail exactly during an update; but the FILE is no longer
  the channel, because a spool nobody reads and a report nobody receives looked
  identical from the outside, and that is how four live jobs told nobody.
- **Authorization never rides argv.** The delegation token comes from
  `--delegation-token-file` (0600, consumed and unlinked) or an env var — not
  the command line (binding law, `fleet_cli.go`).
- **Absence is still evidence.** If the supervisor itself dies (OOM, reboot),
  there is a pid that is gone and no `settled` event: the host appends
  `lost{evidence}`. Nothing can report `running` forever.

The lagspace lane runner becomes a composer of `workass run` steps — one
`process` child per stage, preflight per stage, no repo-specific launcher
logic, no marker-file protocol of its own.

## 6. Two rules for staying in sync

Everything about liveness and recovery is these two rules. Only one of them
runs on a timer.

### Rule 1 — Durable acknowledged push (store-and-forward)

A child's events are **stored on its host and delivered with acknowledgements**.
Nothing is fire-and-forget, so nothing has to be "noticed as missing".

- When a child changes — `settled`, `failed`, `lost`, `timed_out`,
  `attention`, `waiting`, `stalled` past a declared threshold — the host
  appends the event to that child's log (durable first, always) and pushes it
  to the parent's daemon immediately:
  `delegation.push {report_grant, delegation_id, events[], last_seq}`.
- The parent applies the events — **deduplicated by seq**, so applying the same
  event twice changes nothing — and replies `ack {through_seq}`.
- The host keeps every event until it is acknowledged. `acked_through` lives
  **on the delegation** (one integer: a delegation has exactly one parent), so
  a restart, a crash, a self-update or a four-day outage changes nothing about
  what is owed.
- On reconnect, the parent states where it got to (`events after_seq: cursor`)
  and the host sends the rest. That is the same delivery, resumed — not a
  second mechanism, not a repair pass, and not a poll.

So delivery is continuous and ordered per child: a disconnect postpones events,
it does not drop them, and the parent is never in a position where something
happened that it has no way to learn.

### Rule 2 — One heartbeat per machine

Events cover everything a host can say. They cannot cover the one case where a
host says nothing at all: it is powered off, crashed, or cut off. That is what
the heartbeat is for, and it is the only timer in the fabric.

Peers exchange a heartbeat about once a minute. If a peer is silent past the
threshold, every parent with children there gets **one** notice — `host X
unreachable since T`, listing the affected children and what was last known of
each. One per host, never one per child. When the peer speaks again, delivery
resumes under rule 1 and the real events arrive.

### What a change is worth waking someone for

Delivery always updates the parent's record; it becomes a **notice** in the
chat only when it is actionable: `settled` (any outcome), `lost`, `timed_out`,
`attention`, `stalled` past a threshold the delegation itself declared, and
`host unreachable` past the threshold. `started`, `queued`, `resumed`, progress
and output growth never notify — they are visible by reading (§3), which is
exactly when freshness matters. Notices are coalesced per chat per 60 s (the
hourly cap is gone, fleet-runtime §14h), and suppressed when a live waiter will consume
the result, when the parent turn was stopped, or when the parent cancelled the
child (§7).

### The invariants these rules preserve

- **Exactly-once effect**: every event carries a seq and every id derives from
  a durable fact (`delegation_id` chosen by the caller, receipt ids, log
  position), so redelivery is free — precedent `scheduledSendOperationID`
  (`cmd/workass/scheduled_messages.go:21`), `subagentCompletionOperationID`.
- **Content is a log entry, never an assertion**: a pushed event is the same
  record the parent can re-read from the host at any time (§8 blast radius).
- **Nothing is unrecoverable**: host truth = its logs + its disk; parent truth
  = cursor + the events it has applied. Heads are caches.
- **Nothing is forgotten before it is consumed**: a terminal log is kept until
  acknowledged, or until retention evicts it (logged, never silent). If the
  parent chat is gone, the log parks in the host's inbox (§11).
- **Version skew**: an unknown `v` is refused with a structured error naming
  the supported range, and delivery falls back to plain `events`, which every
  version serves.

## 7. Notices — the 2026-08-10 bug, and why it cannot recur

The wake removed in `aa53975e` had five defects, all legible in that diff:

1. it enqueued a **synthetic user message** (`[workass internal notice] …`)
   through the user-input path, so daemon bookkeeping appeared as the human's
   own turn;
2. it **instructed the model** ("Resume the work that depended on this
   completion") and polluted the chat's language contract badly enough to need
   the disclaimer "This notice is not a user language preference" — the same
   commit deletes `cmd/workass/language_contract_test.go`;
3. exactly-once was **knowingly broken**: "enqueue first, then mark delivered;
   a crash between the two may deliver twice — accepted and documented";
4. it fired on **engine-death seams** (any kind settling to `orphaned`), waking
   a chat about work that died because its engine died;
5. it had **no suppression**: it fired whether or not a waiter was live, the
   parent turn had been stopped, or the child had been cancelled.

The repair shipped on 2026-10-16 (`f9a096f1`, "A finished subagent wakes its
chat like Claude Code, never as the user"): `taskNotification` in
`_meta.workassCommandInput`, `origin {kind:"task-notification"}`,
`NoticeKind/NoticeTitle/NoticeStatus` on the row, and one quiet `NoticeLine`
(approved mock `desktop/docs/mocks/subsessions/completion-notice.html`).
Notices are **default-on today, for subagents, by the operator's explicit
choice**.

This spec changes one thing: every child kind uses that path, and only for
actionable events (§6). Safeguards, each a regression test (§13): never a user
message; no imperatives and no language instructions; the operation id derives
from the receipt (crash mid-delivery cannot double-deliver); an engine-death
orphan is not an outcome and never notifies; the three suppression conditions
are evaluated before delivery; delivery is coalesced and capped; and
`chat.configure {background_notices: on|failures_only|off}` is the one consent
surface.

## 8. Security

- **Membership** — the fleet key: one ceremony per peer, never per pair
  (`internal/fleet`, D3). Enrolment is challenge-response over
  `fleet:challenge`/`fleet:enroll` (`internal/wire/wire.go:1660`, `:1701`); the
  key never crosses the wire and neither does the derived token.
- **Authentication** — that derived device token, exactly as every other client
  (`?deviceToken=`, `internal/wire/wire.go:1071` → `lease.AuthenticateToken`).
  No new credential class, no new listener, no new port. (mTLS is deferred,
  §15: it would replace a credential that is already never transmitted.)
- **Authorization — grants, not roles**, in both directions:
  - **command grant** (parent → host): which verbs the parent may use.
  - **report grant** (host → parent): which event kinds it may push.
  - **sandbox** (parent → host, carried in `create` and enforced by the host):
    allowed cwd roots, provider/model/mode, resources, `max_runtime`,
    and whether the child may delegate at all, with narrowing-only limits and
    no chaining below full access (`fleet-runtime.md` §11). Today a remote
    spawn silently inherits the host chat's defaults; the sandbox makes the
    delegation say what the child is permitted to be.
  - `grant = {v, grant_id, delegation_id, issuer_peer, holder_peer, subject,
    verbs[], exp, sig}`, HMAC'd with the **issuer's own** secret: only the
    issuer verifies, so there is no shared signing key, no PKI, and no clock
    agreement (expiry is read on the issuer's clock alone).
- **Blast radius.** A compromised host can push about, and lie about, its own
  children to precisely the chats that delegated to it — and a pushed event is
  still only a log entry the parent can re-read from the host, which means its lies
  must still survive the parent's own authenticated read. It cannot address
  other chats or peers, read other delegations, or execute anything. A
  compromised parent can command only its own children, within the sandbox it
  minted.
- **Audit is the log.** `grant_refused`, `cancel_requested{by}` and
  `resumed{by}` are events, so authorization history lives where everything
  else does. No second audit store (§15).
- **Abuse** — per-peer, per-subject rate limits on pushes and heartbeats; invalid grants
  counted like failed enrolments (`internal/wire/wire.go:1728`).
- **Content** — bounded and redacted on send *and* on receipt; a peer's
  redaction is never trusted.

## 9. Capability cards and placement

The card is the existing identity document plus additive blocks: `agents`
(providers, models, modes — today's `MachineAgentCatalog`), `resources`
(`[{name, kind: exclusive|counted, count, vram_gb?}]`), `host`
(`{accepts_kinds, max_concurrent, os, free_disk_gb}`) and
`delegationProtocol {min,max}`. Served where identity already is
(`/workass/health` + `machinebook.Card`), refreshed by the existing probe.

`delegation.create` may address a host by **constraint** instead of by name:
`requires {provider, model, resource, vram_gb, os, free_disk_gb, paths[]}`.
`paths[]` is answered by a **cheap existence/readability probe at create time**
— the host returns which paths are missing, so a missing input is a refusal in
seconds instead of a crash in hour three. Content manifests and a data catalogue
are deliberately out (§15); staging data is an ordinary `process` child whose
client verifies what it copied.

## 10. Resource holds and queues

(Nothing here is about liveness — §6 owns that. A *hold* is permission to use a
GPU, not evidence that anything is alive.)

A resource named in a policy is admitted by its host: `accepted → queued{position}
→ running`, fair FIFO, one visible queue per resource with each waiter's
delegation, parent and age. The hold is **tied to the supervisor process**
(§5), so a child that dies — however it dies — releases it; the daemon never
has to guess whether a lock is abandoned. `max_hold` is enforced as a **report**
(an `attention` event naming the holder), not a kill: killing someone's
eight-hour training run to be fair is a worse outcome than saying who is
holding the GPU. Priorities and preemption are deferred (§15).

Two layers, deliberately: the host's queue grants turns, and the supervisor
additionally holds an OS-level lock (flock / named mutex), because a queue only
the daemon honours is not a lock — a process started outside the fabric must
still be excluded.

## 11. Tree semantics

- **Cancel propagates down**, per `policy.on_cancel ∈ {cancel_children,
  detach_children}`, as desired state (§4), retried across partitions.
- **Orphans.** When a parent chat is deleted or settled,
  `policy.on_parent_gone ∈ {finish, detach, cancel}` applies per child; a child
  that finishes with no parent parks its terminal log in the host's **inbox**,
  readable by any authorized reader later, rather than disappearing.
- **One view.** A delegation tree, across peers, showing kind, host, state,
  host reachability, resource, runtime and the last log line — per chat and
  per peer. Mock before pixels (§13, P7).

## 12. How today's code maps on

**Kept unchanged** — chats, actors, the engine and its ledger; both receipt
types; the notice presentation path (`TurnPresentation.NoticeKind/Title/Status`,
task-notification origin, `NoticeLine`); obligations and dispositions
(`internal/chat/reducer.go:3503`, `internal/acp/disposition.go`); fleet keys and
device leases; the machine book and its probes; `RedactSensitiveText`;
`internal/durablefs`; the condition probes in `internal/scheduledmessage`.

**Generalized** (same types, same ids, additive `delegation_id` + log) —
`SubagentRun` → kind `chat`; `SpawnedWorkItem` + external work → kind `process`
(today's `kind` string survives as a presentation facet); `ForkRun`/`ForkChild`
→ kind `fork`; `scheduledmessage.Entry` → kind `wait`, its probes becoming the
shared condition evaluator for declared waits (§13 P3);
`provider_chat_subagent_completion.go` → the single notice path for all kinds;
`remoteAgentMethodKinds` keeps every existing method exactly as it is and
additionally resolves locality for `delegation.*`; `SubagentRun.ProgressSequence`
and `attentionDeliveredSequence` become the log's seq and the parent's cursor.

**Deleted, once P5 lands** — the premise that a report cannot travel outward,
and with it: agent-side polling watchers; owner-side `ssh` mirrors;
`clampControllerWait` as the answer to cross-peer waiting (the clamp stays for
legacy blocking waits; a wait is now a child); marker-file protocols in client
repos; and the local-only *reasons* that exist only because a daemon could not
act (`external.register`/`settle` stay host-local in execution but become
parent-addressable through a delegation).

**New** — `internal/delegation` (log, head, cursor, state machine, reconciler,
inbox), `internal/fleetclient` (TLS dial, key-holder enrolment, token
persistence, invoke), the `delegation.*` channels, grant mint/verify, and
`workass run`.

## 13. Migration, compatibility, phases

- **M1** Existing subagent, spawned-work and scheduled-message records load as
  **local delegations with an implicit parent** (their current
  `ParentChatID`/`ParentTabID`) and are **projected into a log with a synthetic
  `first_seq`** — a `created` plus whatever terminal they already have. No
  history is invented for events that were never recorded.
- **M2** All new persistence is additive JSON; older snapshots decode and
  unknown fields are ignored (precedent: the ignored `wake` fields).
- **M3** No tool is removed or renamed. `workass_spawn_subagent`,
  `workass_register_external_work`, `workass_schedule_message` and the fork
  tools keep their exact semantics and create delegations underneath. New verbs
  are additive: `workass_delegate`, `workass_list_delegations`,
  `workass_command_delegation`, `workass_read_delegation`.
- **M4** A peer on an older build advertises no `delegationProtocol`: it hosts
  old-style children and is reconciled pull-only. A mixed-version fleet is a
  supported steady state, not a window.
- **M5** Parent gone → `policy.on_parent_gone` + inbox (§11).
- **M6** A chat that moves peers re-registers its outstanding delegations and
  re-mints their grants; the host accepts either until the first new one is
  used.

**P1 — log + delegation core, local only.** `internal/delegation`, events,
head, cursor, state machine, the five parent verbs, and today's subagents /
external work / scheduled messages rewired underneath. Tests: the existing
`internal/acp` + `internal/chat` + `cmd/workass` suites stay green; projection
parity (a subagent created via `delegation.create` is indistinguishable from
one created by `agent.spawn`); idempotent repeats; cursor replay returns the
same events; compaction appends `truncated` and a behind-cursor reader resyncs
from the head; M1 implicit-parent load.

**P2 — `workass run` + honest process evidence.** Preflight, process group /
scope / Job object, report-and-acknowledge (§14c), `lost`, `timed_out`, one-shot `stalled`,
refusal of evidence-less creation, liveness on every platform the daemon builds
for. Tests: child survives a daemon restart *and* a daemon update; kill the
supervisor ⇒ `lost`, never `running`; events written while the daemon is down
are ingested on start; token never appears in argv (`ps` assertion); preflight
catches a missing input, a dangling symlink, an absent tool and full disk.

**P3 — waiting as a state.** A turn ends `settled` or `waiting{conditions}`;
the host holds the conditions level-triggered and resumes the **same native
session** when one fires (`internal/acp/fork_turn.go:21`, `forks.go:574`).
A turn that ends claiming to wait without declaring a condition raises
`attention{ended-while-waiting}` — the disposition tier already classifies this
(`internal/acp/disposition.go`, `dispositionSourceHarness`). Tests: declared
wait resumes with context on Claude and on Codex; resume survives engine
recycle (`bridge.go:1667`) and daemon restart; undeclared wait raises attention
immediately rather than after a 90-minute grace; a root chat with no parent
lands in `needs_input` instead.

**P4 — actionable-only notices.** Default-on through the existing
task-notification path, with §6's actionable filter, coalescing, cap and the
per-chat switch.
Tests: the five `aa53975e` anti-regressions; progress and queue events never
notify; five settles in one window ⇒ one notice; twenty children on one
unreachable host ⇒ one notice; `off` silences.

**P5 — peer transport and the two rules.** `internal/fleetclient`, the
`delegation.*` channels, grants, durable acknowledged delivery, and the
per-machine heartbeat. Tests: key-holder enrolment with no human; forged,
expired, wrong-subject, wrong-chat and out-of-sandbox grants refused; a
redelivered event is a no-op (dedup by seq); an event appended while the link
is down is delivered on reconnect from the parent's last seq; a host restarted
between append and delivery still owes exactly those events (`acked_through`);
a parent restarted between apply and ack re-receives and deduplicates;
peer-to-peer delivery with no renderer process anywhere; a four-day outage
delivers the whole backlog once, in order, with no poll running; a host that is
powered off produces exactly one notice naming it, whatever the number of
children; a cancel issued while the host was gone applies when it returns; a
three-peer nesting case routes a grandchild's events to its own parent and
nothing to the grandparent; an unreachable host shows `last known`, never
`running`.

**P6 — cards, placement, resource queues.** Constraint resolution, path
existence probe, FIFO queues with visible position, lease release on child
death, `max_hold` as an attention. Tests: a constraint resolves to the only
capable peer; an unsatisfiable one fails fast with per-candidate reasons; a
missing required path refuses at create time; two parents contend FIFO; killing
a supervisor releases its lease; a host at `max_concurrent` queues.

**P7 — one tree, and the owed UI.** The cross-peer delegation tree and the
`stalled` visibility still owed by `turn-disposition.md:4`. Mock first.

**P8 — doctrine cleanup.** `docs/WORKASS-TOOLS.md` recipes; retire polling and
mirror patterns from agent-facing docs; the renderer relay becomes an explicit
fallback transport.

## 14. Failure table

| Failure | Handled by |
|---|---|
| Delegator's session restarts | children are peer-owned records with logs, not harness state |
| Child finishes while nobody watches | actionable notice (§7) + cursor/ack |
| Child dies silently | the host sees a gone pid with no `settled` ⇒ `lost{evidence}` (§4) |
| Child never starts | evidence-less create refused; `accepted` without `started` past deadline ⇒ `timed_out` |
| Supervisor itself dies (OOM, reboot) | pid gone + no `settled` ⇒ host appends `lost` |
| Daemon restart or self-update mid-child | `process` children are not daemon children; events are queued in the outbox and replayed in order on reconnect |
| Liveness by text matching | evidence is the log, the pid and the output file on the host; never a pattern over a remote command line |
| Stale `running` after a host goes dark | one notice per machine: `host unreachable since T, last known: …` (§6, rule 2) |
| Report cannot reach the parent | store-and-forward delivery with acks (rule 1); needs no renderer and no human |
| Child ends its turn with work left | declared `waiting`; undeclared ⇒ `attention{ended-while-waiting}` (P3) |
| Settled child cannot be continued | `command {verb: resume}` against the retained native session |
| Blocked child ends silently | `attention` event; unanswered ⇒ parent obligation `needs_input` |
| Resource contention | host FIFO queue + supervisor-held OS lock (§10) |
| Pipelines with missing inputs | per-stage preflight in `workass run`; `requires.paths` refused at create |
| Operator cannot see state | heads + logs per peer and per chat; the tree (P7) |
| Token cost | notices are daemon-side; no polling anywhere; progress is read only when asked |
| Quota/credential exhaustion in a child | terminal `failed` with the provider reason; failures exempt from the cap |
| Partition in either direction | delivery resumes from the parent's last seq; events were never discarded |
| Parent asleep when the child ends | the host holds the events until acknowledged and delivers them on reconnect, once |
| The same event delivered twice | dedup by seq: the second application changes nothing |
| Push storm / hostile peer | rate limits + abuse counters (§8) |
| Grant replay / theft | one delegation, one direction, named verbs, `exp`, revoke-on-consume; events are re-readable from the host |
| Sandbox escape by a child | sandbox enforced at the host (cwd roots, provider, resources, depth) |
| Recursive delegation storm | narrowed work cannot use its host's links; per-peer rate limits and the notice cap bound the rest |
| Version skew | structured refusal naming the supported range, then plain `events` delivery (§6) |
| Peer offline for days | the backlog is delivered on reconnect from the parent's last seq; grants re-minted then |
| Cursor behind retention | `truncated{to_seq}` + resync from head, and the mirror says so |
| Clock disagreement between peers | expiry is only ever read by the issuer; ordering is by seq, not time |
| Cancel during a partition | cancel is desired state, re-applied on reconnect (§4) |
| Parent chat deleted mid-flight | `on_parent_gone` + host inbox (§11) |
| Nesting misroute | one-hop routing: a push is addressed by its own delegation's parent (§2) |
| Duplicate `create` retry | idempotent by caller-chosen `delegation_id` |
| Delivery interrupted mid-flight | unacknowledged events stay owed; `acked_through` survives crash, restart and update |
| Host powered off, crashed or cut off (says nothing at all) | the per-machine heartbeat (rule 2) — the one case events cannot cover |
| Oversized log, receipt or leaked secret | per-log caps, byte-bounded tails, redaction on send and receive |

## 15. Deliberately cut or deferred

Cut (the simpler form is in this spec):

- **Output-progress as log events** → output is a file read by byte offset. A
  training run would otherwise write its stdout into the protocol.
- **Per-child liveness leases and freshness scores** → one heartbeat per
  machine (§6, rule 2). A reachable host is authoritative about its own
  children; an unreachable one is one sentence, not N.
- **The periodic reconcile tick** → nothing polls. Events are stored and
  delivered with acknowledgements (§6, rule 1), so there is nothing for a timer
  to go looking for; the only timer left in the fabric is the per-machine
  heartbeat.
- **A separate audit log** → authorization decisions are events.
- **Host-side subscriber registries** → the cursor lives at the parent, so
  there is no fan-out state to leak, expire or clean up.
- **Data catalogues and content manifests for placement** → a cheap existence
  and readability probe at create time. A content index is a second product
  (invalidation, partial copies, who refreshes it) and every failure it would
  have caught is caught by preflight anyway.
- **Repo-specific launchers** → `workass run`, shipped with the daemon.

Deferred (named, not designed away):

- **Priorities and preemption** in resource queues; FIFO plus a visible queue
  first, and a real starvation complaint before anything cleverer. `max_hold`
  reports, never kills.
- **mTLS between peers.** The device token is derived, never transmitted; mTLS
  would be a larger change to a credential that is already not on the wire.
- **Content-bearing push** (`delegation.report`, `trust_push`). The one rule
  that stops a compromised peer from asserting history.
- **A DAG/pipeline engine inside Workass.** Stage ordering belongs to the
  client composing `workass run` steps.
- **Daemon-side spawning.** Unchanged non-goal: the daemon supervises by
  evidence and never lends its own environment to a child.
- **Cross-peer tree UI** beyond P7's read-only view.

## 16. Decisions — DECIDED 2026-10-30

Accepted by the operator on **2026-10-30** ("go"); implement as written. The
fleet-wide decisions live in `fleet-runtime.md` §16 and are not repeated here.

1. **Fan-out notifies once in aggregate** — a `fork` run produces one notice,
   never one per child.
2. **`on_parent_gone`** — `process` **finishes** (the work is usually the
   point), `chat`, `wait` and `fork` **cancel**; orphan terminal logs park in
   the host inbox either way.
3. **Three numbers, fixed** —
   (a) a machine silent for **10 min** produces **one** `home down` notice per
   host (the heartbeat is a fixed ≈1 min, derived from this);
   (b) notices are **coalesced per chat per 60 s** (the cap of 6 per chat per
   hour was removed, fleet-runtime §14h);
   (c) logs are kept **7 days or 1,000 events per delegation**, whichever comes
   first — also the grant lifetime, since grants are re-minted on every
   connect.
4. **A host may refuse by policy** (`host.accepts_kinds`), as an immediate
   structured error **naming the capability it lacks** — never a silent queue.
5. **`max_hold` is report-only** — a holder past its declared hold raises an
   `attention` naming it; a host never kills a holder to be fair.
