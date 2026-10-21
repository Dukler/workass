import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, MessageImage, Msg } from '../src/store/types.ts';

// A reply's screenshots made opening a chat a 4.8 MB read (2026-10-13). History
// reads now ask for images by reference: the text paints at once, each image
// loads once when in view, and a cached image never loads twice.
let vite: ViteDevServer;
let Transcript: React.ComponentType<{ chat: Chat }>;
let loadImageSrc: (tabId: string, image: MessageImage) => Promise<string | null>;
let cachedImageSrc: (image: MessageImage) => string;
let appStore: { state: { chats: Chat[]; activeId: string | null }; clearToastTimers(): void };

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  ({ Transcript } = await vite.ssrLoadModule('/src/components/Transcript.tsx') as { Transcript: typeof Transcript });
  ({ loadImageSrc, cachedImageSrc } = await vite.ssrLoadModule('/src/components/MessageImage.tsx') as { loadImageSrc: typeof loadImageSrc; cachedImageSrc: typeof cachedImageSrc });
  appStore = (await vite.ssrLoadModule('/src/store/store.ts') as { store: typeof appStore }).store;
});
after(async () => { appStore.clearToastTimers(); await vite.close(); });

const shot: MessageImage = { mimeType: 'image/png', data: '', name: 'player close-up', source: 'player.png', deferredImageRef: 'images/abc' };

test('a deferred reply image paints a placeholder, never inline bytes', () => {
  const chat = {
    id: 'tab-images', chatId: 'chat-images', sessionId: 'session-images', title: 'Screenshots', titleLocked: true,
    group: null, cwd: '/repo', providerId: 'claude', currentModelId: null, currentModeId: null, pending: false, draft: '',
    messages: [
      { id: 'u1', role: 'user', content: 'show me', status: 'done', at: null, events: [], images: [{ ...shot, deferredImageRef: 'images/user', source: undefined }] },
      { id: 'a1', role: 'assistant', content: 'Here it is:\n\n![player close-up](player.png)\n', status: 'done', at: null, events: [], images: [shot], turnRootId: 'a1', turnTerminal: true },
    ] as unknown as Msg[],
  } as Chat;
  appStore.state.chats = [chat];
  appStore.state.activeId = chat.id;
  const html = renderToStaticMarkup(React.createElement(Transcript, { chat }));
  assert.equal((html.match(/Cargando imagen…/g) ?? []).length, 2, 'the inline reply image and the user attachment wait for their bytes');
  assert.doesNotMatch(html, /data:image\/png;base64,/);
  assert.match(html, /class="assistant-inline-image ri ri-inline" data-state="pending"[^>]*disabled/);
  assert.match(html, /class="ri ri-user" data-state="pending"/);
});

test('an image reference is fetched once and then served from the cache', async () => {
  const calls: unknown[] = [];
  const previous = (globalThis as any).window;
  (globalThis as any).window = { api: { archiveLoad: async (_tab: string, options: unknown) => { calls.push(options); return [{ data: 'iVBORw0KGgo=' }]; } } };
  try {
    const image = { ...shot, deferredImageRef: 'images/cache-test' };
    assert.equal(cachedImageSrc(image), '');
    const [first, second] = await Promise.all([loadImageSrc('tab-images', image), loadImageSrc('tab-images', image)]);
    assert.equal(first, 'data:image/png;base64,iVBORw0KGgo=');
    assert.equal(second, first);
    assert.equal(await loadImageSrc('tab-images', image), first);
    assert.equal(cachedImageSrc(image), first);
    assert.deepEqual(calls, [{ imageRef: 'images/cache-test' }], 'one request for concurrent and repeated loads');
  } finally {
    if (previous === undefined) delete (globalThis as any).window; else (globalThis as any).window = previous;
  }
});
