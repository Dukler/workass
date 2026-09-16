# Connected artifact transport — 2026-09-16

User scope: replace direct remote IP/port artifact access with sharing through
connected Workass instances. Implement, publish, activate only explicitly named
machines, and test the installed path. This spec supersedes the remote-origin
URL qualification introduced in 0.1.158. No new dependencies.

## Ownership and route

The renderer already owns authenticated MachineSocket connections and paired
machine identities. Reuse them; do not create another remote HTTP client,
WebSocket connection, credential store, or public listener. The wire envelope
remains invoke/reply/event byte-compatible; additive artifact channels are allowed.
The initial receiver is the native Workass desktop shell. A plain browser without
that shell bridge must show remote artifacts as unavailable, never fall back to a
remote address. Local artifact behavior stays compatible.

A remote artifact URL in a desktop controller is:
`<local-shell-origin>/workass/connected-artifacts/<machineId>/<artifactId>/<path>`.
It identifies the owning machine and registered artifact, not its current address.
Relative HTML/CSS/image/font/media links keep that prefix naturally. Query strings
are preserved; root-relative links authored outside the artifact directory are
not rewritten. Never rewrite user-authored HTML or inject scripts.

The local shell HTTP server handles this prefix before the normal daemon proxy.
It forwards GET/HEAD reads through main-process IPC to the trusted renderer, which
uses only the exact ready machine link. Never choose the active chat or another
machine as a fallback. A disconnected/unpaired/unavailable machine returns 503;
unsupported old peers return 502 with a short useful error. No direct HTTP fallback.
The native browser remains per full tagged chat id. This is artifact transport,
not remote-agent browser automation and not a general TCP/HTTP tunnel.

## Private shell boundary

The HTTP bridge is loopback-only, restricted to the actual local shell Host and
GET/HEAD. Every request requires a fresh process-local random capability header
(`X-Workass-Artifact-Access`), compared without leaking its value. The secret stays
in Electron main; it is never a URL, renderer value, log, document, or receipt.
Electron injects it only for this exact origin/path and only requests belonging
to the main Workass renderer or BrowserManager-owned webContents, including their
artifact iframe subresources. External browsers, LAN clients, wrong hosts,
unowned webContents, and forged/missing headers cannot use the bridge. Respect
existing webRequest listeners; do not accidentally replace them. Deny cross-site
initiators outside the shell origin (opaque sandbox initiators are permitted
only for owned artifact frames); top-level native artifact navigation is allowed.

IPC accepts replies only from the owning main renderer/frame. Correlate replies
with random request ids, discard late/duplicate responses, and impose bounded
concurrency and timeout. Close pending requests when the window/server closes.
No service worker, credential transfer, browser security bypass, or weakened CSP.

## Remote read protocol

Register authenticated read channels on the daemon using the existing wire hub:
- `artifact:open`: `{path, method, headers}`. Path must start with
  `/workass/artifacts/` and contain no traversal/encoded separators/backslashes.
  Method GET or HEAD only. Headers allowlist: Range, If-Range, If-None-Match,
  If-Modified-Since. Return `{transferId,status,headers}`.
- `artifact:read`: `{transferId}` returns `{bodyBase64,eof}`; chunks <=128 KiB.
- `artifact:close`: `{transferId}` idempotently releases the transfer.

Use artifacthost.Registry.ServeHTTP as the only file authorization/serving oracle;
reuse its withheld checks, path validation, MIME, disposition, CSP, ETag,
Last-Modified, and HTTP range behavior. Do not expose a generic filesystem reader.
Capture its response through a bounded streaming pipe, not a whole-file buffer.
Only allow safe response headers (Content-Type/Length/Range/Disposition,
Accept-Ranges, ETag, Last-Modified, Cache-Control, Content-Security-Policy,
X-Content-Type-Options, X-Workass-Withheld, Referrer-Policy, Location if safely artifact-relative).
Never relay Set-Cookie, authorization, hop-by-hop headers, or arbitrary redirects.
Do not advertise direct remote URLs in errors or redirected Location values.

Transfers use random opaque ids, at most 32 live transfers per daemon, 30-second
idle expiry, and deterministic cleanup after EOF/error/close. Waiting for response
headers and reading a chunk must be bounded; cancellation must release blocked
pipe writers. The shell allows at most 16 concurrent HTTP artifact requests,
awaits HTTP backpressure, stops reads after browser abort/disconnect, and closes
the remote transfer in finally. Keep only one chunk per request in memory and
check body/status/header types and chunk sizes at both sides of IPC.

Expose the three read channels through the normal authenticated hub read path;
unpaired/revoked sockets cannot invoke them. Direct artifact HTTP requests from
non-loopback clients are denied: remote sharing now requires the paired WebSocket.
Loopback legacy local artifact access remains for existing local consumers.

## Renderer integration

A narrow `window.workassArtifacts` preload API carries requests/replies; no
secret is exposed. The renderer routes each request to `machines.linkFor(id)` and
checks that the same link is still current/ready after async completion. Calls use
the existing data connection and the exact artifact channel allowlist. No cached
artifact payload in chat state, no replay after reconnect, no automatic retries.

Replace address-derived origins in store/browser helpers with the local bridge
prefix. Missing shell bridge or machine state yields unavailable text, no empty
href/src and no remote fallback. Apply to Markdown links/images, visualizations,
and native browser navigation. Preserve explicit local artifact behavior and
exact chat ownership. Remote visualization iframes may load again because the
response is served at the local shell origin; preserve original sandbox and CSP.
Do not modify unrelated remote chat focus or agent-control behavior.

## Lanes / manifests

A (daemon): `internal/artifacthost/transfer.go`, `transfer_test.go`,
`cmd/workass/artifact_transfer.go`, `artifact_transfer_test.go`,
`cmd/workass/main.go`, `internal/httpserve/server.go`, `server_test.go`,
`internal/wire/wire.go` only if the read channel allowlist requires it.

B (native bridge): `desktop/shell/connected-artifacts.js`, its tests (including the disposable
Electron smoke harness `connected-artifacts.electron.cjs`),
`desktop/shell/view-server.js`, its test, `desktop/shell/main.js`,
`desktop/shell/preload.js`, `desktop/shell/browser-manager.js`, its test.

C (renderer): `desktop/renderer2/src/connected-artifacts.ts`, its tests,
`src/browser.ts`, `src/store/store.ts`, `src/markdown/VisualizeBlock.tsx`,
`src/components/BrowserPanel.tsx`, renderer artifact/browser regression tests.
Existing touched AssistantMessage/inline/MarkdownBlock/RightRail components may
change only to pass the new route. No dependency or frozen-envelope changes.

Coordinator owns this spec, cross-lane review, missing integration tests, builds,
source reconciliation, generated bundle, publication and authorized activation.
Agents do not publish/restart/install. All coding and deterministic tests use dev
or isolated fixtures; production use is limited to authorized live verification.

## Acceptance

1. RPC fixtures exercise HTML, relative CSS/image assets, binary downloads, HEAD,
   byte ranges/416, conditional requests/304, withheld/missing files, traversal,
   bounded large transfer, cancellation, expiry and unpaired access rejection.
2. Native bridge tests show missing/wrong capability and unowned request denied;
   wrong machine/disconnect cannot route locally; no direct remote HTTP request;
   backpressure, abort, malformed/oversize replies, concurrency limits clean up.
3. Renderer tests show exact ready link selection, no raw remote address in URLs,
   stale link rejection, normal local behavior, remote iframe uses local origin.
4. Lead review reads all production diffs and tests before dev activation.
5. Canonical Electron and daemon dev rebuilds healthy, production PIDs unchanged.
6. Publish once through ship.sh after clean reviewed committed pushed source.
7. Activate only current-human-authorized machines with exact observed versions
   and a stable operation id. No automatic retry of an installation failure.
8. Live test uses a harmless remote-hosted HTML fixture with relative CSS/image,
   interaction, a downloadable binary and Range request. The browser address
   contains local origin plus machine/artifact ids, never san-laptop's address.
   Prove direct unauthenticated remote artifact HTTP is denied, while the paired
   tunnel succeeds; switch chats and reconnect without crossing identities.
9. Explicitly report any remaining platform/transport or live coverage gap.


## Packaging correction — 2026-09-16

Release 0.1.159 omitted connected-artifacts.js from both explicit shell packaging
lists, causing startup to fail. Recovery lane: scripts/package-workass-macos.sh,
scripts/stage-windows-portable.sh, desktop/scripts/check-shell-dependencies.cjs,
and desktop/shell/package-dependencies.test.js. Include the module on both
platforms and resolve every staged relative CommonJS dependency before packaging.
The regression must use the actual platform file lists and fail when the module
is removed. This corrects packaging only; no installer activation or direct
filesystem replacement is authorized by a report of the failure.
