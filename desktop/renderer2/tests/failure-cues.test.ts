import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';

test('a failed tool with no command or location renders a quiet falló trail without a cross', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({
    root,
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/messages.tsx') as {
    ToolDetail: React.ComponentType<{ t: ToolEvent }>;
  };
  const tool: ToolEvent = {
    key: 'failed-no-detail', at: 0, kind: 'tool', id: 'failed-no-detail', toolKind: 'other',
    title: 'Call hidden tool', status: 'failed', command: null, location: null,
    terminalId: null, input: null, output: null,
  };

  const html = renderToStaticMarkup(React.createElement(loaded.ToolDetail, { t: tool }));
  assert.match(html, /<span class="evt-fail">falló<\/span>/);
  assert.doesNotMatch(html, /class="evt-loc/);
  assert.doesNotMatch(html, /✕/);
});

test('the finished-subagents fold shows a falló cue only when a subagent failed', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({
    root,
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/TareasCard.tsx') as {
    TareasCard: React.ComponentType;
  };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  };
  const header: ToolEvent = {
    key: 'subagent-1', at: 0, kind: 'tool', id: 'subagent-1', toolKind: 'agent',
    title: 'Check the renderer', status: 'failed', command: null, location: null,
    terminalId: null, input: null, output: null, subagentId: 'subagent-1',
    subagentHeader: true, subagentProvider: 'codex', subagentModel: 'gpt-5.6-sol[high]',
  };
  storeModule.store.state.meta = { profile: 'dev' };
  storeModule.store.state.activeId = 'tab-1';
  storeModule.store.state.chats = [{
    id: 'tab-1',
    cwd: null,
    messages: [{
      id: 'assistant-1',
      role: 'assistant',
      content: '',
      status: 'done',
      at: null,
      events: [header],
    }],
  }];

  const failedHtml = renderToStaticMarkup(React.createElement(loaded.TareasCard));
  assert.match(failedHtml, /<span class="dc-fail"> · falló<\/span>/);
  assert.doesNotMatch(failedHtml, /class="dc"/);
  assert.doesNotMatch(failedHtml, /✕/);

  header.status = 'completed';
  const succeededHtml = renderToStaticMarkup(React.createElement(loaded.TareasCard));
  assert.doesNotMatch(succeededHtml, /class="dc-fail"/);
  assert.doesNotMatch(succeededHtml, /class="dc"/);
  assert.doesNotMatch(succeededHtml, /✕/);
});

test('failure cue styles use the quiet deletion color without changing titles', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.evt-fail\s*\{\s*color:\s*var\(--del\);\s*font-size:\s*11px;\s*flex:\s*0\s+0\s+auto;\s*\}/);
  assert.match(css, /\.dc-fail\s*\{\s*color:\s*var\(--del\);\s*\}/);
});
