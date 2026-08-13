# PORT-SPEC — binding laws for the workass Go daemon

Status: v1 (2026-07-09, Fable). The wire-contract inventory (§2b) is produced
by an extraction lane and reviewed before P1 starts; everything else here is
final unless the user changes scope.

## 1. Target architecture

One Go binary, `workass`, an always-on daemon that owns ALL state. Clients
(Electron shell, any browser, phone) are views over WebSocket. Modules:

- **state store** — chats, transcripts, activity, config; hot in RAM, spilled
  to disk in the SAME file formats the Node app uses today (`state/`,
  transcripts, `app-config.json`) so existing tools interop during migration.
- **ACP bridge manager** — one engine subprocess per chat; lifecycle states
  `warm | active | idle | hibernated`; spare-session pre-warm pool.
  MULTI-PROVIDER MODEL (user law 2026-07-10): there is NO global provider.
  The daemon holds a provider REGISTRY (mock/devin/qwen/claude/codex/custom
  from config); all enabled providers are available simultaneously; each chat
  binds its provider at session creation. Bridges/spares are per-provider.
  SELECTION UX (user law 2026-07-10b): ONE model selector in the composer,
  grouped by provider (small section dividers per agent, all models of all
  enabled agents listed); choosing a model binds the chat's provider. The
  daemon aggregates per-provider catalogs into one grouped catalog event.
  USER CATALOG (user correction 2026-07-15): in production, deterministic mock
  models and the fixed local smoke-test identities (`workass-dev` and the Qwen
  `coder-model` canary) MUST NOT be advertised, selectable, spawnable, or
  rendered by any production user or agent surface, including model settings,
  favorites, tracked-subagent catalogs, transcript rows, and the Turnos rail.
  Production rejects explicit fixture selections. Deterministic fixtures run
  only in development/test profiles, which MUST expose the complete fixture
  catalog for manual testing and allow those models to be favorited. Do not hide legitimate
  vendor preview/beta models or arbitrary user-owned local models by broad name
  heuristics. A user may favorite any visible model from the picker; favorites
  are keyed by exact provider id + base model id, persist in daemon-owned app
  settings, and render in saved order as a compact section above the ordinary
  provider groups. A missing provider/model remains saved but invisible until it
  returns; favorites never rewrite provider/model ids or select a model by
  themselves.
  AUTH: workass stores NO credentials — installed agent CLIs self-auth
  (plug-and-play, Goal 5); only custom-command providers accept env config.
  No API-key UI anywhere.
  PROVIDER DETECTION — FINAL DESIGN (normative, 2026-07-10):
  1. The user NEVER writes provider config. providers.json is a
     DAEMON-OWNED CACHE: written by detection, read at boot, regenerated
     if deleted. The only human input it persists is explicit
     enable/disable decisions made in Settings.
  2. On every daemon startup (async, non-blocking) and on providers:detect:
     probe PATH for known agent CLIs (devin, qwen, claude) with ACP
     handshakes; probe local model servers (127.0.0.1:1234 LM Studio,
     127.0.0.1:11434 Ollama via /v1/models).
  3. Detected agent CLI → provider auto-enabled (status ready, latency).
     Detected local server WITHOUT a wrapping CLI → auto-register a
     provider running cmd/workass-agent pointed at that server (one
     provider per server; models = the server's /v1/models). qwen present
     AND server present → both providers exist (different agents, same
     backend).
  4. User disables (Settings) are ALWAYS respected across re-detections; user
     env overrides win over autoEnv. Absent binary → not-found (hidden
     from composer); server down → inactive with reason.
  5. Grouped catalog + providers:list re-emit after each detection pass.
  6. Provider CLI update discovery is daemon-owned and non-installing: check
     immediately after detection, then hourly while the daemon remains alive.
     If every comparable provider lookup fails, retry after 5, 15, and 30
     minutes. Scheduled checks are single-flight, so detection, retries, and the
     hourly clock cannot stampede the registries or reorder update snapshots.
  Acceptance: fresh state dir + no providers.json + cold boot → composer
  offers every installed agent and every running local server with zero
  user action, on Mac (qwen/LM Studio) and Windows (devin) alike.
  FRONTIER AGENTS: CLAUDE CODE + CODEX — FINAL DESIGN (normative, 2026-07-22):
  Workass follows T3 Code's direct-provider boundary. Zed ACP compatibility
  adapters MUST NOT participate in discovery, packaging, or any runtime
  process tree.
    - Claude Code → Anthropic's official `@anthropic-ai/claude-agent-sdk`
      `query()` API, with `pathToClaudeCodeExecutable` set to the user's
      installed official `claude` executable. The SDK uses the user's existing
      Claude Code settings and vendor-owned login.
    - Codex → the user's installed official `codex app-server` JSON-RPC
      protocol directly. The app-server uses the user's existing Codex login.
  1. Built-in provider `claude` is named "Claude Code" (badge `native`) and
     resolves only `WORKASS_CLAUDE_CODE` or official `claude` on PATH.
     Built-in provider `codex` is named "Codex" (badge `native`) and resolves
     only `WORKASS_CODEX` or official `codex` on PATH. There is no npx,
     adapter-directory, or compatibility-package fallback.
  2. Neither vendor transport speaks Workass's private session contract.
     Workass therefore owns two thin stdio hosts: `claude-native-host.mjs`
     maps the frozen Workass session surface to the official Agent SDK;
     `codex-native-host.mjs` maps it to app-server. Their stdout is JSON-RPC
     only and their diagnostics go to stderr. They MUST NOT implement or
     launch a Zed ACP package.
  3. Status semantics: official executable + native handshake succeeds →
     `ready`; a vendor auth failure → `needs-login` with a Settings hint to
     run `claude auth login` or `codex login`; executable absent → `not-found`
     and hidden from the composer. Workass never presents its own credential
     prompt for either provider.
  4. Packaging (P5): `scripts/vendor-frontier-hosts.sh` stages the two
     Workass-owned hosts plus one checksum-pinned official Claude Agent SDK
     package under `dist-bin/frontier-hosts/<platform>/`. It never downloads
     a Claude or Codex ACP package. Packaged builds include one portable Node
     runtime for the official JavaScript SDK/hosts; the vendor CLIs remain
     user-owned installs and are never copied by Workass.
  5. TOS LAWS (binding, violations rejected): use only the official Agent SDK
     and official vendor executables/protocols; Workass NEVER reads, stores,
     proxies, or forwards OAuth tokens or files under `~/.claude` / `~/.codex`;
     no API-key UI for these subscription providers; login and token refresh
     always happen in the vendor's own software. SDK/CLI calls inherit the
     user's normal vendor settings rather than reimplementing authentication.
  6. All §3 landmines apply unchanged (one engine per chat, serialized
     `session/prompt`, bounded stderr tails, redaction before display, and
     terminal reconciliation). Direct hosts preserve the frozen renderer wire
     contract; the renderer MUST NOT grow provider-specific transport logic.
  7. The direct hosts MUST preserve streaming text/thoughts, tool lifecycle,
     permissions, MCP configuration, images, resume, cancellation, live
     steering receipts, usage/context, model/mode/effort controls, and Codex
     rate-limit/reset surfaces. Claude effort is a model-specific axis:
     catalog detection MUST NOT assign effort to a model whose official SDK
     control surface omits it, and any synthetic default alias is hidden after
     it is reconciled to its unique explicit model.
  8. Frontier provider records name the official native provider commands.
     Runtime configuration does not translate retired adapter records or keep
     adapter-name aliases. A provider record that does not name its configured
     executable is ordinary explicit user configuration and succeeds or fails
     under the current provider launch contract; it is never silently rewritten
     by a compatibility branch.
  9. PLAN LIMITS (user correction 2026-07-13): Workass MUST expose the
     authenticated provider's real five-hour and weekly utilization/reset
     windows without reading vendor OAuth files or calling undocumented HTTP
     endpoints. The packaged Codex native host advertises the version-gated
     `_meta.workassCodexRateLimitsRequest` extension and maps
     `_workass/codex/rate-limits` to the official app-server
     `account/rateLimits/read` RPC. The packaged Claude native host advertises
     `_meta.workassClaudeUsageRequest` and maps `_workass/claude/usage` to the
     installed Agent SDK's structured usage control API. Workass refreshes
     asynchronously on session attach, after terminal turns, whenever the active
     chat requests the context popover, and every five minutes for already-live
     provider bridges. Popover refresh is account/provider-scoped: it MUST NOT
     acquire or change the visible chat's session binding. It reuses any live
     bridge for that provider or opens one disposable metadata-only session, so
     plan data remains available while another provider owns the active turn.
     These paths send no provider prompt and consume no model tokens. An expired reset
     boundary makes the prior percentage stale: the renderer MUST suppress that
     percentage and show a refresh state until a newer provider snapshot arrives;
     it must never concatenate contradictory reset prose such as
     "Se reinicia reiniciado". Workass normalizes
     provider percentages/reset boundaries into `chat:plan-usage`, and retains
     the latest snapshot only in daemon memory. Provider reset windows remain
     inside the existing context-ring utilization popover; they MUST NOT add
     always-visible clocks, labels, badges, or controls to the composer row.
     Missing windows stay absent, and this renderer chrome MUST NOT alter or
     annotate model-authored response content. An
     earned Codex rate-limit reset is a separate scarce account credit, not one
     of those clocks. When `account/rateLimits/read` exposes
     `rateLimitResetCredits.availableCount > 0`, Workass MUST show a distinct
     gift/reset affordance in the Workspace/account menu while leaving the
     original composer context indicator unchanged. An available action MUST
     look actionable; muted disabled styling is reserved for an unavailable
     capability or the brief in-flight consume state. It MUST
     never consume a credit automatically. A controller-confirmed use calls the
     version-gated official `account/rateLimitResetCredit/consume` operation with
     one stable idempotency key per logical attempt, then refetches
     `account/rateLimits/read` before updating either the count or clocks. A
     transient retry reuses the same key; `alreadyRedeemed` is idempotent
     success, and `nothingToReset` leaves the credit available. Provider title,
     description, expiry, and opaque credit id are displayed only when supplied.
     An absent/rejected extension
     degrades to the existing ACP usage metadata; it MUST NOT delay session
     initialization or fabricate percentages. Adapter patches are exact-version
     and exact-shape gated, and an unaudited upstream version fails packaging.
  9a. CONTEXT STATE (user correction 2026-07-22): session context is
      passive application status; the user MUST NOT have to send or ask the
      model anything to recover it. Every provider-authored `used/size` update
      is scoped to the exact tab + chat + provider, retained in daemon-owned
      session state, and rehydrated after renderer reload, controller reconnect,
      daemon restart, native resume, and provider switching. A stale renderer
      save cannot erase or overwrite a newer daemon-observed reading. The
      existing composer context ring reflects that provider-scoped reading
      without a model turn; exact percentage, token counts, and plan windows
      remain in its click-open popover. Do not add a permanent context word,
      percentage, numeric badge, or other text beside the ring. Missing data
      renders as an honest empty state, never fabricated `0%`, and a provider
      switch never presents the previous provider's context as the newly
      selected model's.
  9b. CHAT CONTROL DURABILITY (user correction 2026-07-22): an exact-chat
      provider/model/effort/permission change committed through daemon or agent
      control MUST survive every renderer save already in flight. The daemon
      advances a monotonic per-chat runtime-control revision for each such
      commit; renderer mirrors echo it unchanged. When a save's revision does
      not equal the daemon revision, providerId, currentModelId, currentModeId,
      and per-model control memory remain daemon-authoritative. A renderer that
      has hydrated the current revision remains allowed to make normal user
      selections. Starting a turn is the final daemon authority and fences any
      control change it applies. A delayed stale save MUST NOT switch an idle or
      recovered chat back to an expired or unauthenticated provider.
  9c. FOREGROUND TERMINAL TRUTH (user correction 2026-07-22): once a foreground
      turn is terminal, none of its foreground tool calls may remain visibly or
      durably `in_progress`, `pending`, or `running`. `job:end` settles any tool
      whose provider omitted the final tool-call update: successful turns map
      it to `completed`, failed turns to `failed`, and cancelled turns to
      `cancelled`. Daemon restart applies the same reconciliation after orphaned
      turns are marked failed and migrates older terminal transcript rows. This
      rule does not guess whether separately tracked spawned/background work is
      finished; that surface retains its own provider/process terminal oracle.
  9. CLAUDE SPAWNED WORK (user correction 2026-07-15): Workass MUST passively
     observe background Bash, Agent, and Workflow work started naturally by a
     Claude-provider chat. The spawning agent does not call a Workass
     registration tool. The exact-version Claude adapter patch forwards only
     bounded typed `task_started`, `task_progress`, `task_updated`,
     `task_notification`, and `background_tasks_changed` fields; prompts and
     provider transcripts never ride this side channel. The daemon correlates
     those events with the original tool call's `run_in_background`, task id,
     and validated Claude task-output path. Missing terminal notifications are
     reconciled from the provider's live-task level signal and process ownership
     of the output file. Output paths are accepted only beneath a Claude temp
     task directory, and every exposed tail is bounded and secret-redacted.
     Task ids parsed from fallback prose are normalized away from sentence
     punctuation and merge into a structured record only under the same
     tool-call id + normalized task id; structured descriptions and lifecycle
     state remain authoritative over fallback placeholders.
     Silence and elapsed wall-clock time are NEVER terminal evidence. A
     live-session task that remains `running` pins its ACP engine against idle
     hibernation and RSS/age recycling without a duration limit, even after the
     foreground turn ends. Only a terminal task event, authoritative absence
     from the provider's live-task snapshot, or confirmed process exit releases
     that pin. A pathless record restored after daemon replacement has no live
     ACP supervisor to preserve; after a bounded reconnect grace it becomes
     `orphaned`, never falsely `exited`.
     State is per exact tab+chat, survives daemon restart, and writes a bounded
     durable receipt on terminal completion. The renderer shows a separate
     per-chat background-work card after Turnos: running items first, terminal
     items in their own collapsed fold, full wrapping titles, and an opt-in live
     output tail. A chat's sidebar activity dot remains active while any of its
     spawned-work records is running, even after the foreground model turn has
     become idle, and settles as soon as every record is terminal. It MUST NOT
     mix these items into the global proc/engine surface or require optional
     explicit registration. Agent MCP exposes exact-pair read-only list and
     receipt tools; a pair mismatch is rejected. TERMINAL DELIVERY (user law
     2026-08-10): those receipts remain internal state only. Settling background
     work MUST NOT enqueue a synthetic chat message, start or resume an agent
     turn, create pending wake state, hold an obligation open, or block an app
     update. The user or an explicitly running coordinator may read receipts on
     demand; completion never speaks as the user.
  10. PERMISSION CARD TERMINALITY (user correction 2026-07-15): a permission
      request has one transcript owner only while its daemon resolver is live.
      Every terminal path—explicit decision, timeout fallback, session cancel,
      or subagent interruption—emits an exact id-based resolution event. The
      renderer removes that card immediately and, after hydration/reconnect,
      reconciles persisted cards against the daemon pending set — a read any
      approved device may perform, not only the controller. A resolved subagent permission MUST NOT remain stuck at the bottom of
      the parent chat.
  Acceptance: Mac with Claude Code and Codex both logged in → fresh state
  dir, cold boot, zero config: composer shows Claude and Codex groups; a
  real prompt turn streams end_turn on each; logging out of one flips it
  to needs-login with the hint after providers:detect.
- **job runner** — headless `devin -p` spawns with streaming, per-kind
  permission modes (mirror today's behavior).
- **proc registry** — every child process registered, listable, killable;
  auxiliary workspace processes are first-class (Goal 6/7 dependency).
- **LAN server** — HTTP + WebSocket speaking the frozen wire protocol (§2);
  serves the embedded renderer (`go:embed`).
  PHONE QR REACHABILITY (user correction 2026-08-10): a production Mac that
  offers the phone-pairing QR MUST bind its authenticated TLS listener to the
  LAN. A loopback-only daemon MUST refuse to draw the QR; it must never encode
  a private-LAN address that the running listener cannot accept.
  Renderer note (decision 2026-07-10): the legacy renderer
  (`desktop/renderer/`, vanilla) stays frozen and working against both
  backends. The NEW renderer (`desktop/renderer2/`) is React 19 + Vite + TS —
  competitor-grounded (Claude desktop, T3 Chat, Cursor are React) — built on
  the Mac only, shipped as a static bundle (go:embed in P5); Windows never
  runs npm. Perf doctrine from T3 Chat: local-first chat state, block-level
  markdown streaming (re-render only the block receiving tokens),
  virtualized lists.
- **controller lease** — device tokens, exactly one controller;
  notify/show and `lan:access-request` route to the controller only (plan P3).
  SEEING IS NOT DECIDING (2026-07-26, for the phone client): permission
  VISIBILITY is not controller-scoped. `chat:permission-request`,
  `chat:permission-resolved` and the `chat:permissions-pending` read go to
  every APPROVED device; only `chat:permission-decide` requires the lease.
  Rationale: an approved device already reads every message through
  `session:get`, so withholding the question protects nothing, while a phone
  that must seize the lease to see a card drags every prompt off the desktop
  its owner is working at. The lease exists to stop two devices answering the
  same card, not to stop one of them reading it.
  PAIRING REWORK (user law 2026-07-10, supersedes PIN): NO PIN ceremony.
  Unknown device connects → daemon emits `lan:access-request`
  {requestId, ip, deviceName, userAgent} to the CONTROLLER (and desktop
  shell) → host approves/denies via `lan:access-decide` (channel names
  reused from the legacy app) → on approve the daemon silently issues the
  device token to the waiting client. Approved devices (name, ip, last
  seen, token hash) are listable/revocable (settings · Dispositivos).
  Localhost stays auto-trusted.
  NETWORK (user constraint 2026-07-10): a managed machine is reachable ONLY
  on port 80 — production daemon binds :80 by default on Windows
  (configurable); clients discover the daemon by probing port 80 for
  `GET /workass/health` → {app:"workass", version, name} identity JSON.
- **agent API** — environment brief injection, app-state query, notify(),
  show() (plan Goal 7; v1 in P2).
- **macOS release identity** (user law 2026-07-19): every production
  `Workass.app` and standalone Workass daemon build is signed with one
  persistent cryptographic identity. Ad-hoc/CDHash-only production signatures
  are forbidden because macOS treats each rebuild as new code and invalidates
  privacy grants. Before installation, the staged and installed releases must
  satisfy each other's designated requirements; an incompatible identity is a
  hard stop except for one explicit migration away from the legacy ad-hoc
  build. The updater stops the old shell before changing its on-disk bundle,
  performs a same-volume staged swap with rollback, verifies the installed
  signature, and launches only afterward. Private single-Mac development may
  bootstrap one explicitly approved local signing identity; public
  distribution uses Developer ID signing plus notarization. An updater never
  substitutes for stable signing. A public macOS artifact is self-contained:
  it carries the daemon, vendored ACP adapters, and a checksum-pinned portable
  Node runtime, and its first launch can install the daemon as a user
  LaunchAgent without npm, Homebrew, a source checkout, or elevated access.
  Public signing is nested-code-first, timestamped, hardened-runtime signing;
  both the app and DMG are notarized and stapled. Release output is immutable
  and versioned and includes a DMG, update ZIP, checksums, release metadata,
  and notary receipts. The Electron shell itself is exact-version pinned and
  fetched only at Mac build time from its official release; its published
  checksum is verified before development activation or release packaging, and
  an arbitrary global Electron install is never a release input. Automatic
  update activation stays disabled until it can
  prove foreground turns and tracked asynchronous work are quiescent, stop the
  shell and daemon before replacement, and roll back after failed health,
  controller, or catalog recovery.
- **Windows portable updates** (user law 2026-08-10): extracted Windows builds
  update from the platform-specific manifest on the latest stable GitHub
  Release. Authenticode is not required for this private portable lane. The
  shell accepts only an HTTPS feed for the exact Windows/amd64 portable target,
  verifies the archive's declared size and SHA-256 before extraction, rejects
  unsafe archive paths, revalidates the embedded release/shell versions and
  required PE32+ x86-64 runtime files, then uses the same quiescent daemon
  handoff, sibling-directory swap, health/controller/catalog gates, and
  rollback contract. Release tags and assets are immutable. Builds predating
  this law require one manual portable replacement before in-app updates can
  bootstrap themselves.

Zero third-party Go dependencies unless this spec grants them. WebSocket is
hand-rolled RFC 6455 exactly as `desktop/lan-server.js` already proves
(server side of the same codec). JSON via encoding/json.

## 2. Wire protocol (FROZEN)

Transport: WebSocket, text frames, one JSON object per frame.

- Client → server: `{ "t": "invoke", "id": <seq>, "channel": "<name>", "args": [...] }`
- Server → client: `{ "t": "reply", "id": <seq>, "result": <any>, "error": <string|null> }`
- Server → client (push): `{ "t": "event", "channel": "<name>", "payload": <any> }`

Args are an ARRAY spread into the handler (matches Electron IPC arity).
Handlers are the same set the renderer's `window.api` maps to — the mapping
in `desktop/lan-server.js` (`LAN_BRIDGE_JS`) is the authoritative client-side
enumeration. Static file serving + `/lan-bridge.js` injection behavior is
preserved so today's renderer runs against the daemon without edits.

### 2b. Channel inventory
Extracted to `docs/WIRE-CONTRACT.md` (lane deliverable): every channel with
direction, args shape, result shape, error behavior, side effects, and the
`main.js` handler location. Channels that are Electron-shell-local (dialogs,
clipboard, window state) are listed in their own section — they stay in the
shell, NOT the daemon.

## 3. ACP landmines (each one was learned the hard way — all preserved)

1. **Client terminals OFF by default.** Advertising `terminal:true` triggers
   the APIINT-14 engine crash. Opt-in env `ASSISTANT_ACP_CLIENT_TERMINALS=1`
   only. (`desktop/main.js:291-297`)
2. **Cold-init timeout 60s** (`ASSISTANT_ACP_INIT_TIMEOUT_MS`): the Devin CLI
   is itself Electron; first launch is slow. (`main.js:298-301`)
3. **Bounded stderr tail** (default 16KB, `ASSISTANT_ACP_STDERR_TAIL_CHARS`)
   kept per engine and dumped on exit so a `code:1` crash is diagnosable.
   Engine stdout is NEVER tapped into logs (it is JSON-RPC noise).
4. **Don't code multiple agents like shit** (user wording, 2026-07-11 —
   final form of the old "one engine per chat" rule). History: the legacy
   app crashed because a dual LAN/local state split ACCIDENTALLY drove
   one chat with two engines — bad coding, not an inherent limit. The
   actual laws that survive:
   a. No unmanaged concurrency: never let two code paths race the same
      session/engine by accident (single owner per session; serialized
      prompts per bridge, item 5). Also never multiplex unrelated chats
      onto one engine process — a wedged engine must not poison other
      conversations. (`main.js:2346-2349`)
   b. Provider switching is free at turn boundaries: `job:start` with a
      different providerId detaches the old session (slow close in
      background), rebinds the chat, and the resurrect path resumes that
      chat's provider-native session when possible, then delta-seeds only
      Workass turns it has not seen. A provider with no usable native session
      gets the shared history exactly once — the new agent still inherits the
      conversation. Mid-turn switch rejected. Regression:
      TestWireProviderSwitchMidChatSharesContext.
   c. DELIBERATE multi-agent per chat (subagents) is implemented and is not a
      violation. Each concurrent child gets its own session/engine and one
      explicit owner: the spawning turn while that turn runs, then the chat
      itself (user law 2026-07-16: a parent turn ending — done, failed, or
      cancelled — never cancels running children; they are ADOPTED by the
      chat, keep their Turnos visibility and event routing unchanged, and
      settle into the same bounded durable receipts). Explicit cancellation
      (subagent cancel, forced chat delete, daemon reset) remains immediate;
      a settling subagent still reaps its own children. The daemon enforces
      per-turn/global fan-out, ownership, cancellation, and bounded durable
      receipts; what stays forbidden is accidental double-driving, never
      designed concurrency.
   d. The agent-facing orchestration contract is schema-versioned. Catalog v2
      is the sole source for provider/model/effort/mode ids and exposes the
      user's optional 1..10 intelligence, taste, and cost ratings (higher cost
      means more expensive), freeform note, provider-neutral permission
      intents, recommendation profiles, and hard limits. Omitted spawn fields
      inherit the parent or a scored profile; permission inheritance translates
      semantic intent across providers (`agent-full-access` ↔
      `bypassPermissions`, `agent` ↔ `acceptEdits`, `read-only` ↔ `plan`) so a
      headless child never silently falls into an unattended ask mode. Agents
      never guess identifiers.
   e. Coordinator feedback is durable-before-delivery. Messages sent to a
      running child are queued first, then use acknowledged live steering when
      the adapter supports it; otherwise they become the immediate follow-up.
      Group wait supports first/all completion. Retry creates a new child with
      the prior resolved selection and a `retryOf` link. Every settled child
      writes a bounded, redacted, per-chat receipt. A child permission request
      is a first-class, latched `waiting_permission` attention event: single and
      group wait tool calls are forcibly completed with the bounded child
      notification as model-visible tool output instead of silently blocking.
      A fast decision cannot erase an unread notification, and an unresolved
      request is returned by every subsequent wait, so the coordinator must
      observe which subagent is waiting and why. Permission approval remains
      controller-owned. A later turn on the same exact tab+chat addresses
      adopted children through the same tools (first touch re-binds event and
      wait routing to the live turn), and an authenticated owner with NO
      currently running turn may still spawn, list, wait, message, retry, and
      cancel (user law 2026-07-16 — coordinators continuing after harness
      background-task notifications run outside any Workass job): a spawn
      without a live turn is born adopted and anchors its visible events to
      the chat's most recent visible turn when one is resolvable.
   f. Agents have first-party chat-control parity through that same MCP server:
      bounded list/read, create, rename, configure, focus, delete, send/queue/
      steer, and cancel. Every operation authenticates the injected owner
      capability and addresses an exact immutable `tabId + chatId`; title,
      position, and "active tab" are never mutation targets. Provider/model/
      effort/mode values are catalog-validated, mutations land in the
      daemon-owned session mirror first, and receipts return exact identity plus
      resolved controls/delivery ids. Agent-authored FIFO rows are daemon-drained
      so they survive renderer loss; React only rehydrates the authoritative
      snapshot and may never double-drive them.
5. **Serialized `session/prompt` per bridge** (prompt queue). (`main.js:2484`)
6. **Exact provider lanes; no replacement-session fallback** (user law
   2026-08-10, supersedes the 2026-07-12 resurrection order): Workass owns the
   visible semantic chat ledger; each provider owns the hidden context of one
   immutable provider-native thread per exact Workass provider lane. A lane is
   scoped by chat, provider realm, machine, and workspace epoch. Restarting or
   replacing a host process may only attach to that lane's saved native thread.
   Exact resume is a mandatory conformance invariant for a durable provider,
   not an optional user-flow branch; a valid Codex/Claude thread must resume.
   Any failure is a provider/runtime defect contained by a broken lane, never a
   reason to change identities.

   Once a lane has a native binding, Workass MUST NOT call `session/new`, load a
   different thread, replay archived transcript text, or seed a replacement
   session as recovery. Attach failures retry only the same native identity and
   otherwise fail closed as a visible lane error.

   A lane is established only after its actor stores a nonzero `ThreadRef`. If
   `session/new` fails before either that `ThreadRef` or a provisional provider
   candidate is recorded, no prompt could have been dispatched: the caller did
   not yet have a native session id. The lane therefore remains absent. There is
   no background retry, transcript replay, or provider fanout; the next explicit
   selection or input for that exact target lane starts one fresh create
   generation and preserves the earlier failed/ambiguous outbox receipt for
   audit. This is initial creation, not replacement-session recovery. A saved
   provisional candidate retains its reconciliation and delivery-ambiguity
   boundaries because it may already own an input.

   A new native thread may be created only when the target lane is provably
   absent, or after an explicit user fork/reset/workspace transition creates a
   new lane epoch. Historical workspace epochs remain stored and resumable;
   moving the chat detaches them but never deletes them. Changing provider
   inside one Workass chat selects another
   lane; returning to a previous provider resumes that lane's exact thread.
   Cross-provider context enters a lane only through a capability-gated,
   non-sampling, receipt-bearing context-import operation. Missing or ambiguous
   import support blocks the switch; ordinary prompts and transcript replay are
   forbidden substitutes. Import support requires versioned capability
   negotiation, deterministic operation/range/digest identity, idempotency, and
   authoritative operation readback. After a crash Workass reads that receipt
   before deciding whether an absent operation may be sent; unknown acceptance
   is never resent. Before any delivery, Workass durably records one
   stable operation id. An ambiguous acknowledgement never causes an automatic
   resend.

   Provider-native compaction stays inside the same native thread. Workass
   retains the full visible ledger and records only typed lane-private coverage
   and checkpoint metadata; it never overwrites visible history with a provider
   summary or fabricates a context reset. The complete identity, state-machine,
   migration, and conformance laws are binding in
   `docs/PROVIDER-LANE-ARCHITECTURE.md`.
7. **MCP fanout guard**: engines can over-spawn MCP subprocesses; a periodic
   guard watches and reaps. Port the guard, not just the spawn.
   (`main.js:794-836`)
8. **Redaction before anything leaves the server**: secret-shaped strings
   (api_key|token|secret|password|credential|bearer) are scrubbed from tool
   previews, logs, config reads, and UI payloads.
9. **Permission flow**: `session/request_permission` → forward to controller
   client → wait for the person; cancel outstanding requests when a job/session
   dies. (`main.js:2373-2411`) Workass arms NO deadline of its own — the old
   180s fallback/deny (and the hosts' 120s peer-request cap) expired cards a
   user was still reading and answered "nothing chosen" on their behalf; the
   origin harness owns that clock now (user 2026-07-25). `PermissionTimeout`
   remains opt-in for a caller that wants one, and never applies to a question.
10. **Interactive chat spawns vs headless jobs**: chats use the permission
    gate; headless `devin -p` jobs carry their own `--permission-mode`
    (auto for read-only consulta drafts, dangerous for operative tasks;
    global override env `ASSISTANT_DEVIN_PERMISSION_MODE`).
11. **Stream coalescing**: stdout token bursts flushed on ~16ms ticks,
    thoughts ~24ms — do not forward every chunk as its own event.
    (`main.js:1563-1567`)
12. **stdout purity** for any ACP server we ship: JSON-RPC only; stderr for
    diagnostics.
13. **Provider-aware steering** (user correction 2026-07-12): never infer native
    steering from a concurrent ACP `session/prompt`. Preserve three distinct
    paths: (a) packaged Codex exposes app-server's real `turn/steer` through the
    version-gated `_workass/codex/steer` adapter request; Workass MUST wait for
    its `{turnId}` acknowledgement, and an absent/rejected extension interrupts
    the active turn and immediately runs the persisted follow-up instead of
    hanging; (b) Claude steers LIVE when the packaged adapter advertises the
    version-gated `_workass/claude/steer` extension (user correction
    2026-07-16, supersedes the cancel-first rule): the direction is injected
    into the running SDK query's streaming input and Workass MUST wait for the
    adapter's accepted prompt UUID (`{turnId}`) — a frame written to stdin is
    not acknowledgement. Timeout → uncertain: never re-send, never interrupt.
    Explicit rejection only occurs when no live turn exists, so it never
    interrupts either. The `_workass_claude_steer_consumed` session update is
    the semantic "applied" receipt. Older Claude adapters keep the legacy
    fallback: persist a separate FIFO follow-up FIRST, then send
    `session/cancel` so the queued direction starts at the cancellation
    terminal — never wait for natural completion;
    (c) every other agent keeps the capability-gated `_session/steer`
    notification, falling back to the client queue when unsupported. The
    deterministic mock remains the oracle for path (c); real-adapter canaries
    verify protocol acknowledgement/cancellation, never model quality.
14. **Queue and steer are separate user intents** (user correction 2026-07-13):
    while a turn is running, ordinary `Enter` appends one durable FIFO follow-up
    and never interrupts the active turn; `Cmd+Enter` explicitly invokes the
    provider-aware steering law above. `Shift+Enter` remains newline. The send
    button is the explicit live-steer/stop control and must not erase the normal
    queue shortcut.
    A submitted direction has exactly one visible owner. For native Codex, it
    leaves the composer immediately as one durable pending preview while the
    current sampling step finishes; it becomes an ordinary transcript user row
    only when the matching `userMessage.clientId` receipt commits it between
    sampling steps, immediately before the next model/tool step. It MUST NOT
    split a streaming sentence at click time. Adapters without that receipt use
    their successful acknowledgement as the commit boundary; turn end reveals
    an admitted but unconsumed row after the completed assistant and never
    replays it. Generic live steering retains one stable pending transcript row.
    An acknowledged native steer remains visibly "Steering…" while its semantic
    receipt boundary is still pending; only a committed/applied row may say
    "Steered". Attachments transfer into that pending owner in the same renderer
    commit and remain continuously visible through acknowledgement, receipt,
    transcript commit, or an explicit one-time transfer to FIFO.
    Only an explicit live-steer rejection may transfer its owner to one FIFO
    row. FIFO promotion removes the queue row and creates the transcript turn in one
    renderer commit; transcript → queue → transcript rubber-banding is
    forbidden.
    A daemon-owned resume may become active while a renderer is still awaiting
    cold session initialization. If an ordinary capability-aware `job:start`
    then loses the exact chat's atomic start race, Workass MUST return one
    durable FIFO receipt, withdraw that invoke's exact optimistic pair, and
    drain it once after the active turn. It MUST NOT write a failed "already in
    progress" assistant row, run two prompts concurrently, or let a stale
    renderer save resurrect the withdrawn pair.
15. **Attachment interaction is zero-copy first** (user correction 2026-07-13):
    choosing/pasting images creates browser object-URL previews synchronously
    without reading bytes, starting a session, or serializing the chat. A queued
    follow-up appears immediately and owns those drafts; base64 preparation runs
    asynchronously after paint, yields between files, and gates FIFO dispatch
    until every image is durable. Partial/failed preparation never sends a text-
    only turn and is visibly retryable. Files/object URLs never enter session or
    localStorage JSON, and every removal/acceptance releases them.
    Every visible raster image or thumbnail supports the platform's native
    right-click **Copy Image** action and focused `Cmd+C` / `Ctrl+C`. Copying
    transfers decoded image pixels—not a local path or Markdown source—and the
    shortcut MUST NOT intercept ordinary text copy unless an image is focused
    or the image lightbox is open. An ordinary primary click on chat media keeps
    opening the existing Workass lightbox. On macOS, `Cmd+click` opens that
    raster in the OS-default viewing app; on other desktop platforms the
    equivalent gesture is `Ctrl+click`. The modifier path materializes only
    bounded validated raster bytes in a private temporary file—never a
    daemon-local source path—and must not also trigger the lightbox. macOS
    `Ctrl+click` remains available to the native context menu.
16. **Folder navigation is exact and transactional** (user correction
    2026-07-13): `fs:list-dir(path)` may paint only that exact requested server
    path; a mismatched reply is an error, never a redirect. Only the newest
    request commits, navigation controls lock while it is in flight, and the
    second half of a double-click may not click through into a replacement row
    at the same screen coordinate.
17. **Typed final answers remain typed** (user law 2026-07-14): only when a
    provider explicitly marks an assistant chunk as `commentary` or
    `final_answer`, preserve that
    exact phase through stream coalescing, the additive `job:event` payload,
    session mirror, crash journal, archive, restart hydration, and copy. The
    visible final answer appears exactly once in the ordinary assistant Markdown
    flow; the typed phase is data, not dedicated visual chrome, and it is never
    duplicated in normal commentary. Unknown phases and
    providers without typed phases retain the legacy assistant-content path.
    A provider/model allowlist and inference from terminal results, headings,
    regexes, or prose are forbidden. The provider-neutral ACP metadata is
    `_meta.workassAssistantPhase`; Codex's native `_meta.codex.phase` remains a
    compatibility source. The provider-native conversation and
    terminal `Job.Result` retain the full combined assistant output in original
    order. The Codex/GPT final answer stays fully visible during streaming and
    after its terminal seal, rendered exactly like ordinary assistant Markdown.
    It has no dedicated wrapper, bar, tint, animation, heading, label, collapse,
    preview, clamp, or show-more state. Its complete original Markdown is
    rendered as authored, without duplicating or adding body content.
18. **Cancellation chrome is sufficient** (user correction 2026-07-14): an
    otherwise empty cancelled assistant turn persists with `status:cancelled`
    and renders only the quiet terminal stamp. Never inject a large synthetic
    `Detenido.` assistant paragraph. Real partial commentary or final output is
    preserved exactly when cancellation happens after visible work.
19. **Provider-owned context compaction wins** (user corrections 2026-07-15
    and 2026-08-10): Workass MUST NOT sample a summary, create a fresh session,
    replay transcript text, or fabricate a zero-token usage reset for any
    established provider lane. Codex and Claude Code compact their native
    threads in place; their native usage/compaction stream is authoritative and
    manual `/compact` continues to route to the provider. A provider without
    native compaction receives a visible context-limit state until its adapter
    implements a verified same-lineage checkpoint capability.
20. **Tool-result images are visible transcript media** (user corrections
    2026-07-15 and 2026-07-22): raster image content returned structurally by
    an MCP/ACP tool MUST survive the bridge, session journal, archive, and
    reload, and MUST render as a standalone assistant-media row immediately
    before its associated tool status row, never inside the tool's grouped or
    foldable wrapper. Verbose text output remains folded. Images open in the
    existing lightbox. The bridge accepts only
    bounded inline PNG/JPEG/WebP/GIF data; remote URLs and SVG are rejected.
    Agent chat-control reads expose attachment metadata rather than duplicating
    base64 into model context. A tool image MUST NOT disappear merely because
    the tool row itself is collapsed.
21. **Natural ACP assistant images require no Workass dialect** (user correction
    2026-07-20): every provider may return a standard structured ACP raster image
    block or author ordinary Markdown `![label](path)` in assistant output.
    Workass normalizes both at the daemon boundary into the same bounded durable
    transcript media; providers are never prompted to call a Workass-only image
    tool or invent attachment syntax. Markdown file targets are accepted only
    when the resolved regular file remains beneath that exact chat cwd after
    symlink resolution. Only bounded PNG/JPEG/WebP/GIF bytes are imported;
    remote URLs, SVG, traversal, and oversized payloads are rejected. The image
    renders at its authored Markdown position and is itself the lightbox/copy
    target. A matching ordinary `[Open](path)` companion link is suppressed as
    redundant, while unmatched ordinary links remain links; no image link may
    navigate the controller to a daemon-local path. Terminal media
    is persisted before `job:event end`, survives archive/reload, and agent
    chat-control reads expose metadata only. Legacy visible mirror rows are
    recovered under the same safety and byte bounds when their files still
    exist.

## 4. Lifecycle additions (new in the daemon — not ports)

- **Hibernation**: idle chat (no turn for N min, default 20, config) →
  engine killed, transcript and native session binding stay on disk; next user
  turn lazily resumes the exact provider-native session. A missing, divergent,
  or unsupported binding fails closed and never creates or replays a replacement.
  Actively-working chats are PINNED
  (in-flight prompt or live-session background Bash/Agent/Workflow work) and
  never reaped, regardless of output silence or elapsed time. Recent tool
  activity advances the idle clock but is not the liveness oracle. Long-lived
  engines recycle after RSS/age thresholds only at a positively quiescent
  boundary. Per-engine RSS is logged and exposed to the UI.
- **Controller lease**: one controller device at any instant, but the
  handover is IMPLICIT (user law 2026-07-26, supersedes "takeover is
  explicit"). Acting on any device takes the lease and broadcasts
  `lan:controller-changed`; nothing asks a human to "take control" first.
  Rationale: one human owns the whole fleet (no accounts by design), so an
  explicit ceremony bought no safety — you cannot type on two devices at
  once — while making the phone in your hand refuse to send the message you
  are looking at. The invariant the lease exists for is unchanged: exactly
  one device owns a decision at a time, so two can never answer the same
  permission card. `lan:take-control` still works and is still additive.
  EXCEPTION: `lan:access-decide` and `lan:revoke` remain explicitly
  controller-only. Admitting or revoking a DEVICE is the real security
  boundary and must not be reachable by a lease a device took for itself.
  Reads were never gated — every approved device sees the full session and
  live `job:event` stream regardless of the lease. First connect from an
  unknown device → controller-approval flow (lan:access-request →
  approve → silent token issuance); see §1 — the PIN ceremony is
  superseded (user law 2026-07-10).
- **Mock hosting**: the daemon serves the repo's design mocks read-only under
  `/workass/mocks/` (no-cache, traversal-guarded) when a mocks directory is
  configured, so review mocks never depend on throwaway ad-hoc servers dying
  with their shell.
- **Artifact hosting** (user correction 2026-07-20): every real Workass ACP
  session receives `workass_host_artifact` through the daemon-owned agent MCP.
  The tool registers one existing supported file or static directory beneath
  that exact agent session's cwd and returns a stable
  `/workass/artifacts/<artifact-id>/` URL plus ready-to-use Markdown. HTML sites,
  documents, data exports, raster/vector images, media, fonts, and ordinary
  downloadable bundles share this one primitive. Sources stay in place and are
  served live, read-only, no-cache, byte-range capable, traversal/symlink
  guarded, credential-shape filtered, and sandboxed; registration survives daemon
  restart. A directory defaults to `index.html` when present and otherwise
  requires an explicit supported entry. The hidden legacy MCP/control aliases
  `workass_host_html` / `html.host`, existing `/workass/html/…` URLs, and
  `html-hosts.json` records remain readable and migrate without link loss, but
  new catalogs, receipts, state, and Go server code use artifact terminology.
  The tool MUST NOT open or focus UI by itself, and the frozen renderer wire
  protocol is unchanged.
- **Credential-shape filter, and no silent withholding** (bug report
  2026-07-26): artifact names are filtered by the SHAPE of a credential file —
  key/keystore extensions, and names whose words OPEN with a credential phrase
  (`credentials.json`, `api-token.txt`, `service-account*.json`, a
  `secrets/` directory) — never by the §3.8 secret-shaped word list. That list
  is for redacting secret VALUES out of text; matched against filenames it
  withheld `_tokens.css`, `design-tokens.json`, `password-reset.html` and
  `credential-flow.svg`, which is most of what a design mock ships. Formats that
  cannot carry a usable credential here (stylesheets, images, fonts, media,
  documents) are never name-filtered at all. Every refusal MUST state its
  reason: `workass_host_artifact` returns a bounded `withheld[]` at host time,
  and a withheld request answers 403 with the reason in the body, in an
  `X-Workass-Withheld` header, and once in the daemon log. A withheld stylesheet
  whose page still returns 200 renders unstyled and reads as a broken app.
- **Browser control descriptor**: the shell and the daemon MUST resolve the
  same profile-scoped descriptor path by default (derived from the profile's
  mutable data root), with the env override honored by both and the legacy
  `~/.workass` location retained as a read fallback — never as a silently
  divergent default.
- **Environment brief**: injected at ACP session seed AND prepended to
  headless spawn prompts: app identity, available surfaces, user
  presence, controller device, and the Show API instruction ("never open
  OS windows to show the user something"). INTERNAL RECEIPTS (user law
  2026-07-15): command/tool output remains in Workass's authoritative event
  history and profile/build logs. The brief MUST tell providers not to echo
  raw receipts or exhaustive file manifests into final answers; user-facing
  handoffs summarize relevant command outcomes and disclose failures, skipped
  checks, or uncertainty. Raw output is included only when the user explicitly
  requests it or it is necessary to explain a failure. LANGUAGE PRECEDENCE
  (user correction 2026-07-20): Workass-owned restore, replay, compaction,
  maintenance, wake, tool, UI, and locale text is internal context and MUST
  NEVER choose an agent's reply language. The language of the current
  human-authored user request wins. This rule is repeated immediately before
  every ACP turn, including already-seeded and provider-resumed sessions; a
  Workass-generated notice continues in the language of the latest human
  request unless that user explicitly asks for another language. All
  Workass-owned prompt scaffolding and role labels are provider-neutral English,
  and restored content is clearly delimited from the current request.

## 5. Verification strategy (how lanes prove their work cheaply)

- Protocol correctness: run against `desktop/acp/mock-server.mjs` (the
  oracle); handshake via `desktop/scripts/probe-acp.mjs` — expected
  `ok:true`, protocolVersion 1.
- Wire compatibility: the UNMODIFIED renderer in a plain browser drives a
  mock chat end-to-end through the daemon (P1 gate).
- Side-by-side A/B: old Electron main and the daemon speak the same
  protocol; point the renderer at either and diff behavior.
- Model quality is NEVER a test oracle.

## 6. Phase gates

The provider/chat refactor phases and acceptance gates are maintained in
`docs/PROVIDER-LANE-ARCHITECTURE.md`. Other work uses the explicit phase gates
in this specification and its task prompt; no absent external plan is required.

## 7. ACP event-semantics index (pointers, not new law)
One map from event family to the binding text above, so nobody re-derives a
contract from code. Each entry is where that semantics is normatively pinned.

- **Steering** — §3 landmines 13 (provider-aware dispatch: Codex `{turnId}`
  admission wait, Claude native live steer via `_workass/claude/steer` with
  durable-FIFO-then-interrupt as the legacy fallback, generic `_session/steer`)
  and 14 (queue vs steer intents; admission ≠ consumption: an acknowledged
  steer stays "Steering…" until the canonical `userMessage` receipt; only a
  consumed/committed row may read "Steered"; timeout → uncertain, never
  replayed). Durable-before-delivery: §3.4e.
- **Typed assistant phases (results)** — §3 landmine 17. Provider-neutral
  `_meta.workassAssistantPhase`; `_meta.codex.phase` is the compatibility
  source; only `commentary`/`final_answer` cross the boundary; phase
  boundaries are never coalesced into one stream event; unknown phases fall
  back to the legacy content path.
- **Plan usage** — §1.8. Metadata RPCs only (`_workass/codex/rate-limits`,
  `_workass/claude/usage`); four refresh triggers (attach, post-turn,
  5-minute live loop, active-chat warm); zero provider prompts; reset credits
  are §3 landmine 19.
- **Spawned work (Claude background tasks)** — §1.9. Five typed lifecycle
  events; prose fallback may only enrich the structured record of the same
  tool call; task ids normalized away from sentence punctuation; liveness
  reconciled via output-file process ownership; bounded durable receipts.
- **Permissions** — §1.10 (terminal resolution event + pending-set
  reconciliation) and §3 landmine 9 (timeout/fallback, cancel-on-death).
- **Tracked subagents (adoption, turnless owner ops)** — §3.4c (parent turn
  end never cancels children; explicit cancel/delete/reset remain immediate)
  and §3.4e (re-binding on first touch by a later turn; turnless spawn born
  adopted with a RootJobID hint or suppressed visibility; latched attention).
- **Catalog / effort discovery** — §1.7 (metadata-only per-model probing,
  synthetic `default` de-dup) and §3.4d (catalog v2 is the sole id source).
