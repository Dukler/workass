# Workass agent browser: desktop viewport and reliable observation

Status: implementation spec, 2026-09-22. Authority: the current human request
to investigate Codex desktop browser improvements, specify the Workass changes,
and delegate their implementation to a Luna agent. Implement sections 3–8.
Section 2's follow-on items are researched roadmap items, not this build's
acceptance requirements. The subsequent human request on 2026-09-23 authorizes
continued Luna implementation, coordinator review, and publication after the
acceptance gates pass. Production activation remains outside this task.

## 1. Problem and observed source behavior

An agent must be able to operate a real desktop page while the browser panel is
closed, another chat is selected, or the Workass window is minimized. Panel
layout must not silently turn that page into a mobile breakpoint. The agent
must be able to inspect and deliberately change its viewport, see screenshots,
and know whether an action actually targeted the observed page.

Baseline: primary main was clean at investigation start. Browser manager and
control-server tests passed: 22 tests, zero skips. Baseline evidence is under
`.dev/rebuild/browser-improvements-baseline*.log`.

Concrete defects/limits in the current source:

- `desktop/shell/browser-manager.js`: `create()` does not establish a desktop
  viewport. `activate()` and `resize()` use `safeBounds()` from the panel as
  the native page size. Missing dimensions are clamped to one pixel.
- `desktop/renderer2/src/components/App.tsx`: the shared right rail defaults
  to 312 px, with a 260–900 px resize range. That is presentation space, not a
  suitable default desktop layout width.
- `hide()` detaches the view. `browser.screenshot` uses `capturePage()` and
  treats background/minimized capture as unavailable. A regression test
  currently asserts this failure instead of background capture success.
- `cmd/workass/browser_mcp.go`: despite the historical filename, these are
  direct Workass CLI tools. There is no viewport control; screenshot has no
  crop/full-page options or dimensions in its text result.
- Snapshot is a top-document DOM extraction, with 12,000 text characters and
  the first 200 interactive elements. It omits explicit truncation, effective
  viewport and snapshot identity. Click invokes the first CSS match's
  JavaScript `.click()`; scroll only addresses the top document.
- Good behavior to preserve: persistent profile; one owned live page adopted
  when the human opens its chat; machine-tagged ownership; background keyboard
  input through CDP; verified Monaco/CodeMirror/contenteditable typing; durable
  mutation receipts; controller checks; and artifact authorization.

The above is source/test evidence, not a claim that every platform has been
visually reproduced. Native Windows rendering still needs its own acceptance.

## 2. Codex comparison and scope decisions

Research sources, accessed 2026-09-22:

1. [Official browser documentation](https://learn.chatgpt.com/docs/browser?surface=app):
   shared page viewing, visual comments, separate persistent browser profile,
   downloads, and browser-extension integration.
2. [Official desktop changelog](https://learn.chatgpt.com/docs/changelog):
   May 21 describes richer annotations, image-asset extraction and structured
   page inspection; June 11 describes controlled CDP diagnostics, fewer browser
   round trips, and download/recovery improvements; later entries cover
   background-tab restoration, stable tab layout and WebMCP.
3. Installed OpenAI browser package `26.915.31945`, read-only source at
   `/Users/dukler/.codex/plugins/cache/openai-bundled/browser/26.915.31945/docs/`:
   `api.json`, `accessibility.md`, `visibility.md`,
   `capabilities/browser/viewport.md`, `capabilities/browser/visibility.md`,
   `capabilities/tab/cdp.md`. This documents viewport set/reset, separate
   visibility, viewport/full-page/cropped screenshots, semantic/DOM targets,
   frame locators, waits, dialog and download APIs, and cursor-based diagnostics.
   Capabilities vary by backend: the installed schema does NOT establish that
   every AX feature is enabled in every in-app-browser backend.
4. [Electron webContents API](https://github.com/electron/electron/blob/main/docs/api/web-contents.md):
   device emulation separates view size and display scale; capture options
   include hidden capture. These APIs are mechanisms to test against Workass's
   pinned Electron runtime, not proof that detached-view capture already works.

There is no public complete inventory of Codex's internal browser changes.
The comparison covers documented features and the locally installed API, not
an invented claim of complete internal parity. Workass's 1440×900 default below
is our explicit product choice, not an asserted Codex default.

| Capability family | Workass decision |
| --- | --- |
| Independent desktop viewport, deliberate responsive sizes, background operation | Build now, highest priority |
| Accurate viewport/full-page/cropped images and pixel-to-page mapping | Build now |
| Rich bounded state, semantic targets, current observation identifiers | Build now, preserving existing editor support |
| Trusted pointer actions, nested scrolling, bounded waits and batch feedback | Build now |
| Basic console/runtime-error diagnostics with bounded cursors | Build now |
| Multiple tabs per chat, owned popup adoption, dialogs, uploads/download receipts | Follow-on lane after the foundation; preserve current behavior here |
| Point-and-comment annotations and temporary style previews | Follow-on renderer/product lane |
| Image asset export and a genuinely read-only structured-JS sandbox | Follow-on lane; do not label unrestricted evaluate as read-only |
| External Chrome/Edge attachment, history/bookmark management, profile import | Separate integrations; not required to fix the Workass-owned browser |
| WebMCP/site tools and cloud/manual-auth browser handoffs | Separate integrations, not a dependency of this implementation |

## 3. Architecture and invariants

Retain the existing provider-neutral path: Workass CLI → exact daemon actor →
authenticated shell control server → BrowserManager → owned Chromium page.
No provider-specific branch, new browser automation dependency, new daemon,
second navigation session, raw-CDP public tool, or Workass MCP endpoint.
The frozen LAN invoke/reply/event contract is unchanged. Additive shell IPC
and CLI schemas are allowed in the manifest below.

Separate these per-entry concepts:

- `viewport`: logical page CSS width/height and device scale factor.
- `presentationBounds`: where the human sees the page in the shell.
- `presentationScale`: fitting the logical viewport into that rectangle.
- `visible`: whether this entry is shown to the human; not whether it is usable.
- `documentGeneration`: changes on document replacement/destruction.
- `viewportGeneration`: changes only when logical metrics change.

One page remains authoritative through panel open/close, chat switching,
background adoption, navigation and viewport changes. Preserve history,
cookies, forms, editor state and scroll. Native tab IDs remain stable until the
page really closes. The existing shared browser profile remains shared;
ownership boundaries for agent operations remain exact and machine-tagged.

Implement a serialized per-entry browser-operation boundary for metrics,
capture and input. Two captures/actions must not observe a half-applied size.
Do not hold a daemon actor lock across shell HTTP. Keep ordinary navigation
and human input responsive. Release per-entry resources on close/destroy,
including any capture host, observation refs, timers, listeners and diagnostics.

### 3.1 Rendering mechanism gate

Before completing the tool/UI wiring, add a real pinned-Electron fixture that
proves independent layout and capture. Use Electron device emulation and/or
the owned debugger's CDP metrics/capture APIs through ONE metrics owner; do not
leave competing overrides. Apply logical metrics before first navigation.

Start with the current WebContentsView. If a detached target cannot reliably
paint at the required dimensions, attach that SAME view to a shell-owned,
non-focusing hidden capture host while it is not presented. Reparent it when
shown; never open another page or navigate a clone for screenshots. A hidden
host must be lazily created, profile-scoped, and destroyed with its entries.
Use the smallest bounded host arrangement proven by the fixture. Do not force
show/restore/focus the user's window, move the user's pointer, or bypass
security settings to make a screenshot succeed.

This is an explicitly allowed implementation decision within the spec. Record
the selected mechanism and evidence. A real blank/corrupt capture is a failure,
not a passing test with mocked image bytes or an instruction to open the panel.

## 4. Viewport and presentation contract

Default every new page to **1440×900 CSS pixels, DPR 1**, desktop pointer/UA
behavior. This holds when never shown, hidden, minimized, and in a narrow rail.
Do not derive logical dimensions from `getContentSize()`, monitor resolution,
or the panel's resize observer. A zero-size/collapsed panel changes only
presentation. Preserve explicit responsive dimensions until reset or page close.

Agent tools, all using the existing optional exact `tab_id`:

- `workass_browser_set_viewport`: required integer `width`, `height`, and
  `operation_id`. Width 320–3840, height 240–2160. Reject invalid, fractional,
  non-finite, missing or out-of-range dimensions; never silently clamp them.
- `workass_browser_reset_viewport`: required `operation_id`, restores 1440×900
  DPR 1. It does not navigate or change visibility.
- Existing list/snapshot and successful viewport results expose requested and
  observed effective metrics, generations, visibility and supported capture
  modes. Success requires observed CSS metrics to match, not just a sent CDP
  command. Reads remain read-only and require no operation ID.
- Add optional `visible` to `workass_browser_open`. Explicit `false` creates or
  navigates an owned background page without requesting the pane. Preserve the
  legacy omitted-field behavior. Explicit `true` uses the existing owning-chat
  open request; it never selects another chat or steals OS focus.

Keep viewport selection as shell-owned ephemeral page runtime state, retained
while that page lives. Do not add a new durable chat/preferences store in this
lane. After shell recreation, a fresh page defaults to desktop and reports it.

Human UI: add a compact accessible viewport selector in BrowserPanel with
Desktop 1440×900, Laptop 1280×800, Narrow 390×844, and Custom width/height.
Narrow tests layout only; do not claim a complete mobile device emulation.
Show the effective dimensions. Provide Reset. Fit the fixed page into the panel
without changing its CSS layout. Reuse the existing rail expansion control;
do not redesign the global shell. Native mouse hit testing, text selection,
keyboard focus and browser scrolling must match the displayed scaled page.

## 5. Screenshot contract

Extend `workass_browser_screenshot` additively with `mode`:
`viewport` (default), `full_page`, or `clip`. Clip requires `clip:{x,y,width,height}`
in document CSS pixels. Reject incompatible options and invalid rectangles.
Viewport capture uses the current scroll origin; full-page capture uses the
document content extent without changing page breakpoints or scroll position.

Return the existing image content block, plus structured metadata in its text
result: `screenshot_id`, exact tab identity, mode, viewport dimensions/DPR,
image pixel width/height, CSS capture rectangle, scroll origin, document and
viewport generations, and pixel-to-CSS scale. Preserve metadata through the
Workass CLI image-file materialization path. No base64 in ordinary text/logs.

The default 1440×900 viewport screenshot must be 1440×900 actual PNG pixels,
regardless of Retina display, narrow rail, background state or shell zoom.
Do not stretch a tiny screenshot or silently downsample the requested view.
Bound allocation before capture: at most 16 million pixels and 8 MiB PNG bytes,
within existing transport limits. Oversized full pages return an explicit
bounded error advising a clip; report actual dimensions. Never silently cut off
content or return an empty/stale image as success. Read-only capture is never
a durable page mutation and must not replay navigation/input.

## 6. Observation and interaction contract

Extend snapshot without removing `url`, `title`, `text`, `editors`, or
`interactive`. Include `snapshot_id`, document/viewport generations, effective
metrics, scroll, loading/error state, and explicit text/node truncation counts.
Add bounded semantic page structure (headings, landmarks, labels, role/name,
checked/expanded/selected/disabled/editable state) using the owned CDP AX/DOM
surface. Preserve the existing editor model extraction and password omission.
Inspect open shadow roots and owned frame targets; identify frame boundaries.
If a frame is inaccessible, report that limitation rather than dropping it.
Default output limits: 32 KiB text and 500 semantic nodes; obey the tool's total
response budget and return a truncation marker. No silent first-200 cutoff.

Give actionable observed nodes opaque element refs bound to the exact tab,
document generation and snapshot. Keep CSS selector support for compatibility.
Click/type can target either `selector` or `element_ref` + `snapshot_id`, never
both. Before action, re-resolve the observed node and validate its identity,
connected/visible/enabled state. Ambiguous selectors fail instead of picking
the first match. Navigation, detached frames and removed/replaced nodes make
old refs stale; return a clear refresh-snapshot error. Do not guess a new node.

Extend click with `x`,`y`,`screenshot_id` as a third, mutually exclusive target
form for canvas/visual UI. Coordinates are image pixels and must map through
the returned screenshot metadata. Reject stale generation/scroll/metrics or
out-of-image coordinates. Resolve semantic/selector clicks to a hit-tested
point and dispatch trusted CDP pointer input, without OS focus. Avoid returning
success merely because JavaScript `.click()` returned. Report blocked/covered,
disabled, stale and missing targets distinctly. Preserve existing typing and
keyboard verification behavior; never simplify the working editor paths.

Extend scroll with an optional observed element ref + snapshot identity for
nested scroll regions; retain existing top-page x/y defaults. Return the actual
observed scroll position/change. No scroll in an unrelated frame or chat.

Add `workass_browser_wait` as a bounded read-only observation tool:
optional exact `tab_id`, `condition` (`dom_ready`, `load`, `element_visible`,
`element_hidden`), selector required only for element conditions, and
`timeout_ms` default 5000 / maximum 15000. Use events/condition observation;
never wait indefinitely for network-idle. Timeout returns current safe state
and a clear reason. It does not rerun an action.

Keep batch 1–20 sequential actions on one owned tab. Validate every action's
schema before starting. Add optional `observe_after:true` to return one final
snapshot after the batch, reducing round trips. Stop at first failure or stale
target and return completed indexes, failed index, and unexecuted remainder.
Keep one durable operation identity: no automatic action replay or new IDs on
timeout. Navigation-invalidated refs must stop the remainder. Retain per-action
outcomes and expose no unrelated tabs through nested batch arguments.

## 7. Diagnostics and integration boundaries

Add read-only `workass_browser_diagnostics`: optional `tab_id`, optional
`after_sequence`, and `limit` (default 20, max 100). Return a bounded per-tab
ring of observed console warnings/errors and uncaught runtime exceptions,
monotonic cursor and truncation flag. Maximum 200 entries / 256 KiB per entry
owner; each message at most 2 KiB. Do not persist these page messages to disk.
Mask sensitive values using the existing redaction rules. Do not return request
headers, cookies, storage, bodies, console object expansion or browser secrets.
Navigation identifies a new document generation; closed tabs release the ring.

Register every new mutation in BOTH `workassToolMutates` and the shell's
`MUTATING_METHODS`; update their contract tests. Preserve daemon actor receipts,
shell durable receipt readback, digest validation and controller admission.
All selectors/refs/coordinates/options participate in immutable request identity.
Unknown or cross-chat IDs fail closed. A retry with the same operation ID reads
the receipt; it must never execute the click/resize/batch again.

Update `internal/agenttext/catalog.json` descriptions and `docs/WORKASS-TOOLS.md`
so any provider can discover effective resolution, change it deliberately, use
background capture, and recover from stale observations. Preserve the minimal
environment brief. Do not expand permissions for delegated agents: their
existing browser-tool exclusion remains. Luna validates with isolated fixtures
and the dev shell, not by borrowing this parent's private tool context.

## 8. Implementation order and acceptance

1. Rendering proof and pure viewport/capture state helpers; prove desktop
   dimensions on a real Electron page before broad API work.
2. Shell lifecycle, viewport/capture implementation and operation serialization.
3. CLI/daemon schemas, mutation receipts and screenshot metadata propagation.
4. Panel sizing controls and native input correctness at display scale.
5. Snapshot refs, trusted targeting, nested scroll, wait and batch observation.
6. Bounded diagnostics, discovery docs, regressions and dev acceptance.

Create a deterministic local HTML fixture with responsive desktop/mobile
markers, a tall page with bottom marker, labelled forms, repeated labels,
overlays/disabled controls, nested scroll, shadow DOM, same- and cross-origin
frames, editor content, canvas targets, delayed UI, and console/error markers.
Use fixed localhost fixture servers, never model judgment or a vendor site as
the oracle. Existing mock ACP tests remain the provider oracle.

Required acceptance matrix:

- Never opened → background navigation: innerWidth=1440, innerHeight=900,
  desktop media query true, correct nonblank PNG dimensions/content.
- Open at 312 px → resize rail → close → select another chat → minimize shell:
  same native page, same logical metrics and correct capture; no focus stealing.
- Two chats: changing/capturing A never alters B's viewport, selected pane,
  navigation, focus or typed state. Test machine-tagged equal chat IDs.
- Set 1920×1080, set 390×844, reset: observed media queries, viewport and PNG
  agree; invalid dimensions cause no mutation. Preserve configured narrow size
  through hide/reopen. Scroll/form/session state survives all metric changes.
- Retina/non-Retina and changed shell zoom: screenshot coordinates hit the
  intended target; human click/selection hit the same scaled element.
- Full page includes bottom marker without reflow/scroll changes; clip has the
  right origin/content; oversize/blank/detached/crashed capture reports failure.
- References fail safely after navigation/replacement; labelled controls,
  shadow/frame targets, nested scroll, code-editor typing and canvas clicks work.
- Delayed UI wait succeeds or times out accurately; blocked clicks don't pass;
  batch stops correctly and receipt replay causes no second effect.
- Snapshot/diagnostic bounds, redaction and truncation are explicit. No
  screenshot/diagnostic data crosses owner or controller fences.
- Close/destroy releases views/hosts/listeners and is idempotent. Existing
  artifact header isolation, profile persistence and CDP adapter tests pass.

Commands: targeted Node browser tests; relevant renderer tests and typecheck;
`go test ./cmd/workass -run 'Browser|Tool.*Operation|ToolsCLI' -count=1`;
new pinned-Electron smoke runner; then required repository/dev gates.
The smoke runner uses a fresh temporary user-data directory, local fixture
servers, deterministic assertions on dimensions/DOM/pixels, and saves evidence
under `.dev/rebuild/`. It must not touch real browsing profiles or production.

Use workass-build: rebuild Electron `--profile dev` first (daemon PID stable),
then daemon `--profile dev`, require healthy handoff, controller/catalog/browser
reconnect and unchanged production PIDs. Baseline was prod 6385/6393, dev
40242/25242; re-read before mutation in case another authorized task changed it.
Do not package as a test. Report native Windows rendering as unmeasured unless
the real fixture was executed on Windows; cross-compilation is not that proof.

Deliver implementation plus a concise validation receipt and screenshots.
Do not mark the task complete with only fake-WebContents tests. If the rendering
mechanism cannot meet the matrix, report the exact evidence and blocker; do not
silently lower the default size, require a visible panel, or drop acceptance.

## 9. Closed implementation manifest

- `desktop/shell/browser-manager.js`, `browser-manager.test.js`
- New `desktop/shell/browser-viewport.js`, `browser-viewport.test.js`,
  `browser-observation.js`, `browser-observation.test.js` if needed for separation
- `desktop/shell/browser-control-server.js`, `browser-control-server.test.js`
- `desktop/shell/main.js`, `desktop/shell/preload.js` (browser lifecycle/IPC only)
- `desktop/renderer2/src/browser.ts`, `components/BrowserPanel.tsx`,
  `styles/app.css` (browser controls only)
- `desktop/renderer2/tests/browser-bounds.test.ts`, `browser-owner.test.ts`,
  new `browser-viewport.test.ts`
- `cmd/workass/browser_mcp.go`, `browser_mcp_test.go`,
  `browser_mutation_test.go`, `stateless_mcp.go` (browser classification only),
  `provider_chat_source_contract_test.go` (browser mutation manifest only),
  `tools_cli_test.go`
- `internal/agenttext/catalog.json`, `docs/WORKASS-TOOLS.md`, this spec
- New `scripts/tests/browser-desktop-smoke.cjs` and
  `scripts/tests/fixtures/browser-desktop.html`
- `cmd/workass/embedded/dist/**` only as generated by the normal dev renderer
  sync; no hand editing generated files.

No dependencies, vendor/Windows launch scripts, desktop/package.json, provider
adapters, chat-state redesign or updater changes. If the inspected source
requires an additional file, report the concrete dependency for coordinator
review before expanding this manifest. Do not discard other worktree changes.
