import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';

// Approved mock rail-v7 (2026-11-05), after f-bg2: RUNNING spawned work = flat
// inline rows in the Trabajos tab, visible without opening any fold; FINISHED
// spawned work = ONE «Terminados hoy» fold of flat lines — no fold nested
// inside a fold for today's work.
test('running spawned work is inline; finished work is a single flat fold', async (t) => {
  // One clock for the fixture and the components: whatever Date.now another
  // test file left behind (tests share one process in the suite), the rows'
  // times and the fold's "today" are read from the same instant.
  const fixedNow = new Date().setHours(12, 0, 0, 0);
  const realNow = Date.now;
  Date.now = () => fixedNow;
  t.after(() => { Date.now = realNow; });
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent' });
  let store: { state: Record<string, unknown>; clearToastTimers(): void } | undefined;
  t.after(async () => { store?.clearToastTimers(); await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx') as {
    FinishedWork: React.ComponentType<{ chat: unknown }>;
  };
  const { TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx') as { TareasCard: React.ComponentType };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, unknown>; clearToastTimers(): void } };
  store = storeModule.store;
  const chat = { id: 'tab-1', chatId: 'chat-1', messages: [] };
  storeModule.store.state.activeId = 'tab-1';
  storeModule.store.state.chats = [chat];
  storeModule.store.state.hasSpawnedWorkChannels = false;
  Object.assign(storeModule.store, { selfMachineName: 'workbench' });
  storeModule.store.state.spawnedWorkByChat = {
    ['tab-1\u0000chat-1']: [
      {
        id: 'bash-1', taskId: 'bash-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'running',
        label: 'Serve the complete interactive comparison gallery without truncating this title',
        startedAt: new Date(Date.now() - 5000).toISOString(), updatedAt: new Date(Date.now()).toISOString(), pid: 321,
        outputFile: '/private/tmp/claude-501/project/session/tasks/bash-1.output',
        summary: 'Reempaquetando la app y esperando el gate.',
      },
      {
        id: 'agent-1', taskId: 'agent-1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'agent', status: 'exited',
        label: 'Review the UI', startedAt: new Date(Date.now() - 9000).toISOString(), updatedAt: new Date(Date.now()).toISOString(), finishedAt: new Date(Date.now()).toISOString(),
      },
      {
        id: 'bash-2', taskId: 'bash-2', tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'bash', status: 'failed',
        label: 'notarize.sh', startedAt: new Date(Date.now() - 8000).toISOString(), updatedAt: new Date(Date.now()).toISOString(), finishedAt: new Date(Date.now() - 2000).toISOString(),
        exitCode: 1,
      },
    ],
  };

  // Running rows: inline, flat, no <details> wrapper, live dot + what it does visible.
  const tab = renderToStaticMarkup(React.createElement(TareasCard));
  const live = tab.slice(0, tab.indexOf('<details class="wdone"'));
  assert.match(live, /class="wjob"/);
  assert.match(live, /title="Serve the complete interactive comparison gallery without truncating this title"/);
  // daemon prose we cannot classify stays verbatim, after where it runs
  assert.match(live, /<span class="wjob-mt">workbench · Reempaquetando la app y esperando el gate\.<\/span>/);
  assert.match(live, /title="[^"]*pid 321"/);
  assert.match(live, />Ver salida</);
  assert.match(live, /class="wdot"/);
  assert.doesNotMatch(live, /<details/);
  assert.doesNotMatch(live, /Review the UI/); // finished items never render inline

  // Finished fold: one details, flat r-dline rows; the running item is not in
  // it. The row says WHAT ran, never how it ended — the fold's old « · 1 falló»
  // count and every red cue were removed on the user's call (2026-07-25), so
  // this pins their absence.
  const card = renderToStaticMarkup(React.createElement(loaded.FinishedWork, { chat }));
  assert.match(card, /Terminados hoy/);
  assert.match(card, /<span class="wdone-n">2<\/span>/);
  assert.match(card, /Review the UI/);
  assert.match(card, /notarize\.sh/);
  assert.doesNotMatch(card, /falló|fracas|error/i);
  assert.match(card, /r-dline/);
  assert.doesNotMatch(card, /trabajos terminados|wdone-old/);
  assert.doesNotMatch(card, /Serve the complete interactive comparison gallery/);
  assert.equal((card.match(/<details class="wdone"/g) ?? []).length, 1, 'one finished fold');
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
  for (const fold of ['wdone-b', 'wq-list']) {
    const body = css.match(new RegExp(`\\.${fold}\\s*\\{([^}]*)\\}`))?.[1] ?? '';
    assert.match(body, /max-height:/, fold);
    assert.match(body, /overflow-y:\s*auto/, fold);
    assert.match(body, /overscroll-behavior:\s*contain/, fold);
  }
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
  const server = await createServer({ root, server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent' });
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

// A supervised job's wait is not its run (fleet-runtime 14e). The wire keeps
// status 'running' for older renderers; the row reads the additive fields.
test('a queued job says so and a finished one counts only its run', async (t) => {
  const root = fileURLToPath(new URL('..', import.meta.url));
  const server = await createServer({ root, server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent' });
  let store: { state: Record<string, unknown>; clearToastTimers(): void } | undefined;
  t.after(async () => { store?.clearToastTimers(); await server.close(); });
  const loaded = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx') as {
    FinishedWork: React.ComponentType<{ chat: unknown }>;
  };
  const { TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx') as { TareasCard: React.ComponentType };
  const railView = await server.ssrLoadModule('/src/rail-view.ts') as { setRailQueueOpen(key: string, open: boolean): void; resetRailViews(): void };
  const storeModule = await server.ssrLoadModule('/src/store/store.ts') as { store: { state: Record<string, unknown>; clearToastTimers(): void } };
  store = storeModule.store;
  const chat = { id: 'tab-1', chatId: 'chat-1', messages: [] };
  const at = (msAgo: number) => new Date(Date.now() - msAgo).toISOString();
  const base = { tabId: 'tab-1', chatId: 'chat-1', providerId: 'claude', kind: 'external', updatedAt: at(0) };
  const hour = 3600_000;
  const seed = (items: unknown[]) => {
    storeModule.store.state.activeId = 'tab-1';
    storeModule.store.state.chats = [chat];
    storeModule.store.state.hasSpawnedWorkChannels = false;
    storeModule.store.state.spawnedWorkByChat = { ['tab-1\u0000chat-1']: items };
  };
  railView.resetRailViews();
  railView.setRailQueueOpen('tab-1', true);

  seed([{ ...base, id: 'waiting', taskId: 'waiting', label: 'waiting job', status: 'running', startedAt: at(3 * hour), queuedAt: at(3 * hour) }]);
  const waiting = renderToStaticMarkup(React.createElement(TareasCard));
  assert.match(waiting, /<span class="wq-t" title="waiting job">waiting job<\/span><span class="wq-w">espera 3 h<\/span>/);
  assert.doesNotMatch(waiting, /class="wjob"|class="wdot"/);

  seed([{ ...base, id: 'started', taskId: 'started', label: 'started job', status: 'running', startedAt: at(3 * hour), queuedAt: at(3 * hour), execStartedAt: at(hour) }]);
  const started = renderToStaticMarkup(React.createElement(TareasCard));
  assert.match(started, /<span class="wjob-t">1 h<\/span>/);
  assert.doesNotMatch(started, /class="wq|>3 h</, 'it has left the queue and runs from its exec');
  assert.match(started, /class="wdot"/);

  // An older daemon sends none of the fields and its row is what it always was.
  seed([{ ...base, id: 'old', taskId: 'old', label: 'old job', status: 'running', startedAt: at(2 * hour) }]);
  const old = renderToStaticMarkup(React.createElement(TareasCard));
  assert.match(old, /<span class="wjob-t">2 h<\/span>/);
  assert.match(old, /class="wdot"/);

  seed([
    { ...base, id: 'ran', taskId: 'ran', label: 'ran job', status: 'exited', startedAt: at(5 * hour), finishedAt: at(0), queuedAt: at(5 * hour), execStartedAt: at(hour), runMs: hour },
    { ...base, id: 'never', taskId: 'never', label: 'never ran', status: 'failed', startedAt: at(5 * hour), finishedAt: at(0), queuedAt: at(5 * hour) },
  ]);
  const finished = renderToStaticMarkup(React.createElement(loaded.FinishedWork, { chat }));
  assert.match(finished, /ran job<\/span><span class="r-dl">60m/);
  assert.match(finished, /never ran<\/span>(?!<span class="r-dl">)/);
  assert.doesNotMatch(finished, /300m/);
  railView.resetRailViews();
});
