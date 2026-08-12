# Claude commands surface — slash commands, subagent types, output styles

Status: SPEC (approved scope: data plumbing only; a separate mock lane owns all visuals).
Scope: host `scripts/claude-native-host.mjs` → daemon `internal/acp` → wire → renderer data contract.

## Binding laws (restated; violations are rejected)
- The `invoke/reply/event` wire contract (PORT-SPEC §2, `desktop/lan-server.js`, `internal/httpserve/lan_bridge.go`) is FROZEN. Everything below is ADDITIVE: new optional fields on existing payloads, new channels. An old renderer ignores unknown fields/channels; an old daemon answers "unknown channel" (feature-detect precedent: `lan_bridge.go` comment at the `machines:*` api entries). Precedent: role/obligation fields.
- ONE engine per chat; `session/prompt` serialized per bridge. Nothing here starts turns or multiplexes engines.
- Secrets redacted before display or send: catalog strings pass the daemon's existing redaction prefilter (`internal/acp/redact_prefilter.go`) before any emit or reply.
- Never rename referenced ids/paths. Every name below is NEW; nothing existing is renamed.

## 1. SDK ground truth (vendored: `dist-bin/frontier-hosts/darwin-arm64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.d.ts`)
- `SDKControlInitializeResponse` (~L3419): `{ commands: SlashCommand[], agents: AgentInfo[], output_style: string, available_output_styles: string[], models: ModelInfo[], account, fast_mode_state? }`.
- `SlashCommand` (~L6588): `{ name, description, argumentHint: string, aliases?: string[] }` (name has no leading slash).
- `AgentInfo` (~L105): `{ name, description, model?: string }` (model absent = inherits parent's).
- Mid-session change push: `SDKCommandsChangedMessage` (~L2907) `{ type:'system', subtype:'commands_changed', commands: SlashCommand[], uuid, session_id }` — full-list REPLACE semantics (doc ~L2904). COMMANDS ONLY: no change push exists for agents or output styles in this build; those refresh only on a new `initializationResult()`. Output style has no runtime setter (`outputStyle` exists only in Settings, ~L6077) — this surface is READ-ONLY.
- `SDKLocalCommandOutputMessage` (~L3934): `{ type:'system', subtype:'local_command_output', content: string }` — output of local commands (/usage, /cost), meant to render as assistant text.
- Today the host's `openQuery()` calls `query.initializationResult()` and keeps ONLY `.models` (via `modelRows()`); commands/agents/output styles are dropped. `handleMessage()` has no `type:'system'` branch, so `commands_changed` and `local_command_output` are dropped too.
- NOTE: `scripts/claude-native-host.mjs` is under an in-flight `ClaudeSession` refactor — anchor edits by FUNCTION NAME; line numbers below are 2026-07-28 approximations.

## 2. Wire shape: `commandCatalog` (one object, used identically on every hop)
```json
{ "commands": [{ "name": "review", "description": "…", "argumentHint": "<pr>", "aliases": ["cr"] }],
  "agents":   [{ "name": "Explore", "description": "…", "model": "sonnet" }],
  "outputStyle": "default",
  "availableOutputStyles": ["default", "explanatory"],
  "commandsTruncated": 0, "agentsTruncated": 0, "stylesTruncated": 0,
  "asOf": 1785000000000 }
```
Clamps (host applies; daemon RE-applies defensively — host binary and daemon can skew):
- `commands` ≤ 512 entries; `agents` ≤ 128; `availableOutputStyles` ≤ 64.
- Per string: `name`/`argumentHint`/`model`/style names ≤ 80 chars, `description` ≤ 200, `aliases` ≤ 4 × 80. Clip, never drop, a too-long field; DROP overflow entries and count them in `*Truncated` (precedent: `boundedTurnList`/`clipTurnField` in the host).
- Worst case ≈ 380 KiB — fine for the wire, but NEVER persisted: not into `session-state.json`, not into chat archives (64 MiB hydration lesson). Memory only, everywhere.
- Semantics law (host's own turn-evidence rule): absent field = UNKNOWN (old host); `[]` = proven empty. Never collapse the two.

## 3. Host (`scripts/claude-native-host.mjs`)
H1. Capture. In `openQuery()` (~L472+), after `const initialized = await query.initializationResult()`: build `this.commandCatalog = clampCatalog(initialized)` from `commands`, `agents`, `output_style`, `available_output_styles` (clamps of §2, `asOf: Date.now()`). Keep the existing `modelRows` line untouched.
H2. Expose on open. In `handleRequest` where `session/new|session/resume` respond (`respond(id, { sessionId?, configOptions, availableModels })`, ~L1278): add additive `commandCatalog: session.commandCatalog`. Established Workass lanes use exact `session/resume`; `session/load` is not a recovery path. Also advertise `workassClaudeCommandCatalog: true` inside the existing `initialize` response `_meta` block (~L1269) so the daemon can distinguish "old host" from "proven empty".
H3. Change notification. Add a `type === 'system'` branch to `handleMessage()`:
   - `subtype === 'commands_changed'`: replace `this.commandCatalog.commands` (clamped) + `asOf`, then `notify(this.sessionId, { sessionUpdate: '_workass_claude_commands', commandCatalog: this.commandCatalog })`. Payload is ALWAYS the full catalog; receivers replace wholesale.
   - Emit the same notify after EVERY successful `openQuery()` that is not answering a `session/new|resume` request (mid-session engine restarts — `queryNeedsRestart` path — never produce an open reply, and a restarted CLI may have been upgraded on disk). Replace is idempotent; no diffing.
H4. Local command output (companion; required for picked commands to be visible). Same `system` branch, `subtype === 'local_command_output'`: forward `message.content` through the existing assistant-text path (`notify(sessionId, { sessionUpdate: 'agent_message_chunk', content: [{ type:'text', text }] })`) so `/usage`-style commands render; without this a picked local command produces an empty turn.
Generation safety: `consume()` already drops messages from retired generations before `handleMessage` — no extra guard needed.

## 4. Daemon (`internal/acp`)
D1. Types (`types.go`): `type CommandCatalog struct` mirroring §2 (json tags exactly as §2). Add additive field to `SessionInfo` (~L315): `CommandCatalog *CommandCatalog`, json tag `commandCatalog,omitempty`. `SessionInfo` rides new-lane and exact-resume replies; no replacement-session path exists.
D2. Apply on attach. In `attachSession` (`manager.go` ~L2386), next to `applyConfigOptionsForSession` / `applyAvailableModels`: `b.applyCommandCatalog(sessionID, res["commandCatalog"])` — parse, RE-clamp (§2), run strings through the redaction prefilter, store on the Manager cache (D4), include in the returned `SessionInfo`, and emit D5's event. Missing/invalid field → store nil (UNKNOWN), emit nothing.
D3. Change updates. New case in `handleNotification`'s `sessionUpdate` switch (`bridge.go` ~L714): `case "_workass_claude_commands"` → same apply path as D2. Must NOT require a live job (precedent: `_workass_claude_turn` comment — chat-level data, arrives between turns); key by `b.chatIdentity()`. Old daemons hit the `default:` branch and ignore the kind — additive-safe. Gate on `normalizeProviderID(b.providerID) == "claude"` (precedent `manager.go:3537`).
D4. Cache + invalidation. Manager-level, memory-only: `map[chatKey]{sessionID, catalog}` keyed by tabID/chatID (NOT sessionID — survives host recovery; store the current sessionID inside). Replace on every D2/D3. Hibernation (`lifecycle.go` `hibernate`, child killed ~L215) keeps the cache so a hibernated chat still answers D6; wake re-attaches through exact `session/resume` and D2 replaces the entry with the resumed engine's truth. Daemon restart empties the cache (renderer sees UNKNOWN until next attach). Entry deleted when the chat's session is forgotten/closed.
D5. Event (additive channel): `chat:commands`, payload `{ "tabId", "chatId", "sessionId", "commandCatalog" }`, emitted on D2 and D3. Not added to the sticky replay set (`manager.go:1968` — that cache is one-payload-per-channel; wrong for per-chat data). Late clients use D6.
D6. Invoke (additive channel): `chat:commands-get`, args `{ tabId, chatId }` → `{ "supported": bool, "live": bool, "commandCatalog": CommandCatalog|null }`. `supported:false` for non-claude providers or when the host never advertised `workassClaudeCommandCatalog`; `live:false` when the bridge is hibernated (cached snapshot); `null` catalog = UNKNOWN. Old daemon: "unknown channel" error → renderer treats as unsupported.
D7. Prompt-preamble guard. In the prompt-block builder that prepends the `[Workass attachment context]` image notice (`manager.go` ~L3436): when the trimmed prompt text starts with `/`, append the notice as a SEPARATE trailing text block instead of prefixing — a prefix would stop the CLI from recognizing the command (commands are recognized by leading slash of the message text, §5).

## 5. Prompt semantics — what the renderer sends for `/foo`
Verified against the SDK types: there is NO dedicated command field on user messages. `sdkUserMessage()` sends plain `message.content` blocks; the CLI recognizes a slash command from prompt TEXT (sdk.d.ts ~L69 "Slash commands are processed", local-command output type ~L3932). Therefore:
- Picking command `foo` with argument string `A` sends prompt text `"/foo A"` (`"/" + name + (args ? " " + args : "")`) through the EXISTING turn-start path (`job:start` → `session/prompt`). No new wire field, no new host method. Aliases: send the canonical `name`, never the alias.
- The slash must begin the message text (D7 protects this when images are attached).
- Unknown/stale names are SAFE: the CLI passes an unrecognized `/name` to the model as plain text. The renderer must never hard-block a send on catalog membership.
- Commands are turn-starting prompts. Sending one through the steer path is out of contract (harmless — treated as text/queued input — but unspecified).
- Subagent types and output styles are display data only in this spec: `agents` inform the renderer (e.g. rail/subagent affordances); output style is read-only state (no setter in this SDK build — a future style switch would be a new `session/set_config_option` axis + query restart, explicitly out of scope).

## 6. Renderer contract (data only — mock lane owns visuals)
- Inputs: `SessionInfo.commandCatalog` (new-lane/exact-resume reply), `chat:commands` event, `chat:commands-get` on hydration/reconnect for the active chat.
- Replace wholesale on every payload; render `*Truncated > 0` as "list clamped"; filter/fuzzy-match client-side; long names clamp visually (standing law).
- Feature detection: channel error or `supported:false` → hide the surface entirely; `commandCatalog:null` → UNKNOWN (hide palette, allow free-typed `/…` text); `commands:[]` → proven empty state.
- Never persist the catalog into renderer session state (`session:save`).

## 7. Failure modes
- Zero commands: `commands:[]` + `workassClaudeCommandCatalog:true` = proven empty; renderer may say so. Absent field (old host) = UNKNOWN; show nothing, claim nothing.
- Provider is not claude (codex/devin/mock/custom): their hosts never emit the field; D3 gate ignores stray kinds; `chat:commands-get` → `supported:false`. Renderer hides the surface; typed `/text` still goes through as plain prompt text.
- Stale list after CLI upgrade: a running engine serves its old list until the process cycles; hibernation wake, engine restart, or new session re-runs `initializationResult()` and H3/D2 replace it. Staleness is bounded by engine lifetime, `asOf` makes it visible, and §5's pass-through rule means a stale pick degrades to plain text — never a wedge.
- Skewed host/daemon: old host + new daemon → UNKNOWN everywhere (no crash); new host + old daemon → extra reply field ignored, `_workass_claude_commands` falls into `handleNotification`'s default branch (ignored, verified).

## 8. Verification (mechanical, mock-first)
- Host: stub SDK fixture returning 600 commands + a scripted `commands_changed` → assert clamped reply field (512 + `commandsTruncated:88`), the `_workass_claude_commands` notify, and `local_command_output` forwarding. stdout stays JSON-RPC-only.
- Daemon: extend `bridge_test.go` fakes (`fakeConfigOptions` pattern) with `commandCatalog` on session/new → assert `SessionInfo.CommandCatalog`, `chat:commands` emit, `chat:commands-get` across warm/hibernated/unknown, non-claude gate, re-clamp of an over-limit host payload.
- Wire: old-renderer smoke (current renderer against new daemon) must show zero behavior change.
