import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

let server: ViteDevServer;
let renderInline: (...args: unknown[]) => React.ReactNode[];
let AssistantMessage: React.ComponentType<Record<string, unknown>>;

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  ({ renderInline } = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (...args: unknown[]) => React.ReactNode[];
  });
  ({ AssistantMessage } = await server.ssrLoadModule('/src/components/AssistantMessage.tsx') as {
    AssistantMessage: React.ComponentType<Record<string, unknown>>;
  });
});

after(async () => { await server.close(); });

test('natural ACP image markdown collapses a matching Open link into the clickable image', () => {
  const source = '/workspace/calibration ready.png';
  const media = {
    resolve: (target: string) => target === source
      ? { src: 'data:image/png;base64,cG5n', alt: 'Calibration ready' }
      : null,
    open: () => undefined,
  };
  const nodes = renderInline(`[Open calibration](<${source}>)\n![Calibration ready](<${source}>)`, 'media', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.doesNotMatch(html, /Open calibration|assistant-image-open/);
  assert.doesNotMatch(html, /href="\/workspace|!Calibration ready/);
});

test('an unrelated ordinary link remains visible beside imported assistant media', () => {
  const media = {
    resolve: (target: string) => target === '/workspace/preview.png'
      ? { src: 'data:image/png;base64,cG5n', alt: 'Preview' }
      : null,
    open: () => undefined,
  };
  const nodes = renderInline('[Open notes](/workspace/notes.txt)', 'ordinary-link', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /<a[^>]+href="\/workspace\/notes.txt"[^>]*>Open notes<\/a>/);
});

test('an unresolved local image is a quiet pending label, never a broken file navigation', () => {
  const media = { resolve: () => null, open: () => undefined };
  const nodes = renderInline('![Preview](/workspace/not-ready.png)', 'pending', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-image-pending"/);
  assert.doesNotMatch(html, /href=|^!|>!</);
});

test('artifact-host markdown renders as an inline same-origin image without embedded bytes', () => {
  const media = { resolve: () => null, open: () => undefined };
  const nodes = renderInline('![Calibration](/workass/artifacts/calibration-a1b2/)', 'hosted', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="\/workass\/artifacts\/calibration-a1b2\/"/);
  assert.doesNotMatch(html, /assistant-image-pending/);
});

test('assistant transcript rows bind imported media to their authored Markdown positions', () => {
  const source = '/workspace/preview.png';
  const msg = {
    id: 'assistant-media', role: 'assistant', content: '', status: 'running', at: null, events: [],
    result: `[Open preview](${source})\n![Preview](${source})`,
    images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Preview', source }],
  };
  const html = renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.doesNotMatch(html, /Open preview|assistant-image-open/);
  assert.doesNotMatch(html, /!<a|href="\/workspace/);
});

test('source-less saved assistant images recover their unique authored positions', () => {
  const msg = {
    id: 'legacy-assistant-media', role: 'assistant', content: 'Before image.\n\n[Open gold](/workspace/gold.png)\n![Gold first RGB](/workspace/gold.png)\n\nAfter image.', status: 'done', at: null, events: [],
    images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Gold first RGB' }],
  };
  const html = renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.match(html, /Before image\.[\s\S]*class="assistant-inline-image"[\s\S]*After image\./);
  assert.equal((html.match(/class="assistant-inline-image"/g) ?? []).length, 1);
  assert.doesNotMatch(html, /Imágenes de la respuesta|Open gold|assistant-image-pending/);
});

test('repeated Markdown labels recover the first imported image position', () => {
  const msg = {
    id: 'repeated-label-media', role: 'assistant', content: '![Preview](/workspace/one.png)\n\nLater: ![Preview](/workspace/two.png)', status: 'done', at: null, events: [],
    images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Preview' }],
  };
  const html = renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.match(html, /class="assistant-inline-image"[\s\S]*Later:[\s\S]*assistant-image-pending/);
  assert.doesNotMatch(html, /aria-label="Imágenes de la respuesta"/);
});

test('six saved training review images follow their authored positions despite a later repeated label', () => {
  const names = ['Gold first RGB', 'Silver first RGB', 'Gold six-frame review', 'Silver six-frame review', 'Gold train review', 'Silver train review'];
  const content = names.map((name, index) => `Section ${index + 1}: ![${name}](/workspace/review-${index + 1}.png)`).join('\n\n')
    + '\n\nLater: ![Silver train review](/workspace/later.png)';
  const msg = {
    id: 'six-training-images', role: 'assistant', content, status: 'done', at: null, events: [],
    images: names.map((name) => ({ mimeType: 'image/png', data: 'cG5n', name })),
  };
  const html = renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.equal((html.match(/class="assistant-inline-image"/g) ?? []).length, 6);
  assert.doesNotMatch(html, /aria-label="Imágenes de la respuesta"/);
  for (let index = 1; index <= 6; index += 1) assert.match(html, new RegExp(`Section ${index}:[\\s\\S]*?class="assistant-inline-image"`));
  assert.match(html, /Later:[\s\S]*assistant-image-pending/);
});

test('ambiguous legacy image names remain in the gallery', () => {
  const msg = {
    id: 'ambiguous-assistant-media', role: 'assistant', content: '![Preview](/workspace/one.png)', status: 'done', at: null, events: [],
    images: [
      { mimeType: 'image/png', data: 'cG5n', name: 'Preview' },
      { mimeType: 'image/png', data: 'cG5n', name: 'Preview' },
    ],
  };
  const html = renderToStaticMarkup(React.createElement(AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.match(html, /aria-label="Imágenes de la respuesta"/);
  assert.match(html, /assistant-image-pending/);
});
