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
