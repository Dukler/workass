# Workass rules

Every binding law, one line each. Read this, then only what the table below names
for your change. A newer dated owner law beats an older one.

## Laws

- **Wire contract**: `{t:'invoke',id,channel,args}`, `{t:'reply',id,result,error}` and `{t:'event',channel,payload}` are frozen and byte-compatible; `args` is an array; the server adapts, the renderer never does.
- **ACP landmines**: client terminals off by default (APIINT-14 crash); 60 s cold-init timeout; bounded stderr tail, engine stdout never logged; one owner per session, no unrelated chats on one engine; `session/prompt` serialized per bridge; MCP fanout guard; tokens coalesced; a dying job cancels its permission requests and Workass arms no deadline on a card.
- **Provider lanes**: one foreground turn per chat; a chat may own several lanes, each with one exact native thread. Once established: no `session/new`, no other thread, no transcript replay or replacement session, no resend after ambiguous delivery. The harness alone says running or terminal. No provider-name branches outside registration and adapters.
- **stdout purity**: any ACP server we ship emits JSON-RPC only on stdout; diagnostics go to stderr.
- **Identifier law**: never rename referenced files, ids or paths; add aliases.
- **Update permission**: applying an update needs the owner's permission, now or programmed for a stated time or condition on stated machines. Builds, tests, agents and old permissions grant nothing; no retry without new permission; never run an installer or swap directly. Full rule: `AGENTS.md` Hard rules.
- **Detached lanes**: start with `workass run` (preferred), or `scripts/spawn-tracked-lane.sh` plus `workass_register_external_work` in the same turn. Unregistered detached spawns are rejected. Completions arrive as task-notification notices only, so build no watchers.
- **Receipts**: exact commands and output stay in tool history and logs; final responses cite the command and its one-line outcome, with no file manifests or byte counts, and always say what was skipped or failed.
- **Design direction**: modern-minimal, neutral surfaces, light and dark AA; green is the accent only, never body text or large backgrounds; no decorative textures.
- **Secrets**: anything matching `api_key|token|secret|password|credential|bearer` is redacted before logs, UI payloads, documents or sends. Never read, store or forward vendor OAuth files (`~/.claude`, `~/.codex`); no API-key UI.
- **Zero dependencies**: no third-party dependency unless a spec grants it. Go is stdlib-first. `desktop/renderer2/` (React, Vite, TS) is the granted exception, built on the Mac only; Windows never runs npm.
- **Discovery**: outside Windows, sweeps also probe port 8788; the daemon sweeps by itself only while a book machine completes a pinned TLS handshake on an attached network, or while it holds a fleet key and knows no machine; otherwise only on request. Unchanged sweeps back off 10, 20, 40, 60 s. Windows keeps port 80.
- **Enrolment**: verify the daemon's certificate proof before sending any enrolment proof; a missing proof means none is sent. A card replaces a known certificate only with fleet-key proof. Refuse pairing from a non-private source.
- **Windows**: signed bundled Node runtime and tools client only; never run the unsigned Go PE as a shell CLI; no tasklist or PowerShell query loops; no shortcut sweeps. No unsigned Go builds or test runs on the work laptop. Never touch `Rebuild-Relaunch.ps1`, `build-lite.ps1`, Windows launch scripts, `desktop/vendor/` or `desktop/package.json`.
- **macOS**: opening the app starts the server directly; no LaunchAgent, login start or respawn; a killed server stays stopped. Production builds use one persistent signing identity, never ad hoc.
- **Workass tools**: direct authenticated CLI/API, not MCP; vendor and user MCP stay. Session lifecycle takes no automatic Git snapshots.
- **Tests**: reproduce a bug in an isolated profile before fixing it; `desktop/acp/mock-server.mjs` is the oracle, model quality never is; no deleted assertions, new skips or arbitrary sleeps; playback tests run muted. Only the repository gate reruns a failed test, once and alone; a pass is named `WORKASS_TEST_FLAKE`, a second failure fails.
- **Scope**: build what the spec says and report a wrong spec; stay inside your declared files; commit in this repo.

## Read this when touching

| Touching | Read |
|---|---|
| Wire channels, frames, remote mounts | `docs/PORT-SPEC.md` §2; `internal/wire/wire.go`; `internal/httpserve/lan_bridge.go` |
| ACP bridge, engines, permissions | `docs/PORT-SPEC.md` §3 items 1-12 |
| Steering, queue, composer | `docs/PORT-SPEC.md` §3 items 13-14; `docs/PROVIDER-LANE-ARCHITECTURE.md` §5.3 |
| Lanes, resume, provider switching | `docs/PROVIDER-LANE-ARCHITECTURE.md` §2, §6, §8; `docs/PORT-SPEC.md` §3 item 6 |
| Adding or changing a provider | `docs/PROVIDER-LANE-ARCHITECTURE.md` §1, §3, §10; `docs/PORT-SPEC.md` §1 |
| Hibernation, controller lease, artifacts | `docs/PORT-SPEC.md` §4 |
| Agent tools, `workass run`, subagents | `docs/WORKASS-TOOLS.md` |
| Updates, packaging, signing | `AGENTS.md` Hard rules; `docs/DISTRIBUTION.md`; `docs/MACOS-SIGNING.md`; `specs/windows-zip-updater.md` |
| Discovery, pairing, enrolment | `docs/specs/portable-networking.md` §3, §6; top of `docs/PORT-SPEC.md` |
| Windows behavior | `AGENTS.md` Windows section; `docs/specs/endpoint-protection-compliance.md` |
| Profiles, ports, rebuilds | `docs/ENVIRONMENTS.md` |
| Running a daemon, live checks | `docs/DAEMON-DEV.md`; `node scripts/wire-call.mjs --url wss://127.0.0.1:PORT --insecure CHANNEL '[]'` |
| Test suite or gate | `AGENTS.md` test sections; `docs/TEST-SUITE-10S-SPEC.md` |
| Mock server, probes | `desktop/acp/README.md` |
| Linux hosts | `docs/LINUX-HEADLESS.md` |
