# Fix 2 — model identity must survive hibernate→resume and agent-side /model

Status: SPEC APPROVED FOR BUILD (dev-daemon lane; prod untouched until gates pass)
Owner: Fable (spec + gate review). Builder: Sol/codex lane. Build AFTER fix 1
lands (one tree-editing lane at a time).
Incident receipts: chat tab-1784314720441-27 on 2026-07-17 — user set
`claude-fable-5[1m][high]` via /model; across hibernate→resume cycles the
per-turn banner showed `claude-fable-5[1m][high]` → `claude-fable-5` →
`claude-fable-5[high]`; the degraded id was persisted as the chat's stored
selection; `workass_create_chat` inheriting it failed with
'model "claude-fable-5[high]" is not available for provider "claude"'.
Composite ids appear as KEYS in the chat's stored per-model controls map.

## Binding laws for this lane (restated verbatim — the builder does not inherit them)
- Never rename referenced files/ids/paths/JSON keys; add aliases only.
- Do NOT commit. Do NOT touch files outside scope (tree has extensive
  unrelated uncommitted work — leave it exactly as found).
- Wire contract (`invoke/reply/event`, desktop/lan-server.js) is frozen; new
  event payload FIELDS are additive-only; the renderer must keep working
  unchanged.
- The frozen renderer contract represents model+effort as `base[effort]`
  composite ids (manager.go:2291-2293 comment) — that representation stays.
- Every claim in your final report needs a receipt (command + outcome).

## Root cause (confirmed by code read)
Three interacting defects:

D1 — one-way reconciliation. Every turn, `ensureSessionControls`
(manager.go:802, impl 2190) forces the renderer-stored selection onto the
bridge ("the renderer's selection wins every turn", manager.go:798-801).
A `/model` issued INSIDE the agent session changes the adapter's state but
is never captured into Workass chat controls, so the next turn actively
reverts or mangles it.

D2 — cold-resume suffix stripping. `SetModel` (manager.go:2098) resolves a
composite via `resolveModelWriteLocked` (manager.go:2222). On a freshly
resumed bridge `b.axisEffortsByModel[base]` is typically undiscovered, so:
(a) the effort write block (manager.go:2126-2146) silently no-ops when
`supportedEffort == ""` — the `[high]` is dropped without error; and
(b) if the literal bracketed base (e.g. `claude-fable-5[1m]`) is not yet in
`b.models`, `resolveModelWriteLocked` returns the raw composite
(baseModel==nil path, manager.go:2245-2247) which the adapter coerces,
dropping suffixes unpredictably. `currentModelSelectionLocked`
(manager.go:2294) then re-composes from stale `b.efforts`, producing ids
like `claude-fable-5[high]` (effort re-attached, `[1m]` lost).

D3 — degraded persistence. The applied id is written back
(manager.go:817-819: `opts.ModelID = appliedModelID`) and flows into the
chat's persisted controls as if user-chosen, and composite ids leak in as
keys of the per-model controls map. Locate the exact persistence path in
`cmd/workass/` (session_store.go / config_store.go / chat_control.go) and
cite it in your report — evidence says stored `currentModelId` became
`claude-fable-5[high]` and the per-model map gained composite keys.

## Required invariants (the closed spec — implement to these, cite code for each)

I1 — Never silently downgrade a user selection. The persisted chat selection
is USER INTENT. If applying `base[suffixes…]` yields an applied id that lost
suffixes purely because effort/model discovery was incomplete (axis for that
base NOT yet known — `_, known := b.axisEffortsByModel[base]`), the stored
selection MUST remain the user's original composite; the applied (degraded)
id may drive the current turn's runtime banner but is NOT persisted. The
next `ensureSessionControls` retries the full composite (idempotent, and
discovery has usually completed by then).

I2 — Authoritative downgrade is allowed but explicit. If the adapter
authoritatively reports the suffix unsupported (axis for that base IS known
and lacks the level, and it's not a direct model-id variant either), the
downgraded id MAY be persisted — with a log line stating original,
applied, and reason. Never persist a degraded id without that receipt.

I3 — Two-way reconciliation. When the bridge observes an adapter-side
current-model change that Workass did not write (agent-side /model:
detected in `applyConfigOptions` / config echo where the new current model
differs from the last value Workass wrote for that session and no Workass
write is in flight), capture it: update the chat's stored selection
(normalized to the composite `base[effort]` form) and emit the existing
controls-changed event so the renderer header updates. "Renderer wins"
becomes "last writer wins, both writers observed".

I4 — Composite ids never become model-map keys. Whatever per-model keyed
maps exist in chat controls persistence use BASE model ids as keys.
On load, migrate polluted stores: a composite key merges into its base key
(base wins on conflict) and is removed; log each migration once.
`workass_create_chat` (and any model validation) must accept a composite id
by resolving base+effort against the catalog exactly like a turn does —
inheriting `claude-fable-5[1m][high]` or `claude-fable-5[high]` must work.

I5 — Literal bracketed adapter ids keep working. Claude has literal model
ids like `opus[1m]` / `claude-fable-5[1m]` (manager.go:2106-2108). The
resolution order in `resolveModelWriteLocked` (exact adapter id first) is
correct and stays; `base[1m][high]` must round-trip to base `…[1m]` +
effort `high` once the axis is known.

## Regression tests (Go, `internal/acp` + the store package you touch)
- T1: fresh bridge, axis undiscovered; ensure with `base[high]` → applied
  comes back stripped → assert persisted selection still `base[high]`;
  after axis discovery a subsequent ensure applies effort and applied ==
  selection.
- T2: axis known and lacks `high` → downgrade persisted + logged (assert
  log via test logger).
- T3: adapter-side model change between turns (simulate config echo) →
  next turn does NOT revert; stored selection updates; event emitted.
- T4: store with polluted composite keys loads → keys migrated to base;
  create-chat-style validation accepts composite ids.
- T5: `claude-fable-5[1m][high]` round-trips byte-for-byte across a
  simulated hibernate→resume once discovery completes; banner id equals
  the user selection.
Use the existing bridge test harness (bridge_test.go / manager tests) and
the mock adapter patterns already in the package; the desktop mock server
(desktop/acp/README.md) is the oracle for any end-to-end check.

## Verification gates (dev daemon ONLY — never prod on port 8788)
1. `go build ./... && go vet ./internal/acp/ && go test ./internal/acp/... ./cmd/workass/...`
2. Dev-daemon manual gate: set a composite model, force hibernate
   (short `-hibernate-ttl`), resume with a new turn → banner and stored
   selection both show the original composite. Capture logs as receipts.
