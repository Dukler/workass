import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { after, before, test } from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer, type ViteDevServer } from 'vite';
import type { ToolEvent } from '../src/store/types.ts';

let server: ViteDevServer;
let ToolDetail: React.ComponentType<{ t: ToolEvent }>;
let TareasCard: React.ComponentType;
let store: { state: Record<string, unknown> };

before(async () => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  server = await createServer({
    root,
    server: { middlewareMode: true },
    appType: 'custom',
    logLevel: 'silent',
  });
  ({ ToolDetail } = await server.ssrLoadModule('/src/components/messages.tsx') as {
    ToolDetail: React.ComponentType<{ t: ToolEvent }>;
  });
  ({ TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx') as {
    TareasCard: React.ComponentType;
  });
  ({ store } = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { state: Record<string, unknown> };
  });
});

after(async () => { await server.close(); });

test('a failed tool with no command or location renders a quiet falló trail without a cross', () => {
  const tool: ToolEvent = {
    key: 'failed-no-detail', at: 0, kind: 'tool', id: 'failed-no-detail', toolKind: 'other',
    title: 'Call hidden tool', status: 'failed', command: null, location: null,
    terminalId: null, input: null, output: null,
  };

  const html = renderToStaticMarkup(React.createElement(ToolDetail, { t: tool }));
  assert.match(html, /<span class="evt-fail">falló<\/span>/);
  assert.doesNotMatch(html, /class="evt-loc/);
  assert.doesNotMatch(html, /✕/);
});

test('the finished-subagents fold shows a falló cue only when a subagent failed', () => {
  const header: ToolEvent = {
    key: 'subagent-1', at: 0, kind: 'tool', id: 'subagent-1', toolKind: 'agent',
    title: 'Check the renderer', status: 'failed', command: null, location: null,
    terminalId: null, input: null, output: null, subagentId: 'subagent-1',
    subagentHeader: true, subagentProvider: 'codex', subagentModel: 'gpt-5.6-sol[high]',
  };
  store.state.meta = { profile: 'dev' };
  store.state.activeId = 'tab-1';
  store.state.chats = [{
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

  const failedHtml = renderToStaticMarkup(React.createElement(TareasCard));
  assert.match(failedHtml, /<span class="dc-fail"> · falló<\/span>/);
  assert.doesNotMatch(failedHtml, /class="dc"/);
  assert.doesNotMatch(failedHtml, /✕/);

  header.status = 'completed';
  const succeededHtml = renderToStaticMarkup(React.createElement(TareasCard));
  assert.doesNotMatch(succeededHtml, /class="dc-fail"/);
  assert.doesNotMatch(succeededHtml, /class="dc"/);
  assert.doesNotMatch(succeededHtml, /✕/);
});

test('failure cue styles use the quiet deletion color without changing titles', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(css, /\.evt-fail\s*\{\s*color:\s*var\(--del\);\s*font-size:\s*11px;\s*flex:\s*0\s+0\s+auto;\s*\}/);
  assert.match(css, /\.dc-fail\s*\{\s*color:\s*var\(--del\);\s*\}/);
});
