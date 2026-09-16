# Unified artifact links — 2026-09-16

User approval: one hosting action, one stable Workass link; transport chosen by
Workass, never by agents. Supersedes the public URL contract in
connected-artifact-transport.md; all its transport/security bounds remain.

## Closed contract

Canonical root-relative link: `/workass/artifacts/@<machineId>/<artifactId>/<path>`.
The @ segment cannot collide with generated legacy artifact ids. Host exactly
once. The registry, configured with the daemon's persistent machine identity,
returns this URLPath and Markdown for new registrations and operation readback.
Do not persist changed artifact ids, duplicate registrations or alter operation
digests. Legacy receipt URLs remain readable and are projected to canonical
links on readback. Keep LocalURL as a compatibility field but omit it for
identity-configured production registries: agents should use returned Markdown.
No new tool or hosting mode. If no identity is available, retain legacy output.

The native shell accepts canonical links and the existing connected-artifacts
route as an input alias. New renderer navigation never generates the old route.
Canonical links carry authoritative ownership regardless of current chat.
Legacy unqualified links still inherit their original chat owner. Absolute
legacy links are localized as today. Preserve queries, fragments and relative
assets; reject malformed identity/artifact segments and traversal before URL
normalization. Redirects preserve the incoming route family and exact owner.

The shell uses the existing private bridge for every qualified artifact,
including this machine. The renderer installs that bridge exactly once during
renderer initialization, before machine discovery and even when the machine
list has zero remote peers. Its local adapter calls only the original
`window.api.artifactOpen`, `window.api.artifactRead`, and
`window.api.artifactClose` methods (the daemon adds those methods); it never
uses arbitrary `invoke`. Remote ids use the existing ready MachineSocket. No
added sockets, remote HTTP, credentials or public sharing. The local API exposes
artifactConnectionGeneration() (zero unless ready); local artifact methods reject
while disconnected rather than queue. Pin local transfers to that generation so
reconnect cannot replay old reads.

The adapter captures the initial local machine identity and the original API
object before each asynchronous operation. It accepts a reply only when both
are still the same after the await; a changed identity or API object makes the
operation stale and closes any transfer it owns. Local and remote transfers
retain the original connection/owner checks, cancellation, EOF/error cleanup,
and stale-link rejection semantics. An unknown/offline owner fails closed,
never falls back to current chat/local.
Plain browser clients may serve their own canonical links through the daemon;
the registry rejects canonical requests naming another machine. Existing remote
bridge absence behavior remains. Keep legacy local URL serving compatibility.

## Manifests

Daemon Luna lane: internal/artifacthost/registry.go, registry_test.go,
cmd/workass/main.go, cmd/workass/agent_mcp.go, internal/httpserve/lan_bridge.go.
Registry optional constructor identity parameter preserves old callers/tests.
Production passes identity.MachineID. Canonical owned HTTP must preserve
redirect path and all existing registry validation. Stored receipt validators
accept legacy and own canonical formats only. Add focused persistence/readback,
wrong owner, relative assets, HEAD/range and legacy tests.

Shell Luna lane: desktop/shell/connected-artifacts.js, connected-artifacts.test.js,
connected-artifacts.electron.cjs, view-server.js, view-server.test.js.
Add canonical route detection/header injection/parser/redirects alongside alias;
no packaging list changes or new runtime modules. Test real Electron canonical
native navigation and iframe CSS/image/interaction plus forbidden requests.

Renderer Luna lane: desktop/renderer2/src/connected-artifacts.ts,
src/store/store.ts, src/wire/types.ts, src/components/BrowserPanel.tsx,
src/components/RightRail.tsx, src/components/AssistantMessage.tsx,
src/markdown/VisualizeBlock.tsx, tests/*artifact*.test.ts.
Keep existing exported names as aliases. Introduce explicit owner parsing shared
by navigation/readiness checks, preserving chat/browser ownership independently.
Bridge adapter for local API is narrow and validates original local API object
and machine identity before/after asynchronous calls. No arbitrary invoke API.
All existing Markdown/image/visualization/native browser call sites must resolve
canonical links even in a local chat. A canonical link's `@machineId` always
overrides the chat's owner, including when pasted into another chat. A legacy
unqualified link inherits the original chat owner. Update focused tests and
typecheck.

Lead also owns src/markdown/inline.tsx and tests/unified-artifact-markdown.test.ts
under desktop/renderer2 for absolute/legacy link recognition, plus
tests/artifact-local-wire.test.ts for the local socket boundary.
Lead owns this spec, integration/review/fixes within above manifests, additional
focused tests as needed, dev rebuild and generated embedded assets. No automatic
production activation. Publication follows the existing authorized release scope.

## Acceptance

Single tool result link works on its owner and a paired receiver, including when
pasted into a different chat. Explicit canonical ownership wins over that
chat's owner; legacy links inherit it. Relative CSS/images/downloads, ranges,
redirects, legacy links, owner collisions, offline/disconnected/missing
owners, unsupported bridge, cancellation and header privacy remain covered.
Renderer coverage must include explicit-owner cross-chat navigation, local
canonical navigation with zero remote peers, mismatched/offline owner failure,
stale identity/API detection, transfer cleanup, and the typed local adapter;
run the renderer typecheck with those focused tests. Run focused Go, shell,
renderer tests, typecheck, real Electron fixture then canonical dev shell/daemon
rebuild. Review every production diff; disclose installed-machine test gaps.
