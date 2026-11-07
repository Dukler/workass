import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';

// Owner, 2026-11-07 (mock sidebar-hammer): a working chat's row says so with a
// hammer and its time alone, and no row grows a line for background work, so
// every full-size row is the same height.

let vite: ViteDevServer;
let SidebarV2: React.ComponentType;
let store: { state: Record<string, any>; clearToastTimers(): void };
const NOW = Date.parse('2026-11-07T12:00:00Z');
const realNow = Date.now;

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  ({ SidebarV2 } = await vite.ssrLoadModule('/src/components/SidebarV2.tsx') as { SidebarV2: typeof SidebarV2 });
  ({ store } = await vite.ssrLoadModule('/src/store/store.ts') as { store: typeof store });
});

after(async () => { Date.now = realNow; store.clearToastTimers(); await vite.close(); });

const chat = (id: string, extra: Record<string, unknown> = {}) => ({
  id, chatId: `chat-${id}`, title: id, titleLocked: true, cwd: '/work/project', providerId: 'claude',
  messages: [], draft: '', pending: false, lastActivityAt: NOW - 60_000, ...extra,
});

test('a working row shows a hammer and its time, no word and no background line', () => {
  Date.now = () => NOW;
  const turnStartedAt = NOW - 106 * 60_000;
  store.state.chats = [
    chat('busy', { messages: [{ id: 'a1', role: 'assistant', content: '', status: 'running', turnStartedAt }] }),
    chat('quiet'),
  ];
  store.state.activeId = 'quiet';
  store.state.spawnedWorkByChat = {
    'busy\u0000chat-busy': [{ id: 'job-1', taskId: 'job-1', tabId: 'busy', chatId: 'chat-busy', providerId: 'claude', kind: 'external', label: 'train', status: 'running', startedAt: new Date(NOW - HOUR()).toISOString(), updatedAt: new Date(NOW).toISOString() }],
  };
  const html = renderToStaticMarkup(React.createElement(SidebarV2));
  const busy = html.slice(html.indexOf('data-sv2-row="busy"'), html.indexOf('data-sv2-row="quiet"'));
  assert.ok(busy.length > 0, 'the working row is drawn');
  assert.match(busy, /<span class="sv2-pill working"><svg viewBox="0 0 24 24"[^>]*><path d="m15 12-9\.373 9\.373/, 'the hammer');
  assert.match(busy, /<span role="status" aria-label="Trabajando" title="Trabajando"><span class="sv2-dur">106m<\/span><\/span>/);
  assert.doesNotMatch(html.replace(/(aria-label|title)="Trabajando"/g, ''), /Trabajando/, 'the word is not drawn');
  assert.doesNotMatch(html, /en segundo plano|sv2-l3|stroke-dasharray/);
});

function HOUR() { return 60 * 60_000; }
