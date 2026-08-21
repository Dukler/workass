# AGENTIC-PARITY — the "finish all the agent stuff" checklist

User directive (2026-07-10): implement every modern agentic-work feature the
Claude Code / Codex generation has that workass needs; battle-test; STOP
before Assist. Grounded in the July-2026 changelogs (Claude Code:
auto-compact, checkpoints/rewind via double-Esc, queued messages, plan mode;
Codex app: queue-or-steer follow-ups, diff review, fork-through-turn,
token budgets, remote pairing). ACP note: model/agent features live
agent-side; workass owns the CLIENT/DAEMON side listed here.

## Daemon features (Sol lanes, serialized)
D1. **Provider-owned compaction** — usage% is tracked per lane (normalize usage
    meta keys incl. workass.mock/*). Native compaction advances the same
    provider thread and reports a verified checkpoint through its adapter.
    Providers without native compaction receive a visible context-limit event;
    Workass never asks a model to summarize itself, replaces the thread, resets
    usage, or replays transcript text.
D2. **Steer / queue** — `app-chat:steer` has provider-aware live semantics:
    Codex routes acknowledged input into the active app-server turn; Claude
    injects acknowledged input into the running SDK query; generic ACP agents
    use `_session/steer` only when their capabilities advertise it. Unsupported
    or definitely rejected steering returns the same input to the composer and
    never interrupts or creates FIFO work. Ordinary queue submission is a
    separate durable intent.
D3. **Turn checkpoints + rewind (git-based)** — at each turn end with
    changes (Entorno tracking knows), record `git stash create`-style
    snapshot refs per touched repo; invokes chat:checkpoints (list per chat)
    and chat:rewind {turnId} restoring the pre-turn state (guard: refuses if
    repo dirtied outside the chat since). Makes work-card "Deshacer" real.
D4. **Crash recovery** — engine exit → bounded host restart and exact native
    thread resume. Ambiguous in-flight acceptance blocks new admission until
    reconciliation; no replay or replacement thread.
D5. **Fork chat** — app-chat:fork {tabId, atTurn?} creates an explicit new lane
    epoch only through a provider-native fork or verified non-sampling context
    import; transcript prompt seeding is forbidden.
D6. **Daemon polish (gap punch list)** — proc:changed on bridge close;
    config:set real persistence (app-config.json semantics + hot reload);
    lifecycle values read channel (config:get gains engine section);
    detect-acp exposed in bridge JS; lan:devices lastSeen refresh.
D7. **Notify routing** — notify invoke → controller-only event (Agent API
    seed); renderer surfaces via Web Notifications when permitted.
D8. **Diff read channel** — chat:diff {repo, path} → unified diff text of
    that file vs the chat's turn baseline (feeds Revisar viewer). Read-only.

## Renderer2 features (Opus lanes, serialized)
R1. **Composer agent/model picker** — grouped by provider from catalog
    groups (daemon ready); per-chat binding at creation; effort selector.
R2. **Steer/queue UX** — while a turn runs, ordinary Enter creates one durable
    FIFO row and Command+Enter or the text-bearing send button invokes live
    steer. Unsupported/rejected steering restores the untouched composer input;
    it never silently changes intent into queue work.
R3. **Context meter + compaction indicator** — compact context ring near the
    composer, with exact values in its click-open popover; compaction announced
    in-transcript as a quiet step row.
R4. **Esc = interrupt; double-Esc = rewind menu** — checkpoint list per
    turn (D3), confirm + restore; work-card Deshacer wired to same.
R5. **Revisar diff viewer** — work-card Revisar opens a panel with D8 diffs
    (block-rendered, add/del semantics).
R6. **Slash + attachments** — slash passthrough with a minimal autocomplete
    of agent-advertised commands when the catalog provides them; image
    paste/attach honoring promptCaps.image (legacy parity).
R7. **Notifications** — permission requests + turn completion via Web
    Notifications (opt-in in Ajustes), controller device only.
R8. **Transcript export** — copy/export chat as markdown.

## Packaging (P5, Sol) — required for "everything working"
P5a. go:embed renderer2 dist (flag keeps --renderer-dir override);
P5b. cross-compile matrix + Windows :80 defaults + service wrapper scripts
     (NOT replacing the blessed rebuild flow; installs alongside);
P5c. Windows RSS sampling (tasklist/wmic path) closing the P2 TODO.

## Battle-test (gate to STOP)
B1. **Soak lane vs mock** — scripted: 10 concurrent chats, hibernate/resurrect
    cycles, steer+queue, compaction forced (tiny threshold), crash injection
    (kill engine mid-turn), LAN client reconnect + takeover, checkpoints
    rewind; zero errors/leaks (RSS receipts) over 30 min.
B2. **Real-agent canary** — Qwen Code + LM Studio per desktop/acp/README:
    real inference through the full stack; the mock stays the correctness
    oracle (model quality never a test oracle).
B3. Fix findings; re-run B1+B2 green → campaign STOPS. Assist next (on
    user's word only).

Routing: D-lanes Sol (one at a time, internal/acp collision domain);
R-lanes Opus (renderer2 domain); tracks run parallel. Fable gates each.
