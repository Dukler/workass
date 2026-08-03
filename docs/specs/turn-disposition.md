# Turn disposition — an honest "done" per prompt

Status: SPEC v3 — v2 BUILT and PROMOTED; v3 (§V3) is the open lane, spec only.
Headless only. The UI lane that makes `stalled` visible is still owed.
**§V3 at the end of this file supersedes the marked parts of v1/v2. Read it
before building anything here — it replaces the evidence model.**
Owner: Fable (design + gate review). Builder: Opus (no codex lane available).
Origin: user, 2026-07-27 — "i need a way to track the actual status done of a
prompt i gave to you"; design settled with Fable (receipt
`wa-subagent-1785128675289-1`), gated by Fable (receipt
`wa-subagent-1785130185121-2`, verdict BUILD WITH THESE CHANGES).

v2 folds in gate amendments C1–C11. The two that mattered most: v1 mapped
failed turns onto `done`, and v1 left a phantom `working` record on an idle
chat after a cancel — both reproducing, inside the new ledger, the exact
phantom-state dishonesty this spec exists to kill.

## Binding laws for this lane (restated verbatim — the builder does not inherit them)

- Never rename referenced files/ids/paths; add aliases if a new name is needed.
- Do NOT commit. Do NOT touch files outside the scope below (the tree has
  extensive unrelated uncommitted work — leave it exactly as found).
- stdout purity for any ACP server code: JSON-RPC only on stdout, diagnostics
  to stderr.
- Wire contract (`invoke/reply/event`, `desktop/lan-server.js`) is frozen. New
  event payload FIELDS are additive-only. **This lane adds NO new channel.**
- User-visible UI chrome requires a mock the user approves BEFORE it exists.
  **This lane ships no chrome** — see Non-goals.
- Secrets are redacted before display or send. Every stored free-text field
  goes through `redactSensitiveText` and is length-bounded.
- Receipts: every claim in the final report needs the command you ran and its
  outcome.

## Problem (confirmed by code read)

The finished cue means one thing only: *a turn ended while you weren't looking*.
`store.ts:2934` sets `unread` when a job ends in a non-active chat; `unread` is
the sole input to `resolveStatus`'s `done` branch (`SidebarV2.tsx:95`). Nothing
anywhere records whether the user's **request** was answered.

An async agent parks by ending its turn. Where the park is on daemon-visible
work the chat correctly reads *Trabajando* — `working` outranks `done`
(`SidebarV2.tsx:99` before `:112`) and `live` counts running non-service
spawned work (`chat-activity.ts:16`). Three cases escape that and produce a
false "Listo":

1. Harness-internal waits with no registry record (self-scheduled wakeups).
2. The settled-work gap: `Wake == "pending"` armed (`spawned_work.go:726`) but
   nothing running; the wake is delivered only while idle.
3. Prose parks — a turn that ends asking a question, which is really
   needs-input wearing done's clothes.

ACP already carries a turn-end signal: `session/prompt` returns `stopReason`,
passed through unnormalized at `manager.go:3276` and shipped on the
`job:event {type:"end"}` payload (`manager.go:968` → `Job.Public()`,
`types.go:521`) as `string | null` (`wire/types.ts:74`). The renderer already
discriminates on it — for exactly one value, `'cancelled'` (`store.ts:2881`,
`:2935`, `:3848`). **`end_turn` is emitted for both a finished request and a
park** (`canary.go:873` asserts it on success), so the native signal alone
cannot answer the question.

## The model

### Obligation, not turn

The unit is the **chat's open obligation**, opened by a human-authored prompt.
Prompts serialize per bridge — `b.promptMu.Lock()` at `manager.go:3250` — so at
most one prompt is in flight per chat and per-chat obligation is equivalent to
latest-prompt status. Do NOT build a per-prompt ledger in this lane.

### States

| State         | Meaning                                                        |
|---------------|----------------------------------------------------------------|
| `working`     | A turn is running for this chat.                                |
| `parked`      | No turn running; something will resume this chat on its own.    |
| `needs_input` | Blocked on the user: permission, refusal, truncation, failure.  |
| `done`        | The request is answered; nothing further is coming.             |
| `stalled`     | Claimed parked (or left working), but nothing live is behind it.|

Closed obligations also record a close reason: `superseded` (a new human
prompt) or `cancelled` (the user stopped the turn). Neither is a cue.

### Sources

`native` (provider turn-end signal) · `declared` (model said so) · `inferred`
(daemon evidence). Every disposition records which produced it.

## The interface

New file `internal/acp/disposition.go`.

```go
type TurnDisposition string

const (
	// DispositionUnknown is the honest answer when a signal cannot tell a
	// finished request from a park. It is NOT a failure — it hands the
	// question to the inference tier instead of guessing.
	DispositionUnknown    TurnDisposition = ""
	DispositionDone       TurnDisposition = "done"
	DispositionNeedsInput TurnDisposition = "needs_input"
	DispositionParked     TurnDisposition = "parked"
	// DispositionCancelled closes the obligation silently. It is never a cue
	// and never renders: the user stopped this themselves.
	DispositionCancelled TurnDisposition = "cancelled"
)

type CompletionSignal struct {
	Disposition TurnDisposition
	Source      string // "native" | "declared" | "inferred"
	Note        string // bounded 1000, redacted; may be empty
}

// CompletionAdapter maps one provider's turn-end vocabulary onto the shared
// dispositions. Implementations MUST return DispositionUnknown rather than
// guess: mapping a yield to "done" is exactly the bug this exists to fix.
type CompletionAdapter interface {
	ProviderID() string
	FromNative(stopReason string, exitCode *int, interrupted bool) CompletionSignal
}
```

### The default ACP adapter (`acpCompletionAdapter`)

Registered for every provider without a more specific adapter. Evaluated **in
this order** — the first match wins:

| Condition                                   | Disposition   | Why |
|---------------------------------------------|---------------|-----|
| `interrupted == true`                       | `Unknown`, and the state machine writes **nothing** | Set only under `m.resetting` (`manager.go:951`) — a daemon restart, not a user act, and the process is exiting. The boot rule catches the persisted `working`. |
| `stopReason == "cancelled"` or `code == 130`| `Cancelled`   | The user stopped it. Close silently; never a cue. Matches the `unread` exclusion at `store.ts:2934`. |
| `code != nil && *code != 0`                 | `NeedsInput`  | **C1.** The ACP error path (`manager.go:1121-1128`) ends failed turns with code 1 and `stopReason` `""` or `"engine-crash"`. A crash is not silence, and today's renderer already shows a red failed pill for these — inferring `done` would regress that. Note carries the bounded, redacted error. |
| `refusal`                                   | `NeedsInput`  | The model declined; the decision is the user's. |
| `max_tokens`, `max_turn_requests`           | `NeedsInput`  | The turn was cut off; continuing is the user's call. |
| `end_turn`                                  | `Unknown`     | Emitted for both done and parked. **Never map this to done.** |
| empty / anything else                       | `Unknown`     | Unrecognised vocabulary is not evidence. |

Adding a provider means adding an adapter, not touching the state machine.

## Declaration: `workass_report_outcome`

New tool on the workass-agent MCP server, alongside the external-work tools in
`cmd/workass/agent_mcp.go` (schema next to `workass_register_external_work` at
`:255`; dispatch next to `:336`; control method `outcome.report` in
`cmd/workass/agent_control.go` next to `:246`).

```
workass_report_outcome {
  state: "done" | "needs_input" | "parked",   // required
  note:  string,                              // optional, bounded 1000, redacted
  tab_id, chat_id                             // optional; default = calling chat
}
```

Owner-authenticated exactly like `workass_settle_external_work` — reuse
`resolveExternalWorkOwner` (`spawned_work.go:331`). Available to every provider:
`agentMCPServers` (`manager.go:2390`) gates on ephemeral / command / owner-key
with **no provider check**, so codex receives it too.

**Semantics (C9).** Repeated calls within one turn: last wins. A turn that ends
cancelled discards its declaration. A tracked subagent whose owner key resolves
to the parent pair **must not** be able to declare the parent chat's outcome:
reject when the resolved owner is a subagent session, with a test.

### The mandate (C6)

The anchor in v1 was wrong. `manager.go:1287`'s agentLine lives in
`buildEnvironmentBrief` (`:1275`) → `buildAppChatPrompt` (`:1196`), which runs
only when `markSeeded` first returns true for a session (`:1092`). The genuinely
per-turn injection is `buildTurnRuntimeIdentity` (`:1220`, called `:1101`) and
the `perTurn*Rule` consts at `:1212`–`:1217`.

- **Full mandate in the seed**, appended to the agentLine block at `:1287`.
- **One-clause reminder per turn**, as a new `perTurnOutcomeRule` const beside
  `perTurnLanguageRule` / `perTurnHostUIRule` and prepended in
  `buildUserRequestBlock` (`:1217`). A seed-only mandate degrades over long or
  compacted sessions, and the declared tier is what the design leans on.

## The clamp

Precedence for the disposition of an ending turn:

1. **A pending permission for this chat ⇒ `needs_input`.** Evidence: an entry
   in `m.permissions` whose `permissionRequest.JobID` resolves to a `Job` with
   this `TabID`/`ChatID` (`manager.go:1673`, `types.go:515`). Note: this is
   near-unreachable live, since `session/request_permission` blocks
   `session/prompt` — keep it as a cheap invariant, do not claim it covers the
   mid-turn case (mid-turn the obligation is `working`, which is correct).
2. **Declared** (`workass_report_outcome` this turn), then **native**, then
   **inferred** — first non-`Unknown` wins.
3. **Demotion**, applied last.

### Demotion carries the burden of proof

A `done` from any source is demoted to `parked` **only on live park evidence**.

> **Liveness requirement (load-bearing).** A registry row that merely says
> `status == "running"` is NOT evidence on a platform that can verify it.
> Workass has already been stuck once by a phantom running row vetoing
> recovery; a veto built on stale state reproduces that incident with a new
> name.

**Evidence timing (C3).** Reconciliation is a ticker (`reconcileSpawnedWork`,
`spawned_work.go:982`); no pass runs at turn end. The clamp therefore triggers
**one synchronous evidence collection scoped to the ending chat's rows** —
`lsof` is already bounded to 1.5 s (`spawned_work_probe_unix.go:47`). The
periodic stall check hooks at the **end of `reconcileSpawnedWork`, after
`classifySpawnedWorkServices`** (`spawned_work.go:1077`), so a same-tick service
promotion deterministically removes evidence.

Live park evidence for a chat is any of:

- A spawned-work record with `Status == "running"`, `Role != "service"`, and:
  - kind `external`: done-file absent **and** (`PID == nil` **or**
    `externalPIDAlive(pid)`) — external rows may register with no PID
    (`spawned_work.go:366`), and reconcile only PID-checks when non-nil (C8);
  - liveness `probed`, **probe supported**: the PID probe returned a pid for
    its output file this collection;
  - liveness `probed`, **probe unsupported**: counts as evidence while its
    level authority stands (owning engine alive, `MissingSince` zero). **C4 —
    this is the production path.** `spawned_work_probe_other.go:8` returns
    `(nil, false)` on Windows, so without this rule every park on the work
    laptop stalls at 90 minutes while the lane genuinely runs. A *supported*
    probe returning empty stays non-evidence, keeping the stuck-queue
    protection intact on Unix;
  - liveness `subagent`: the in-memory subagent registry still holds the run;
  - liveness `in-process`: the owning engine is not exited.
- A record with `Wake == "pending"` not yet delivered (`spawned_work.go:726`) —
  the settled-work gap. Disjoint from the running-row bullets by construction:
  `markSpawnedWorkWakePendingLocked` only arms on non-running records.

**A probe that fails this collection** (timeout ⇒ `ok == false`,
`spawned_work_probe_unix.go:55`) preserves the prior verdict rather than reading
as absence — mirror `classifySpawnedWorkServices` (`spawned_work.go:1149`).

> **Trap to respect.** `hasRunningSpawnedWorkForChat` is NOT a usable evidence
> oracle: it also excludes external and subagent classes for bridge-pinning
> reasons (`spawned_work.go:877`). The evidence set is its own function.

### Stall detection

An obligation in `parked` with no live park evidence for `stalledGrace`
transitions to `stalled`.

```go
// stalledGrace is deliberately generous: a self-scheduled wake is invisible to
// the daemon and clamps at 3600s in the harness, so 90 minutes is 1.5x the
// worst legitimate case. A shorter clock converts normal parking into a false
// alarm — the failure mode that teaches the user to ignore the cue.
const stalledGrace = 90 * time.Minute
```

**Clock semantics (C7).** Measured from `ParkedSince`, the only persisted
clock. Consequence, pinned deliberately: a chat parked longer than the grace on
genuinely live work goes `stalled` on the first tick after that evidence dies.
That is correct — nothing will wake it. A probe blip must not trigger it, which
is what the preserve-prior-verdict rule above guarantees.

**Restart (C2b).** After `loadSpawnedWorkSnapshots()` completes on boot:

- a persisted `parked` obligation whose live evidence cannot be re-established
  ⇒ `stalled` **immediately**, not after the grace — the restart is itself
  proof that an in-process or harness-internal wait died;
- a persisted `working` obligation with no running job ⇒ `stalled`
  **immediately** — turns cannot survive the daemon, and a `working` record on
  an idle chat is the phantom-row incident wearing a new name.

Daemon-visible parks survive (snapshots reload at `spawned_work.go:1593`/
`:1671` and PIDs re-probe), so neither rule fires for a wait that genuinely
lived.

## State machine

| From        | Event                                                | To            |
|-------------|------------------------------------------------------|---------------|
| *(any)*     | human-authored prompt accepted                        | `working`; previous obligation closed `superseded`, receipt kept |
| *(any)*     | turn starts with **no** new human prompt (wake-driven)| `working`; rescinds the prior close (C5 — this applies from `done`, `stalled` and `needs_input`, not only `parked`) |
| `working`   | turn ends, disposition `done` (post-clamp)            | `done`        |
| `working`   | turn ends, disposition `needs_input`                  | `needs_input` |
| `working`   | turn ends, disposition `parked`, or `unknown` **with** live evidence | `parked` |
| `working`   | turn ends, disposition `unknown`, no live evidence    | `done` (source `inferred`) |
| `working`   | turn ends, disposition `cancelled`                    | closed `cancelled`, silently — **not** left `working` |
| `working`   | turn ends `interrupted`                               | no write; boot rule stalls it |
| `parked`    | no live evidence for `stalledGrace`                   | `stalled`     |
| `parked`    | boot, evidence unrecoverable                          | `stalled`     |
| `working`   | boot, no running job                                  | `stalled`     |

A mid-turn human **steer** does not open or close an obligation; `PromptID`
stays the opening prompt (C10).

The `unknown` + no-evidence → `done` row is what preserves today's behaviour for
providers that never declare: today's cue, minus the false parks. Combined with
C1's failure carve-out, **no provider regresses.**

## Persistence

`state/obligations/<tabID>.json`, one array of records keyed by chat, written
with the same atomic tmp + rename + `0o600` pattern as
`persistSpawnedWorkSnapshot` (`spawned_work.go:1593`). Record:

```go
type ObligationRecord struct {
	TabID, ChatID string
	State         string // working | parked | needs_input | done | stalled
	Source        string // native | declared | inferred
	CloseReason   string // "" | superseded | cancelled
	Note          string // bounded 1000, redacted
	OpenedAt      string // RFC3339Nano, the human prompt
	UpdatedAt     string
	ClosedAt      string // empty while open
	PromptID      string // startOpts.UserMessageID of the opening prompt
	ParkedSince   string // empty unless State == parked; drives stalledGrace
}
```

Closed obligations are kept as receipts, capped at 128 per chat (newest wins),
mirroring the spawned-work receipt cap. **Chat/tab deletion drops that chat's
records** on the next persist.

## Wire (additive only)

One additive field in `Job.Public()` (`types.go:521`), which feeds **both**
`job:event {type:"end"}` (`manager.go:968`) and `job:event {type:"start"}`
(`manager.go:925`):

```go
"disposition": map[string]any{"state": ..., "source": ...},  // omitted when unknown
```

Omitting on unknown is what keeps `start` payloads clean — pin it with a test.

Chat-level obligation is exposed **read-only**, no push:

- `obligation.get {tab_id, chat_id}` on the agent-control surface (dotted-name
  pattern matching `external.register`, `agent_control.go:246`).
- An additive `obligation` field on the `session:get` reply, at the daemon-side
  assembly site.

**No new channel. No renderer change.** A renderer that ignores both fields
behaves exactly as today.

## Hard cases (all closed)

- **Provider that never declares (codex, qwen, mock):** native + inferred.
  `end_turn` → `unknown` → no live evidence → `done`. Identical to today.
  Inference may never produce `needs_input` from silence — only a pending
  permission, a non-zero exit, or an explicit native `refusal`/`max_tokens`.
- **Model forgets to declare:** silence tier. No penalty.
- **Model declares `done` while a wake is armed:** clamped to `parked`.
- **Model declares `parked` forever:** `stalledGrace` → `stalled`.
- **Model declares `done` and the work is wrong:** undetectable by
  construction. The receipt records "declared done at T" so the failure is
  attributable; the user's read arbitrates.
- **Turn killed by the user:** obligation closes `cancelled`, silently.
- **Daemon restart mid-turn or mid-park:** see Restart.
- **Superseding prompt:** serialization (`manager.go:3250`) means a new human
  prompt closes the open obligation as `superseded` and opens a new one.
- **"thanks":** opens an obligation, closes trivially. Do not build gratitude
  detection.

## Non-goals (explicitly out of this lane)

- Any pill, label, badge, or `resolveStatus` change. `stalled` **will** render
  as needs-attention in the follow-up UI lane, mock-first — not here.
  Re-semantizing "Listo" changes *when* an existing pill appears, which is
  behaviourally chrome even with zero new pixels.
- Prose-question detection. A false needs-attention teaches the user to ignore
  the cue; the mandate is the channel, not a heuristic.
- A per-prompt ledger beyond the obligation receipt.
- Any new wire channel.

## Test obligations

Go, in `internal/acp/disposition_test.go` unless noted:

1. `end_turn` maps to `Unknown`, never `done`.
2. `refusal` and `max_tokens` map to `needs_input`.
3. User cancel (`cancelled` / code 130) closes the obligation `cancelled` and
   leaves **no** `working` record.
4. Declared `done` + armed wake ⇒ `parked` (demotion).
5a. Declared `done` + a `running` row whose **supported** probe returns empty
    ⇒ `done`. *(The stuck-queue pin: a stale row must never veto.)*
5b. Declared `done` + a `running` row whose probe is **unsupported** ⇒ `parked`.
    *(The Windows path, otherwise untested by construction on the Mac box.)*
6. Declared `done`, nothing live ⇒ `done`.
7. Pending permission for the chat outranks a declared `done` ⇒ `needs_input`.
8. `parked` past `stalledGrace` with no live evidence ⇒ `stalled`.
9. Boot with an unrecoverable `parked` obligation ⇒ `stalled` immediately.
10. A turn starting with no new human prompt rescinds a prior close — from
    `done` and from `stalled`, not only `parked`.
11. A new human prompt closes the open obligation `superseded` and opens a new
    one.
11. Obligation survives a snapshot round trip.
12. Undeclared provider (`mock`) reproduces today's cue: `end_turn`, nothing
    live ⇒ `done`.
13. **The headline.** Undeclared provider, `end_turn` ⇒ `Unknown`, **with** live
    spawned work ⇒ `parked`. *(The false-"Listo" fix itself; every other
    demotion test uses a declared done.)*
14. Failed turn (code 1, `engine-crash` or empty) ⇒ `needs_input`, not `done`.
15. Boot with a persisted `working` obligation and no running job ⇒ `stalled`.
16. `note` is redacted and bounded through `workass_report_outcome`.
17. Owner-auth: a key not bound to the chat cannot write its outcome; a tracked
    subagent cannot declare its parent's outcome.
19. A probe failure (`ok == false`) does not stall a parked obligation.

Renderer, in `desktop/renderer2/tests/`:

20. A `job:event` payload carrying an unknown additive `disposition` field is
    tolerated and changes no rendered state (wire-contract law).
21. `disposition` is absent from `job:start` payloads when unknown.

## Build order

1. `disposition.go` — types, adapter, ACP mapping. Tests 1, 2, 15.
2. Obligation record + persistence + state machine. Tests 3, 8–13, 16.
3. Clamp + evidence collection + liveness. Tests 4, 5a, 5b, 6, 7, 14, 19.
4. MCP tool + control method + seed mandate + per-turn clause. Tests 17, 18.
5. Additive wire fields + `obligation.get` + `session:get`. Tests 20, 21.

Optional, mechanical: an additive disposition assertion next to
`canary.go:873`.

Gate: `scripts/gate.sh` must end `GATE_PASS`.

---

# §V3 — harness-native evidence (the open lane)

Origin: user, 2026-07-27, after a 76-minute false park in this very chat —
"stalled grace IS THE BANDAID"; "IM 100% SURE THAT CLAUDE CODE HANDLES THIS
AUTOMATICALLY"; "look up how t3 code does it". Both claims are correct and are
now backed by receipts. v3 supersedes **§Sources**, **§Stall detection**, and
closes **P3** (the daemon cannot see a harness-initiated turn).

## V3.0 What the research changed

v1/v2 built a private evidence model because we believed the harness told us
nothing beyond `stopReason`. That belief was wrong. The Claude Agent SDK we
already vendor (`@anthropic-ai/claude-agent-sdk` **0.3.217**, CLI **2.1.220**)
publishes every fact this spec invented machinery to guess.

**Probe receipt** (`/tmp/workass-turn-probe/probe.mjs`, real SDK + real
`claude` binary, one background shell task, no Workass in the loop). Elided
only for length; the ordering and every field below is verbatim:

```
  780ms HOOK UserPromptSubmit prompt_id=551dc6e9…
 5628ms HOOK Stop  background_tasks=[{id:bh9pgzu5n,type:shell,status:running,…}]
                   session_crons=[]  prompt_id=551dc6e9…
 5630ms MSG  result subtype=success stop_reason=end_turn terminal_reason=completed
 9676ms MSG  system subtype=task_notification
 9720ms HOOK UserPromptSubmit prompt_id=6ba3698f…      <- NEW turn, nobody typed
12533ms HOOK Stop  background_tasks=[] session_crons=[] prompt_id=6ba3698f…
12536ms MSG  result origin={kind:"task-notification"} terminal_reason=completed
```

That trace **is** the 76-minute incident, reproduced in 12 seconds:

- turn 1 ends `end_turn` while a background task is still running — a **park**,
  and the harness says so at the instant the turn ends;
- turn 2 is born with **no `session/prompt`** — the case whose prose the daemon
  drops on the floor (`bridge.go:700-717`, guarded on `job != nil`);
- turn 2 ends with **nothing in flight and nothing scheduled** — a **done**,
  provable rather than inferred.

### The four native signals (all confirmed present in our runtime)

| Question v1/v2 guessed at | Harness signal | Receipt |
|---|---|---|
| Is this done, or parked waiting to be woken? | `Stop.background_tasks[]` + `Stop.session_crons[]` | probe, above. The SDK typedoc states the purpose verbatim: *"Lets hooks distinguish 'session is done' from 'session is paused waiting for background work to wake it'."* |
| Did a turn start that nobody asked for? | `UserPromptSubmit` hook fires with a fresh `prompt_id` while no `session/prompt` is outstanding | probe, 9720ms |
| Who started it? | `SDKMessage.origin.kind` — `human` \| `task-notification` \| `auto-continuation` \| `peer` \| `coordinator` \| `observer` \| `channel` \| `observer-activity` | probe, 12536ms |
| Why did the turn really end? | `result.terminal_reason` — 18 values incl. `completed`, `max_turns`, `budget_exhausted`, `api_error`, `aborted_tools`, `hook_stopped`, `tool_deferred` | probe, 5630ms |
| Is the model blocked on a human? | `Notification` hook, `notification_type` ∈ `permission_prompt`, `idle_prompt`, `agent_needs_input`, `agent_completed`, … | `sdk.d.ts` `NotificationHookInput` |
| Did the turn die on an API error? | `StopFailure` hook, typed `error: SDKAssistantMessageError` | `sdk.d.ts` |
| What is the unit of the user's request? | `prompt_id` — typedoc: *"UUID correlating a user prompt with all subsequent events until the next prompt."* | probe, both turns |

`prompt_id` settles the turn-vs-prompt argument v2 deferred: the harness
already keys exactly the unit we wanted, so we adopt its identifier instead of
minting one.

**`session_crons` is the one signal not exercised by the probe.** Static
receipt only: the string appears 4× in the CLI bundle 2.1.220 alongside
`ScheduleWakeup`. Treated below as *load-bearing but unproven*; V3.6 pins the
consequence so an absent-or-broken `session_crons` cannot cause a false alarm.

### What T3 Code does (the cross-check the user asked for)

T3 Code's provider sessions carry `status ∈ idle|starting|running|ready|
interrupted|stopped|error` and an **`activeTurnId`** — *"nil = idle/ready;
populated = actively processing"* — with turn state `running|interrupted|
completed|error`. Needs-input is **not a state**: it is an outstanding request
(`thread.approval-response-requested`, `thread.user-input-response-requested`).

Two lessons, one of which is our bug:

1. **The provider owns turn activity; the client mirrors it.** T3 never infers
   "a turn is running" from its own send bookkeeping — it reads `activeTurnId`
   off the provider session. Workass inferred it from whether *it* had an
   outstanding `session/prompt`, which is precisely why a harness-born turn was
   invisible. V3.2 moves us to T3's side of that line.
2. **Blocked-on-human is an outstanding request, not a guessed state** — which
   is what v2 already does via the permission record, and v3 keeps.

T3 has no `parked`: its `ready` conflates done with parked, so it inherits the
bug this spec exists to kill. We take its architecture, not its state set.

## V3.1 Host contract — `_workass_claude_turn` (additive)

One new additive `session/update`, modelled exactly on the shipped
`_workass_claude_spawned_work` precedent (`bridge.go:722`), which is already
documented as *"may arrive after the user turn ended, so it must not require a
live job."* No new channel; the wire contract is untouched.

`queryOptions()` (`scripts/claude-native-host.mjs:292`) gains a `hooks` block.
It composes with the existing `settingSources: ['user','project','local']`;
the user's own shell hooks keep working.

```
_workass_claude_turn
  phase: "started"
    promptId       string   // BaseHookInput.prompt_id
    humanAuthored  bool     // false iff this.activePrompt === null at hook time
  phase: "ended"
    promptId       string
    backgroundTasks []{ id, type, status }   // NO command, NO description
    sessionCrons    []{ schedule, recurring } // NO prompt text
    terminalReason  string   // result.terminal_reason, "" when absent
    stopReason      string
    originKind      string   // result.origin?.kind, "" when absent (= human)
  phase: "failed"
    promptId, error (bounded string)          // StopFailure
  phase: "attention"
    promptId, notificationType, message (bounded)  // Notification
```

**Laws for the host half — each one is a rejection criterion:**

1. **Redaction before send.** `background_tasks[].command` and `.description`,
   and `session_crons[].prompt`, are free text that can carry secrets or paths.
   They are **never** emitted. Only `id`/`type`/`status` and
   `schedule`/`recurring` cross the wire. (`Wire contract` + secrets law.)
2. **Bounded.** At most 20 entries per array, `notificationType` and `error`
   clipped to 200 chars. Everything else is dropped, and a `truncated` count is
   sent instead.
3. **Subagent firings are not session turns.** Every hook input carries
   `agent_id` *"present only when the hook fires from within a subagent."* A
   hook input with a non-empty `agent_id` MUST be ignored for turn lifecycle.
   Without this, every `Agent` tool call ends the session's turn.
4. **`UserPromptSubmit` blocks the prompt.** It has a 30s timeout and a
   timeout blocks that prompt. The callback does one synchronous stdout write
   and returns `{}`. It must not await anything and must not throw.
5. **`humanAuthored` is decided by `this.activePrompt`, not by origin.**
   `origin` is only present on the *result*, i.e. at turn end — too late for
   birth. `activePrompt` is assigned synchronously in `prompt()` before the
   message is pushed, so it is always set before the SDK can fire the hook for
   a human turn. `originKind` at `phase:"ended"` is corroboration, never the
   discriminator.
6. **Ordering is relied upon and confirmed:** `Stop` fires before the `result`
   (5628ms vs 5630ms). `phase:"ended"` is therefore emitted from the `Stop`
   hook and reaches the daemon before `job:end` for a human turn. A missing
   `Stop` (older CLI, hook disabled by `disableAllHooks`) must degrade to
   exactly v2 behaviour, never to a wrong verdict.

## V3.2 Daemon contract — adopt the harness-born turn (closes P3)

On `_workass_claude_turn phase:"started"` with `humanAuthored:false` and no
running job for the session, the daemon starts a **continuation job** through
the normal start path with `HumanAuthored=false` (the flag that already exists
from v2), so:

- `jobForSession` resolves, and the guards at `bridge.go:700-717` stop
  discarding the model's prose — **the user-visible half of the incident**;
- `resumeObligation` runs instead of opening a new obligation, exactly as a
  wake-driven queue item does today;
- every existing guard, archive path and renderer consumer works unmodified.

Additive `job:end` gains nothing new: v2's `disposition` field already carries
the verdict.

**Counter for the drop.** The `job == nil` discard at `bridge.go:700-717` is
invisible even to diagnosis today. A counter increments on every dropped
job-less `agent_message_chunk`, surfaced in the daemon's existing metrics. If
adoption works, it stays at zero; if it regresses, the number says so.

## V3.3 Clamp precedence (supersedes §Sources)

Sources become **`declared` > `harness` > `native` > `inferred`**, with
`harness` new and `native` (bare `stopReason`) demoted to a fallback for
providers that produce no harness evidence.

| Situation | Verdict | Source |
|---|---|---|
| Model declared `done`, harness reports any `backgroundTasks` or `sessionCrons` | `parked` | `harness` (demotion) |
| Model declared `done`, harness reports both empty | `done` | `declared` |
| Model declared `needs_input` / `parked` | as declared | `declared` |
| No declaration; `Stop` with both arrays empty and `terminalReason == "completed"` | `done` | `harness` |
| No declaration; `Stop` with either array non-empty | `parked` | `harness` |
| `phase:"failed"` (StopFailure) | `needs_input`, note = typed error | `harness` |
| `phase:"attention"` with `notificationType` ∈ `permission_prompt`,`idle_prompt`,`agent_needs_input` | `needs_input` | `harness` |
| `terminalReason` ∈ `max_turns`,`budget_exhausted`,`prompt_too_long`,`structured_output_retry_exhausted` | `needs_input` | `harness` |
| `terminalReason` ∈ `aborted_streaming`,`aborted_tools` | leave obligation open (user cancel) | — |
| No harness evidence at all (codex, qwen, mock, old CLI) | exactly v2 | `native`/`inferred` |

**Harness evidence may demote a declared `done`, and may resolve an
undeclared turn. It may never overrule a declared `needs_input` or `parked`** —
a model that says it is blocked knows something the harness cannot see.

**The demotion rule from v2 §Demotion carries the burden of proof survives and
is strengthened.** Harness evidence is read from the harness at the instant the
turn ends, so unlike the daemon's registry it *cannot* be stale — it is the
liveness the v2 rule demanded, and it retires the stuck-queue hazard for
Claude sessions rather than reintroducing it.

## V3.4 Service classification, one layer up

A `backgroundTasks` entry of `type: "shell"` that never exits (`expo start`)
would pin a chat `parked` forever — the exact bug the service classifier fixed
for registered lanes, rebuilt here. The workass-mobile lane declined to wire
`liveWork` to its pill for this reason and was right.

Rule: a `phase:"ended"` whose *only* park evidence is background tasks that
were already present at the previous turn's end, with unchanged `status`, is
**not** fresh park evidence. It parks once; a second identical report does not
re-arm it. `sessionCrons` are exempt — a schedule is by definition a future
wake.

## V3.5 Stall detection (supersedes §Stall detection)

v2 kept `stalledGrace = 90m` because *"a self-scheduled wake is invisible to
the daemon"*. **`sessionCrons` is that observation.** The premise is gone for
Claude sessions:

- Obligation `parked`, harness evidence **complete** (a `phase:"ended"` was
  received for the latest `promptId`), both arrays empty ⇒ **`stalled`
  immediately.** No grace. The harness has stated there is nothing to wake it.
- Obligation `parked`, harness evidence complete, `sessionCrons` non-empty ⇒
  parked, and the note carries the schedule. No stall while a schedule stands.
- Obligation `parked`, **no** harness evidence (any other provider, older CLI,
  hooks disabled) ⇒ `stalledGrace` at **90 minutes**, unchanged.

The sweep is not deleted — deleting it would strand every non-Claude provider
in permanent silence, and the user's instruction was that the *grace* is the
band-aid, not that detection is. For Claude it becomes evidence-driven and
instant; elsewhere it stays the honest backstop. The 10-minute cut (`7f7d9dda`,
reverted at `a7264a73`) is not revived: it was the wrong lever at the wrong
layer and would have false-alarmed every legitimate sub-hour park.

## V3.6 Failure modes, closed

| Failure | Consequence | Guard |
|---|---|---|
| `session_crons` never populates (unproven signal) | a self-scheduled wake looks like nothing scheduled ⇒ immediate false `stalled` | The immediate-stall path requires `backgroundTasks` **and** `sessionCrons` both empty **and** `terminalReason == "completed"` **and** no daemon-side park evidence. There is deliberately **no** config switch: an early stall is self-healing — the wake reopens the obligation on its next turn, costing one wrong pill for one tick — while a late one costs an hour. A flag here would only have kept the 90-minute grace alive under another name. |
| Hooks disabled (`disableAllHooks`, policy settings) | no harness evidence at all | Absent evidence ⇒ v2 path verbatim. Never a verdict. |
| Older CLI without `background_tasks` | field is `undefined`, not `[]` | `undefined` is *unknown*, `[]` is *empty*. Only `[]` proves quiet. Unknown ⇒ v2 path. |
| Hook fires inside a subagent | every Agent tool call ends the session turn | V3.1 law 3, pinned by test. |
| `UserPromptSubmit` callback slow/throws | the user's prompt is blocked for 30s or fails | V3.1 law 4, pinned by test. |
| Harness-born turn adopted while a human turn is running | two jobs on one chat | Adoption requires *no running job*; a race loses to the human turn, which is the safe direction. |
| Continuation job never ends (adopted, then the SDK dies) | phantom `working` — the incident this spec exists to kill | The adopted job ends on the same `result`/`Stop` path as any other, and v2's restart reconciliation already stalls a `working` obligation with no running job. |

## V3.7 Test obligations (all must fail without the fix)

1. `Stop` with a running background task demotes a declared `done` to `parked`.
2. `Stop` with both arrays empty resolves an **undeclared** turn to `done`,
   source `harness`.
3. A `phase:"started"` with `humanAuthored:false` and no running job adopts a
   continuation job; an `agent_message_chunk` that follows reaches the archive.
   **This is the regression test for the lost message.** Without adoption the
   text is dropped and the assertion fails.
4. The adopted job resumes the existing obligation and does **not** open a new
   one (no new `promptId` on the record).
5. A hook input carrying `agent_id` is ignored for turn lifecycle.
6. `backgroundTasks[].command`, `.description` and `sessionCrons[].prompt`
   never appear on the wire — asserted against a payload containing a
   secret-shaped string.
7. Arrays longer than 20 are truncated and report `truncated`.
8. `parked` + complete harness evidence + both empty ⇒ `stalled` on the next
   sweep tick with **no** grace elapsed.
9. `parked` + `sessionCrons` non-empty ⇒ never stalls, and the note carries the
   schedule.
10. `parked` + **no** harness evidence ⇒ still 90 minutes (non-Claude provider
    unchanged).
12. `StopFailure` ⇒ `needs_input` carrying the typed error, and does not read
    as `done`.
13. `terminalReason: aborted_tools` leaves the obligation open (user cancel).
14. Harness evidence never overrules a declared `needs_input`.
15. Repeated identical `backgroundTasks` across two turn-ends do not re-arm a
    park (V3.4).
16. `background_tasks: undefined` (old CLI) ⇒ v2 path, not `done`.
17. Mock SDK drives 1–16 through the real host and the real bridge — the mock
    is the oracle, per `desktop/acp/README.md`. **The mock must invoke
    `options.hooks` callbacks itself**; it emits none today.
18. The job-less-chunk drop counter is zero across the adoption test.

## V3.8 Build order

1. **Mock first** (`desktop/acp/mock-claude-agent-sdk.mjs`): call the
   registered `hooks`, emit `origin` and `terminal_reason`, and script a
   `task_notification` second turn. Nothing downstream is testable until the
   oracle can produce the trace the probe captured.
2. **Host** (`scripts/claude-native-host.mjs`): `hooks` in `queryOptions`,
   `_workass_claude_turn` emission, redaction + bounds + `agent_id` guard.
3. **Bridge** (`internal/acp/bridge.go`): accept the update; drop counter.
4. **Adoption** (`internal/acp/manager.go`): continuation job on
   `phase:"started"`, `HumanAuthored=false`.
5. **Clamp** (`internal/acp/obligation_clamp.go`): harness tier, precedence
   table, V3.4 rule, V3.5 stall scoping, config flag.
6. Gate must end `GATE_PASS`. Renderer untouched — no chrome in this lane.

## V3.9 Non-goals

- No new pill, no `resolveStatus` change. Re-semantizing "Listo" is still
  chrome and still owed a separate lane.
- No change to `workass_report_outcome` or the mandate. A declaration still
  outranks everything.
- No Windows-specific work: this evidence is provider-side, not `lsof`-side,
  so it works identically on a managed machine — the first tier in this system
  that does.

## V3.10 Gate amendments D1–D11 (Fable, receipt `wa-subagent-1785181335049-7`)

Verdict: **BUILD WITH THESE CHANGES**. Full text `/tmp/fable-v3-gate.md`. These
supersede the §V3 text they touch.

**D1 — Adoption sends NO prompt, and must have something that ends it.**
§V3.2's "through the normal start path" is itself the defect: that path sends
`session/prompt`, which for a harness-born turn would push a spurious user
message into the live SDK stream *and* be settled by the harness turn's own
terminal result. Worse, with no prompt RPC in flight **nothing ends the job** —
the `session/prompt` reply is the only turn-end signal in the ACP contract, and
the reconciliation watchdog (`manager.go:3050-3111`) lives inside that RPC. An
adopted job that never ends parks every later human prompt behind `ErrChatBusy`
(`manager.go:848`) on a phantom `working` — the stuck-queue incident rebuilt.
Adoption is therefore: job record + `setJobForSession` + `resumeObligation` +
`job:start`, **no prompt send, no `promptMu`**; ended by `phase:"ended"`, with
the host emitting `ended` on every terminal result including when
`activePrompt === null`, plus engine-exit and inactivity backstops. Test 3 is
insufficient alone — add: **the adopted job ends and its obligation settles.**

**D2 — Cancel and steer must not be dead controls during an adopted turn.**
Host `interrupt()` gates on `this.activePrompt`, so `session/cancel` is a
silent no-op on a harness-born turn: the stop-button-no-op incident reborn.
Track `harnessTurnActive`; `interrupt()` runs `query.interrupt()` on it. Steer
may keep rejecting, but loudly. `turnStatus` must not report the previous
turn's `completed` during an adopted turn.

**D3 — Immediate stall also requires no daemon-side park evidence.** The
harness cannot see the daemon's wake machinery. A chat parked on a registered
external lane — the blessed `spawn-tracked-lane.sh` workflow — ends its SDK
turn with both arrays empty and `terminal_reason: completed`, and §V3.5 as
written stalls it instantly: a false alarm on the most sanctioned park in the
product. Gate on harness-empty **AND** `!chatHasLiveParkEvidence`
(`obligation_clamp.go:204`).

**D4 — Precedence is capability rules, not a ladder.** A blanket
"harness > native" marks a `refusal` (native ⇒ `needs_input`,
`disposition.go:100-115`) as `done`, and could un-cancel a cancel (checked
first, `obligation_clamp.go:157`). Rules: cancellation is a control tier above
everything; harness evidence **may** demote `done`→`parked`, resolve `Unknown`,
and add `needs_input`; it **may never** override native `needs_input`/
`cancelled` or a declared `needs_input`/`parked`. The `aborted_*` row means
"no harness verdict; native cancel handling decides" — not "leave the
obligation open", which would pin a phantom `working`.

**D5 — Harness evidence is buffered per session, latest-wins, consumed only at
job end.** Steers mint a new `prompt_id` per direction and produce several
Stops per job (the host deliberately spans SDK turn boundaries). An
intermediate `ended`/`failed` never settles a running job. Law: hook callbacks
notify only — they never touch `activePrompt`, `steerBoundaries` or
`settlePrompt`. That is the whole no-regression argument. Add a steer test.

**D6 — `recurring: true` is not evidence this request is unfinished.** A
standing schedule (`/loop`, `CronCreate`) would make `done` permanently
unreachable for a looping chat — strictly worse than v2, forever. Only a
one-shot cron (`recurring: false` — `ScheduleWakeup`) demotes a declared
`done`. And `sessionCrons` suppresses only the *immediate* stall: the 90m
backstop stays as the outer bound, because a scheduled wake can die with the
engine.

**D7 — `phase:"failed"` must not fire through the transparent OAuth retry.**
Suppress it while the retry is engaged, and pin with a fixture test: transient
OAuth + hooks ⇒ final `done`, no auth note.

**D8 — A terminal result whose `origin.kind` is non-human never settles
`activePrompt` and never consumes a steer boundary.** Closes the race where a
harness turn concludes while a human prompt is queued and settles the human's
prompt (`claude-native-host.mjs:756-768`). Fable independently confirmed the
`humanAuthored` discriminator itself **holds**, first prompt included:
`enqueuePrompt` assigns `activePrompt` in the synchronous Promise executor
before `input.push`, and the SDK cannot fire `UserPromptSubmit` for a message
it has not received.

**D9 — Filter harness task evidence through the registry's service
classification.** SDK tasks already mirror into the daemon registry
(`observeClaudeSpawnedWork`, `spawned_work.go:532`) where the classifier lives.
A task whose mirror is `service` is not park evidence even on its first report;
§V3.4's repeat rule alone leaves a dev-server chat parked forever with no stall
path. Compare per task id on `type`+`status`; disappeared-then-reappeared is
fresh. Pin the intended under-park (declared `done` + unchanged still-running
task ⇒ `done` stands) with a test, healed by C5's rescind.

**D10 — undefined-vs-`[]` applies to `session_crons` too**, and must survive
the wire: the field is omitted when the hook lacked it, never sent as empty.

**D11 — Spec text corrections.** `TerminalReason` has **19** values, not 18
(the spec omits `background_requested`, which is park-supporting vocabulary and
must not fall into the `done` row). `NotificationHookInput.notification_type`
is typed plain `string` — the four values are CLI-binary strings, not an
`sdk.d.ts` enum; the receipt is corrected accordingly. The entry point is
`enqueuePrompt`, not `prompt()`. The 30s `UserPromptSubmit` timeout is
unverified in typings and binary — treated as an assumption; the operative
requirement (observer-only, non-blocking, non-throwing) stands without it.
`prompt_id` is *absent until the first user input of the process*, so `""` must
be tolerated. §V3.7's "the mock emits none today" is stale — build step 1 was
already written before this review landed and is gate-reviewed, not rebuilt.
The strongest static receipt for `session_crons` is its own typedoc, which
names the mechanisms outright: *"Session-scoped cron tasks (CronCreate,
ScheduleWakeup, /loop) that will wake this session later."*

**Cut from scope (Fable):** `phase:"attention"` as a clamp input — it fires
mid-turn and is usually resolved mid-turn, and v2's `chatHasPendingPermission`
already answers the end-of-turn question from live state; it ships as a
receipt/note only. The `terminalReason → needs_input` rows as a parallel
failure classifier — those arrive as `is_error` results and v2 already yields
`needs_input`; `terminalReason` stays note enrichment, not a second failure
path that can disagree with the first. Full `origin.kind` plumbing — only
human-vs-not matters here.

**On the user's two claims (Fable's finding):** both correct. The one
overstatement is scope — the grace was also covering daemon-external waits
(external lanes, Windows probe-less parks, peer wakes) that no harness signal
reaches. Keeping the 90m backstop for those is correct, not a concession.
