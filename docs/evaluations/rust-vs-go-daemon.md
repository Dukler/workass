---
status: "EVALUATION ONLY - no rewrite is approved"
date: 2026-07-10
go_commit_evaluated: "2110e6d"
---

# Rust vs Go Daemon Evaluation

This document evaluates a hypothetical Rust rewrite against the Go daemon at commit `2110e6d`. It does not approve or start a rewrite.

## Go Baseline Observed

The evaluated daemon is a single Go module with no third-party Go modules: `go.mod` declares only `module workass` and `go 1.26` (`go.mod:1-3`), and `go list -m all` returned only `workass` (Appendix A).

The current entrypoint parses daemon flags, creates the lease manager, wire hub, ACP manager, registers handlers, then serves HTTP/WebSocket (`cmd/workass/main.go:29-43`, `cmd/workass/main.go:73-120`). ACP defaults include 60s initialize timeout, 180s permission timeout, 16ms stdout flush, 24ms thought flush, 16KB stderr tail, 20m hibernation TTL, 30s RSS sample interval, 12h max age, and a 4GiB RSS recycle threshold (`internal/acp/types.go:12-26`, `internal/acp/types.go:91-125`).

The Go WebSocket protocol is hand-rolled: upgrade computes `Sec-WebSocket-Accept` (`internal/wire/wire.go:173-214`), server text frames are encoded with short/126/127 length forms (`internal/wire/wire.go:583-605`), inbound masked/fragmented frames are decoded manually (`internal/wire/wire.go:612-690`), and invoke/reply/event JSON objects are marshaled by the hub (`internal/wire/wire.go:281-300`, `internal/wire/wire.go:483-494`). The HTTP server injects `/lan-bridge.js` into `index.html` and resolves static paths under the renderer directory (`internal/httpserve/server.go:72-87`, `internal/httpserve/server.go:107-126`); the bridge stores device token/id/name in browser localStorage and sends `{t:"invoke", id, channel, args}` (`internal/httpserve/lan_bridge.go:4-24`, `internal/httpserve/lan_bridge.go:72-100`).

ACP child processes are owned by `Bridge`: the struct stores child/stdin/pending/stderr tail/lifecycle/session/job state plus `promptMu` and `writeMu` (`internal/acp/bridge.go:20-60`). It starts the configured command with stdin/stdout/stderr pipes, spawns stdout/stderr/wait goroutines, and announces process changes (`internal/acp/bridge.go:210-261`). Stdout is parsed as NDJSON JSON-RPC (`internal/acp/bridge.go:281-291`, `internal/acp/bridge.go:367-395`); stderr is kept as a bounded tail and redacted before exit logs (`internal/acp/bridge.go:294-340`). JSON-RPC requests use an incrementing id, a pending map, per-request timers, and a write mutex for stdin serialization (`internal/acp/bridge.go:435-514`). Bridge close resolves pending requests, cancels session permissions, closes stdin, and kills the child process (`internal/acp/bridge.go:913-950`).

The current state model uses Go mutexes: manager maps for bridges, sessions, jobs, permissions, and spare sessions are behind `Manager.mu`; job buffers use `jobMu` (`internal/acp/manager.go:17-34`). Bridge lifecycle fields are behind `Bridge.mu` (`internal/acp/bridge.go:25-47`). Prompt serialization is per bridge through `promptMu` around `session/prompt` (`internal/acp/bridge.go:58`, `internal/acp/manager.go:780-809`). The hibernation path snapshots a candidate, then re-locks and rechecks pinned/running/last-activity state before moving the bridge to hibernated and killing the child (`internal/acp/lifecycle.go:65-105`, `internal/acp/lifecycle.go:107-203`). The race test asserts an arriving prompt aborts reap after the recheck (`internal/acp/bridge_test.go:395-433`).

## Rust Rewrite Plan

### Crate And Module Layout

The Rust crate would mirror the Go module boundaries:

| Rust path | Responsibility | Go source mirrored |
|---|---|---|
| `src/main.rs` | CLI parsing, path normalization, manager construction, listener startup | `cmd/workass/main.go:29-120` |
| `src/wire/mod.rs`, `frames.rs`, `hub.rs`, `pairing.rs` | frozen invoke/reply/event protocol, WebSocket framing, pairing/controller gating | `internal/wire/wire.go:21-64`, `internal/wire/wire.go:76-171`, `internal/wire/wire.go:173-690` |
| `src/httpserve/mod.rs`, `bridge_js.rs`, `assets.rs` | static renderer serving, bridge injection, MIME map, path containment | `internal/httpserve/server.go:14-24`, `internal/httpserve/server.go:42-126`, `internal/httpserve/lan_bridge.go:4-102` |
| `src/acp/types.rs`, `bridge.rs`, `manager.rs`, `lifecycle.rs`, `protocol.rs`, `redact.rs` | ACP protocol, process bridge, lifecycle, jobs, permission routing, redaction | `internal/acp/types.go:12-310`, `internal/acp/bridge.go:20-965`, `internal/acp/manager.go:17-905`, `internal/acp/lifecycle.go:15-605` |
| `src/lease.rs` | device store, token hashing, controller lease | `internal/lease/lease.go:19-262` |
| `build.rs` | renderer bundle manifest generation using `include_bytes!` | current Go serves a directory at runtime (`internal/httpserve/server.go:78-104`) |
| `tests/` | ports of current Go wire/http/acp/pairing/e2e tests | `internal/wire/wire_test.go:19-156`, `internal/wire/pairing_test.go:17-196`, `internal/httpserve/server_test.go:13-67`, `internal/acp/bridge_test.go:18-435`, `cmd/workass/main_test.go:26-325` |

### Runtime And Crates

Chosen runtime: [tokio 1.52.3](https://docs.rs/crate/tokio/1.52.3), with `rt-multi-thread`, `macros`, `net`, `io-util`, `process`, `sync`, `time`, and `fs`. The daemon has many independent IO sources: WebSocket clients, ACP stdout/stderr, child wait handles, lifecycle timers, RSS timers, and permission timeouts (`internal/acp/manager.go:69-73`, `internal/acp/bridge.go:258-260`, `internal/acp/lifecycle.go:30-54`). Tokio maps directly to async TCP, timers, channels, and child processes without adding an HTTP/WebSocket framework.

Planned direct dependencies:

| Crate | Version | Use |
|---|---:|---|
| [tokio](https://docs.rs/crate/tokio/1.52.3) | 1.52.3 | async runtime, TCP, timers, process, fs, channels |
| [serde](https://docs.rs/crate/serde/1.0.228) | 1.0.228 | typed JSON structs |
| [serde_json](https://docs.rs/crate/serde_json/1.0.150) | 1.0.150 | dynamic `Value`, JSON-RPC frames |
| [httparse](https://docs.rs/crate/httparse/1.10.1) | 1.10.1 | minimal HTTP/1 request parsing before WS upgrade/static responses |
| [base64](https://docs.rs/crate/base64/0.22.1) | 0.22.1 | `Sec-WebSocket-Accept` encoding |
| [sha1](https://docs.rs/crate/sha1/0.11.0) | 0.11.0 | WebSocket accept hash |
| [sha2](https://docs.rs/crate/sha2/0.11.0) | 0.11.0 | device token SHA-256 hashes |
| [getrandom](https://docs.rs/crate/getrandom/0.4.3) | 0.4.3 | token bytes and unbiased six-digit PIN generation |
| [subtle](https://docs.rs/crate/subtle/2.6.1) | 2.6.1 | constant-time token hash comparison |

Estimated dependency tree for that set: 9 direct crates; about 26 transitive crates on macOS/Linux, about 33 transitive crates for a Windows GNU target once platform crates are included; about 35 and 42 total crates respectively. This is an estimate because `cargo` is not installed in this workspace, so no local `Cargo.lock` could be generated (Appendix A). The estimate is based on the docs.rs normal dependency declarations for the chosen root crates and common proc-macro/platform dependencies.

### ACP Stdio And Process Management

NDJSON JSON-RPC would be implemented with `tokio::process::Command`, piped stdin/stdout/stderr, `BufReader::read_until(b'\n')`, and a `BridgeActor` owning the child, pending request map, and JSON-RPC write queue. The Go behavior to preserve is: stdout is protocol only (`internal/acp/bridge.go:281-291`, `internal/acp/bridge.go:367-395`), stderr is a bounded diagnostic tail (`internal/acp/bridge.go:294-314`), pending requests have per-request timers (`internal/acp/bridge.go:435-487`), and writes are serialized (`internal/acp/bridge.go:497-514`).

Kill-safe process ownership in Rust would use an explicit `shutdown(reason)` path that closes stdin, resolves all pending oneshot replies, cancels permissions, calls `child.start_kill()` as a drop fallback, and awaits `child.wait()` when shutdown is explicit. This maps to Go `Close`, which resolves pending requests, closes stdin, kills the child, cancels permissions, and forgets sessions (`internal/acp/bridge.go:913-950`).

### WebSocket Server

The rewrite would keep the WebSocket codec hand-rolled rather than use `tokio-tungstenite`, because the server must preserve the existing byte behavior: manual accept key, text frames, 7-bit/126/127 lengths, close handling, ping/pong ignore, and continuation assembly (`internal/wire/wire.go:173-214`, `internal/wire/wire.go:583-690`). `httparse` would parse only enough HTTP/1.1 to distinguish static requests from upgrade requests; WS frame encoding/decoding would stay local code so the frozen protocol remains byte-compatible.

### Shared State And Races

Planned Rust ownership:

| State | Rust owner | Reason |
|---|---|---|
| wire handlers | `Arc<Hub> { handlers: RwLock<HashMap<String, Handler>> }` | Go uses hub-level `sync.RWMutex` for handlers/clients (`internal/wire/wire.go:76-119`). |
| wire clients | `Arc<Hub> { clients: Mutex<HashMap<ClientId, ClientHandle>> }`, with one writer task per client via `mpsc::Sender<Vec<u8>>` | Avoids holding a mutex across socket writes while preserving Go's client drop-on-write-error behavior (`internal/wire/wire.go:149-170`, `internal/wire/wire.go:430-435`). |
| lease store | `Arc<LeaseManager> { state: Mutex<LeaseState> }` | Go serializes devices/controller with one mutex (`internal/lease/lease.go:46-54`, `internal/lease/lease.go:118-124`, `internal/lease/lease.go:144-184`). |
| manager maps | `Arc<Manager> { state: Mutex<ManagerState> }` | Go has one manager mutex for bridge/session/job/permission/spare maps (`internal/acp/manager.go:17-34`). |
| job buffers | `Mutex<HashMap<JobId, JobState>>` or job entries under `ManagerState`, with timer tasks sending flush commands | Go uses `jobMu` plus timers for stdout/thought coalescing (`internal/acp/bridge.go:784-858`). |
| bridge lifecycle | `Arc<BridgeStateLock>` around `BridgeState` plus actor handle | Go keeps lifecycle fields under `Bridge.mu` (`internal/acp/bridge.go:25-47`). |
| JSON-RPC child IO | `BridgeActor` task owns child, pending map, stdin writer, stdout/stderr readers' event channel | Prevents aliasing child handles and centralizes pending response routing (`internal/acp/bridge.go:435-514`). |
| prompt serialization | per-bridge `tokio::sync::Mutex<()>` guard held across the `session/prompt` request future | Matches Go `promptMu` (`internal/acp/bridge.go:58`, `internal/acp/manager.go:780-809`). |

The reap-vs-prompt pin race would be expressed by requiring prompt start to lock bridge state, set `state=Active`, `pinned=true`, and update `last_activity` before awaiting the prompt gate. Hibernation would snapshot a candidate, then lock the same bridge state and recheck `closed`, `StateHibernated`, `pinned`, running jobs, state, and unchanged `last_activity` before extracting the child and marking hibernated. That is the Rust equivalent of Go's recheck (`internal/acp/lifecycle.go:107-171`) and the current race test (`internal/acp/bridge_test.go:395-433`).

### Other Feature Mapping

RSS sampling would use `tokio::process::Command::new("ps").args(["-o", "rss=", "-p", pid])` on macOS/Linux, parse the first field as KiB, and keep Windows returning unsupported unless a new Windows RSS method is approved. That matches current Go, which shells to `ps` and returns "RSS sampling is not implemented on Windows" on Windows (`internal/acp/lifecycle.go:205-280`).

PIN pairing and tokens would persist `state/devices.json` with temp-file then rename, using `getrandom` for token bytes/PINs, `sha2::Sha256` for `sha256:<hex>`, and `subtle` for constant-time comparison. The Go equivalent generates a six-digit PIN, logs it, stores token hashes only, compares hashes constant-time, and atomically writes `devices.json` (`internal/lease/lease.go:83-142`, `internal/lease/lease.go:216-262`).

Renderer embedding would use `build.rs` plus `include_bytes!` to generate a static asset table for the built renderer bundle. The current Go server reads from `RendererDir` at runtime and injects the LAN bridge into `index.html` (`internal/httpserve/server.go:78-87`, `internal/httpserve/server.go:95-104`); a Rust embedded bundle would be a P5-style equivalent, not an exact mirror of the current runtime directory serving.

Estimated Rust LoC: 6,200-7,800 production lines, plus 2,500-3,500 test lines. The estimate is above the current 4,583 Go production lines because the Rust plan includes explicit async actors, typed command enums, build-time asset generation, and ownership plumbing that Go currently expresses with goroutines and mutexes. Current Go line counts are from `wc -l` (Appendix A).

## Comparison Table

| Row | Go as built | Rust as planned |
|---|---|---|
| Third-party deps | 0 external Go modules; `go list -m all` returned only `workass` (Appendix A; `go.mod:1-3`). | 9 direct crates; estimated 26-33 transitive crates depending on target; total estimated tree 35-42 crates. |
| Binary size est. | Measured Windows amd64 PE: 9,861,120 bytes; measured macOS arm64 Mach-O: 9,282,530 bytes (Appendix A). | Estimated 5-12 MB release binary for this minimal Tokio stack, depending on panic/LTO/strip and target; no local Rust build was possible because `cargo` is missing (Appendix A). |
| Idle RSS est. | Not measured: starting the server in the sandbox failed with `bind: operation not permitted` (Appendix A). Estimate for daemon-only idle is 10-25 MiB; engine children dominate once ACP sessions are warm. | Estimate 8-20 MiB daemon-only idle for Tokio plus maps/tasks; engine children remain the same external cost. |
| Cold start | Constructs lease, hub, manager, starts lifecycle/RSS loops, then listens; default spare sessions flag is 0 (`cmd/workass/main.go:41`, `cmd/workass/main.go:73-120`, `internal/acp/manager.go:69-74`). Expected sub-100ms daemon-only on a warmed machine, excluding renderer load and ACP child spawn. | Similar daemon-only path; first run on a machine does not compile, but runtime startup should be in the same order of magnitude. ACP child spawn cost is unchanged because it still launches the same provider process. |
| Cross-compile mac -> windows | Exact working command: `GOOS=windows GOARCH=amd64 GOCACHE=/private/tmp/workass-gocache go build -trimpath -o /private/tmp/workass.exe ./cmd/workass` (Appendix A). | Requires Rust toolchain on Mac, `rustup target add x86_64-pc-windows-gnu`, a Windows linker such as `brew install mingw-w64`, a checked-in `Cargo.lock`, then `cargo build --release --target x86_64-pc-windows-gnu`. The Windows laptop still must not fetch crates. |
| GC/latency at 12 engines, small JSON frames | Go has GC, but the daemon workload is mostly IO, maps, timers, and small JSON; stream coalescing already bounds event bursts at 16ms/24ms (`internal/acp/types.go:17-18`, `internal/acp/bridge.go:784-831`). GC pauses are not expected to dominate over child process IO at 12 engines. | Rust has no GC. Latency risk moves to async scheduling, lock contention, accidental blocking in Tokio workers, and larger compile-time complexity. |
| Memory-safety/race risk in a rewrite | Existing Go has already encoded the prompt pinning and hibernation recheck and has a targeted race test (`internal/acp/lifecycle.go:107-171`, `internal/acp/bridge_test.go:395-433`). | Rust removes data races in safe code, but the rewrite re-exposes protocol/lifecycle races: lost oneshot replies, actor shutdown order, holding async mutexes across awaits, prompt gate starvation, and incorrect hibernate rechecks. |
| LoC | 4,583 production lines including `go.mod`; 6,842 lines including tests (Appendix A). | Estimated 6,200-7,800 production lines plus 2,500-3,500 tests. |
| Build time | Measured local Go native build: 4.0515s; measured Go Windows cross build: 3.9504s (Appendix A). | Estimated 30-120s cold build on first dependency compile; incremental builds depend on Rust codegen and proc-macro churn. No local Cargo receipt available. |
| Maintainability by AI agents | Go code uses stdlib packages and explicit mutex/goroutine patterns. The current repo's implementation is compact and line-cited above. | Rust async code adds `Send + 'static` futures, borrow/lifetime constraints, actor message types, and lock-across-await hazards. No public source gives exact LLM training corpus volume by language, so the factual distinction here is compiler/modeling complexity rather than measured training-token counts. |

## Migration Cost

A rewrite would need to re-prove the existing mock-oracle suite, not reinterpret behavior from model quality. The current Go tests already exercise frame decoding, handshake, invoke/reply errors, broadcast (`internal/wire/wire_test.go:19-156`), pairing/PIN/token/controller behavior (`internal/wire/pairing_test.go:17-196`), static serving and LAN bridge injection (`internal/httpserve/server_test.go:13-67`), mock ACP turns and renderer-consumable job events (`cmd/workass/main_test.go:26-96`), permission routing (`cmd/workass/main_test.go:98-214`), controller-only permission delivery (`cmd/workass/main_test.go:216-325`), ACP init/session/prompt/cancel/error reuse (`internal/acp/bridge_test.go:18-92`), permission timeout/fallback (`internal/acp/bridge_test.go:94-162`), init timeout and stderr tail (`internal/acp/bridge_test.go:164-217`), prompt serialization (`internal/acp/bridge_test.go:219-248`), history replay once (`internal/acp/bridge_test.go:250-273`), hibernation/resurrection/RSS/recycle (`internal/acp/bridge_test.go:275-393`), and the reap-vs-arriving-prompt race (`internal/acp/bridge_test.go:395-433`).

The landmine re-verification burden is per behavior:

| Landmine | Current Go evidence to preserve or reconcile |
|---|---|
| Client terminals off | initialize sends `"terminal": false` (`internal/acp/bridge.go:159-172`). |
| 60s cold init | default and option normalization (`internal/acp/types.go:15`, `internal/acp/types.go:91-93`). |
| Bounded stderr tail | default 16KB, append/truncate, redacted exit log (`internal/acp/types.go:19`, `internal/acp/bridge.go:307-340`). |
| One engine per chat bridge key | bridge key normalization and bridge/session maps (`internal/acp/manager.go:617-650`, `internal/acp/manager.go:668-714`). |
| Serialized `session/prompt` | `promptMu` around prompt request (`internal/acp/bridge.go:58`, `internal/acp/manager.go:780-809`). |
| Replay transcript exactly once | `markSeeded`, history prompt construction, resurrection replacement event (`internal/acp/manager.go:224-261`, `internal/acp/manager.go:333-339`, `internal/acp/manager.go:811-819`). |
| MCP fanout guard | In the evaluated Go daemon files, the only MCP reference is `mcpServers: []` on `session/new` (`internal/acp/manager.go:693`). Whether a Rust rewrite must add a Go-native fanout guard is an open scope question. |
| Redaction before logs/UI | main value redaction and ACP text redaction (`cmd/workass/main.go:27`, `cmd/workass/main.go:103-105`, `cmd/workass/main.go:464-485`, `internal/acp/types.go:28`, `internal/acp/types.go:299-310`, `internal/acp/bridge.go:715-727`). |
| Permission flow | request handling, timeout/fallback, UI event, cancel cleanup (`internal/acp/bridge.go:516-573`, `internal/acp/manager.go:541-611`). |
| Interactive vs headless jobs | Evaluated Go currently returns `not implemented until P2` for non-`app-chat` jobs (`internal/acp/manager.go:151-154`). Full rewrite scope for headless jobs is an open question. |
| Stream coalescing | 16ms/24ms defaults and timer-backed flush (`internal/acp/types.go:17-18`, `internal/acp/bridge.go:784-858`). |
| stdout purity | stdout parser treats lines as JSON-RPC; diagnostics use stderr tail/logging (`internal/acp/bridge.go:281-340`, `internal/acp/bridge.go:367-395`). |

Given the frozen protocol, A/B can use the same renderer bridge shape: client invokes are `{t:"invoke", id, channel, args}` (`internal/httpserve/lan_bridge.go:72-77`), server replies/events use `replyFrame`/`eventFrame` (`internal/wire/wire.go:281-300`, `internal/wire/wire.go:483-494`), and the injected bridge is served without renderer edits (`internal/httpserve/server.go:72-87`). A Rust daemon could run on a different port with the same renderer bundle and compare event/reply traces against Go for identical mock-server scripts.

## Appendix A - Command Receipts

Commit:

```text
$ git log -1 --format=%h
2110e6d
```

Go toolchain:

```text
$ go version
go version go1.26.5 darwin/arm64
```

Go module dependency count:

```text
$ go list -m all
workass
```

Production Go LoC:

```text
$ wc -l go.mod cmd/workass/main.go internal/wire/wire.go internal/httpserve/server.go internal/httpserve/lan_bridge.go internal/acp/bridge.go internal/acp/manager.go internal/acp/lifecycle.go internal/acp/types.go internal/lease/lease.go
       3 go.mod
     569 cmd/workass/main.go
     703 internal/wire/wire.go
     159 internal/httpserve/server.go
     102 internal/httpserve/lan_bridge.go
     965 internal/acp/bridge.go
     905 internal/acp/manager.go
     605 internal/acp/lifecycle.go
     310 internal/acp/types.go
     262 internal/lease/lease.go
    4583 total
```

Go LoC including current tests:

```text
$ wc -l go.mod cmd/workass/*.go internal/wire/*.go internal/httpserve/*.go internal/acp/*.go internal/lease/*.go
       3 go.mod
     569 cmd/workass/main.go
     661 cmd/workass/main_test.go
     261 internal/wire/pairing_test.go
     703 internal/wire/wire.go
     314 internal/wire/wire_test.go
     102 internal/httpserve/lan_bridge.go
     159 internal/httpserve/server.go
      67 internal/httpserve/server_test.go
     965 internal/acp/bridge.go
     956 internal/acp/bridge_test.go
     605 internal/acp/lifecycle.go
     905 internal/acp/manager.go
     310 internal/acp/types.go
     262 internal/lease/lease.go
    6842 total
```

Native Go build for size:

```text
$ GOCACHE=/private/tmp/workass-gocache go build -trimpath -o /private/tmp/workass-go ./cmd/workass

$ ls -lh /private/tmp/workass-go
-rwxr-xr-x@ 1 dev  staff   8.9M Jul 10 01:34 /private/tmp/workass-go

$ stat -f '%z bytes' /private/tmp/workass-go
9282530 bytes

$ file /private/tmp/workass-go
/private/tmp/workass-go: Mach-O 64-bit executable arm64
```

Windows cross build for size:

```text
$ GOOS=windows GOARCH=amd64 GOCACHE=/private/tmp/workass-gocache go build -trimpath -o /private/tmp/workass.exe ./cmd/workass

$ ls -lh /private/tmp/workass.exe
-rwxr-xr-x@ 1 dev  staff   9.4M Jul 10 01:34 /private/tmp/workass.exe

$ stat -f '%z bytes' /private/tmp/workass.exe
9861120 bytes

$ file /private/tmp/workass.exe
/private/tmp/workass.exe: PE32+ executable (console) x86-64, for MS Windows
```

Local Cargo availability:

```text
$ cargo new --bin /private/tmp/workass-rust-deps
zsh:1: command not found: cargo

$ which cargo
cargo not found
```

RSS measurement attempt:

```text
$ /private/tmp/workass-go --port 18788 --renderer-dir desktop/renderer --spare-sessions=0 --rss-sample-interval=1h
2026/07/10 01:35:03 [workass] registered 59 daemon wire channels
2026/07/10 01:35:03 [workass] serving renderer /Users/dev/Workspace/workass/desktop/renderer on http://127.0.0.1:18788
2026/07/10 01:35:03 [workass] device state /Users/dev/Workspace/workass/state/devices.json (trust-localhost=true)
2026/07/10 01:35:03 [workass] server stopped: listen tcp 127.0.0.1:18788: bind: operation not permitted
```

## Open Questions

- Is a Rust rewrite allowed to add and vendor a Cargo dependency tree on the Mac build machine, with the Windows laptop only receiving built artifacts?
- Which Windows Rust target is acceptable: `x86_64-pc-windows-gnu` with MinGW, or `x86_64-pc-windows-msvc` with an MSVC/cargo-xwin style toolchain?
- Should a rewrite include a Go-native implementation of the MCP fanout guard, given that the evaluated Go daemon files only pass `mcpServers: []`?
- Should non-`app-chat` headless jobs be in rewrite scope, given that the evaluated Go code returns `not implemented until P2` for those jobs?
- What binary size, cold build time, and dependency count ceilings are acceptable for a Rust daemon?
- Is exact hand-rolled WebSocket framing mandatory in Rust, or could a vetted WS crate be allowed if byte-for-byte frame behavior is proven against the frozen contract?
