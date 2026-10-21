import assert from 'node:assert/strict';
import fs from 'node:fs';
import { test } from 'node:test';
import vm from 'node:vm';
import {
  HTML_RENDER_BASE_CSS,
  HTML_RENDER_CSP,
  HTML_RENDER_MAX_FIT_HEIGHT,
  HTML_RENDER_SANDBOX,
  HTML_RENDER_STATIC_SANDBOX,
  HTML_RENDER_STORE_MARKER,
  buildHtmlSrcdoc,
  clampFrameHeight,
  createGrowthGuard,
  htmlBlockIdentity,
  htmlFrameBootstrap,
  htmlPageTitle,
  htmlRenderFromToolCall,
  htmlStreamSize,
  htmlThemeVars,
  isHtmlFence,
  readFrameMessage,
  storedHtmlSource,
} from '../src/markdown/html-render.ts';

const VARS = { '--background': '#0f0e0d', '--foreground': '#f2efe9' };

test('the frame CSP is exact: no network of any kind, inline script and style only', () => {
  assert.equal(HTML_RENDER_CSP, "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; worker-src 'none'; manifest-src 'none'; form-action 'none'; base-uri 'none'; object-src 'none'");
  assert.doesNotMatch(HTML_RENDER_CSP, /unsafe-eval|https?:|\*/);
});

test('the sandbox is exact and never grants same-origin, popups, modals, navigation or downloads', () => {
  assert.equal(HTML_RENDER_SANDBOX, 'allow-scripts allow-forms');
  assert.equal(HTML_RENDER_STATIC_SANDBOX, '');
  for (const forbidden of ['allow-same-origin', 'allow-popups', 'allow-modals', 'allow-top-navigation', 'allow-downloads', 'allow-pointer-lock', 'allow-presentation']) {
    assert.equal(HTML_RENDER_SANDBOX.includes(forbidden), false, forbidden);
  }
});

test('the CSP is the first element of the head, and a page cannot loosen it', () => {
  const page = '<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="default-src *"><base href="https://evil.example/"></head><body><p>hola</p></body></html>';
  const srcdoc = buildHtmlSrcdoc(page, { vars: VARS, scheme: 'dark', mode: 'scripted' });
  assert.ok(srcdoc.startsWith(`<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="${HTML_RENDER_CSP}">`));
  const body = srcdoc.indexOf('</head><body>');
  assert.ok(body > 0);
  // The page's own policy and base land after our <body>, where the parser
  // ignores a CSP meta and policies can only intersect anyway.
  assert.ok(srcdoc.indexOf('content="default-src *"') > body);
  assert.ok(srcdoc.indexOf('<base href="https://evil.example/">') > body);
  assert.ok(srcdoc.endsWith(page), 'the page follows verbatim');
  assert.match(srcdoc, /<meta http-equiv="x-dns-prefetch-control" content="off">/);
  // The bootstrap runs before any byte of the page.
  assert.ok(srcdoc.indexOf('<script>(') < body);
  assert.match(srcdoc, /<style id="wa-theme">:root\{color-scheme:dark;--background:#0f0e0d;--foreground:#f2efe9;\}/);
});

test('static mode runs no script and sends every link to a popup the sandbox refuses', () => {
  const page = '<a href="https://evil.example/" target="_self">x</a><a href="https://evil.example/"target=_top>y</a>';
  const srcdoc = buildHtmlSrcdoc(page, { vars: VARS, scheme: 'light', mode: 'static' });
  assert.doesNotMatch(srcdoc, /<script/);
  assert.match(srcdoc, /<base target="_blank"><style id="wa-theme">:root\{color-scheme:light;/);
  assert.doesNotMatch(srcdoc.slice(srcdoc.indexOf('<body>')), /\starget=|"target=/);
  assert.match(srcdoc, /data-wa-target="_self"/);
  // A static frame scrolls inside its fixed height, so it keeps its scrollbar.
  assert.doesNotMatch(srcdoc, /scrollbar-width:none/);
  assert.match(HTML_RENDER_BASE_CSS, /scrollbar-width:none/);
});

test('a theme value can never close the style element or inject a rule', () => {
  const srcdoc = buildHtmlSrcdoc('', { vars: { '--background': 'red;}</style><script>alert(1)</script>', 'bad name': 'x' }, scheme: 'dark', mode: 'scripted' });
  const style = srcdoc.slice(srcdoc.indexOf('<style id="wa-theme">'), srcdoc.indexOf('</style>') + 8);
  assert.doesNotMatch(style, /<script|alert\(1\)<|bad name/);
  assert.equal((style.match(/<\/style>/g) ?? []).length, 1);
});

function tokens(theme: 'dark' | 'light'): Record<string, string> {
  const css = fs.readFileSync(new URL('../src/styles/tokens.css', import.meta.url), 'utf8');
  const block = theme === 'dark'
    ? css.slice(css.indexOf(':root {'), css.indexOf(':root[data-theme="light"]'))
    : css.slice(css.indexOf(':root[data-theme="light"]'));
  return Object.fromEntries([...block.matchAll(/(--[a-z0-9-]+):\s*([^;]+);/g)].map((m) => [m[1], m[2].trim()]));
}

test('the page theme maps tokens.css onto t3code names, with green only as the accent', () => {
  for (const theme of ['dark', 'light'] as const) {
    const values = tokens(theme);
    const vars = htmlThemeVars((token) => values[token] ?? '');
    assert.equal(vars['--background'], values['--bg'], `${theme} background is the thread's`);
    assert.equal(vars['--foreground'], values['--ink']);
    assert.equal(vars['--muted-foreground'], values['--muted']);
    assert.equal(vars['--card'], values['--panel']);
    assert.equal(vars['--border'], values['--line2']);
    assert.equal(vars['--primary'], values['--ink']);
    assert.equal(vars['--primary-foreground'], values['--bg']);
    assert.equal(vars['--destructive'], values['--del']);
    assert.equal(vars['--warning'], values['--amber']);
    assert.equal(vars['--link'], values['--link']);
    assert.equal(vars['--radius'], '10px');
    for (const [name, value] of Object.entries(vars)) assert.ok(value, `${theme} ${name} resolves`);
    const green = Object.entries(vars).filter(([, value]) => value === values['--acc']).map(([name]) => name).sort();
    assert.deepEqual(green, ['--accent', '--chart-1'], theme);
  }
});

test('the host reads only three message shapes, with bounded heights and absolute http(s) links', () => {
  assert.deepEqual(readFrameMessage({ type: 'wa-html:size', height: 812.2 }), { type: 'size', height: 813 });
  assert.deepEqual(readFrameMessage({ type: 'wa-html:size', height: 9_999_999 }), { type: 'size', height: HTML_RENDER_MAX_FIT_HEIGHT });
  assert.deepEqual(readFrameMessage({ type: 'wa-html:size', height: 3 }), { type: 'size', height: 16 });
  assert.deepEqual(readFrameMessage({ type: 'wa-html:link', url: 'https://example.com/a b' }), { type: 'link', url: 'https://example.com/a%20b' });
  assert.deepEqual(readFrameMessage({ type: 'wa-html:escape' }), { type: 'escape' });
  for (const rejected of [
    null, undefined, 'wa-html:size', 42, [], { type: 'wa-html:theme', vars: {} }, { type: 'other' },
    { type: 'wa-html:size', height: 0 }, { type: 'wa-html:size', height: -5 }, { type: 'wa-html:size', height: Number.NaN },
    { type: 'wa-html:size', height: Infinity }, { type: 'wa-html:size', height: '200' },
    { type: 'wa-html:link', url: 'javascript:alert(1)' }, { type: 'wa-html:link', url: 'data:text/html,hi' },
    { type: 'wa-html:link', url: '/workass/artifacts/x/' }, { type: 'wa-html:link', url: 'relative/page.html' },
    { type: 'wa-html:link', url: 'file:///etc/passwd' }, { type: 'wa-html:link', url: 42 },
    { type: 'wa-html:link', url: `https://example.com/${'x'.repeat(9000)}` },
  ]) {
    assert.equal(readFrameMessage(rejected), null, JSON.stringify(rejected)?.slice(0, 80));
  }
  assert.equal(clampFrameHeight(20_000.4), 20_000);
});

test('the growth guard trips at the 11th increase within one second, never on a slow accordion', () => {
  let clock = 0;
  const loop = createGrowthGuard({ now: () => clock });
  let applied = loop.next(400);
  for (let step = 1; step <= 10; step++) {
    clock += 50;
    applied = loop.next(400 + step * 30);
    assert.equal(loop.frozen, false, `increase ${step}`);
  }
  clock += 50;
  applied = loop.next(5000);
  assert.equal(loop.frozen, true);
  assert.equal(applied, 4000, 'frozen at min(height, 4000)');
  assert.equal(loop.next(9000), 4000, 'stays frozen until the block remounts');

  clock = 0;
  const accordion = createGrowthGuard({ now: () => clock });
  for (let step = 0; step < 40; step++) {
    clock += 400;
    accordion.next(step % 2 ? 600 : 300);
  }
  assert.equal(accordion.frozen, false);
  assert.equal(accordion.next(900), 900);
});

test('the page title comes from <title>, else <h1>, else a plain default', () => {
  assert.equal(htmlPageTitle('<title> Latencia del daemon · 24 h </title><h1>x</h1>'), 'Latencia del daemon · 24 h');
  assert.equal(htmlPageTitle('<div><h1 class="t">Cobertura <b>por</b> paquete &amp; más</h1></div>'), 'Cobertura por paquete & más');
  assert.equal(htmlPageTitle('<p>sin título</p>'), 'Página HTML');
});

test('only html and htm fences render, case-insensitively', () => {
  for (const lang of ['html', 'HTML', 'Html', 'htm', 'html preview', '  html  ']) assert.equal(isHtmlFence(lang), true, lang);
  for (const lang of ['htmlbars', 'xhtml', 'xml', 'svg', 'jsx', '', 'text html']) assert.equal(isHtmlFence(lang), false, lang);
});

test('the writing line reports size so far and line count', () => {
  assert.equal(htmlStreamSize('a'.repeat(1126) + '\n'.repeat(18)), '1,1 KB · 19 líneas');
  assert.equal(htmlStreamSize('<p>'), '0,0 KB · 1 línea');
  assert.notEqual(htmlBlockIdentity('<p>a</p>'), htmlBlockIdentity('<p>b</p>'));
  assert.equal(htmlBlockIdentity('<p>a</p>'), htmlBlockIdentity('<p>a</p>'));
});

const CALL = '"$WORKASS_TOOLS_COMMAND" tools call workass_render_html --input /tmp/page.json';
const RESULT = '{"htmlRender":{"id":"latencia-del-daemon-3f9a2b1c4d5e","title":"Latencia del daemon","urlPath":"/workass/artifacts/@mac-1/latencia-del-daemon-3f9a2b1c4d5e/"},"message":"Shown to the reader."}\n';

test('a completed workass_render_html call yields its page reference', () => {
  assert.deepEqual(htmlRenderFromToolCall({ status: 'completed', command: CALL, output: RESULT }), {
    id: 'latencia-del-daemon-3f9a2b1c4d5e', title: 'Latencia del daemon',
    urlPath: '/workass/artifacts/@mac-1/latencia-del-daemon-3f9a2b1c4d5e/',
  });
  // Codex titles the row with the command line itself; the head of the output
  // is all the daemon keeps, so a cut-off message still yields the reference.
  assert.ok(htmlRenderFromToolCall({ status: 'completed', title: `/bin/zsh -lc '${CALL}'`, output: `exit 0\n${RESULT.slice(0, RESULT.indexOf('Shown') + 3)}…` }));
});

test('anything else stays an ordinary tool row', () => {
  assert.equal(htmlRenderFromToolCall({ status: 'in_progress', command: CALL, output: RESULT }), null);
  assert.equal(htmlRenderFromToolCall({ status: 'failed', command: CALL, output: RESULT }), null);
  assert.equal(htmlRenderFromToolCall({ status: 'completed', command: 'cat result.json', output: RESULT }), null, 'a command that only prints the shape');
  assert.equal(htmlRenderFromToolCall({ status: 'completed', command: CALL, output: '{"error":"page uses 100vh"}' }), null);
  assert.equal(htmlRenderFromToolCall({ status: 'completed', command: CALL, output: RESULT.slice(0, 60) }), null, 'reference cut short');
  for (const urlPath of ['https://evil.example/x/', '/workass/artifacts/../state/', '/other/latencia-del-daemon-3f9a2b1c4d5e/', '/workass/artifacts/another-id/']) {
    const output = `{"htmlRender":{"id":"latencia-del-daemon-3f9a2b1c4d5e","title":"x","urlPath":${JSON.stringify(urlPath)}}}`;
    assert.equal(htmlRenderFromToolCall({ status: 'completed', command: CALL, output }), null, urlPath);
  }
});

test('the stored page is the agent source once the daemon CSP prefix is removed', () => {
  const page = '<!doctype html><title>x</title><p>hola</p>';
  const stored = `<!doctype html><meta http-equiv="Content-Security-Policy" content="${HTML_RENDER_CSP}">${HTML_RENDER_STORE_MARKER}${page}`;
  assert.equal(storedHtmlSource(stored), page);
  assert.equal(storedHtmlSource(page), page);
  assert.equal(storedHtmlSource(`<!doctype html><meta http-equiv="Content-Security-Policy" content="x">${page}`), `<!doctype html><meta http-equiv="Content-Security-Policy" content="x">${page}`);
});

// ── The bootstrap, run in a bare context ─────────────────────────────────────

function bootstrapHarness() {
  const posted: unknown[] = [];
  const listeners: Record<string, Array<(event: unknown) => void>> = {};
  const parent = { postMessage: (message: unknown) => { posted.push(message); } };
  const style = { textContent: '' };
  const frameWindow = { RTCPeerConnection: function RTC() {}, webkitRTCPeerConnection: function RTC() {} };
  class FakeIframe {}
  Object.defineProperty(FakeIframe.prototype, 'contentWindow', { configurable: true, enumerable: true, get() { return frameWindow; } });
  Object.defineProperty(FakeIframe.prototype, 'contentDocument', { configurable: true, enumerable: true, get() { return { defaultView: frameWindow }; } });
  const documentElement = { scrollHeight: 640, clientHeight: 150, getBoundingClientRect: () => ({ height: 120 }) };
  const win: Record<string, unknown> = {
    parent,
    RTCPeerConnection: function RTC() {},
    webkitRTCPeerConnection: function RTC() {},
    RTCDataChannel: function RTC() {},
    HTMLIFrameElement: FakeIframe,
    frames: { length: 0 },
    addEventListener: (type: string, listener: (event: unknown) => void) => { (listeners[type] ??= []).push(listener); },
  };
  const doc = {
    documentElement,
    getElementById: (id: string) => (id === 'wa-theme' ? style : null),
    addEventListener: (type: string, listener: (event: unknown) => void) => { (listeners[`doc:${type}`] ??= []).push(listener); },
  };
  win.document = doc;
  const context = vm.createContext({ window: win, document: doc, URL });
  vm.runInContext(`(${htmlFrameBootstrap.toString()})(${JSON.stringify('BASE')});`, context);
  const fire = (type: string, event: unknown) => { for (const listener of listeners[type] ?? []) listener(event); };
  // Messages are built in the bootstrap's own realm; compare them as data.
  const messages = () => JSON.parse(JSON.stringify(posted)) as unknown[];
  return { posted, messages, win, style, parent, fire, frameWindow, FakeIframe, documentElement };
}

test('the bootstrap is self-contained and removes WebRTC, here and in frames the page nests', () => {
  const { win, frameWindow, FakeIframe } = bootstrapHarness();
  assert.equal(win.RTCPeerConnection, undefined);
  assert.equal(win.webkitRTCPeerConnection, undefined);
  assert.equal(win.RTCDataChannel, undefined);
  const nested = Object.create(FakeIframe.prototype) as { contentWindow: Record<string, unknown> };
  assert.equal(nested.contentWindow, frameWindow);
  assert.equal(frameWindow.RTCPeerConnection, undefined, 'a nested frame is stripped as soon as the page reaches it');
});

test('the bootstrap reports the page height and follows the theme only from its parent', () => {
  const { messages, fire, style, parent, documentElement } = bootstrapHarness();
  fire('load', {});
  assert.deepEqual(messages(), [{ type: 'wa-html:size', height: 640 }]);
  documentElement.scrollHeight = 150;
  fire('load', {});
  assert.deepEqual(messages().at(-1), { type: 'wa-html:size', height: 120 }, 'a shorter page shrinks its frame');
  fire('message', { source: {}, data: { type: 'wa-html:theme', scheme: 'light', vars: { '--background': '#fff' } } });
  assert.equal(style.textContent, '', 'a message from anywhere but the parent is ignored');
  fire('message', { source: parent, data: { type: 'wa-html:theme', scheme: 'light', vars: { '--background': '#fff}</style>', 'not a var': 'x' } } });
  assert.equal(style.textContent, ':root{color-scheme:light;--background:#fff/style;}BASE');
});

test('the bootstrap turns only trusted clicks on absolute http(s) links into a request', () => {
  const { posted, messages, fire } = bootstrapHarness();
  const click = (href: string, isTrusted = true) => {
    let prevented = false;
    fire('click', { isTrusted, target: { closest: () => ({ getAttribute: () => href }) }, preventDefault: () => { prevented = true; } });
    return prevented;
  };
  assert.equal(click('https://evil.example/login'), true);
  assert.deepEqual(messages().at(-1), { type: 'wa-html:link', url: 'https://evil.example/login' });
  const before = posted.length;
  assert.equal(click('https://evil.example/untrusted', false), true, 'a scripted click is stopped and not forwarded');
  assert.equal(click('relative.html'), true);
  assert.equal(click('#section'), true);
  assert.equal(click('javascript:alert(1)'), true);
  assert.equal(posted.length, before);
  fire('keydown', { key: 'Escape' });
  assert.deepEqual(messages().at(-1), { type: 'wa-html:escape' });
});
