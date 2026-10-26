import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

// Approved mock f-bg2 (2026-07-23): RUNNING spawned work = inline .bgr rows,
// visible without opening any fold; FINISHED spawned work = ONE flat
// "Segundo plano" fold — no fold nested inside a fold.
test('running spawned work is inline; finished work is a single flat fold', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  let store: { state: Record<string, unknown>; clearToastTimers(): void } | undefined;
  t.after(async () => { store?.clearToastTimers(); await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx') as {
    SpawnedWorkCard: React.ComponentType<{ chat: unknown }>;
    SpawnedWorkLive: React.ComponentType<{ chat: unknown }>;
  };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, unknown>; clearToastTimers(): void } };
  store = storeModule.store;
  const chat = { id: 'tab-1', chatId: 'chat-1', messages: [] };
  storeModule.store.state.hasSpawnedWorkChannels = false;
  storeModule.store.state.spawnedWorkByChat = {
    ['tab-1\u0000chat-1']: [
      {
        id: 'bash-1', taskId: 'bash-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'running',
        label: 'Serve the complete interactive comparison gallery without truncating this title',
        startedAt: new Date(Date.now() - 5000).toISOString(), updatedAt: new Date().toISOString(), pid: 321,
        outputFile: '/private/tmp/claude-501/project/session/tasks/bash-1.output',
        summary: 'Reempaquetando la app y esperando el gate.',
      },
      {
        id: 'agent-1', taskId: 'agent-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'agent', status: 'exited',
        label: 'Review the UI', startedAt: new Date(Date.now() - 9000).toISOString(), updatedAt: new Date().toISOString(), finishedAt: new Date().toISOString(),
      },
      {
        id: 'bash-2', taskId: 'bash-2', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'failed',
        label: 'notarize.sh', startedAt: new Date(Date.now() - 8000).toISOString(), updatedAt: new Date().toISOString(), finishedAt: new Date(Date.now() - 2000).toISOString(),
        exitCode: 1,
      },
    ],
  };

  // Running rows: inline, flat, no <details> wrapper, pulse + live summary visible.
  const live = renderToStaticMarkup(React.createElement(loaded.SpawnedWorkLive, { chat }));
  assert.match(live, /class="bglive"/);
  assert.match(live, /Serve the complete interactive comparison gallery without truncating this title/);
  // the kind reads as a word, not the wire enum (mock rail-actions, 2026-07-27)
  assert.match(live, /comando · pid 321/);
  // daemon prose we cannot classify stays verbatim — and gets NO action glyph
  assert.match(live, /<span class="bgr-sum"[^>]*>Reempaquetando la app y esperando el gate\.<\/span>/);
  assert.doesNotMatch(live, /class="bgr-act"/);
  assert.match(live, /ver salida/);
  assert.match(live, /r-pulse/);
  assert.doesNotMatch(live, /<details/);
  assert.doesNotMatch(live, /Review the UI/); // finished items never render inline

  // Finished fold: one details, flat r-dline rows; running item and the old
  // nested "trabajos terminados" fold are gone. The row says WHAT ran, never how
  // it ended — the fold's old « · 1 falló» count and every red cue were removed
  // on the user's call (2026-07-25), so this pins their absence.
  const card = renderToStaticMarkup(React.createElement(loaded.SpawnedWorkCard, { chat }));
  assert.match(card, /Segundo plano/);
  assert.match(card, /<span class="r-count">2<\/span>/);
  assert.match(card, /Review the UI/);
  assert.match(card, /notarize\.sh/);
  assert.doesNotMatch(card, /falló|fracas|error/i);
  assert.match(card, /r-dline/);
  assert.doesNotMatch(card, /trabajos terminados/);
  assert.doesNotMatch(card, /Serve the complete interactive comparison gallery/);
  assert.equal((card.match(/class="r-meta"/g) ?? []).length, 1, 'one background fold');
  assert.equal((card.match(/class="r-sa"/g) ?? []).length, 1, 'historical agents use the shared inspection row');
});

test('spawned-work rail CSS clamps titles and bounds output tails', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const title = css.match(/\.bgr-n\s*\{([^}]*)\}/)?.[1] ?? '';
  const tail = css.match(/\.bgr-tail\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(title, /overflow-wrap:\s*anywhere/);
  assert.match(title, /-webkit-line-clamp:\s*2/);
  assert.match(tail, /max-height:\s*180px/);
  assert.match(tail, /overflow:\s*auto/);
  // Fold bodies scroll INSIDE the fold — opening a long list must never spawn
  // a pane-wide scrollbar on the rail (user 2026-07-24).
  const foldBody = css.match(/\.r-meta-b\s*\{([^}]*)\}/)?.[1] ?? '';
  assert.match(foldBody, /max-height:/);
  assert.match(foldBody, /overflow-y:\s*auto/);
  assert.match(foldBody, /overscroll-behavior:\s*contain/);
  // No lingering styles from the removed nested-fold design.
  assert.doesNotMatch(css, /bgwdone|\.bgcard/);
});

// Claude Code reports every command an agent runs as a task under that agent.
// A long agent left hundreds of finished lines OUTSIDE its row, which no click
// could fold (user, 2026-10-26): only running work hangs outside the row; the
// finished work folds with it, and a task that is one of the agent's own listed
// calls is drawn once, as the call.
test('an agent row folds its finished work; only running work hangs outside', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  let store: { clearToastTimers(): void } | undefined;
  t.after(async () => { store?.clearToastTimers(); await server.close(); });
  const cards = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx') as {
    SpawnedWorkKids: React.ComponentType<{ chat: unknown; items: unknown[]; nowMs: number }>;
    SpawnedWorkDoneKids: React.ComponentType<{ items: unknown[]; nowMs: number; calls?: unknown[] }>;
  };
  const rows = await server.ssrLoadModule('/src/components/SubagentRow.tsx') as { SubagentRow: React.ComponentType<Record<string, unknown>> };
  store = (await server.ssrLoadModule('/src/store/store.ts') as { store: { clearToastTimers(): void } }).store;
  const chat = { id: 'tab-1', chatId: 'chat-1', messages: [] };
  const now = Date.now();
  const kid = (id: string, status: string, extra: Record<string, unknown> = {}) => ({
    id, taskId: id, tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status, parentTaskId: 'agent-1',
    label: `label ${id}`, startedAt: new Date(now - 9000).toISOString(), updatedAt: new Date(now).toISOString(),
    ...(status === 'running' ? {} : { finishedAt: new Date(now - 1000).toISOString() }), ...extra,
  });
  const items = [
    kid('shell-live', 'running', { outputFile: '/tmp/shell-live.output', pid: 77 }),
    kid('cmd-done', 'exited', { toolCallId: 'call-cmd' }),
    kid('shell-done', 'exited', { toolCallId: 'call-shell', outputFile: '/tmp/shell-done.output' }),
  ];
  const outside = renderToStaticMarkup(React.createElement(cards.SpawnedWorkKids, { chat, items, nowMs: now }));
  assert.match(outside, /label shell-live/);
  assert.doesNotMatch(outside, /label cmd-done|label shell-done/);

  const calls = [{ key: 'k1', at: now, kind: 'tool', id: 'call-cmd', toolKind: 'execute', title: 'Bash', status: 'completed', command: 'ls', terminalId: null, input: null, output: null, location: null, subagentId: 'agent-call' }];
  const inside = renderToStaticMarkup(React.createElement(cards.SpawnedWorkDoneKids, { items, nowMs: now, calls }));
  assert.match(inside, /label shell-done/);
  assert.doesNotMatch(inside, /label cmd-done/); // it is the listed call
  assert.doesNotMatch(inside, /label shell-live/);
  assert.match(renderToStaticMarkup(React.createElement(cards.SpawnedWorkDoneKids, { items, nowMs: now })), /label cmd-done/);

  const node = { id: 'agent-call', label: 'Fix floating browser movement', provider: 'claude', calls: [], header: null,
    work: { ...kid('agent-1', 'exited'), kind: 'agent', parentTaskId: undefined } };
  const row = renderToStaticMarkup(React.createElement(rows.SubagentRow, {
    n: node, nowMs: now, chat,
    kids: React.createElement(cards.SpawnedWorkKids, { chat, items, nowMs: now }),
    doneKids: React.createElement(cards.SpawnedWorkDoneKids, { items, nowMs: now }),
  }));
  const close = row.indexOf('</details>');
  assert.ok(row.indexOf('label shell-done') > 0 && row.indexOf('label shell-done') < close, 'finished work folds with the row');
  assert.ok(row.indexOf('label cmd-done') < close, 'finished commands fold with the row');
  assert.ok(row.indexOf('label shell-live') > close, 'running work stays visible outside the fold');
});
