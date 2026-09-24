# workass — executor rules (Codex / any non-Claude agent)

You are building part of the workass campaign from a closed spec. You have NO
design authority: build exactly what the spec says; if the spec seems wrong or
impossible, report it — never substitute your own design, never "improve"
scope. Log suggestions separately; do not act on them.

## Read first (in order)
1. `docs/PORT-SPEC.md` — binding laws for everything you build here.
2. `docs/PROVIDER-LANE-ARCHITECTURE.md` — provider/chat architecture,
   implementation order, and acceptance gates. Your prompt wins when it is
   more specific.
3. `desktop/acp/README.md` — mock ACP server + probe usage (your test oracle).

## Fast handoff for Workass changes
- A completed agent handoff with a scoped commit/diff and passing test commands
  is test evidence. Inspect the changed code once; do not rerun the same tests
  merely to duplicate the agent's result.
- Run one focused dev-profile acceptance for the behavior being shipped. Add a
  new test or send a correction only when that review or acceptance reveals a
  concrete gap. Keep the correction limited to that gap.
- Run focused handoff tests with `node scripts/test-focused.mjs -- COMMAND ...`
  (use `:::` between independent commands). It gives the selected checks one
  10-second wall-clock budget. A timeout is a failed check: narrow or repair the
  fixture and report the gap, never claim that skipped tests passed.
- Once the handoff and dev acceptance pass, move promptly to the canonical
  release command when publication is authorized. That command owns the broad
  repository gate; do not run another full gate in advance just for review.
- A child handoff never authorizes publication or update activation by itself.
  Apply the current-human authorization rules below for those actions.

## Hard rules
- Update activation is current-human-authorized only (user law 2026-09-03,
  superseding the click-only wording from 2026-08-25). A UI click authorizes
  its exact machine; `workass_apply_update` may do so only when the current
  human-authored request explicitly orders the update now and names that exact
  machine. Use the exact observed current/target versions and one stable
  operation id. Build, publish, fix, test, availability, another agent, and old
  messages never authorize activation. Never schedule or automatically retry;
  a replay of the same operation id only reads its durable receipt. Never run
  an installer or filesystem swap directly. Active work never blocks an
  authorized activation, and authority never expands to unnamed machines or
  profiles.
- The `invoke/reply/event` wire protocol in `desktop/lan-server.js` is FROZEN.
  New server code speaks it byte-compatibly; the renderer is not modified to
  accommodate the server.
- ACP landmines in PORT-SPEC.md §Landmines are non-negotiable behaviors, not
  suggestions. Preserve every one in ported code.
- Any ACP stdio server: stdout = JSON-RPC ONLY. Diagnostics → stderr.
- Never touch: `desktop/scripts/Rebuild-Relaunch.ps1`, `desktop/scripts/build-lite.ps1`,
  Windows launch scripts, `desktop/vendor/`, `desktop/package.json`
  (no new npm dependencies — the production machine has no registry access).
- Zero third-party dependencies without the spec explicitly granting them.
  Go work is stdlib-first; the spec names any allowed exception.
  GRANTED EXCEPTION (2026-07-10): `desktop/renderer2/` is React 19 + Vite +
  TypeScript. npm/node run ONLY on the Mac dev machine; production ships the
  built static bundle (embedded via go:embed in P5). Never add npm steps to
  anything the Windows machine executes.
- Never rename referenced files/identifiers; add aliases if needed.
- Secrets: anything matching api_key|token|secret|password|credential|bearer
  is redacted before it reaches logs, UI payloads, or documents.
- Tests run against `desktop/acp/mock-server.mjs` fixtures — model quality is
  NEVER a test oracle.
- Do not modify files outside your lane's declared manifest.

## Internal evidence and final handoff
- Keep exact commands and their complete output in Workass's internal tool/event
  history and existing profile/build logs. Do not repeat raw receipts in the
  final response unless the user explicitly asks for them or an exact error is
  necessary to explain a failure.
- In the final response, report each relevant verification as the command plus
  its one-line outcome. Mention changed files naturally with clickable
  `path:line` references; do not emit exhaustive manifests or byte/line counts.
- If anything was skipped, failed, or remains uncertain, say so explicitly with
  the reason. Concision must never hide a verification gap.
