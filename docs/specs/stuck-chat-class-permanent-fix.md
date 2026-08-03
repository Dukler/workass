# Stuck-chat class — permanent fix (spec, 2026-07-24)

Owner: Fable lane. Verified by 7-agent adversarial workflow (all CONFIRMED) +
live forensics of the 2026-07-24 incident. Evidence cited as file:line against
commit 363f855.

## The class, in one sentence

Renderer and daemon each hold chat state; every sync is an edge-triggered push
with no timeout, no rejection on loss, no periodic pull, and no revision
gating — so one dropped frame turns the renderer into a permanent zombie that
then writes its stale state back over daemon truth.

## Verified defects

- **D1 wedge**: wire shim never settles pending invokes on socket close
  (`internal/httpserve/lan_bridge.go:36,62-68`); `store.init()` awaits
  `getSession` (store.ts:638) → one dropped reply (05:32:54) wedged init
  forever, ConnectionMonitor never constructed (store.ts:726), and every
  `agent:apply session-refresh` / `socket-open` reconcile chained behind the
  dead promise (store.ts:600-614).
- **D2 invisible turns**: `onJobEvent 'start'` with no local anchor silently
  drops the event (store.ts:1848-1866); all subsequent data/acp/media/end
  events resolve null. `Job.Public()` (internal/acp/types.go:486-515) omits
  userMessageId/assistantMessageId although AgentPrepareQueuedTurn mints them
  (session_store.go:1517-1523).
- **D3 cancel no-op**: `cancelChatTurn` ignores the `job:cancel` reply
  (store.ts:1823-1829); `manager.CancelJob` returns bare false for dead jobs
  (manager.go:1458-1464) → permanent phantom running row vetoes
  `_send`/`flushNextQueued`/`recoverIdleQueues` (store.ts:144-147,1710,846).
- **D4 parked drainer**: `scheduleDrain` spins at 10Hz forever on a
  non-daemon-owned queue head (chat_control.go:390-421;
  session_store.go:1434-1451,703-710); renderer rows carry no `source`/
  `queuedAt` (image-drafts.ts:141-144); `mergeQueueWithAuthoritativeDaemonRows`
  resurrects renderer rows from stale saves (session_store.go:757-767). Also:
  drain abandons the FIFO silently on non-busy errors (chat_control.go:415).
- **D5 queue-row loss**: `flushNextQueued` removes the head before `_send` is
  accepted; a failed send never restores it (Sol lane diagnosis, matches
  movement-fix's lost 90-char question).
- **D6 journal resurrection + boot-recovery kill**: post-end job events
  re-create deleted journals with no `prepare` (RecordJobEvent acp/data/media
  have no `job.Finished` guard, session_store.go:2557-2621; lazy-create at
  2817-2834); ONE bad journal aborts the whole replay AND the post-load
  pipeline incl. `interruptOrphanedTurnsLocked` (session_store.go:172-209,
  3086) — prod boot recovery has been dead since Jul 18. Three poisoned files
  exist in prod `.session-stream/` (08e247*, 8ce1c8*, ffb6f3*).
- **D7 snapshot obesity**: 92% of the 75.8MB snapshot is base64 screenshots
  (69.8MB / 134 blobs; 13.1MB exact duplicates event.images vs
  message.images). 256MiB frame limit is a ceiling, not a bound.
- **D8 push-only divergence surface**: full sweep confirmed no periodic pull
  and no revision gate on `session:save` (stale renderer overwrites daemon
  queue/controls — the parked-FIFO and picker-rollback amplifier).
- **D9 picker rollback**: mirror restore wholesale-replaces chats mid-click
  (store.ts:795-809 vs setModel at 1421-1447), `recoverRememberedCatalogModel`
  rewrites restored 'default'→remembered opus (model-selection.ts:84-96),
  stale `appChatSetModel` echo re-applies old model. (Final fencing detail
  incorporated from the codex fixin-lane report.)
- **D10 latches**: `planUsageLoadingByProvider` never clears on a lost reply
  (store.ts:1397-1419); update-chain waiter pends forever on one missed
  terminal event (store.ts:2272-2308).

## Work packages

### Lane 1 — daemon (Go)

**WP-F journal integrity** (session_store.go)
1. Loader: validate-then-apply per journal; on any validation failure move the
   file to `.session-stream/quarantine/` (keep bytes; write `<name>.reason`)
   and CONTINUE; the post-load pipeline (materialize, interrupt orphans,
   finalize, snapshot rewrite, cleanup, jobs reset :180-209) ALWAYS runs.
   Keep the fatal guard for an unreadable canonical snapshot (:157-162).
   `loadErr` must distinguish "n journals quarantined" from fatal.
2. Writer: RecordJobEvent `data`/`assistant-media`/`acp` drop records when
   `job.Finished`; `appendJournalRecordLocked` refuses to lazy-create a file
   for a non-`prepare` first record (only `beginJournalLocked` creates); a
   failed first append unregisters + unlinks.
3. Tests: acp-first journal → quarantined, others replayed, orphan
   interruption runs; late event after end → no file re-created; prepare
   append failure → no residue; good-sorts-before-bad → good fully recovered.

**WP-E drainer + adoption** (chat_control.go, session_store.go, manager.go)
1. Drainer exits instead of spinning when head is non-daemon-owned or chat
   running; wake sites: (a) new job-end callback from manager, (b)
   `reconcileAgentQueueRevision` on queue change, (c) capped backoff recheck
   1s→30s while a renderer-owned head exists. Keep the
   draining/drainPending single-goroutine invariant. Non-busy start errors:
   bounded retries (3, backoff), then park the row + enqueue a server notice,
   never silently abandon.
2. Adoption: daemon stamps `observedAt` on source-less rows at merge; when
   chat idle and head is renderer-owned and older than 60s, adopt IN PLACE
   (same id, source:"agent", adoptedFrom:"renderer"), bump agentQueueRevision,
   record id in a capped (256) adopted-ledger; `mergeQueueWithAuthoritative-
   DaemonRows` must drop incoming renderer rows whose id is in the ledger
   (kills resurrection/double-send). Skip rows with pending attachmentState.
3. Thread the originating queue-row id through job start opts so
   `QueueRendererStartCollision` and `PrepareTurn` drop ids already in the
   adopted ledger.

**WP-B(d) job payload**: `Job.Public()` additively gains `userMessageId`,
`assistantMessageId`, and `promptText` (the REDACTED canonical user-row text,
never raw opts.Prompt).

**WP-D(d) structured cancel**: `job:cancel` reply becomes
`{cancelled,reason:'cancelled'|'idle'|'unknown'}` (wire-additive; old clients
treated a bare bool — keep shape tolerant).

**WP-C(d) state digest**: new invoke channel `state:digest` (additive,
feature-detected): per chat {tabId, chatId, runningJobId|null,
lastMessageId, messageCount, queueLen, queueHeadId, agentQueueRevision,
runtimeControlRevision, providerId, currentModelId, currentModeId,
pendingPermissionIds}; global {catalogHash per provider, settingsRevision,
procHash}. Reply hard-capped small (no bodies, no images); test encoded size
< 64KiB with 200 chats. Also: `session:save` gains revision gating — reject or
field-merge saves whose agentQueueRevision/runtimeControlRevision are older
than daemon's (never adopt stale queue/controls wholesale).

**WP-G snapshot leaning**
1. Lever 2 first: serialize event.images as refs when identical bytes exist on
   the owning message (dedupe ~13MB, no protocol change).
2. Lever 1: externalize image `.data` to content-addressed files
   `state/images/<sha256>` with descriptors+ref in the snapshot; REHYDRATE
   server-side in `session:get` so the renderer contract is unchanged. Bounds
   disk; wire lean-get is a follow-up and NOT in this lane.
3. Respect `_archivedCount`: never remove message rows.

### Lane 2 — renderer (TS) + lan_bridge

**WP-A wire lifecycle**
1. `lan_bridge.go` shim JS: reject all `pending` invokes on `ws.onclose`
   (typed error), clear map; per-invoke timeout (default 30s, 120s for
   `session:get`/`session:save`); socket-generation tag discards stale replies.
2. `store.ts init()`: construct ConnectionMonitor FIRST (right after event
   handler wiring); every hydration await individually try/caught so a failed
   step degrades instead of wedging; failed `getSession` retries with backoff
   via the monitor path; `hydrated` reached even on partial boot.
3. `agentRefresh` chain: wrap each link with timeout+catch — a failed
   reconcile logs and never parks the chain. `connection.ts` loop: race
   `onReconnect()` against a deadline.

**WP-B(r) unanchored turns**
1. `onJobEvent 'start'` unanchored branch: synthesize canonical user row
   (payload promptText, dedupe by userMessageId) + assistant running row with
   assistantMessageId; register chatJobs/jobRef; bumpApp. Old-daemon payloads
   without ids: schedule a reconcile instead. `fromMirror` dedupes by id so
   later hydration cannot double rows.
2. `'end'` unanchored: paint terminal row from e.job (status/result/error/
   images).
3. `'data'`/`'acp'` must NEVER flip a terminal-status row back to running.
4. Unanchored permission requests: attach by tabId to the synthesized/live
   assistant row; keep `chatPendingPermissions` pull as backfill.

**WP-D(r) cancel finalize**: await `cancelJob`; on `cancelled:false`/bare
false → finalize row locally as cancelled, clear jobRef/chatJobs, run
recoverIdleQueues + kick reconcile. Never finalize locally when reply is true.

**WP-E(r) transactional flush**: `flushNextQueued` (store.ts:1710) removes/
releases the head row only after `_send === true`; failure or exception
preserves the exact FIFO head (no loss, D5) — the helper at
image-drafts.ts:175 already expresses this shape; thread the queue-row id
into job start opts (daemon dedupe).

**WP-C(r) digest heartbeat**: ConnectionMonitor ping = `state:digest` when
available (fallback `appMeta`); compare per-chat running/queue/controls/
permissions + global hashes; on divergence run SCOPED repair through the
guarded chain (getSession restore / chatPendingPermissions / catalog refresh).
Detection cadence 5s, repair debounced.

**WP-I picker fencing** (closed spec from the codex fixin-lane report):
every model-write await at store.ts:1383, 1440, 1477, 2043 gains a fence
capturing {chat identity, sessionId, providerId, requested modelId,
_controlRevision} — a delayed reply is ignored if ANY differ;
`acceptAppliedModel` (store.ts:245) must pass the same fence; mirror
restores (store.ts:790 fromMirror path) preserve locally-newer
provider/model/mode fields, letting the daemon win only when its
runtimeControlRevision is strictly higher; `reconcileCatalogControls`
recovery never rewrites a selection explicitly picked this session while
the catalog is stale/absent. (Historical note: the original Fable→Opus
rollback was a composite-id parse bug — `claude-fable-5[1m][xhigh]` sent
whole to the SDK — already fixed at manager.go:2325 and live since the
05:37 promotion; WP-I closes the remaining renderer-side class.)

**WP-H latches**: clear `planUsageLoadingByProvider` in `finally`; update
waiters get a deadline + chainActive reset.

## Binding laws (restate to every executor)

- Wire contract frozen: `invoke/reply/event` in desktop/lan-server.js
  unchanged; ALL new channels/fields additive + feature-detected (`has()`);
  renderer must work against old daemon and vice versa.
- stdout purity for ACP servers; secrets redacted before display/send.
- Never rename referenced files/ids/paths; add aliases.
- One tree-editing lane; production processes untouched (daemon PID 91261,
  Workass.app) — dev profile only for live checks.
- Receipts: commands + one-line outcomes; raw output only on failure.
- Tests are part of each WP, not optional; mock oracle
  (desktop/acp/mock-server.mjs) is the ACP test bench.

## Gates

Per lane: `go test ./cmd/workass ./internal/... -count=1`, focused `-race`,
`go vet`; renderer: full vitest suite + typecheck + build. Integration: mock
oracle drain-while-disconnected scenario (queue a daemon-owned row with no
client, attach client mid-turn, turn must be visible + permission prompts
must surface). Then dev daemon rebuild via
`scripts/rebuild-workass-macos.sh daemon --profile dev`, live dev
verification, adversarial review workflow on the full diff, commit, and prod
promotion staged at an idle boundary as a registered external lane.
