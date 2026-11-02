# Workass

An agentic chat command center. A Go daemon owns all state and hosts provider chats (Claude Code, Codex, Pi, OMP, Devin, other ACP agents); the React UI, Electron shell and phone app are clients over one WebSocket protocol.

## Layout

- `cmd/workass` daemon plus `tools` and `run` CLI
- `internal/acp` bridges, subagents; `internal/chat` actor; `internal/wire` protocol; `internal/agenttext/catalog.json` all text sent to agents
- `scripts/*-native-host.mjs` provider hosts; `scripts/release/` releases
- `desktop/renderer2` UI, embedded from `cmd/workass/embedded/dist`; `desktop/shell` Electron; `desktop/acp` mock server, probes

## Build, test, release

- Build: `go build ./...`. Renderer: `npm run build` in `desktop/renderer2`, `scripts/sync-renderer2.sh`, commit the embedded output.
- One test: `go test ./internal/acp -run NAME -count=1`; `node --test scripts/tests/NAME.test.mjs`.
- All: `node scripts/test-suite.mjs`. `sh scripts/gate.sh` adds build, vet and one rerun per failed test; a pass there prints `WORKASS_TEST_FLAKE name=...`.
- Release: `scripts/release/ship.sh` publishes and installs nothing.

## Dev daemon

`WORKASS_PROFILE=test WORKASS_TEST_ROOT=$(mktemp -d) go run ./cmd/workass --port 18791 --beacon=false` runs the mock provider on loopback. The test root holds its state, config and `HOME`, so provider CLIs are not logged in there; for logged-in providers use `WORKASS_PROFILE=dev` with a scratch `--state-dir`. In a source checkout, with neither a test root nor `--state-dir`, the daemon refuses to start. Call it: `node scripts/wire-call.mjs --url wss://127.0.0.1:18791 --insecure CHANNEL '[]'`. `desktop/scripts/dev-launch-macos.sh` starts the dev app.

## Known traps

- Frame shapes `{t:'invoke',id,channel,args}`, `{t:'reply',id,result,error}`, `{t:'event',channel,payload}` stay byte-compatible: older clients and the phone app depend on them, and on referenced file, id and path names.
- Client terminals are off because they crash the engine (APIINT-14).
- ACP stdout carries JSON-RPC only; another line breaks the peer's parser.
- One owner per session and one `session/prompt` at a time per bridge: racing prompts wedge the engine.
- A lane resumes its exact native thread; a replacement session would split the provider's context.
- Secret-shaped text is redacted before logs and UI payloads (`docs/PORT-SPEC.md` §3 item 8).
- Production Windows has no npm registry and runs endpoint protection, so its tools CLI is a signed Node shim.
- Tests assert against `desktop/acp/mock-server.mjs` because model output varies.
- A `workass run` job survives daemon restarts; other detached processes are untracked until `workass_register_external_work`.
- Playback tests reach the speakers unless muted.

## Read when touching

| Touching | Read |
|---|---|
| Wire, remote mounts | `docs/PORT-SPEC.md` §2 |
| ACP bridge, permissions, steering, queue | `docs/PORT-SPEC.md` §3 |
| Lanes, providers | `docs/PROVIDER-LANE-ARCHITECTURE.md` |
| Hibernation, lease, artifacts | `docs/PORT-SPEC.md` §4 |
| Agent tools | `docs/WORKASS-TOOLS.md` |
| Updates, signing | `docs/DISTRIBUTION.md`, `docs/MACOS-SIGNING.md` |
| Discovery, pairing | `docs/specs/portable-networking.md` |
| Windows | `docs/specs/endpoint-protection-compliance.md` |
| Profiles, daemons | `docs/ENVIRONMENTS.md`, `docs/DAEMON-DEV.md` |
| Test suite | `docs/TEST-SUITE-10S-SPEC.md` |
| Mock, probes | `desktop/acp/README.md` |
| Linux | `docs/LINUX-HEADLESS.md` |
