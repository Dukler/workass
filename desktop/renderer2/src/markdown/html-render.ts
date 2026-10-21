// HTML pages in the chat (approved mock desktop/docs/mocks/html-render,
// 2026-10-21): a finished ```html fence, or a page the agent published with
// workass_render_html, renders as its page in a sandboxed frame with no
// network. This module is the pure half: no React and no DOM globals at module
// scope, so node --test can check every security string exactly.
//
// Security model (PLAN.md "Security model", normative):
//   1. <iframe sandbox="allow-scripts allow-forms" srcdoc=…>, never
//      allow-same-origin, never a src/blob:/data: URL.
//   2. The CSP below is the first element of the frame's <head>. The agent's
//      document is appended after our <body>: the parser folds a second
//      <html>/<head>/<body> into ours and ignores a CSP <meta> outside <head>,
//      so a page can only ever tighten the policy.
//   3. The bootstrap runs before any page script (removes WebRTC, follows the
//      theme, reports the height, turns link clicks into a request).
//   4. The host accepts three messages, only from a frame it registered.
//   5. The Electron shell cancels every navigation of a render frame
//      (desktop/shell/render-frame-guard.js). Without that guard (an older
//      shell, a plain-browser client) pages render static: no scripts.

export const HTML_RENDER_CSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; worker-src 'none'; manifest-src 'none'; form-action 'none'; base-uri 'none'; object-src 'none'";
export const HTML_RENDER_SANDBOX = 'allow-scripts allow-forms';
export const HTML_RENDER_STATIC_SANDBOX = '';
// t3code's html_render input cap (apps/server/src/mcp/toolkits/html/tools.ts:19).
export const HTML_RENDER_MAX_SOURCE = 512_000;
// t3code fits a frame to its page up to this height (htmlRender.ts:14).
export const HTML_RENDER_MAX_FIT_HEIGHT = 20_000;
export const HTML_RENDER_MIN_HEIGHT = 16;
// A static page cannot report its height (it runs no script), so it gets one
// fixed frame and scrolls inside it. This is the degraded fallback only.
export const HTML_RENDER_STATIC_HEIGHT = 480;
// A page that never reports a size still shows, at this height, after 3s.
export const HTML_RENDER_FALLBACK_HEIGHT = 160;
export const HTML_RENDER_MAX_MOUNTED = 12;
export const HTML_RENDER_TOOL_NAME = 'workass_render_html';
export const HTML_RENDER_DEFAULT_TITLE = 'Página HTML';

export type HtmlRenderMode = 'scripted' | 'static';
export type HtmlScheme = 'dark' | 'light';

const FONT_SANS = '-apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, sans-serif';
const FONT_MONO = '"SF Mono", "SFMono-Regular", Menlo, Consolas, monospace';

// The page's base stylesheet, as in the mock. Its scrollbar is hidden because
// the frame always fits the page (t3code htmlRender.ts:375-377).
export const HTML_RENDER_BASE_CSS = 'html{background:var(--background);color:var(--foreground);font-family:var(--font-sans);font-size:13.5px;line-height:1.6;-webkit-font-smoothing:antialiased;-webkit-text-size-adjust:100%;scrollbar-width:none}html::-webkit-scrollbar{display:none}body{margin:0}code,kbd,pre,samp{font-family:var(--font-mono)}a{color:var(--link)}:where(:focus-visible){outline:2px solid var(--ring);outline-offset:-2px}';
// A static frame has a fixed height and scrolls, so it keeps its scrollbar.
const STATIC_BASE_CSS = HTML_RENDER_BASE_CSS
  .replace(';scrollbar-width:none', '')
  .replace('html::-webkit-scrollbar{display:none}', '');

/** True for an ```html (or ```htm) fence; the info string's first word decides. */
export function isHtmlFence(lang: string): boolean {
  const word = String(lang ?? '').trim().split(/\s+/)[0]?.toLowerCase() ?? '';
  return word === 'html' || word === 'htm';
}

// Runs INSIDE the frame, before any page script. It is serialized with
// toString(), so it must never reference anything outside its own body.
// Ported from the mock's bootstrap(), with two additions: the page gets no
// nested frames, and link clicks are caught on window (first in the capture
// order) rather than on document.
/* eslint-disable */
export function htmlFrameBootstrap(BASE: string): void {
  var w: any = window, d: any = document, P: any = w.parent, last = -1;
  // WebRTC is the one way out that no CSP governs (Chromium 140 ignores a CSP
  // `webrtc` directive, and the shell's disable_non_proxied_udp policy still
  // lets TURN over TCP connect): the page's own copy is removed here.
  ['RTCPeerConnection', 'webkitRTCPeerConnection', 'RTCDataChannel', 'RTCSessionDescription', 'RTCIceCandidate'].forEach(function (k) {
    try { delete w[k]; } catch (e) {}
    try { if (typeof w[k] !== 'undefined') Object.defineProperty(w, k, { value: undefined, writable: false, configurable: false }); } catch (e) {}
  });
  // A frame the page nests gets a fresh window, WebRTC included, and neither
  // frame-src nor the shell's navigation guard sees a nested srcdoc or
  // javascript: frame load. Chromium gives every nested sandboxed document its
  // own opaque origin, so the page cannot reach into one; it could only run
  // that frame's own script. So the page gets no nested frames at all: each is
  // removed as it is inserted, in the mutation microtask, before the nested
  // document's load (a later task) can start any script. Shadow roots are
  // watched too; declarative ones are disabled (the host also neutralizes
  // `shadowrootmode` in the source).
  var NEST = 'iframe,frame,object,embed,portal,fencedframe';
  function prune(node: any) {
    if (!node || node.nodeType !== 1) return;
    if (node.matches(NEST)) { node.remove(); return; }
    var inner = node.querySelectorAll(NEST);
    for (var i = 0; i < inner.length; i++) inner[i].remove();
  }
  var watch = new w.MutationObserver(function (records: any) {
    for (var i = 0; i < records.length; i++) for (var j = 0; j < records[i].addedNodes.length; j++) prune(records[i].addedNodes[j]);
  });
  watch.observe(d, { childList: true, subtree: true });
  var attach = w.Element.prototype.attachShadow;
  w.Element.prototype.attachShadow = function () {
    var root = attach.apply(this, arguments);
    watch.observe(root, { childList: true, subtree: true });
    return root;
  };
  [w.Element && w.Element.prototype, w.ShadowRoot && w.ShadowRoot.prototype, w.Document].forEach(function (o: any) {
    if (!o) return;
    try { delete o.setHTMLUnsafe; } catch (e) {}
    try { delete o.parseHTMLUnsafe; } catch (e) {}
  });
  ['write', 'writeln'].forEach(function (k) {
    var original = d[k];
    if (typeof original !== 'function') return;
    d[k] = function () {
      var text = Array.prototype.join.call(arguments, '').replace(/shadowroot/gi, 'data-wa-shadowroot');
      return original.call(d, text);
    };
  });

  var s = d.getElementById('wa-theme');
  function apply(m: any) {
    if (!s || !m.vars || typeof m.vars !== 'object') return;
    var c = ':root{color-scheme:' + (m.scheme === 'light' ? 'light' : 'dark') + ';';
    for (var k in m.vars) if (/^--[a-z0-9-]+$/.test(k)) c += k + ':' + String(m.vars[k]).replace(/[;{}<>]/g, '') + ';';
    s.textContent = c + '}' + BASE;
  }
  w.addEventListener('message', function (e: any) {
    var m = e.data;
    if (e.source === P && m && typeof m === 'object' && m.type === 'wa-html:theme') apply(m);
  });

  // t3code's measurement (htmlRender.ts:405): the page's own height, or the
  // scroll height when it overflows the frame.
  function size() {
    var r = d.documentElement;
    var v = Math.ceil(r.scrollHeight > r.clientHeight ? r.scrollHeight : r.getBoundingClientRect().height);
    if (v === last) return;
    last = v;
    P.postMessage({ type: 'wa-html:size', height: v }, '*');
  }
  if (w.ResizeObserver) {
    var o = new w.ResizeObserver(size);
    o.observe(d.documentElement);
    d.addEventListener('DOMContentLoaded', function () { if (d.body) o.observe(d.body); size(); });
  }
  w.addEventListener('load', size);

  // A link never navigates the frame. A trusted click on an absolute http(s)
  // link asks the host, which shows the full URL and opens it only if the
  // reader confirms. Relative links would resolve against the app's origin
  // (a srcdoc document inherits its parent's base URL), so they do nothing.
  w.addEventListener('click', function (e: any) {
    var t = e.target, a = t && t.closest ? t.closest('a[href],area[href]') : null, u;
    if (!a) return;
    e.preventDefault();
    if (!e.isTrusted) return;
    try { u = new URL(a.getAttribute('href')); } catch (x) { return; }
    if (u.protocol === 'https:' || u.protocol === 'http:') P.postMessage({ type: 'wa-html:link', url: u.href }, '*');
  }, true);
  w.addEventListener('keydown', function (e: any) { if (e.key === 'Escape') P.postMessage({ type: 'wa-html:escape' }, '*'); }, true);
}
/* eslint-enable */

// The page's theme, under t3code's variable names (htmlRender.ts:182-229) so
// one page works in both apps. Green stays the accent: --accent and --chart-1
// are the only variables that resolve to Workass's --acc.
export function htmlThemeVars(read: (token: string) => string): Record<string, string> {
  const v = (token: string) => String(read(token) ?? '').trim();
  return {
    '--background': v('--bg'), '--foreground': v('--ink'), '--muted': v('--field'), '--muted-foreground': v('--muted'),
    '--card': v('--panel'), '--card-foreground': v('--ink'), '--popover': v('--panel'), '--popover-foreground': v('--ink'),
    '--secondary': v('--field'), '--secondary-foreground': v('--ink2'), '--border': v('--line2'), '--input': v('--field'), '--ring': v('--link'),
    '--primary': v('--ink'), '--primary-foreground': v('--bg'), '--accent': v('--acc'), '--accent-foreground': v('--bg'),
    '--link': v('--link'), '--destructive': v('--del'), '--warning': v('--amber'), '--success': v('--add'), '--info': v('--link'),
    '--code-background': v('--field'), '--code-foreground': v('--ink2'),
    '--chart-1': v('--acc'), '--chart-2': v('--link'), '--chart-3': v('--amber'), '--chart-4': v('--del'), '--chart-5': v('--muted'),
    '--radius': '10px',
    '--font-sans': FONT_SANS,
    '--font-mono': FONT_MONO,
  };
}

function cssValue(value: string): string {
  return String(value ?? '').replace(/[;{}<>]/g, '');
}

export function htmlThemeRule(vars: Record<string, string>, scheme: HtmlScheme): string {
  let rule = `:root{color-scheme:${scheme === 'light' ? 'light' : 'dark'};`;
  for (const [name, value] of Object.entries(vars)) {
    if (/^--[a-z0-9-]+$/.test(name)) rule += `${name}:${cssValue(value)};`;
  }
  return `${rule}}`;
}

// Static mode has no script to stop a link, so a link must not be able to
// target its own frame: <base target="_blank"> sends every click to a popup the
// sandbox refuses, and a page's own target= attributes are neutralized first.
function neutralizeTargets(source: string): string {
  return source.replace(/([\s"'/])target(\s*=)/gi, '$1data-wa-target$2');
}

// A declarative shadow root (<template shadowrootmode>) would hide a nested
// frame from the bootstrap's watch, so the page never gets one.
function neutralizeShadowRoots(source: string): string {
  return source.replace(/([\s"'/])shadowroot/gi, '$1data-wa-shadowroot');
}

export function buildHtmlSrcdoc(
  source: string,
  options: { vars: Record<string, string>; scheme: HtmlScheme; mode: HtmlRenderMode },
): string {
  const scripted = options.mode === 'scripted';
  const base = scripted ? HTML_RENDER_BASE_CSS : STATIC_BASE_CSS;
  return '<!doctype html><html><head>'
    + `<meta http-equiv="Content-Security-Policy" content="${HTML_RENDER_CSP}">`
    + '<meta http-equiv="x-dns-prefetch-control" content="off">'
    + '<meta name="viewport" content="width=device-width,initial-scale=1">'
    + (scripted ? '' : '<base target="_blank">')
    + `<style id="wa-theme">${htmlThemeRule(options.vars, options.scheme)}${base}</style>`
    + (scripted ? `<script>(${htmlFrameBootstrap.toString()})(${JSON.stringify(base)});</script>` : '')
    + '</head><body>'
    + neutralizeShadowRoots(scripted ? source : neutralizeTargets(source));
}

export type HtmlFrameMessage =
  | { type: 'size'; height: number }
  | { type: 'link'; url: string }
  | { type: 'escape' };

export function clampFrameHeight(height: number): number {
  return Math.min(HTML_RENDER_MAX_FIT_HEIGHT, Math.max(HTML_RENDER_MIN_HEIGHT, Math.ceil(height)));
}

/** The only three shapes the host accepts from a frame; anything else is null. */
export function readFrameMessage(data: unknown): HtmlFrameMessage | null {
  if (!data || typeof data !== 'object' || Array.isArray(data)) return null;
  const record = data as Record<string, unknown>;
  switch (record.type) {
    case 'wa-html:size': {
      const height = record.height;
      if (typeof height !== 'number' || !Number.isFinite(height) || height <= 0) return null;
      return { type: 'size', height: clampFrameHeight(height) };
    }
    case 'wa-html:link': {
      const raw = record.url;
      if (typeof raw !== 'string' || !raw || raw.length > 8192 || !/^https?:\/\//i.test(raw)) return null;
      let url: URL;
      try { url = new URL(raw); } catch { return null; }
      if (url.protocol !== 'https:' && url.protocol !== 'http:') return null;
      return { type: 'link', url: url.href };
    }
    case 'wa-html:escape':
      return { type: 'escape' };
    default:
      return null;
  }
}

function decodeEntities(text: string): string {
  return text
    .replace(/&lt;/gi, '<').replace(/&gt;/gi, '>').replace(/&quot;/gi, '"')
    .replace(/&#39;|&apos;/gi, "'").replace(/&nbsp;/gi, ' ').replace(/&amp;/gi, '&');
}

/** The page's name: its <title>, else its first <h1>, else «Página HTML». */
export function htmlPageTitle(source: string): string {
  const head = String(source ?? '').slice(0, 64 * 1024);
  const match = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(head) ?? /<h1[^>]*>([\s\S]*?)<\/h1>/i.exec(head);
  const text = match ? decodeEntities(match[1].replace(/<[^>]*>/g, '')).replace(/\s+/g, ' ').trim() : '';
  return (text.length > 200 ? `${text.slice(0, 199)}…` : text) || HTML_RENDER_DEFAULT_TITLE;
}

/** «1,1 KB · 19 líneas»: the quiet size line while a page is being written. */
export function htmlStreamSize(source: string): string {
  const bytes = new TextEncoder().encode(source).byteLength;
  const kb = (bytes / 1024).toFixed(1).replace('.', ',');
  const lines = source ? source.split('\n').length : 0;
  return `${kb} KB · ${lines} ${lines === 1 ? 'línea' : 'líneas'}`;
}

export interface GrowthGuard {
  /** The height to apply for a reported height; freezes after a growth loop. */
  next(height: number): number;
  readonly frozen: boolean;
}

// t3code rejects viewport-height units at publish (HtmlRender.ts:71-97): a
// height that follows the frame makes a fit-to-content frame grow without end.
// A fence cannot be rejected, so a page that grows more than `limit` times
// within `windowMs` is frozen at min(height, freezeAt) until it remounts.
export function createGrowthGuard(options: { limit?: number; windowMs?: number; freezeAt?: number; now?: () => number } = {}): GrowthGuard {
  const limit = options.limit ?? 10;
  const windowMs = options.windowMs ?? 1000;
  const freezeAt = options.freezeAt ?? 4000;
  const now = options.now ?? (() => Date.now());
  let previous = 0;
  let frozenAt: number | null = null;
  const increases: number[] = [];
  return {
    next(height: number): number {
      if (frozenAt !== null) return frozenAt;
      if (height > previous && previous > 0) {
        const at = now();
        increases.push(at);
        while (increases.length && at - increases[0] > windowMs) increases.shift();
        if (increases.length > limit) {
          frozenAt = Math.min(height, freezeAt);
          return frozenAt;
        }
      }
      previous = height;
      return height;
    },
    get frozen() { return frozenAt !== null; },
  };
}

// ── Pages the agent published with workass_render_html ──────────────────────
// t3code renders a page from a completed html_render tool item whose output
// carries its reference (packages/shared/src/toolOutput.ts:180-187). Workass
// tools are a CLI the provider runs through its shell, so the item is the
// shell call that names the tool, and its stdout is the tool's JSON result.

export interface HtmlRenderReference {
  id: string;
  title: string;
  urlPath: string;
}

const TOOL_CALL = /\btools\s+call\s+workass_render_html\b/;
const ARTIFACT_URL_PATH = /^\/workass\/artifacts\/(?:@[A-Za-z0-9._~-]{1,200}\/)?[a-z0-9-]{1,200}\/$/;

// The JSON object that starts at `start`, or '' when it is cut short (the
// daemon keeps only a bounded head of a tool's output).
function balancedObject(text: string, start: number): string {
  let depth = 0;
  let quoted = false;
  for (let index = start; index < text.length; index++) {
    const char = text[index];
    if (quoted) {
      if (char === '\\') index++;
      else if (char === '"') quoted = false;
      continue;
    }
    if (char === '"') quoted = true;
    else if (char === '{') depth++;
    else if (char === '}' && --depth === 0) return text.slice(start, index + 1);
  }
  return '';
}

export function readHtmlRenderReference(value: unknown): HtmlRenderReference | null {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return null;
  const record = value as Record<string, unknown>;
  const id = typeof record.id === 'string' ? record.id : '';
  const urlPath = typeof record.urlPath === 'string' ? record.urlPath : '';
  const title = typeof record.title === 'string' ? record.title.replace(/[\u0000-\u001f]/g, ' ').trim() : '';
  if (!/^[a-z0-9-]{1,200}$/.test(id) || !ARTIFACT_URL_PATH.test(urlPath) || !urlPath.endsWith(`/${id}/`)) return null;
  return { id, urlPath, title: title.slice(0, 200) || HTML_RENDER_DEFAULT_TITLE };
}

export function htmlRenderFromToolCall(tool: {
  status?: string | null; command?: string | null; title?: string | null; input?: string | null; output?: string | null;
}): HtmlRenderReference | null {
  if (tool.status !== 'completed') return null;
  if (![tool.command, tool.title, tool.input].some((text) => typeof text === 'string' && TOOL_CALL.test(text))) return null;
  // The result is one JSON line with the reference first. Only the reference
  // object itself has to be whole: the message after it may be cut off.
  const output = String(tool.output ?? '');
  const match = /\{\s*"htmlRender"\s*:\s*\{/.exec(output);
  if (!match) return null;
  const json = balancedObject(output, match.index + match[0].length - 1);
  if (!json) return null;
  try {
    return readHtmlRenderReference(JSON.parse(json));
  } catch {
    return null;
  }
}

// The daemon stores a published page behind a CSP meta of its own, so the
// hosted URL has no network even when opened outside the chat. The marker ends
// that prefix; what follows is the agent's page, byte for byte.
export const HTML_RENDER_STORE_MARKER = '<!--workass-html-render-v1-->';

export function storedHtmlSource(text: string): string {
  const raw = String(text ?? '');
  if (!/^<!doctype html><meta http-equiv="Content-Security-Policy"/i.test(raw)) return raw;
  const at = raw.indexOf(HTML_RENDER_STORE_MARKER);
  return at >= 0 && at < 4096 ? raw.slice(at + HTML_RENDER_STORE_MARKER.length) : raw;
}

// A cheap stable identity for in-memory per-block state (the code/page choice,
// the last reported height) that must survive the transcript's windowing.
export function htmlBlockIdentity(source: string): string {
  let hash = 0x811c9dc5;
  for (let index = 0; index < source.length; index++) {
    hash ^= source.charCodeAt(index);
    hash = Math.imul(hash, 0x01000193);
  }
  return `${source.length}:${(hash >>> 0).toString(36)}`;
}
