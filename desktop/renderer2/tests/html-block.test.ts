import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

type Signed = { block: { kind: string; lang?: string; closed?: boolean; raw?: string }; sig: string };
let server: ViteDevServer;
let parseBlocks: (text: string) => Signed[];
let parseBlocksGrowing: (key: string, text: string) => Signed[];
let MarkdownBlock: React.NamedExoticComponent<Record<string, unknown>> & { compare?: (a: Record<string, unknown>, b: Record<string, unknown>) => boolean };
let ChatFindQueryContext: React.Context<string>;
let HtmlLinkAskView: React.ComponentType<{ url: string; onCancel: () => void; onOpen: () => void }>;
let openConfirmedLink: (url: string, opener: (url: string, target: string, features: string) => unknown) => void;
let frames: {
  registerHtmlFrame(handlers: Record<string, unknown>): () => void;
  claimLinkAsk(close: () => void): () => void;
  pushEscapeLayer(close: () => void): () => void;
};
let render: { HTML_RENDER_SANDBOX: string; HTML_RENDER_CSP: string; HTML_RENDER_MAX_SOURCE: number };

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ parseBlocks, parseBlocksGrowing } = await server.ssrLoadModule('/src/markdown/blocks.ts') as { parseBlocks: typeof parseBlocks; parseBlocksGrowing: typeof parseBlocksGrowing });
  ({ MarkdownBlock } = await server.ssrLoadModule('/src/markdown/MarkdownBlock.tsx') as { MarkdownBlock: typeof MarkdownBlock });
  ({ ChatFindQueryContext } = await server.ssrLoadModule('/src/markdown/find-context.ts') as { ChatFindQueryContext: typeof ChatFindQueryContext });
  ({ HtmlLinkAskView, openConfirmedLink } = await server.ssrLoadModule('/src/markdown/HtmlBlock.tsx') as { HtmlLinkAskView: typeof HtmlLinkAskView; openConfirmedLink: typeof openConfirmedLink });
  frames = await server.ssrLoadModule('/src/markdown/html-frames.ts') as typeof frames;
  render = await server.ssrLoadModule('/src/markdown/html-render.ts') as typeof render;
});

after(async () => { await server.close(); });

// The test process is shared with other renderer suites: every global a test
// installs is put back before it ends.
function withGlobals<T>(values: Record<string, unknown>, run: () => T): T {
  const saved = Object.keys(values).map((key) => [key, Object.getOwnPropertyDescriptor(globalThis, key)] as const);
  for (const [key, value] of Object.entries(values)) Object.defineProperty(globalThis, key, { value, configurable: true, writable: true });
  try {
    return run();
  } finally {
    for (const [key, descriptor] of saved) {
      if (descriptor) Object.defineProperty(globalThis, key, descriptor);
      else delete (globalThis as Record<string, unknown>)[key];
    }
  }
}

const guardedShell = () => ({ window: { workassRenderFrames: { supported: true, navigationGuard: true } } });
const PAGE = '<!doctype html><title>Cobertura</title><style>h1{margin:0}</style><h1>Cobertura por paquete</h1><p>Total 76 %</p>';
const fence = (body: string, close = true) => `\`\`\`html\n${body}${close ? '\n```' : ''}`;
const html = (sb: Signed, props: Record<string, unknown> = {}) => renderToStaticMarkup(React.createElement(MarkdownBlock, { sb, ...props }));
const decode = (text: string) => text.replace(/&quot;/g, '"').replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&#x27;/g, "'").replace(/&amp;/g, '&');

test('a closed fence renders exactly one sandboxed srcdoc frame, with no src, under the icon row', () => {
  const [block] = parseBlocks(fence(PAGE));
  const out = withGlobals(guardedShell(), () => html(block));
  assert.equal((out.match(/<iframe/g) ?? []).length, 1);
  assert.match(out, new RegExp(`sandbox="${render.HTML_RENDER_SANDBOX}"`));
  assert.doesNotMatch(out, /<iframe[^>]*\ssrc=/);
  assert.match(out, /referrerPolicy="no-referrer"|referrerpolicy="no-referrer"/i);
  const srcdoc = decode(/srcDoc="([^"]*)"|srcdoc="([^"]*)"/i.exec(out)?.slice(1).find(Boolean) ?? '');
  assert.ok(srcdoc.startsWith(`<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="${render.HTML_RENDER_CSP}">`));
  assert.ok(srcdoc.endsWith(PAGE));
  assert.match(out, /title="Cobertura"/, 'the frame is named after the page');
  assert.match(out, /class="hb hb-b"/);
  for (const label of ['Ver el código', 'Copiar código', 'Ampliar']) assert.match(out, new RegExp(`class="hbtn"[^>]*aria-label="${label}"`));
  assert.doesNotMatch(out, /codebox/, 'the page shows first');
});

test('without the shell navigation guard the page renders static: no scripts at all', () => {
  const [block] = parseBlocks(fence(PAGE));
  const out = html(block);
  assert.match(out, /<iframe[^>]*sandbox=""/);
  assert.match(out, /data-static=""/);
  const srcdoc = decode(/srcDoc="([^"]*)"|srcdoc="([^"]*)"/i.exec(out)?.slice(1).find(Boolean) ?? '');
  assert.doesNotMatch(srcdoc, /<script/i);
  assert.match(srcdoc, /<base target="_blank">/);
  assert.match(out, /style="height:480px"/);
});

test('an open fence while the reply streams is one quiet line with its size so far, never a frame', () => {
  const body = '<!doctype html>\n<h1>Cobertura</h1>\n<div class="row">';
  const [block] = parseBlocks(fence(body, false));
  const out = withGlobals(guardedShell(), () => html(block, { streaming: true }));
  assert.doesNotMatch(out, /<iframe/);
  assert.match(out, /class="hb-line"/);
  assert.match(out, />Escribiendo página</);
  assert.match(out, /class="m">0,1 KB · 3 líneas</);
  assert.match(out, />Ver código</);
  assert.doesNotMatch(out, /codebox/, 'the code stays behind its peek');
});

test('an open fence renders its page once the reply has stopped', () => {
  const [block] = parseBlocks(fence(PAGE, false));
  const out = withGlobals(guardedShell(), () => html(block, { streaming: false }));
  assert.equal((out.match(/<iframe/g) ?? []).length, 1);
});

test('a source over the size limit stays code', () => {
  const body = `<p>${'x'.repeat(render.HTML_RENDER_MAX_SOURCE)}</p>`;
  const [block] = parseBlocks(fence(body));
  const out = withGlobals(guardedShell(), () => html(block));
  assert.doesNotMatch(out, /<iframe/);
  assert.match(out, /class="codebox"[^>]*data-language="html"/);
});

test('an open find whose query is in the source shows the source, without the page switch', () => {
  const [block] = parseBlocks(fence(PAGE));
  const withQuery = (query: string) => withGlobals(guardedShell(), () => renderToStaticMarkup(
    React.createElement(ChatFindQueryContext.Provider, { value: query }, React.createElement(MarkdownBlock, { sb: block })),
  ));
  const found = withQuery('por paquete');
  assert.doesNotMatch(found, /<iframe/);
  assert.match(found, /class="codebox hb-code"/);
  assert.match(found, /Cobertura por paquete/);
  assert.doesNotMatch(found, />Página</);
  assert.match(withQuery('no está en la página'), /<iframe/);
  assert.match(withQuery(''), /<iframe/);
});

test('ordinary code fences are unchanged', () => {
  for (const source of ['```js\nconst a = 1;\n```', '```go\nfunc main() {}\n```', '```htmlbars\n{{x}}\n```']) {
    const [block] = parseBlocks(source);
    const out = withGlobals(guardedShell(), () => html(block));
    assert.match(out, /^<div class="codebox" data-language=/);
    assert.doesNotMatch(out, /<iframe|hb-/);
  }
});

test('a sealed html block above the streaming tail never re-renders while 200 chunks flow', () => {
  const head = `Antes.\n\n${fence(PAGE)}\n\n`;
  let text = head;
  const first = parseBlocksGrowing('html-isolation', text + 'cola');
  const sealed = first.find((sb) => sb.block.kind === 'code')!;
  assert.ok(sealed && sealed.block.closed);
  const compare = MarkdownBlock.compare!;
  for (let chunk = 0; chunk < 200; chunk++) {
    text += `palabra${chunk} `;
    const next = parseBlocksGrowing('html-isolation', text).find((sb) => sb.block.kind === 'code')!;
    assert.equal(compare({ sb: sealed, streaming: true }, { sb: next, streaming: true }), true, `chunk ${chunk}`);
  }
});

test('the host answers only a registered frame, and opens a link only from a focused frame the reader just used', () => {
  const listeners: Array<(event: unknown) => void> = [];
  const iframe = { contentWindow: { id: 'page' }, style: {} };
  const stranger = { id: 'stranger' };
  const seen: string[] = [];
  const fakeDocument = { activeElement: iframe as unknown, documentElement: { getAttribute: () => null } };
  let activation = true;
  withGlobals({
    window: { addEventListener: (type: string, listener: (event: unknown) => void) => { if (type === 'message') listeners.push(listener); } },
    document: fakeDocument,
    navigator: { userActivation: { get isActive() { return activation; } } },
  }, () => {
    const sizes: number[] = [];
    const off = frames.registerHtmlFrame({ iframe, scripted: true, onSize: (h: number) => sizes.push(h), onLink: (url: string) => seen.push(url) });
    const send = (source: unknown, data: unknown) => { for (const listener of listeners) listener({ source, data }); };
    send(iframe.contentWindow, { type: 'wa-html:size', height: 640 });
    send(stranger, { type: 'wa-html:size', height: 999 });
    assert.deepEqual(sizes, [640]);

    send(iframe.contentWindow, { type: 'wa-html:link', url: 'https://evil.example/login' });
    assert.deepEqual(seen, ['https://evil.example/login']);
    send(stranger, { type: 'wa-html:link', url: 'https://evil.example/other' });
    activation = false;
    send(iframe.contentWindow, { type: 'wa-html:link', url: 'https://evil.example/no-gesture' });
    activation = true;
    fakeDocument.activeElement = null;
    send(iframe.contentWindow, { type: 'wa-html:link', url: 'https://evil.example/not-focused' });
    fakeDocument.activeElement = iframe;
    send(iframe.contentWindow, { type: 'wa-html:link', url: 'javascript:alert(1)' });
    assert.deepEqual(seen, ['https://evil.example/login']);

    off();
    send(iframe.contentWindow, { type: 'wa-html:size', height: 10 });
    assert.deepEqual(sizes, [640], 'an unregistered frame is never answered');
  });
});

test('the link confirmation shows the whole URL and only «Abrir» opens it, without an opener', () => {
  const url = `https://workass.example.com.evil.example/very/long/path/${'segment/'.repeat(20)}?account=1`;
  const out = renderToStaticMarkup(React.createElement(HtmlLinkAskView, { url, onCancel: () => {}, onOpen: () => {} }));
  assert.match(out, /role="alertdialog"/);
  assert.match(out, />¿Abrir este enlace en el navegador\?</);
  assert.ok(decode(out).includes(`<span class="u">${url}</span>`), 'the URL is shown in full');
  assert.match(out, />Cancelar<\/button><button type="button" class="go">Abrir<\/button>/);
  const opened: unknown[][] = [];
  openConfirmedLink(url, (...args) => { opened.push(args); return null; });
  assert.deepEqual(opened, [[url, '_blank', 'noopener,noreferrer']]);
});

test('one link confirmation at a time, and Esc closes the topmost page overlay only', () => {
  const closed: string[] = [];
  const releaseA = frames.claimLinkAsk(() => closed.push('a'));
  frames.claimLinkAsk(() => closed.push('b'));
  assert.deepEqual(closed, ['a'], 'a second confirmation replaces the first');
  releaseA();

  const keyListeners: Array<(event: unknown) => void> = [];
  withGlobals({ window: { addEventListener: (type: string, listener: (event: unknown) => void, capture: boolean) => { if (type === 'keydown' && capture) keyListeners.push(listener); } } }, () => {
    const order: string[] = [];
    const popDialog = frames.pushEscapeLayer(() => order.push('dialog'));
    const popAsk = frames.pushEscapeLayer(() => order.push('ask'));
    let stopped = 0;
    const press = (key: string) => { for (const listener of keyListeners) listener({ key, preventDefault() {}, stopImmediatePropagation() { stopped++; } }); };
    press('Escape');
    popAsk();
    press('Escape');
    popDialog();
    press('Escape');
    press('Enter');
    assert.deepEqual(order, ['ask', 'dialog']);
    assert.equal(stopped, 2, 'Esc never reaches the app (which would cancel a running turn) while an overlay is open');
  });
});
