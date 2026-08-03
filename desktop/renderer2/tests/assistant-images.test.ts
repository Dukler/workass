import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

test('natural ACP image markdown collapses a matching Open link into the clickable image', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (...args: unknown[]) => React.ReactNode[];
  };
  const source = '/workspace/calibration ready.png';
  const media = {
    resolve: (target: string) => target === source
      ? { src: 'data:image/png;base64,cG5n', alt: 'Calibration ready' }
      : null,
    open: () => undefined,
  };
  const nodes = loaded.renderInline(`[Open calibration](<${source}>)\n![Calibration ready](<${source}>)`, 'media', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.doesNotMatch(html, /Open calibration|assistant-image-open/);
  assert.doesNotMatch(html, /href="\/workspace|!Calibration ready/);
});

test('an unrelated ordinary link remains visible beside imported assistant media', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (...args: unknown[]) => React.ReactNode[];
  };
  const media = {
    resolve: (target: string) => target === '/workspace/preview.png'
      ? { src: 'data:image/png;base64,cG5n', alt: 'Preview' }
      : null,
    open: () => undefined,
  };
  const nodes = loaded.renderInline('[Open notes](/workspace/notes.txt)', 'ordinary-link', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /<a[^>]+href="\/workspace\/notes.txt"[^>]*>Open notes<\/a>/);
});

test('an unresolved local image is a quiet pending label, never a broken file navigation', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (...args: unknown[]) => React.ReactNode[];
  };
  const media = { resolve: () => null, open: () => undefined };
  const nodes = loaded.renderInline('![Preview](/workspace/not-ready.png)', 'pending', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-image-pending"/);
  assert.doesNotMatch(html, /href=|^!|>!</);
});

test('artifact-host markdown renders as an inline same-origin image without embedded bytes', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/markdown/inline.tsx') as {
    renderInline: (...args: unknown[]) => React.ReactNode[];
  };
  const media = { resolve: () => null, open: () => undefined };
  const nodes = loaded.renderInline('![Calibration](/workass/artifacts/calibration-a1b2/)', 'hosted', true, media);
  const html = renderToStaticMarkup(React.createElement(React.Fragment, null, ...nodes));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="\/workass\/artifacts\/calibration-a1b2\/"/);
  assert.doesNotMatch(html, /assistant-image-pending/);
});

test('assistant transcript rows bind imported media to their authored Markdown positions', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/AssistantMessage.tsx') as {
    AssistantMessage: React.ComponentType<Record<string, unknown>>;
  };
  const source = '/workspace/preview.png';
  const msg = {
    id: 'assistant-media', role: 'assistant', content: '', status: 'running', at: null, events: [],
    result: `[Open preview](${source})\n![Preview](${source})`,
    images: [{ mimeType: 'image/png', data: 'cG5n', name: 'Preview', source }],
  };
  const html = renderToStaticMarkup(React.createElement(loaded.AssistantMessage, { tabId: 'media-tab', msg, profile: 'dev' }));
  assert.match(html, /class="assistant-inline-image"/);
  assert.match(html, /src="data:image\/png;base64,cG5n"/);
  assert.doesNotMatch(html, /Open preview|assistant-image-open/);
  assert.doesNotMatch(html, /!<a|href="\/workspace/);
});
