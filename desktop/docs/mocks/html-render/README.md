# HTML en el chat: research brief

Status: mock for approval (`index.html`, screenshots in `shots/`). Nothing is
implemented. The closed plan is `PLAN.md`.

Reference: t3code at `/tmp/t3code-src/repo`, commit `e822ba78`. Paths below
are relative to that repo unless they start with `desktop/`, `cmd/` or
`internal/` (Workass).

## The short version

- **t3code never renders a ` ```html ` fence.** A page reaches the thread only
  when the agent calls t3code's own MCP tool `html_render`. The server stores
  the page as an attachment and the timeline draws it in a sandboxed frame
  once the tool call has completed. The only fenced block t3code renders is
  Mermaid, which uses a code/diagram switch.
- In Workass the same experience can be **client-only**: render a closed
  ` ```html ` fence in a `srcdoc` frame with no network. The transcript text is
  already on the wire, so the daemon and wire protocol stay unchanged.
- The t3code tool route maps onto Workass's existing `visualize` /
  artifact-host pipeline. That is a later, additive step, not this one.

## What t3code does

### 1. What gets rendered

| Source | Rendered? | Where |
|---|---|---|
| `html_render` MCP tool call (agent publishes a page) | Yes, as its own timeline row, above the final reply | `packages/shared/src/htmlRender.ts:9`, `apps/server/src/mcp/toolkits/html/tools.ts:74-107`, `apps/web/src/session-logic.ts:747-760`, `packages/shared/src/toolOutput.ts:180-187` |
| `html_preview` MCP tool | Agent only: headless Chrome PNG, `contentHeight` and console. Never shown as a frame | `tools.ts:26-70`, `apps/server/src/htmlRender/HtmlRender.ts:470-502` |
| HTML files and attachments in the file preview | Yes, with a "Show HTML source / Show rendered page" switch, rendered by default | `apps/web/src/components/files/AttachmentFilePreview.tsx:45-48,88,222-229`, `FilePreviewPanel.tsx:1067-1071,1091` |
| MCP Apps (`ui://` resources) | Yes, in a separate frame type | `apps/web/src/components/chat/McpAppFrame.tsx:460-517` |
| ` ```html ` fence | **No.** Highlighted code only | `apps/web/src/components/chat/MarkdownCodeBlock.tsx` (no render branch) |
| ` ```mermaid ` fence | Yes: a diagram, with a code switch | `MarkdownMermaidCodeBlock.tsx:11-84`, `ChatMarkdown.tsx:1525-1535` |
| Raw HTML inside markdown | Parsed, then sanitized to inert markup (`rehype-raw` + `rehype-sanitize`, no script, style or iframe) | `packages/shared/src/markdownPipeline.ts:131-163,188-193` |

The agent is told when to use the tool
(`packages/provider-core/src/server/orchestrationInstructions.ts:38`): build a
self-contained page, check it with `html_preview`, publish it with
`html_render` before the final reply, and don't announce or restate it.

### 2. Placement

- **Inline page (`html_render`).** The page is borderless, on the thread's own
  background, as wide as the reply column (728px default), with its left edge
  on the reply text (`htmlRender.ts:26,242-249`). One glass "Open full screen"
  button appears top-right on hover, focus, or always on coarse pointers
  (`HtmlRenderFrame.tsx:92-132`).
- **Expanded.** `HtmlRenderDialog` (`HtmlRenderDialog.tsx:27-78`) is 92vw
  wide and `min(86vh, 100vh-160px)` tall. A close button sits above the
  panel; title and zoom controls (50–200% in 25% steps, CSS `zoom`) sit below.
- **Mermaid (a fence it renders).** The code-block frame is dropped and a
  slim, always-visible, right-aligned icon row (view switch + copy) sits above
  the diagram (`MarkdownCodeBlock.tsx:223-231`).
- **MCP Apps.** These go full screen in place, using the top layer, so the
  frame never reloads (`McpAppFrame.tsx:460-480`).

### 3. Code/render switch

- **Mermaid:** the diagram is the default. The switch is hidden while
  streaming, its icon shows the destination (`CodeIcon` / `WorkflowIcon`), and
  a render error falls back to the code (`MarkdownMermaidCodeBlock.tsx:26-31,42,63`).
  Chat-find reveals the source when a match is inside the block (`:28-29,73-77`).
- **Files:** rendered is the default. Workspace files remember the last choice
  in localStorage (`FilePreviewPanel.tsx:1067-1071`).
- **`html_render` pages:** there is no switch at all.

### 4. Sizing

- The agent declares a height between 80 and 2000px (`htmlRender.ts:10-11`).
  At publish, the server measures the page in headless Chrome at nine widths
  from 320 to 1144px (`htmlRender.ts:24`, `HtmlRender.ts:375-405`). Before the
  page loads, the frame takes the taller of the two measured widths around the
  current one (`htmlRender.ts:85-120`).
- Once loaded, the page reports its own height (`ui/notifications/size-changed`,
  sent by a `ResizeObserver`). The frame fits it exactly, up to 20,000px
  (`htmlRender.ts:14,100-110`; `BrowserDocumentFrame.tsx:279-292`). A frame
  shorter than its page would scroll inside the thread and take the reader's
  scroll, so t3code never allows that.
- `vh`/`svh`/`lvh`/`dvh` units are rejected at publish, because a height that
  follows the viewport makes a fit-to-content frame grow without end
  (`HtmlRender.ts:71-97`). The page's scrollbar is hidden (`htmlRender.ts:375-377`).
- On mobile, a nested frame caps at the agent's height and scrolls inside
  (`apps/mobile/src/features/threads/HtmlRenderWebView.tsx:114-143`).

### 5. Theme injection

- Variables follow shadcn naming: `--background` (identical to the thread),
  `--foreground`, `--muted(-foreground)`, `--card`, `--border`, `--ring`,
  `--primary`, `--accent`, `--destructive`, `--warning`, `--success`,
  `--info`, `--code-*`, `--chart-1..6`, `--radius`, `--font-sans/mono`
  (`htmlRender.ts:178-229`). The agent learns them from the tool description
  (`:231-240`).
- The server injects a bootstrap into `<head>` (`injectHtmlRenderBootstrap`,
  `:405-468`): a default dark/light stylesheet plus a script. Before first
  paint, the script reads the theme JSON from the `#t3-theme=` URL fragment,
  strips the fragment, and then follows live changes sent as
  `ui/notifications/host-context-changed` postMessages
  (`BrowserDocumentFrame.tsx:125-133`, `useHtmlRenderTheme.ts:30-49`).
- To avoid a white flash, the frame keeps `color-scheme: light` until load and
  then takes the theme's scheme (`BrowserDocumentFrame.tsx:308-312`).

### 6. Sandbox, CSP, network

- **Frame:** `sandbox="allow-scripts allow-forms"`, never `allow-same-origin`,
  `loading="lazy"` (`BrowserDocumentFrame.tsx:295-301`; McpAppFrame is the
  same at `:502-503`).
- **Served page:** `Content-Security-Policy: sandbox allow-scripts allow-forms
  allow-popups allow-downloads`, plus `nosniff`, served from expiring signed
  asset URLs (`apps/server/src/http.ts:61-66,96-140`).
- **Network: open.** No `default-src` is set, so CDN scripts "load as-is"
  (`tools.ts:23-24`). The server inlines local images as `data:` URLs after
  checking their bytes (10 MiB per image, 25 MiB per page;
  `HtmlRender.ts:39-41,188-328`). Input is capped at 512,000 chars (`tools.ts:19`).

### 7. Streaming

- **`html_render`: no partial render, ever.** The page arrives whole as a tool
  argument, and the row exists only once the tool item is `completed`
  (`session-logic.ts:747-750`).
- **Mermaid:** shows code while the fence is open, and the diagram once the
  fence closes or the message stops streaming
  (`ChatMarkdown.tsx:1531`: `isStreaming && !isClosedCodeFence`).

### 8. Security boundaries on the host side

- **Links.** The bootstrap catches clicks on http(s) links and posts
  `ui/open-link`. The host opens the link only if the message comes from that
  frame, the frame has focus, and the user is active
  (`BrowserDocumentFrame.tsx:210-232`, `htmlRender.ts:273-290`). An image
  click follows the same rule and opens the app's viewer (`:254-272`).
- **Shortcuts.** Only an allowlist of view-changing app shortcuts is replayed
  from a focused frame. A page can lie about keys, so nothing that changes a
  thread is ever replayed (`BrowserDocumentFrame.tsx:37-59,170-187`).
- **Mobile.** Top-frame navigation away from the page is refused; new windows
  go to the external browser if they are http(s)
  (`HtmlRenderWebView.tsx:144-151`).

## What Workass has today

- ` ```html ` renders as code: `blocks.ts:105-121` → `MarkdownBlock.tsx:55-56`
  → `CodeBlock.tsx`. Open fences are never highlighted (`CodeBlock.tsx:43-49`).
- A sealed block keeps its signature and React key while the tail streams
  (`blocks.ts:209-255`, `MarkdownBlock.tsx:83-90`). A closing fence keeps the
  tail's key (`timeline-layout.ts:526-532`), so a code block can turn into a
  page without remounting its neighbours.
- **`visualize`** is the Codex-app reference line
  `U+E200 visualize U+E202 {json} U+E201` (`visualize.ts:12-13`). The daemon
  captures a file of at most 1 MiB from a visualizations root, wraps it with a
  meta CSP (CDN allowlist, `connect-src 'none'`), and hosts it under
  `/workass/artifacts/` (`cmd/workass/visualize.go:32,515-523`). It is served
  with `sandbox allow-scripts allow-forms allow-downloads; connect-src https:;
  frame-ancestors 'self'` (`internal/artifacthost/registry.go:1230-1235`).
  `VisualizeBlock` draws a card with a title bar and a **fixed 360/440px**
  `sandbox="allow-scripts"` frame (`VisualizeBlock.tsx:97-127`, `app.css:482-498`).
  This is Workass's equivalent of `html_render`.
- **Renderer CSP: none.** There is no meta in `desktop/renderer2/index.html`
  and no header from `internal/httpserve/server.go`. `srcdoc` frames are
  therefore allowed and inherit no policy, so the frame has to bring its own.
- **Electron window** (`desktop/shell/main.js:232-247,413-416`):
  `contextIsolation` and `sandbox` are on. `setWindowOpenHandler` sends any
  URL to `shell.openExternal`. A frame without `allow-popups` never reaches
  it. There is no `will-frame-navigate` guard yet.
- **Theme:** `store.ts:6886-6890` toggles `data-theme` on `<html>`, so frames
  can follow it with a single `MutationObserver`.
- **Redaction:** the daemon redacts transcript text at ingestion, and the
  renderer mirrors it (`redact.ts`). A page renders the same, already-redacted
  bytes as its code view.

## Mapping onto Workass

| t3code | Workass (recommended) |
|---|---|
| Trigger: `html_render` tool, completed | A closed ` ```html ` fence, or the message no longer running (the Mermaid rule) |
| Server attachment + signed URL | `srcdoc` built in the renderer. No storage, no URL, **no daemon or wire change** |
| Bootstrap injected server-side into `<head>` | The same job done client-side as a **prefix**. A CSP meta comes first, then the theme `<style>` and the bootstrap script, then `<body>`, then the agent's document. The parser folds the agent's `<html>/<head>/<body>` into ours, and a CSP meta outside `<head>` is ignored, so a page can only tighten the policy |
| Header `CSP: sandbox …`, network open | Meta CSP `default-src 'none'`, `connect-src 'none'`, `img/font/media-src data: blob:`, `form-action 'none'`, `base-uri 'none'`. **No network at all** |
| `allow-scripts allow-forms` | Same flags. `form-action 'none'` keeps form submits from leaving |
| Theme via `#t3-theme=` fragment + postMessage | The theme `<style>` is built into the srcdoc, plus a `wa-html:theme` postMessage on `data-theme` changes. Same variable names as t3code, mapped from `tokens.css`, so one page works in both apps |
| `ui/notifications/size-changed`, fit up to 20,000px | `wa-html:size`, same measurement; the cap is a variant (below) |
| Open-link gate | The same gate, plus a confirmation showing the full URL. The open uses the existing `target=_blank` → `setWindowOpenHandler` path |
| Server rejects `vh` units | Can't reject agent text client-side, so a **growth-loop guard** stops a page that keeps growing with its frame |
| Measured heights before load | No server measure. The frame stays invisible until its first size report, so the transcript never shows a 24px flash |
| (not needed) | **Electron backstops:** `will-frame-navigate` cancels any navigation of a render frame; `setWebRTCIPHandlingPolicy('disable_non_proxied_udp')` on the main window (voice uses only `getUserMedia`); sandboxed-frame process isolation verified |
| `html_render` tool | Later and optional: a `workass_render_html` MCP tool reusing `RegisterCapturedHTML` and the visualize reference (an additive wire field). Not in this plan |

Remote chats work unchanged: the fence is transcript text, so a chat hosted
on another machine renders on the viewing client with no artifact transport.

## Variants offered in the mock (one tab per real choice)

| Group | Options | Recommended |
|---|---|---|
| Marco | A · borderless, hover-only controls (t3code `html_render`) · B · borderless + slim always-visible icon row (t3code Mermaid) · C · inside the existing `.codebox` with a Página/Código switch | **B** |
| Altura | Completa (t3code, up to 20,000px) · Hasta una pantalla (fit up to ~1 screen, then fade + "Mostrar todo") | **Hasta una pantalla** |
| Vista inicial | Página primero (Mermaid default) · Código primero (today) · Lo marca el agente (` ```html preview `) | **Página primero** |
| Mientras se escribe | Código en vivo (today / Mermaid) · Una línea (size so far + "Ver código") · Página en vivo | **Una línea** |
| Ampliar | Diálogo con zoom (t3code) · Ventana completa (browser full-screen pattern) | **Diálogo** |

Why B and not A: t3code itself uses B for the one fence it renders. A hides
the code switch behind hover, and a borderless frame lets a page paint
something that looks like Workass's own UI (see
`shots/marco-a-sandbox-1280-dark.png`). With no network, that spoof can't send
anything anywhere, but B's row still marks the block as a rendered page.

## Open questions for the user

1. Should rendering stay fence-only for now, with the tool route
   (`workass_render_html`, the agent publishing deliberately) as a separate
   decision later?
2. **No network inside pages** (the recommendation) means CDN chart libraries
   won't load, so agents must inline their JS. The alternative is the
   `visualize` CDN allowlist (`cmd/workass/visualize.go:32`).
3. Links inside pages: confirm-then-open (in the mock), or inert?
4. Should the cap be about one screen (as in the mock), or t3code's full fit?

## Files

- `index.html`: interactive mock. It's self-contained, uses the real tokens,
  and runs real sandboxed frames: the hostile page's log is the browser's
  actual answer. URL parameters: `theme`, `marco`, `altura`, `vista`,
  `stream`, `expand`, `open=1`.
- `shots/`: headless Chromium screenshots. They cover `overview-{1280,2560}-{dark,light}`,
  each variant, the overlays, and `theme-live-switch-1280` (loaded dark,
  switched live).
- `PLAN.md`: the closed implementation plan.
