import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import { projectIconCacheKey, projectIconDataURL } from '../src/project-icon.ts';
import { tagId } from '../src/wire/machineIds.ts';

test('project icon result accepts only bounded image data URLs', () => {
  assert.equal(projectIconDataURL({ found: true, mimeType: 'image/png', base64: 'iVBORw==' }), 'data:image/png;base64,iVBORw==');
  assert.equal(projectIconDataURL({ found: false, mimeType: 'image/png', base64: 'iVBORw==' }), null);
  assert.equal(projectIconDataURL({ found: true, mimeType: 'text/html', base64: 'PGgxPg==' }), null);
  assert.equal(projectIconDataURL({ found: true, mimeType: 'image/png', base64: '<script>' }), null);
});

test('project icon cache is partitioned by owning machine', () => {
  const cwd = '/workspace/workass';
  assert.notEqual(projectIconCacheKey('chat-local', cwd), projectIconCacheKey(tagId('m-builder', 'chat-remote'), cwd));
  assert.equal(projectIconCacheKey(tagId('m-builder', 'chat-a'), cwd), projectIconCacheKey(tagId('m-builder', 'chat-b'), cwd));
});

test('the project fallback carries one accessible remote-machine initial', async (t) => {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/ProjectIcon.tsx') as {
    ProjectIcon: React.ComponentType<Record<string, unknown>>;
  };
  const html = renderToStaticMarkup(React.createElement(loaded.ProjectIcon, {
    chatId: tagId('m-builder', 'chat-7'),
    cwd: '/srv/project-without-artwork',
    remote: { machine: 'builder', initial: 'B', title: 'Proyecto remoto en builder' },
  }));

  assert.match(html, /class="sv2-picon"/);
  assert.match(html, /role="img"/);
  assert.match(html, /aria-label="Proyecto remoto en builder"/);
  assert.match(html, /<svg/);
  assert.match(html, /class="sv2-remote"[^>]*>B<\/span>/);
});

// A reload asks for this daemon's icons by etag and reuses the kept icon on a
// receipt; another machine's icons, and a daemon that names no etag, are read
// exactly as before.
test('a reload reuses a kept icon on the daemon receipt', async () => {
  const hadWindow = Object.prototype.hasOwnProperty.call(globalThis, 'window');
  const previousWindow = (globalThis as any).window;
  const hadStorage = Object.prototype.hasOwnProperty.call(globalThis, 'sessionStorage');
  const previousStorage = (globalThis as any).sessionStorage;
  const data = new Map<string, string>();
  (globalThis as any).sessionStorage = {
    getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => { data.set(k, v); }, removeItem: (k: string) => { data.delete(k); },
  };
  const sent: unknown[][] = [];
  const replies: unknown[] = [
    { found: true, mimeType: 'image/png', base64: 'iVBORw==', etag: 'icon-1' },
    { unchanged: true, etag: 'icon-1' },
    { found: true, mimeType: 'image/png', base64: 'iVBORw==' },
  ];
  (globalThis as any).window = { api: { projectIcon: async (...args: unknown[]) => { sent.push(args); return replies.shift(); } } };
  const server = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  try {
    const first = await server.ssrLoadModule('/src/project-icon.ts') as typeof import('../src/project-icon.ts');
    assert.equal(await first.loadProjectIcon('chat-1', '/workspace/a'), 'data:image/png;base64,iVBORw==');
    // A reload: a fresh module (no in-memory icons) on the same window storage.
    server.moduleGraph.invalidateAll();
    const reloaded = await server.ssrLoadModule('/src/project-icon.ts') as typeof import('../src/project-icon.ts');
    assert.notEqual(reloaded, first);
    assert.equal(await reloaded.loadProjectIcon('chat-1', '/workspace/a'), 'data:image/png;base64,iVBORw==');
    assert.equal(await reloaded.loadProjectIcon(tagId('m-builder', 'chat-2'), '/workspace/b'), 'data:image/png;base64,iVBORw==');
    assert.deepEqual(sent, [
      ['chat-1', '/workspace/a', { ifNoneMatch: '' }],
      ['chat-1', '/workspace/a', { ifNoneMatch: 'icon-1' }],
      [tagId('m-builder', 'chat-2'), '/workspace/b'],
    ]);
  } finally {
    await server.close();
    if (hadWindow) (globalThis as any).window = previousWindow; else delete (globalThis as any).window;
    if (hadStorage) (globalThis as any).sessionStorage = previousStorage; else delete (globalThis as any).sessionStorage;
  }
});
