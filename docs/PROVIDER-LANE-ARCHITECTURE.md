# Workass provider lanes and chat engine

Status: binding architecture and implementation checklist.

This document defines how one Workass chat may use multiple providers without
losing provider-native context, replaying transcripts into replacement sessions,
or duplicating provider-specific chat behavior across the daemon, renderer, and
native hosts. `docs/PORT-SPEC.md` remains the binding product law; this document
is its detailed provider/chat architecture.

## 1. Required outcome

Workass has one provider registration system, one provider-neutral chat state
machine, and one canonical provider event/error model.

- A normal ACP provider is added declaratively: identity, launcher, arguments,
  discovery information, and reusable defaults.
- A provider with a unique native SDK adds one isolated adapter. It may replace
  at most the cohesive delivery and context strategies; it does not add branches
  to chat, persistence, UI, lifecycle, queue, or permission code.
- Provider adapters translate vendor protocols. They never own Workass chat
  state, persistence, retry policy, queueing, user-visible text, or fallback.
- Provider identifiers are not capability checks. No provider-name branch is
  permitted outside provider registration and provider adapter packages.
- The frozen LAN `invoke/reply/event` protocol remains byte-compatible.

The target is zero provider-specific changes to chat behavior when a provider is
added, not zero unavoidable code for translating a genuinely new vendor API.

## 2. Identity model

The following identities are distinct and MUST NOT be substituted for one
another:

- `ChatID`: the immutable Workass conversation identity.
- `LaneID`: one provider realm inside that chat.
- `ProviderRealm`: provider id plus non-secret account/install identity and the
  machine on which the provider-native thread exists.
- `WorkspaceEpoch`: immutable working-directory identity for a lane. Moving a
  chat creates an explicit new epoch; it never mutates an old native thread's
  execution root.
- `ThreadRef`: the exact provider-native thread identity.
- `ConnectionID`: a disposable adapter/host-process attachment.
- `TurnID`: one provider-native turn.
- `OperationID`: one durable Workass user action/import/control operation.

A lane key is:

```text
ChatID + ProviderRealm + MachineID + WorkspaceEpoch
```

Model, effort, and mode are controls on a lane and are not lane identity.
Provider updates and host-process restarts are not lane identity changes.

One Workass chat may own several lanes, but each established lane owns exactly
one provider-native lineage. Selecting another provider selects another lane;
returning to a provider resumes its exact lane. An official provider event may
advance a native alias only when it carries proof that the provider considers
the identity the same lineage. `/clear`, user forks, account changes, workspace
changes, and unproved id rotation create a new explicit lane epoch instead.

## 3. Architectural boundaries

```text
Desktop / Mobile / Agent control
              | ChatCommand + OperationID
              v
       ChatEngine actor/reducer
  transcript | presentation | lanes | queue | outbox | permissions
              | typed ProviderEffect
              v
        ProviderRegistry
              |
       ProviderDefinition
              |
        LaneFactory / Lane
              |
     ACP transport or native host
              ^
       typed ProviderEvent
```

There is exactly one Workass daemon process. Inside that process, each chat
actor owns every field scoped to its immutable `ChatID`, including title,
draft, grouping, unread/settled state, per-chat pane choice, provider controls,
the semantic transcript, and live runtime ownership. One daemon-wide
application state owns only genuinely global/view settings such as workspace
lists, theme, density, pane sizes, and the current application mode. A pure
snapshot projector combines those authoritative states into the frozen renderer
mirror; it owns and persists nothing. No second daemon, presentation service, or
parallel chat store is permitted.

Physical files may be split for durability or bounded writes, but that never
creates a second logical owner. A persisted renderer mirror may exist only as a
versioned, rebuildable materialized view. It is never recovery authority once a
chat actor has been migrated.

### 3.1 Provider registry

There is one aggregate runtime definition plus one declarative registration
row. Subsystems consume narrow typed facets from those two objects rather than
provider-name branches or one god object with optional methods.

```go
type ProviderDefinition struct {
    Identity ProviderIdentity
    Realm    RealmResolver
    Runtime  LaneFactory
    Metadata MetadataSource
    Update   UpdateStrategy
}

type ProviderRegistration struct {
    Identity, launch, discovery, update parameters
    Adapter ProviderAdapter // defaults to generic ACP
}
```

The registration fields use reusable built-ins. A standard ACP provider needs
only its descriptor parameters. A provider with real semantic differences may
replace the cohesive delivery strategy, context strategy, or both; operational
metadata facets such as catalog presentation or executable discovery remain
small registration policies and never alter chat behavior. Native hosts
implement the same semantic lane contract and keep vendor RPC details behind
their adapter.

The existing executable-resolution `Provider` interface remains a compatibility
facet while registrations migrate. It is not the chat runtime abstraction.

### 3.2 Mandatory lane contract

The required runtime surface stays small and typed:

```go
type LaneFactory interface {
    Create(context.Context, NewLaneRequest) (Lane, ThreadRef, error)
    Resume(context.Context, ThreadRef, ResumeLaneRequest) (Lane, error)
}

type Lane interface {
    Identity() LaneIdentity
    Thread() ThreadRef
    Delivery() DeliveryStrategy
    Context() ContextStrategy
    Events() <-chan ProviderEvent
    Detach(context.Context) error
}
```

The chat engine, not the adapter, decides whether `Create` or `Resume` is legal.
The provider runtime repeats that invariant defensively. `Detach` closes only
the transport/process attachment and never deletes the native thread.

### 3.3 Two cohesive behavior strategies

Provider semantic differences are composed through two strategies:

1. `DeliveryStrategy`
   - turn admission and consumption receipts;
   - stable client input identifiers;
   - live steering;
   - terminal readback/reconciliation;
   - ambiguous-delivery classification.
2. `ContextStrategy`
   - exact resume requirements;
   - non-sampling context import;
   - provider-native compaction/checkpoint observations;
   - verified lineage transitions.

The generic ACP strategies implement standard behavior. Capability support is
represented by a typed implementation and a versioned successful handshake,
not a provider-name check or an unverified boolean. Unsupported behavior returns
a typed `UnsupportedCapability` result; the chat engine applies one central,
explicit policy.

Provider catalog, commands, model controls, plan usage, installation, detection,
and updates share the provider registry but remain operational/metadata facets.
They do not enter the chat reducer.

### 3.4 Transport boundary

The ACP bridge becomes transport-only:

- process lifecycle and exact process-tree ownership;
- NDJSON JSON-RPC request/reply/event transport;
- stdout purity and stderr diagnostics;
- request correlation and stream coalescing;
- versioned capability handshake.

It does not decide provider switching, transcript replay, queue promotion,
steering fallback, compaction policy, UI messages, or chat persistence. Native
hosts expose the same versioned Workass semantic operations and normalize vendor
events before they reach the daemon.

## 4. Canonical provider events and errors

Adapters emit one typed event union. The minimum families are:

- lane attached/detached;
- native lineage observed/advanced with proof;
- turn admitted;
- user input consumed;
- assistant commentary/final delta and terminal content;
- tool and plan lifecycle;
- permission requested/resolved;
- usage/context update;
- native compaction started/completed/checkpointed;
- background work lifecycle;
- turn terminal;
- transport health/reconciliation result.

Every event carries `ChatID`, `LaneID`, `OperationID` when applicable, native
turn identity when available, and a monotonic adapter sequence. Vendor payloads
and `map[string]any` values stop at the adapter boundary.

Errors use one typed taxonomy:

- `TransientTransport`;
- `AuthenticationRequired`;
- `ProviderUnavailable`;
- `NativeThreadMissing`;
- `NativeIdentityConflict`;
- `UnsupportedCapability`;
- `AdmissionRejected`;
- `AcceptanceAmbiguous`;
- `PermissionPending`;
- `ProtocolViolation`;
- `ContextLimitReached`.

Retry policy belongs to the chat/runtime coordinator and keys on error class.
Error-string parsing and provider-specific retry schedules are forbidden.

## 5. Chat engine

Each Workass chat has one serialized actor/reducer. Desktop, mobile, renderer,
LAN, and agent-control input enter the same command mailbox with a stable
`OperationID`.

```text
Reduce(ChatState, ChatCommand) -> NewState + DurableEffects
```

### 5.1 State

The authoritative state contains:

- append-only visible semantic ledger;
- chat-scoped presentation metadata: current tab attachment, title/title lock,
  draft, grouping, cwd/workspace revision, unread/settled state, per-chat pane,
  and renderer-visible control metadata;
- lanes keyed by `LaneID`;
- active and desired lanes;
- one foreground turn owner;
- durable FIFO entries with an immutable target lane;
- pending steer owner;
- permission decisions and background work tied to origin lane/turn;
- delivery/import outbox;
- lane coverage and provider-private checkpoint metadata;
- controls and capability snapshot per lane.

Lane phases are explicit: `absent`, `creating`, `detached`, `resuming`, `ready`,
`importing`, `running`, `reconciling`, `blocked`, and `broken`. An established
lane cannot transition to `creating`. Establishment means that the actor owns a
nonzero immutable `ThreadRef`; a failed create with neither a `ThreadRef` nor a
provisional candidate is still `absent`, never a broken native thread.

### 5.2 Commands

The minimum commands are update chat presentation, select provider, submit,
queue/edit/reorder/remove, steer, cancel, resolve permission, attach host, host
lost, provider event, retry exact resume, explicit fork/reset, change workspace
epoch, delete, and settle/hibernate.

The reducer emits typed effects. Effects are durably recorded before an external
provider call. Their receipts return as commands; effect executors never mutate
state directly.

### 5.3 Foreground and queue laws

- Exactly one foreground turn owns the visible response stream.
- Steering always targets the lane that owns the running turn.
- Selecting another provider during a turn records `DesiredLaneID`; it cannot
  retarget or cancel the current turn.
- An ordinary send snapshots its target lane at admission and never silently
  moves when the selector changes.
- Queue promotion and transcript ownership happen in one reducer transition.
- Explicit cancellation closes a revision-fenced queue dispatch gate before the
  provider effect; only an explicit resume for that exact revision reopens it.
- Live steer and cancellation claim their exact durable effects directly; they
  never wait behind the ordinary outbox drain or hold the actor mutex across a
  provider acknowledgement.
- A message has exactly one visible owner: draft, durable queue, pending steer,
  or transcript.
- Permissions and background work retain their originating lane even after the
  foreground provider changes.
- Events from stale connection generations are rejected before reduction.

## 6. Provider switching transaction

Switching is one durable transaction:

1. Resolve and persist the target `LaneID` and desired controls.
2. At a safe foreground boundary, resume the exact target thread when the lane
   exists, or create it when the lane is provably absent. A message targets one
   immutable lane, so this step never pre-creates or fans out to unselected
   providers.
3. Compute visible semantic ledger events beyond the target lane's coverage.
4. If the target is provably unused, attach one deterministic bounded ledger
   seed to its first real sampling input, immediately before the current user
   request. Commit seeded/excluded coverage only when that exact input is
   consumed. This seed is never available again for that lane.
5. Otherwise import unseen events through `ContextStrategy` using stable
   operation ids and bounded, receipt-bearing chunks.
6. Reconcile ambiguous import results without resending unknown chunks.
7. Advance coverage only for consumed seed events or confirmed import chunks.
8. Commit the active lane and dispatch that lane's next queued message.

Until step 7, the previous lane remains the active, usable lane. Failure leaves
the target lane blocked with a precise reason; it does not half-switch the chat.

An established lane with a later nonempty handoff requires a non-sampling
context-import capability. An ordinary prompt, synthetic user message,
provider sampling turn, `session/load` of another thread, or transcript replay
is not an import implementation for that gap. A provider without safe import
may still join an established Workass chat only while its own lane has never
consumed input, through step 4's one-time seed.

## 7. History and compaction

Workass retains the complete visible semantic ledger. Removing Workass history
would break UI recovery, multi-device synchronization, auditability, provider
switching, and deterministic ownership reconciliation.

Provider-native context remains private to its lane:

- same-provider resume sends no Workass transcript seed;
- a provider lane that has never consumed input may receive the bounded
  first-input seed once; this never replaces or recovers an established lane;
- native compaction stays in the same thread;
- Workass stores coverage cursor, context usage, optional checkpoint id/hash,
  and verified lineage metadata;
- provider summaries or hidden reasoning never replace visible history;
- UI/internal/maintenance notices are never context events;
- cross-provider import uses selected semantic events, not renderer markup or
  raw archived JSON.

If a provider exposes no in-place compaction, Workass does not silently replace
its thread. The lane reports its finite context capability and blocks at a safe
boundary before exhaustion. Any fork/reset is explicit.

## 8. Failure and edge-case laws

### Exact resume

- Exact resume is a mandatory provider-conformance invariant, not a normal
  optional branch. Codex/Claude provider hosts must always resume a valid saved
  thread; failure is a provider/runtime defect surfaced on that exact lane.
- Host crash/restart retries attachment to the same `ThreadRef` only.
- `NativeThreadMissing` is a broken-lane invariant, never a reason to create.
- Provider uninstall/auth failure preserves the lane and allows other lanes to
  operate; reinstall/login may resume it.
- A protocol downgrade that loses a required capability blocks the operation.

### Creation before establishment

- `session/new` must return a native session id before Workass can dispatch a
  prompt. If creation fails before the actor records either a `ThreadRef` or a
  provisional provider candidate, no user input reached that attempted session.
- The lane remains absent and the failed or ambiguous create receipt remains in
  the outbox. Workass does not retry it in the background.
- The next explicit selection or submit for that exact lane starts a fresh create
  generation. It creates only the selected provider lane and never reuses,
  reconciles, or overwrites the older failed effect.
- A provisional candidate is different: it may already own an input and must use
  its negotiated readback/reconciliation contract rather than fresh creation.

### Delivery ambiguity

- Persist an outbox operation before sending.
- Admission, consumption, and terminal completion are separate receipts.
- Timeout after possible acceptance is `AcceptanceAmbiguous`; never resend.
- Use stable provider input ids/readback when supported. Otherwise surface the
  uncertain operation for explicit reconciliation.

### Import ambiguity

- Import is chunked with deterministic ids and content hashes.
- Coverage advances only on confirmed receipt.
- Idempotent retry is legal only when the provider contract proves the same id
  cannot be consumed twice.
- Partial import leaves the target lane non-active and resumable at the first
  unconfirmed chunk.

### Concurrency

- One chat actor serializes desktop, phone, agent, provider, and recovery input.
- Multiple devices deduplicate by `OperationID`.
- A provider switch during steering, permission, compaction, or reconciliation
  waits for the relevant safe boundary.
- Background work never changes foreground lane ownership.

### Realm, workspace, and lineage

- Account/install changes resolve to a different non-secret provider realm.
- Workspace changes create a new explicit epoch.
- Provider `/clear` or fork is an explicit new lane epoch.
- Only a provider-authenticated same-lineage event may advance a native alias.
- Model/catalog changes never replace a thread.

### Storage schema upgrades

- Storage upgrades run to completion before chat actors or provider runtimes
  exist. They perform no provider RPC and never choose, create, resume, replace,
  or replay a native thread.
- The supported actor v19 -> v20 upgrade binds every previously unattributed
  semantic event only when one exact stored lane/thread already proves its
  owner. The supported provider-lane v6 -> v7 upgrade removes
  transcript-derived cursor/hash fields and preserves immutable ownership.
- Duplicate, incomplete, cross-machine, or otherwise ambiguous ownership fails
  the upgrade without writing the actor. Invalid data never becomes a live
  recoverable-invalid chat, repair command, compatibility runtime branch, or
  guessed lane.
- Each converted file is written atomically, fsynced, read back, and
  ownership-checked. A mixed directory after a crash is safe because the next
  startup continues only the files still on the immediately preceding schema.
- Renderer snapshots cannot erase or overwrite lane bindings/outbox state.

Upgrades and ordinary commits use one authority order: durable actor commit,
then rebuildable renderer projection. There is no actor/mirror dual-write
transaction and no recovery path that chooses the newer-looking copy. Once the
release floor no longer contains v19/v6 storage, these two upgrade files are
deleted; their shapes never enter live chat or lane types.

### Renderer projection and event delivery

- Provider events are durably accepted by the owning actor before their frozen
  wire projection is published.
- Adapter sequences are contiguous per connection generation. A missing
  sequence is a protocol/reconciliation failure; it is never silently skipped.
- Backpressure is allowed. Dropping a typed event because a bounded channel is
  full is forbidden.
- The normalized event union must retain every renderer-visible semantic field:
  assistant phase/result boundaries, durable media, tool/plan lifecycle,
  permission labels/questions, steering boundaries, background ownership,
  timestamps, stable message/job ids, and terminal status.
- `session:get` is a pure projection from chat actors plus daemon-global
  application state. `session:save` may submit typed presentation commands and
  global preference updates, but cannot author or overwrite transcript,
  provider-lane, queue-runtime, foreground, outbox, permission, background, or
  usage state.

### Context-import capability boundary

The versioned generic ACP capability is `_meta.workassContextImportV1` with
`mode=non_sampling`, `receipt=operation_readback_v1`, `idempotent=true`, and
bounded event/byte limits. Import and readback use one immutable operation id,
range, and digest. A crash after provider acceptance is recovered by exact
thread resume plus readback; the payload is resent only after authoritative
`not found`.

The current Codex native protocol can accept injected input but does not expose
authoritative readback for that non-sampling import operation. Codex and Claude
therefore advertise exact resume but not cross-provider context import today.
That is an explicit capability boundary: a never-used Codex or Claude lane may
receive the one-time first-input seed, but returning after another provider adds
new unseen events blocks safely. The deterministic mock implements the full V1
protocol and is the conformance oracle. No later prompt, replay, or replacement
session disguises the missing provider capability.

## 9. Anti-slop enforcement

- No provider-name conditions outside registration/adapters (enforced by a
  source contract test).
- No vendor protocol objects outside adapters.
- No chat persistence or UI mutation inside adapters.
- No capability represented only by branding or string matching.
- No generic `Do(method string, map[string]any)` provider interface.
- No god interface forcing every provider to implement unrelated optional
  methods; use the registry plus the two cohesive behavior strategies.
- No micro-interface per RPC; related behavior stays in delivery or context.
- No automatic replacement, resend, or context downgrade; no replay outside
  the provably-unused lane's one-time initial sampling seed.
- No production release of an intermediate half-refactor.

## 10. Verification

### Provider conformance

Every durable chat provider passes the same deterministic suite:

- create only when absent;
- exact resume after host replacement;
- detach does not delete native context;
- stable lane/thread ownership;
- serialized turns;
- cancellation and permission cleanup;
- event normalization and sequence rejection;
- typed capability negotiation;
- no diagnostics on ACP stdout.

Optional capability suites cover live steer, consumption receipts, terminal
readback, context import, native compaction, typed phases, images, catalogs,
usage, and commands. Real providers are protocol canaries; deterministic mocks
are the correctness oracle.

### Chat reducer/property tests

- provider switching and switching back;
- switch during running turn;
- mixed-lane durable FIFO;
- duplicate device submissions;
- crash before and after send/receipt/terminal commit;
- ambiguous admission/import;
- permission and background work across switches;
- stale connection generation events;
- provider removal, login recovery, and protocol downgrade;
- workspace/account transitions;
- compaction concurrent with queued input;
- storage-upgrade collision rejection without partial writes.

### Structural gates

- A dummy ACP provider is added with registration parameters only and passes
  generic chat tests.
- A fixture provider overrides one delivery or context strategy without changes
  to the chat engine.
- Source checks reject new provider-name branches outside approved packages.
- The unmodified frozen LAN protocol passes existing compatibility tests.

## 11. Implementation checklist

No intermediate phase is production-ready by itself.

### Phase A — source of truth and characterization

- [x] Replace the obsolete native-resume fallback law.
- [x] Record this architecture and its acceptance gates.
- [x] Characterize current create/resume/steer/reconcile/compaction behavior.
- [x] Add source-contract tests for provider branching boundaries.

### Phase B — provider contract and registry

- [x] Add typed provider identities, definitions, canonical events, and errors.
- [x] Add generic ACP lane factory and default delivery/context strategies.
- [x] Register existing providers without changing external behavior.
- [x] Move executable discovery, catalog normalization, plan usage, commands,
      updates, steering, reconciliation, and compaction decisions behind their
      registered facets.
- [x] Prove a descriptor-only dummy provider.

### Phase C — chat actor and durable effects

- [x] Add provider-neutral `ChatState`, commands, events, reducer, and effects.
- [x] Route all ChatID-bearing input surfaces through stable `OperationID`
      commands; addressless compatibility bootstrap remains non-owning.
- [x] Move queue, steer ownership, permission, and background ownership into the
      reducer.
- [x] Add durable delivery/import outbox and generation fencing.
- [x] Make chat-scoped presentation state actor-owned and daemon-global UI state
      the only non-chat application authority.
- [x] Preserve the complete renderer-visible typed event contract in actor
      state.
- [x] Commit provider events durably before publication, with backpressure and
      contiguous sequence enforcement.
- [x] Upgrade every v19 nonempty chat before lane selection; reject ambiguous
      ownership without partial writes and never treat it as empty.
- [x] Derive the frozen renderer snapshot from authoritative state and make
      renderer saves presentation-command-only.
- [x] Remove legacy runtime dual writes and every direct manager/session-store
      bypass for ChatID-bearing mutations.

### Phase D — lane storage and exact resume

- [x] Add durable lane records using all identity dimensions; disposable tab ids
      are attachment metadata only.
- [x] Upgrade unambiguous native bindings transactionally before runtime.
- [x] Reject collisions before runtime instead of selecting a thread or
      persisting a recoverable-invalid chat state.
- [x] Remove `session/load`, fresh replacement, transcript replay, and automatic
      Workass compaction from established-lane recovery.
- [x] Enforce established-lane `Create` prohibition at coordinator and runtime.

### Phase E — cross-provider switching

- [x] Add desired/active lane selection and safe-boundary switching.
- [x] Add semantic coverage projection.
- [x] Add receipt-bearing context-import transactions with crash readback.
- [x] Implement and canary the versioned generic context strategy; providers
      that do not negotiate it remain explicitly unsupported.
- [x] Preserve origin lane for queued, steering, permission, and background work.

### Phase F — rollout

- [x] Pass focused, package, race, mock handshake, renderer, and frozen-wire
      gates.
- [x] Activate the reconciled daemon in isolated `dev` and exercise migrated,
      newly-created, rich-event, reconnect, and event-saturation chats through
      the rebuilt renderer.
<!-- Historical note: the provider/lane-core smoke below passed before the
     authoritative projection cutover. It is evidence for that core only, not a
     completed rollout gate. -->
- [x] Exercise one disposable provider-backed chat through the lane core in
      isolated `dev`.
<!--
     Cross-provider and crash-boundary behavior remain covered by deterministic
     conformance suites until the complete renderer projection gate passes.
-->
- [x] Prove source-contract ownership: no ChatID-bearing mutation bypasses the
      actor and no renderer save can mutate semantic/runtime state.
- [x] Prove crash consistency, migration idempotency, rich snapshot parity,
      contiguous event delivery, and zero-loss saturation.
- [ ] Audit existing production bindings read-only and produce a migration
      report before requesting any production activation.
- [ ] Publish only from a clean commit containing the complete reconciled
      product after explicit production authority.
