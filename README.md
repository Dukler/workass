# workass

An agentic chat command center for developers, built around an always-on **Go
daemon** (`workass`) that owns all state. Clients — an Electron shell, any
browser, a phone — are views over a WebSocket wire protocol.

## What it is

- **One Go binary, no runtime deps.** The daemon manages chats, transcripts,
  config, and ACP (Agent Client Protocol) agent subprocesses. It serves the
  renderer over HTTP/WebSocket and embeds the built UI.
- **Multi-provider.** A provider registry (mock / claude / codex / qwen /
  custom) is detected at startup; each chat binds a provider at session
  creation. Installed agent CLIs self-auth — workass stores no credentials.
- **Portable.** The daemon cross-compiles for macOS, Windows, and Linux. A
  checksum-pinned portable Node runtime and vendored ACP hosts ship beside the
  binary, so a target machine needs no npm, no admin, and no source checkout.

## Layout

- `cmd/workass` — the daemon entrypoint.
- `cmd/workass-agent` — a small agent adapter for local model servers.
- `internal/` — daemon internals (ACP bridge, wire protocol, state store).
- `desktop/renderer2` — the React + TypeScript UI (built and embedded).
- `desktop/shell` — the Electron shell (a thin view over the daemon).
- `desktop/acp` — ACP dev infra and the deterministic mock server (test oracle).
- `docs/PORT-SPEC.md` — binding laws for the Go port (wire protocol, ACP
  lifecycle, landmines).
- `scripts/` — build, release, and vendoring scripts.

## Build & run

```sh
go build ./...
./dist-bin/workass            # serves the embedded UI; see --help for flags
```

The renderer is built and embedded via `scripts/sync-renderer2.sh` (invoked by
the build scripts). See `docs/` for the port spec, wire contract, and release
process.

## License

See `LICENSE` if present; otherwise all rights reserved by the author.
