# Fix 4 — daemon-created chats must survive renderer snapshot saves

Status: SPEC APPROVED FOR BUILD (dev-daemon lane; prod untouched until gates
pass). Owner: Fable (spec + gate review). Builder: Sol/codex lane. Queued
behind the active tree-editing lane.
Incident receipts: two `workass_create_chat` calls on 2026-07-17 (focus=false
and focus=true; tabIds srv-tab-18c33744c0356d18-772 and
srv-tab-18c3375375ec40d8-77a) returned ok, then vanished: absent from
`workass_list_chats`, every send failed "chat identity not found". Prod state
inspection confirms both chats were written to session-state.json and later
erased; only transcript-text remnants remain (nothing to clean up).

## Binding laws for this lane (restated verbatim — the builder does not inherit them)
- Never rename referenced files/ids/paths/JSON keys; new fields are
  additive-only (wire contract `invoke/reply/event` is frozen; the renderer
  must keep working unchanged).
- Do NOT commit. Do NOT touch files outside scope (the tree has extensive
  unrelated uncommitted work — leave it exactly as found).
- Every claim in your final report needs a receipt (command + outcome).

## Root cause (verified line-by-line by the spec owner)
`AgentCreateChat` (cmd/workass/session_store.go:611-642) appends the new chat
to `s.snapshot["chats"]`, persists, and returns ok — the chat is real at that
instant. But the renderer autosaves its FULL session mirror on a 600ms
debounce (`desktop/renderer2/src/store/store.ts:351-362` → wire
`session:save` → cmd/workass/main.go:501-511 → `sessionStore.Save`).
`Save` replaces the snapshot with the renderer's copy
(`s.snapshot = s.mergeAuthoritativeTurnsLocked(incoming)`,
session_store.go:274), and `mergeAuthoritativeTurnsLocked`
(session_store.go:2828-2861) starts from `out := incoming` and re-appends
ONLY chats that have job traces (`s.pending` + `s.jobs`). A just-created,
never-prompted chat has no job → the renderer's stale snapshot erases it.
The rescue path (refresh broadcast → renderer `getSession` → adoption) loses
the race to the debounced save, and loses outright when the renderer
subscription is detached (separate bug 3).

## Required behavior changes (scope: cmd/workass/session_store.go, chat_control.go, + tests)

### C1 — persistent protection marker on daemon-authored chats
In `AgentCreateChat`, add an additive field to the chat record it creates:
`"serverAuthored": true` (persisted inside the chat map in the snapshot, so
protection survives a daemon restart before renderer adoption). Do not add
it anywhere else.

### C2 — merge preserves protected chats
In `mergeAuthoritativeTurnsLocked`, after the existing trace-preservation
loop: for every chat in the CURRENT `s.snapshot["chats"]` carrying
`serverAuthored == true` that is missing from `out["chats"]`, re-append the
server copy (`cloneJSON`). A stale renderer save can then never drop a
daemon-created chat.

### C3 — renderer adoption clears the marker (so deletes still work)
When `incoming["chats"]` DOES contain that tabID, the renderer has adopted
the chat: strip the `serverAuthored` field from the merged record (one-way,
once). After adoption, a renderer snapshot omitting the tab is an
intentional user delete and is honored exactly as today. Daemon-side
deletion (`AgentDeleteChat` / `chat.delete`) must remove the chat regardless
of the marker (verify; expected to already hold since it edits the
snapshot directly).

### C4 — create re-asserts listability before replying ok
In the create handler (cmd/workass/chat_control.go:102-133), after
`AgentConfigureChat`, re-read the chat (existing exact-read path, e.g.
`AgentReadChat`) and return an explicit error instead of ok if it is not
addressable. Backstop only; C1-C3 are the real fix.

## Regression tests
`cmd/workass/session_store_test.go`:
- T1 (locks the bug): `AgentCreateChat` → `Save(incoming)` where incoming
  omits the new tab (stale renderer snapshot) → `AgentChatList` still
  contains it AND the exact-identity read used by sends succeeds (must NOT
  yield "chat identity not found"). This test fails on today's code —
  demonstrate that first, then show it passing.
- T2 (adoption + delete): create → `Save` WITH the tab (adoption; marker
  cleared in stored snapshot) → `Save` WITHOUT the tab → chat is really
  gone (no resurrection).
- T3 (restart survival): create → simulate restart (new store loading the
  written file) → stale `Save` without the tab → chat preserved.
`cmd/workass/agent_control_test.go`:
- T4 (integration): `chat.create` → `chat.list` contains it → interleave a
  stale `session:save` → `chat.send` (or queue path) succeeds.

## Verification gates (dev daemon ONLY — never prod on port 8788)
1. `go build ./... && go vet ./cmd/workass/ && go test ./cmd/workass/...`
2. Dev-daemon manual gate: against the dev daemon (port 18788), MCP-create a
   chat while a renderer client is connected, confirm it remains listable
   and addressable past the autosave window; capture log lines as receipts.
