# Fix 3 — renderer projection must detect socket cycles and re-sync

Status: SPEC APPROVED FOR BUILD (dev-daemon lane; prod untouched until gates
pass). Owner: Fable (spec + gate review). Builder: Sol/codex lane. Build
AFTER fix 4 lands (both touch cmd/workass/session_store.go; one tree-editing
lane at a time).
Incident receipts (2026-07-17, all against the SAME healthy prod daemon
28958/port 8788): (a) srv-injected message processed+persisted daemon-side
(chat-archive tab-1783868698407-12.jsonl srv-u-…-707 / srv-a-…-708, both
done; session-state lastAt current) while the open pane stayed on a 24h-old
turn until a focus_chat delivered a fresh agent:apply; (b) sidebar dropped
exactly the claude∩~/Workspace/workass cohort while workass_list_chats kept
returning all chats; full app restart rebuilt it; (c) reopening chats kept
loading stale history afterwards.

## Binding laws for this lane (restated verbatim — the builder does not inherit them)
- The `invoke/reply/event` wire protocol is FROZEN. Everything below is
  additive: new DOM event + window global in the injected shim, one extra
  replayed event to fresh clients, tombstone files, log lines. No channel
  renames, no payload field removals. An OLD renderer against the new daemon
  and the NEW renderer against an old daemon must both keep working
  (feature-detect everything).
- Never rename referenced files/ids/paths/JSON keys.
- Do NOT commit. Do NOT touch files outside scope (tree has extensive
  unrelated uncommitted work — leave it exactly as found).
- Every claim in your final report needs a receipt (command + outcome).

## Root cause (verified line-by-line by the spec owner)
The packaged renderer talks to the daemon through the injected shim
`internal/httpserve/lan_bridge.go` (`window.api`; served by
internal/httpserve/server.go:107). Three verified facts compound:
1. The shim auto-reconnects silently: `ws.onclose` → `setTimeout(connect,
   1500)` (lan_bridge.go:36). Broadcasts emitted during the gap are simply
   never delivered (internal/wire/wire.go broadcastWhere skips non-ready
   clients) and are never replayed — the only client-ready replays today are
   provider events + pending permissions (cmd/workass/main.go:577-582).
2. The renderer's liveness monitor cannot see a short reconnect: its ping is
   a queued invoke that flushes and RESOLVES after the socket returns
   (lan_bridge.go:66 queue + desktop/renderer2/src/wire/connection.ts:79-99),
   so status stays 'connected' and `onReconnected()` — the ONLY full
   re-sync (store.ts:637: getSession → rebuild chats/activeId/jobRefs) —
   never fires.
3. Daemon/MCP-originated mutations depend entirely on ONE fire-and-forget
   `agent:apply{action:"session-refresh"}` frame (cmd/workass/
   chat_control.go:300-302 → store.ts:464-470). If that frame lands in a
   reconnect gap, the mutation is invisible until some later delivered
   frame — potentially days.
Amplifier: `sessionStore.Save` adopts the renderer's chat set wholesale
(session_store.go:274, mergeAuthoritativeTurnsLocked 2828-2861 preserves
only chats with live job traces), so a stale renderer save silently prunes
daemon chats that have no running job. (Fix 4 protects daemon-AUTHORED
chats; this lane adds observability + recoverability for the rest.)

## Required changes

### S1 — socket-generation signal in the injected shim (client-side only)
In `internal/httpserve/lan_bridge.go` LANBridgeJS, inside `ws.onopen`:
increment a module counter, set `window.__workassSocketGen = gen`, and
`window.dispatchEvent(new CustomEvent('workass:socket-open', {detail:{gen}}))`.
Mirror the identical additive change in `desktop/lan-server.js` LAN_BRIDGE_JS
(the two shims are kept in lockstep — see lan_bridge.go:3). No other shim
behavior changes; pending/queue semantics untouched.

### S2 — renderer resyncs on any reconnect
In `desktop/renderer2/src/store/store.ts` init(): add a
`window.addEventListener('workass:socket-open', …)` that, for any gen
AFTER the first-connect gen observed at boot, chains a full re-sync through
the existing serializer exactly like onAgentApply does
(`this.agentRefresh = this.agentRefresh.then(() => this.onReconnected())`).
Feature-detected: with an old shim the event never fires and behavior is
unchanged. Keep the ConnectionMonitor as-is (it still covers long outages
and daemon replacement).

### S3 — every fresh/reconnected client gets one session-refresh from the daemon
In `cmd/workass/main.go` OnClientReady hook (main.go:577-579), after
`ReplayProviderEvents`, also `send("agent:apply", map[string]any{"action":
"session-refresh"})` to THAT client. The current renderer already handles
this event (store.ts:464-470, serialized+idempotent); old renderers ignore
unknown actions. This guarantees MCP-originated state is reconciled on every
socket (re)establishment even if S1/S2 are absent (old embedded renderer).

### S4 — Save may never destroy chats silently
In `sessionStore.Save`/`mergeAuthoritativeTurnsLocked`
(cmd/workass/session_store.go): when the merged result drops chats that
exist in the current daemon snapshot (absent from incoming, no live trace,
not protected by fix 4's serverAuthored marker), (a) write each dropped
chat's full JSON to `state/dropped-chats/<tabId>.json` (overwrite ok) before
it leaves the snapshot, and (b) log one line
`"session save dropped chats"` with the tabIds and count. Semantics are
UNCHANGED (user deletes still work); destruction becomes recoverable and
diagnosable. Coordinate with fix 4's edits in the same function (fix 4
lands first).

## Regression tests
- Go (`cmd/workass`): T1 — a hub client that completes the ready handshake
  receives `agent:apply{session-refresh}` (extend the existing client-ready
  replay test pattern). T2 — Save with an incoming snapshot missing an
  unprotected, traceless chat: chat is dropped from the snapshot AND its
  JSON lands in state/dropped-chats/ AND the log line fires; a chat with a
  live job trace is still preserved as today.
- Renderer: if desktop/renderer2 has a unit-test harness (check
  package.json), add a test that dispatching `workass:socket-open` with a
  bumped gen triggers exactly one serialized onReconnected; otherwise cover
  via the manual gate below and say so in the report.
- Manual dev-daemon gate (port 18788 profile `.dev/profiles/default`; use
  the screenshot + DOM-probe dev endpoints on view port 8799 — never
  iterate blind): (G1) MCP-rename a chat while the renderer is connected →
  sidebar updates without focus/restart. (G2) Interrupt the renderer's
  socket briefly (<5s, e.g. kill the WS by restarting the daemon's listener
  or toggling the connection) while an MCP send lands in the gap → pane
  catches up within seconds via S1/S2/S3, no focus_chat needed. (G3) After
  reconnect, reopening the chat shows the canonical archive tail (catches
  any residual hydration defect — if this still fails after S1-S3, STOP and
  report the observed state; do not improvise a hydration fix).

## Verification gates (dev daemon ONLY — never prod on port 8788)
1. `go build ./... && go vet ./cmd/workass/ ./internal/httpserve/ && go test ./cmd/workass/... ./internal/wire/...`
2. Renderer typecheck/build per the workass-build skill
   (.agents/skills/workass-build/SKILL.md) — do NOT hand-edit anything in
   cmd/workass/embedded/dist.
3. Manual gates G1-G3 with receipts (probe output + log lines).
