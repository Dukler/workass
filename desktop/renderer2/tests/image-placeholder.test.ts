import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { MessageImage, ToolEvent } from '../src/store/types.ts';

// Approved mock image-placeholder, variant C (2026-10-21): an image's frame
// has its final size from the first paint (the daemon's width/height), so the
// row below never moves when the bytes arrive. Without a size (an older
// daemon) each surface uses one default and corrects once on load.
type Surface = 'user' | 'inline' | 'gallery' | 'tool';
type FrameState = 'pending' | 'loaded' | 'failed';
interface ImageModule {
  imageFrameStyle: (image: MessageImage, surface: Surface, natural?: readonly [number, number] | null) => Record<string, number>;
  reservedImageState: (readFailed: boolean, src: string, readySrc: string, brokenSrc: string) => FrameState;
  IMAGE_FRAME_FALLBACK: Record<Surface, readonly [number, number]>;
  ImagePlaceholder: React.ComponentType<{ state: FrameState; alias: string }>;
  MessageImageButton: React.ComponentType<Record<string, unknown>>;
}

let server: ViteDevServer;
let images: ImageModule;
let UserPill: React.ComponentType<{ tabId?: string; text: string; images?: MessageImage[] }>;
let ToolGroup: React.ComponentType<{ tabId: string; tools: ToolEvent[] }>;
let AssistantMessage: React.ComponentType<Record<string, unknown>>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  images = await server.ssrLoadModule('/src/components/MessageImage.tsx') as ImageModule;
  ({ UserPill, ToolGroup } = await server.ssrLoadModule('/src/components/messages.tsx') as {
    UserPill: typeof UserPill; ToolGroup: typeof ToolGroup;
  });
  ({ AssistantMessage } = await server.ssrLoadModule('/src/components/AssistantMessage.tsx') as { AssistantMessage: typeof AssistantMessage });
});
after(async () => { await server.close(); });

let refs = 0;
const deferred = (size?: [number, number]): MessageImage => ({
  mimeType: 'image/png', data: '', name: 'shot', deferredImageRef: `images/placeholder-${refs++}`,
  ...(size ? { width: size[0], height: size[1] } : {}),
});
const inline = (size?: [number, number]): MessageImage => ({
  mimeType: 'image/png', data: 'cG5n', name: 'shot', ...(size ? { width: size[0], height: size[1] } : {}),
});

test('the frame style is the wire size, else the surface default; a measured size wins', () => {
  const { imageFrameStyle, IMAGE_FRAME_FALLBACK } = images;
  assert.deepEqual(IMAGE_FRAME_FALLBACK, { user: [640, 480], inline: [1600, 900], gallery: [1200, 900], tool: [1200, 900] });
  for (const surface of ['user', 'inline', 'gallery', 'tool'] as const) {
    assert.deepEqual(imageFrameStyle(deferred([1440, 900]), surface), { '--iw': 1440, '--ih': 900 });
    const [width, height] = IMAGE_FRAME_FALLBACK[surface];
    assert.deepEqual(imageFrameStyle(deferred(), surface), { '--iw': width, '--ih': height });
  }
  // Unusable sizes fall back instead of producing a zero or NaN frame.
  for (const bad of [[0, 900], [1440, -1], [Number.NaN, 900], [Number.POSITIVE_INFINITY, 2]] as Array<[number, number]>) {
    assert.deepEqual(imageFrameStyle({ ...deferred(), width: bad[0], height: bad[1] }, 'tool'), { '--iw': 1200, '--ih': 900 });
  }
  assert.deepEqual(imageFrameStyle({ ...deferred(), width: '1440' as unknown as number, height: 900 }, 'user'), { '--iw': 640, '--ih': 480 });
  // After load the natural size is final (an older daemon corrects once).
  assert.deepEqual(imageFrameStyle(deferred(), 'inline', [750, 1334]), { '--iw': 750, '--ih': 1334 });
  assert.deepEqual(imageFrameStyle(deferred([1440, 900]), 'inline', [1440, 900]), { '--iw': 1440, '--ih': 900 });
});

test('frame state: pending until the current source loads, failed on a failed read or decode', () => {
  const { reservedImageState } = images;
  assert.equal(reservedImageState(false, '', '', ''), 'pending');
  assert.equal(reservedImageState(false, 'data:a', '', ''), 'pending');
  assert.equal(reservedImageState(false, 'data:a', 'data:a', ''), 'loaded');
  assert.equal(reservedImageState(false, 'data:b', 'data:a', ''), 'pending', 'a new source waits for its own load');
  assert.equal(reservedImageState(true, '', '', ''), 'failed', 'the read by reference failed');
  assert.equal(reservedImageState(false, 'data:a', '', 'data:a'), 'failed', 'the browser could not decode the bytes');
  assert.equal(reservedImageState(false, 'data:b', '', 'data:a'), 'pending');
  // A loaded display rendition switching to a larger one (or the full image)
  // keeps drawing its pixels: the frame never falls back to the placeholder.
  const full = `/workass/chat-image/tab/${'ab'.repeat(32)}?k=d.k`;
  assert.equal(reservedImageState(false, full, `${full}&w=320`, ''), 'loaded');
  assert.equal(reservedImageState(false, `${full}&w=640`, `${full}&w=320`, ''), 'loaded');
  assert.equal(reservedImageState(false, full.replace('tab/', 'other/'), `${full}&w=320`, ''), 'pending', 'another image waits for its own load');
});

function frame(html: string, className: string): { state: string; style: string } {
  const match = new RegExp(`class="${className}" data-state="([a-z]+)" style="([^"]+)"`).exec(html);
  assert.ok(match, `no ${className} frame in ${html}`);
  return { state: match[1], style: match[2] };
}

function renderSurface(surface: Surface, image: MessageImage): string {
  if (surface === 'user') return renderToStaticMarkup(React.createElement(UserPill, { tabId: 'tab', text: 'look', images: [image] }));
  if (surface === 'tool') {
    const tool: ToolEvent = {
      key: `tool-${refs++}`, at: 0, kind: 'tool', id: 'tool', toolKind: 'read', title: 'Capture', status: 'completed',
      command: null, terminalId: null, input: null, output: null, location: null, images: [image],
    };
    return renderToStaticMarkup(React.createElement(ToolGroup, { tabId: 'tab', tools: [tool] }));
  }
  if (surface === 'gallery') {
    const msg = { id: `gallery-${refs++}`, role: 'assistant', content: 'Here.', status: 'done', at: null, events: [], images: [image] };
    return renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'tab', msg, profile: 'dev' }));
  }
  const msg = {
    id: `inline-${refs++}`, role: 'assistant', content: '![shot](shot.png)', status: 'done', at: null, events: [],
    images: [{ ...image, source: 'shot.png' }],
  };
  return renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'tab', msg, profile: 'dev' }));
}

const FRAME_CLASS: Record<Surface, string> = {
  user: 'ri ri-user', inline: 'assistant-inline-image ri ri-inline', gallery: 'assistant-inline-image ri ri-gallery', tool: 'tool-image ri ri-tool',
};

test('every surface paints the placeholder and the loaded image in the same frame, sized or not', () => {
  for (const surface of ['user', 'inline', 'gallery', 'tool'] as const) {
    for (const size of [[1440, 900], undefined] as Array<[number, number] | undefined>) {
      const pendingHtml = renderSurface(surface, deferred(size));
      const loadedHtml = renderSurface(surface, inline(size));
      const pending = frame(pendingHtml, FRAME_CLASS[surface]);
      const loaded = frame(loadedHtml, FRAME_CLASS[surface]);
      const label = `${surface} ${size ? 'sized' : 'unsized'}`;
      assert.equal(pending.state, 'pending', label);
      assert.equal(loaded.state, 'loaded', label);
      assert.equal(pending.style, loaded.style, `${label}: the swap must not change the frame`);
      const [width, height] = size ?? images.IMAGE_FRAME_FALLBACK[surface];
      assert.equal(pending.style, `--iw:${width};--ih:${height}`, label);
      // Pending: the placeholder with today's copy, no bytes.
      assert.match(pendingHtml, /class="ri-media"><span class="ri-ph [a-z-]+" role="img" aria-label="Cargando imagen">/, label);
      assert.match(pendingHtml, /<span class="cap">Cargando imagen…<\/span>/, label);
      assert.doesNotMatch(pendingHtml, /<img|data:image\/png;base64/, label);
      // Loaded: the image fills the same media box; the placeholder is hidden.
      assert.match(loadedHtml, /class="ri-media"><img [^>]*src="data:image\/png;base64,cG5n"/, label);
      assert.match(loadedHtml, /class="ri-ph [a-z-]+" aria-hidden="true"/, label);
    }
  }
});

test('the older pending classes stay on the placeholder as aliases', () => {
  assert.match(renderSurface('user', deferred()), /class="ri-ph assistant-image-pending"/);
  assert.match(renderSurface('inline', deferred()), /class="ri-ph assistant-image-pending"/);
  assert.match(renderSurface('tool', deferred()), /class="ri-ph tool-image-pending"/);
  // The loaded attachment keeps its lightbox and copy affordances.
  assert.match(renderSurface('user', inline()), /<img alt="shot" class="zoomable" title="Ampliar" tabindex="0" aria-keyshortcuts="Meta\+C Control\+C"/);
});

test('a failed image keeps its frame and says so', () => {
  const html = renderToStaticMarkup(React.createElement(images.ImagePlaceholder, { state: 'failed', alias: 'tool-image-pending' }));
  assert.match(html, /^<span class="ri-ph tool-image-pending" role="img" aria-label="Imagen no disponible">/);
  assert.match(html, /<path d="M2.5 2.5 13.5 13.5"><\/path>/, 'the broken-image glyph');
  assert.match(html, /<span class="cap">Imagen no disponible<\/span>/);
  assert.doesNotMatch(html, /Cargando/);
});

test('CSS: variant C runs one accent bar on pending frames near the viewport, never gated on reduced motion', () => {
  const css = readFileSync(fileURLToPath(new URL('../src/styles/app.css', import.meta.url)), 'utf8');
  const bar = /\.ri\[data-state="pending"\]\[data-near\] \.ri-media::after \{([^}]*)\}/.exec(css);
  assert.ok(bar, 'the pending bar rule');
  assert.match(bar[1], /height: 2px/);
  assert.match(bar[1], /bottom: 0/);
  assert.match(bar[1], /background: var\(--acc\)/);
  assert.match(bar[1], /animation: ri-bar 1\.3s ease-in-out infinite/);
  for (const block of css.match(/@media \(prefers-reduced-motion[^{]*\{[^}]*\}/g) ?? []) {
    assert.doesNotMatch(block, /ri-bar|\.ri\b|ri-media/, 'Reduce Motion is on here; the loading line must still run');
  }
  // The only accent in the frame is the progress mark.
  const riRules = css.match(/^\.ri[^{]*\{[^}]*\}/gm) ?? [];
  assert.ok(riRules.length > 5);
  assert.deepEqual(riRules.filter((rule) => rule.includes('var(--acc)')).map((rule) => rule.split('{')[0].trim()), ['.ri[data-state="pending"][data-near] .ri-media::after']);
  // Today's caps: attachment 420 x 320, inline 620 high, gallery 620, tool 340.
  assert.match(css, /\.ri-user \{[^}]*min\(420px, 68vw\)[^}]*318px/);
  assert.match(css, /\.ri-inline \{[^}]*620px/);
  assert.match(css, /\.ri-gallery \.ri-media \{ max-height: 620px; \}/);
  assert.match(css, /\.ri-tool \.ri-media \{ max-height: 340px; \}/);
  assert.doesNotMatch(css, /\.tool-image-pending \{[^}]*min-height/, 'the 170px reservation is replaced by the frame');
});
