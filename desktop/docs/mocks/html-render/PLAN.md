# HTML en el chat: implementation plan

This plan is closed and runs once the user approves a variant in `index.html`.
It assumes the recommended set: **Marco B · Hasta una pantalla · Página
primero · Una línea · Diálogo con zoom**. The "Variant deltas" section lists
what changes if another option is picked. Research and t3code references are
in `README.md`.

## Scope

**In scope:**
- A closed ` ```html ` fence in assistant markdown renders as a sandboxed,
  self-sizing page in place, with a code switch, copy, and a zoom dialog.
- The page has no network and follows the theme live.

**Not in scope:**
- ` ```svg ` or other languages.
- Raw HTML in prose, which stays text.
- User messages.
- A `workass_render_html` agent tool, which is the t3code `html_render` route
  through the `visualize`/artifact host. That is a separate decision.
- Forwarding app shortcuts out of a focused frame (t3code
  `BrowserDocumentFrame.tsx:37-59`); a follow-up.
- Opening links in the in-app browser.

**Daemon and wire: no change.** The fence is already transcript text on the
frozen `invoke/reply/event` channel. Nothing is stored, hosted or sent. The only
non-renderer code is in the Electron shell (navigation guard, WebRTC policy)
and in the test fixture (`[mock:stream-html]`).

## Files to touch

### Renderer (`desktop/renderer2/src`)

| File | Change |
|---|---|
| `markdown/html-render.ts` (new) | Pure logic, no React or DOM globals, so tests run in node. It holds: `HTML_RENDER_CSP`; `HTML_RENDER_SANDBOX = 'allow-scripts allow-forms'`; `HTML_RENDER_STATIC_SANDBOX = ''`; `htmlFrameBootstrap` (a real function, serialized with `toString()`, ported from the mock); `buildHtmlSrcdoc(source, theme, { mode: 'scripted' \| 'static' })`; `htmlThemeVars(read: (token) => string)`, which maps `tokens.css` to t3code's names (table below); `readFrameMessage(data)`, which returns a typed message or `null`; `isHtmlFence(lang)`, true for `html`/`htm` and case-insensitive; `htmlPageTitle(source)`, from `<title>`, else `<h1>`, else `Página HTML`; `clampFrameHeight`; `createGrowthGuard()`; `HTML_RENDER_MAX_SOURCE = 512_000` (t3code `tools.ts:19`) |
| `markdown/html-frames.ts` (new) | One process-wide controller. It owns a single `window` `message` listener and a `Map<WindowProxy, FrameHandlers>`; messages from unregistered windows are dropped. It also owns one `MutationObserver` on `<html data-theme>`, which recomputes the vars and posts `wa-html:theme` to every registered frame while setting each iframe's `color-scheme`. Lazy mounting uses one `IntersectionObserver` (rootMargin of one viewport); a frame more than three viewports away unmounts and keeps its last height as a placeholder. At most 12 frames stay mounted, evicted least recently visible first. A `ResizeObserver` on `.transcriptviewport` supplies the cap (below) |
| `markdown/HtmlBlock.tsx` (new) | Marco B as mocked. A right-aligned 28px icon row (Ver el código · Copiar · Ampliar) sits above a `.hb-clip` holding the frame, the fade and "Mostrar todo". The code view reuses `CodeBlock` with a "Página" action. Streaming shows `HtmlStreamLine`: orbit, "Escribiendo página", "1,1 KB · 19 líneas", and a "Ver código" peek that shows `CodeBlock` with the open, unhighlighted fence. A source over the size limit shows code only. The frame stays invisible (`opacity: 0`) until its first size report, then fades in over 180ms. The fade is **not** gated on `prefers-reduced-motion` (Reduce Motion is on here and a fade is a dissolve) |
| `markdown/HtmlRenderDialog.tsx` (new) | Port of t3code `HtmlRenderDialog.tsx:27-78` with Workass styling from the mock: overlay `rgba(0,0,0,.78)`, a panel 92vw × `min(86vh, 100vh-160px)`, a close button pinned top-right like `.imglb-close`, title and zoom (−, %, +) below, 50–200% in 25% steps. Initial focus goes to close; Esc and a backdrop click close it; focus returns to the expand button. It mounts its own frame (fresh srcdoc), as t3code does |
| `markdown/MarkdownBlock.tsx` | In `case 'code'`, `isHtmlFence(b.lang)` routes to `<HtmlBlock raw lang closed streaming />`; everything else is unchanged. The memo comparator stays the same: a sealed block's `sig` is stable, so a page never re-renders or reloads while later tokens stream |
| `markdown/CodeBlock.tsx` | An optional `actions?: ReactNode` prop renders before Copiar. No other change |
| `icons.tsx` | Add `IcCodeView` (`</>`) and `IcPageView` (window with title bar and two lines of content), with paths from the mock's `#i-code` and `#i-page`, plus `IcMinus`. Reuse `IcExpand`, `IcClose`, `IcStampCopy`, `IcCheck`, `IcChevDown` and `IcPlus` |
| `styles/app.css` | Add the `.hb`, `.hb-row`, `.hb-clip`, `.hb-fade`, `.hb-more`, `.hb-line` and `.hbdlg-*` rules from the mock's "NEW" section. No existing rule changes |
| `components/Transcript.tsx` | Provide `ChatFindQueryContext` (an open find with a non-empty query). `HtmlBlock` consumes it: while find is open and `findMatchOffsets(source, query)` is non-empty, the block shows its code, so the existing `[data-chat-find-text]` ranges can highlight the source (t3code reveals the same way, `MarkdownMermaidCodeBlock.tsx:28-29`). When find closes, the block returns to the reader's last choice |

The following do **not** change: `blocks.ts` (the language is already
captured), `AssistantMessage.tsx` (it already passes `streaming`),
`transcriptExport.ts` (exports keep the fence as code), `wire/*` and the store.

### Electron shell (`desktop/shell`)

| File | Change |
|---|---|
| `render-frame-guard.js` (new) | `shouldBlockFrameNavigation({ isMainFrame, frameUrls })` returns true when the frame or any ancestor is `about:srcdoc`. This covers a render frame and anything nested inside it (a nested `about:blank` frame inherits the sandbox but could otherwise navigate). It is pure and unit-tested |
| `main.js` | (1) `win.webContents.on('will-frame-navigate', (event) => …)`: build `frameUrls` by walking `event.frame` → `.parent`; on block, call `event.preventDefault()` and log `[shell] render frame navigation blocked <origin>` (origin only, never the query). (2) `win.webContents.setWebRTCIPHandlingPolicy('disable_non_proxied_udp')`; voice uses `getUserMedia` only (`voice.ts:155`), so nothing regresses. (3) If gate G3 fails: `app.commandLine.appendSwitch('enable-features', 'IsolateSandboxedIframes')` before `ready`, merged with any existing `enable-features` |
| `preload.js` | Expose `window.workassRenderFrames = { supported: true, navigationGuard: true }` |
| `render-frame-guard.test.js` (new) | Unit tests (below) |

### Test fixture

| File | Change |
|---|---|
| `desktop/acp/mock-server.mjs` | `[mock:stream-html]` streams one reply, a few characters every few ms like `[mock:stream-md]`. It carries four fences: the mock's dashboard (a full document), the chips (a fragment), the simulator (a script) and the hostile page. With `[mock:slow]`, the last fence streams for about 6s. Its sources come from the mock's `PAGE_*` constants |
| `desktop/acp/README.md` | One line under "Special prompt markers" |

## Security model (normative)

1. **Frame:** `<iframe sandbox="allow-scripts allow-forms" srcdoc=… referrerpolicy="no-referrer">`.
   - Never `allow-same-origin`, `allow-popups`, `allow-modals`,
     `allow-top-navigation*`, `allow-downloads` or `allow-pointer-lock`, and
     no `allow=` features.
   - Never a `src`, `blob:` or `data:` URL. `srcdoc` gives an opaque origin
     with nothing to fetch.
2. **CSP:** the first element of the frame's `<head>`, exactly:
   `default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; worker-src 'none'; manifest-src 'none'; form-action 'none'; base-uri 'none'; object-src 'none'`.
   Next come `<meta http-equiv="x-dns-prefetch-control" content="off">`, the
   viewport meta, `<style id="wa-theme">` and the bootstrap `<script>`, then
   `</head><body>`, then the agent's source verbatim. The parser folds a
   second `<html>`/`<head>`/`<body>` into ours, and a CSP `<meta>` outside
   `<head>` is ignored. Policies only intersect, so a page can tighten the
   policy but never loosen it. `'unsafe-eval'` stays off.
3. **Bootstrap** (inside the frame, before any page script):
   - Delete the `RTCPeerConnection` family of globals.
   - Apply `wa-html:theme` only from `window.parent`.
   - Report `wa-html:size` from a `ResizeObserver`, using t3code's
     measurement (`htmlRender.ts:405`).
   - In the capture phase, stop every `a[href]` click. Forward only trusted
     clicks whose href is an absolute http(s) URL (no relative URLs, since a
     srcdoc base would resolve them against the app's origin).
   - Forward Esc.
   - It posts nothing else.
4. **Host contract** (`html-frames.ts`). Messages are accepted only when
   `event.source` is a registered frame's `contentWindow`. The only shapes
   accepted are:
   - `wa-html:size`: a finite height above 0, clamped to 16–20,000.
   - `wa-html:link`: honoured only while `document.activeElement` is that
     iframe and `navigator.userActivation.isActive`. It shows the
     confirmation with the full URL, and on "Abrir" calls
     `window.open(url, '_blank', 'noopener,noreferrer')`. That uses the
     existing `setWindowOpenHandler` → `shell.openExternal` path
     (`main.js:413`).
   - `wa-html:escape`: closes the dialog that owns the frame.

   The host sends only `wa-html:theme`.
5. **Navigation:** the shell's `will-frame-navigate` guard cancels every
   navigation of a render frame and its descendants. This covers
   `location=`, meta refresh and plain link clicks the bootstrap missed.
   - **Fallback.** Without `window.workassRenderFrames?.navigationGuard`
     (an older shell, or a plain-browser LAN client), pages render in
     **static mode**: `sandbox=""`, no scripts, plus
     `<base target="_blank">`, so a link with no popup permission does
     nothing. The code switch still works.
   - Gate G2b proves that static mode cannot navigate.
6. **Process isolation:** a sandboxed frame must not be able to freeze the
   app (gate G3).
7. **Limits:**
   - A source over 512,000 chars shows code only.
   - At most 12 frames are mounted at once.
   - The growth guard catches a page that grows with its frame (a viewport
     unit or `height:100%`, which t3code rejects at publish,
     `HtmlRender.ts:71-97`). More than 10 height increases within 1s freezes
     that frame at `min(height, 4000)` until its block remounts.
8. **Data:**
   - The page source is assistant text that the daemon has already redacted
     at ingestion, so the frame shows the same bytes as the code view.
   - Nothing is persisted.
   - With no egress, a secret that slipped past redaction cannot leave
     through the frame.

**Theme variables.** These are t3code's names (`htmlRender.ts:182-229`), so
one page works in both apps. Green stays an accent: `--accent` and `--chart-1`
are the only green variables.

| Var | Token | Var | Token |
|---|---|---|---|
| `--background` | `--bg` | `--accent` / `--accent-foreground` | `--acc` / `--bg` |
| `--foreground` / `--card-foreground` | `--ink` | `--link`, `--info`, `--ring` | `--link` |
| `--muted`, `--input`, `--secondary`, `--code-background` | `--field` | `--destructive` | `--del` |
| `--muted-foreground` | `--muted` | `--warning` | `--amber` |
| `--card`, `--popover` | `--panel` | `--success` | `--add` |
| `--border` | `--line2` | `--chart-1..5` | `--acc`, `--link`, `--amber`, `--del`, `--muted` |
| `--primary` / `--primary-foreground` | `--ink` / `--bg` | `--radius`, `--font-sans`, `--font-mono` | `10px`, the system UI stack, `"SF Mono", Menlo, Consolas, monospace` |

## Behaviour

- **Trigger:** the page renders when the fence is closed or the message is no
  longer running (the Mermaid rule, t3code `ChatMarkdown.tsx:1531`).
  - While it streams, the one-line placeholder shows.
  - When the fence closes, the tail keeps its React key
    (`timeline-layout.ts:526-532`), so the line becomes the page in place and
    the frame mounts once.
- **Cap:** `max(480, round(0.85 × .transcriptviewport.clientHeight))`.
  - A taller page is cut with a 56px fade and "Mostrar todo".
  - "Mostrar todo" grows the page in place, and "Mostrar menos" restores it.
  - Nothing ever scrolls inside the chat.
- **Copy** always copies the source (`copyCodeText`).
- **The switch is per block**, lives in memory, and is not persisted.
- **Failure:** if the frame never reports a size within 3s of `load`, show it
  at its last height anyway, so a broken page is never invisible.

## Variant deltas

- **Marco A:** the same frame. Controls move into an absolute glass pill with
  `opacity 0 → 1` on `:hover` and `:focus-within`, and stay visible on a
  coarse pointer.
- **Marco C:** wrap the block in `.codebox` and put a `Página | Código`
  segment (`.hb-seg`) in `.code-head`. The body gets a 14px × 16px padding
  on `--bg`.
- **Altura Completa:** drop the cap and `.hb-more`, and fit up to 20,000px.
- **Código primero:** `HtmlBlock` starts on the code view.
- **Lo marca el agente:** split the info string (`html preview` → lang
  `html`, flag `preview`); only flagged blocks start as the page. Agents
  must also be told: one sentence in `internal/agenttext/catalog.json`'s
  Workass instructions. That is a daemon text change, still with no wire
  change, and it needs a daemon promotion.
- **Código en vivo:** the streaming state uses `CodeBlock` as today.
- **Página en vivo:** re-render the partial source with its scripts removed,
  at most 3 times per second. This is not recommended.
- **Ventana completa:** replace the dialog with a fixed full-window layer
  under a 40px bar (title, code switch, copy, exit).

## Tests

All tests run inside the existing `node scripts/test-suite.mjs` matrix,
which has a 10s budget, so each new file must stay under about 300ms. Each
SSR file shares one Vite server, like `tests/markdown-code.test.ts`.

**`renderer2/tests/html-render.test.ts`:**
- The CSP string is exact.
- The sandbox value is exact and contains none of the forbidden tokens.
- The srcdoc's first `<head>` child is the CSP meta, and a page's own
  `<meta http-equiv="Content-Security-Policy">` and `<base>` land after
  `<body>`.
- Static mode uses an empty sandbox plus `<base target="_blank">`.
- The theme map is checked against `tokens.css` values for both themes, and
  only `--accent` and `--chart-1` resolve to `--acc`.
- `readFrameMessage` rejects unknown types, a non-finite or zero height,
  `javascript:`/`data:`/relative URLs, and non-objects.
- The growth guard trips at the 11th increase within 1s and not on a slow
  accordion.
- Title extraction works.
- `isHtmlFence` accepts `html` and `HTML` and rejects `htmlbars`.

**`renderer2/tests/html-block.test.ts`** (SSR):
- A closed fence renders one iframe with that exact `sandbox` and
  `srcdoc`, and no `src`.
- An open fence while streaming shows the one-line placeholder with no
  iframe, and reports the size and line count.
- An open fence after the turn ends renders the page.
- An oversized source shows code only.
- An open find query showing the source exposes `data-chat-find-text`.
- No `navigationGuard` gives static mode.

**Existing suites:**
- `stream-render-isolation.test.ts` gains a case: a sealed html block above
  the streaming tail records no re-render in `__blockRenders` while 200
  chunks flow.
- `markdown-code.test.ts` gains a case: `js`/`go` fences are unchanged.

**`shell/render-frame-guard.test.js`:**
- Blocks `about:srcdoc` and nested `about:blank` under it.
- Allows the main frame.
- Allows a `/workass/artifacts/` visualize frame and the browser pane (a
  separate webContents, never consulted).

**Go:** none, since there is no daemon change. The suite still runs
`go test` as part of the matrix.

## Gates and verification on the running window

These run on a **dev** profile only (`desktop/scripts/dev-launch-macos.sh`,
mock ACP provider), never the prod daemon or the installed app, and with no
real Claude/Codex turns.

1. **G0:** the user picks the variants in the mock.
2. **G1:** the focused tests pass:
   - `node --experimental-strip-types --test desktop/renderer2/tests/html-render.test.ts desktop/renderer2/tests/html-block.test.ts`
   - `node --test desktop/shell/render-frame-guard.test.js`
   - `npm --prefix desktop/renderer2 run typecheck`

   Then the full matrix passes: `node scripts/test-suite.mjs`.
3. **G2 (sandbox, live):** in a dev chat, send
   `[mock:stream-html] [mock:slow]`. Capture with
   `scripts/shot-workass-macos.sh <out.png> <dev view port>`. The hostile
   page's log must read `bloqueado`, `ignorado` or `no existe` on every row,
   as in `shots/overview-1280-dark.png`.
4. **G2b (navigation):** from the dev shell's main process, run
   `win.webContents.mainFrame.framesInSubtree.find(f => f.url === 'about:srcdoc').executeJavaScript("location.href='https://example.com/'")`.
   The frame stays on `about:srcdoc` and the shell logs
   `render frame navigation blocked`. Repeat with the guard flag removed
   from preload: the block renders static, and the same call cannot run
   (scripts are off).
5. **G3 (isolation):** in the same way, run `while(true){}` (fire and
   forget) in one render frame. `GET /__workass-shell/probe` must still
   answer, and a scroll in the transcript must show in a second screenshot.
   If not, add the `IsolateSandboxedIframes` switch (above), relaunch dev and
   repeat.
6. **G4 (theme):** switch the theme in Settings. Screenshots before and after
   must show every page repainted with no reload: the simulator keeps its
   slider position.
7. **G5 (streaming):** a mid-stream screenshot shows the one line, and the
   pages above don't flash. After the stream ends, the last block is a page.
8. **G6 (memory):** a chat with 40 html blocks (send the fixture 10 times).
   `/__workass-shell/perf` RSS before and after scrolling end to end, and at
   most 12 mounted iframes (probe `document.querySelectorAll('iframe.hb-frame').length`).
   Record the numbers for the Windows laptop, which has 32 GB and is tight.
9. **Served ≠ delivered.** Every visual claim is checked by probing the live
   dev client (`/__workass-shell/probe`) and screenshotting it, not by
   reading the built bundle. A renderer reload uses
   `POST /__workass-shell/reload?recoverController=1` on the dev view port.

## Order of work

1. `html-render.ts` and its tests.
2. Shell guard and its tests.
3. `html-frames.ts`, `HtmlBlock` and `HtmlRenderDialog`, plus styles and
   icons.
4. Wire `MarkdownBlock` and the find context.
5. Fixture.
6. G1 through G6.
7. Commit with explicit paths.

Promotion is out of this plan's scope. The renderer gates itself on
`window.workassRenderFrames`, so it can ship before the shell; until the
shell is reinstalled, pages render static.
