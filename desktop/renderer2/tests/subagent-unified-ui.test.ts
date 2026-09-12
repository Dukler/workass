import assert from 'node:assert/strict';
import test from 'node:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { ToolEvent } from '../src/store/types.ts';
import type { SpawnedWorkItem } from '../src/wire/types.ts';
import { extractSubagents, reconcileSubagentWork } from '../src/subagent-layout.ts';

const startedAt = '2026-09-12T12:00:00Z';
const makeWork = (id: string, kind: string): SpawnedWorkItem => ({
  id: `${id}-run`, taskId: `${id}-run`, toolCallId: id, tabId: 'tab', chatId: 'chat',
  providerId: 'codex', assistantBrand: 'gpt', kind, label: `${kind} review`,
  status: 'running', startedAt, updatedAt: startedAt, modelLabel: 'GPT-6-Astra-high',
});
const header = (id: string): ToolEvent => ({ key: id, id, at: 0, kind: 'tool', toolKind: 'agent',
  title: 'Native review', status: 'completed', subagentId: id, subagentHeader: true,
  subagentProvider: 'gpt', subagentModel: 'GPT-6-Astra-high', startedAt: Date.parse(startedAt),
  command: null, location: null, input: null, output: null, terminalId: null });

test('live work retains header metadata and calls, including children from earlier turns', () => {
  const events = [header('native'), { ...header('call'), id: 'native:read', subagentId: 'native',
    subagentHeader: false, toolKind: 'read', title: 'Read file', status: 'in_progress' }];
  const native = { ...makeWork('native', 'agent'), modelLabel: undefined, assistantBrand: undefined };
  const older = makeWork('older', 'agent');
  const nodes = reconcileSubagentWork(extractSubagents(events).nodes, [native, older]).nodes;
  assert.equal(nodes.length, 2);
  assert.equal(nodes[0].model, 'GPT-6-Astra-high');
  assert.equal(nodes[0].provider, 'gpt');
  assert.equal(nodes[0].calls.length, 1);
  assert.equal(nodes[0].header?.status, 'in_progress');
  assert.equal(events[0].status, 'completed', 'projection must not rewrite transcript authority');
  assert.equal(nodes[1].id, 'older');
});

test('native and managed agents share one inspectable row with controls only for managed work', async (t) => {
  const server = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  t.after(() => server.close());
  const { TareasCard } = await server.ssrLoadModule('/src/components/TareasCard.tsx');
  const { SpawnedWorkCard, SpawnedWorkLive } = await server.ssrLoadModule('/src/components/SpawnedWorkCard.tsx');
  const { store } = await server.ssrLoadModule('/src/store/store.ts');
  const { setMachineRouter } = await server.ssrLoadModule('/src/wire/api.ts');
  let mutations = 0;
  setMachineRouter({ spawnedWorkStop: async () => { mutations++; return { ok: true }; } });
  const native = { ...makeWork('native', 'agent'), pid: 123, outputFile: '/fixture/native.output' };
  const managed = { ...makeWork('managed', 'subagent'), id: 'managed', taskId: 'managed' };
  const chat = { id: 'tab', chatId: 'chat', cwd: null, messages: [{
    id: 'parent', role: 'assistant', status: 'completed', content: '', at: null,
    events: [header('native'), header('managed')],
  }] };
  store.state.meta = { profile: 'dev' };
  store.state.activeId = 'tab';
  store.state.chats = [chat];
  store.state.spawnedWorkByChat = { ['tab\u0000chat']: [native, managed] };
  const render = () => renderToStaticMarkup(React.createElement(TareasCard));
  const live = render();
  assert.equal((live.match(/<details class="r-sa"/g) ?? []).length, 2);
  assert.equal((live.match(/class="r-mdl">GPT-6-Astra-high/g) ?? []).length, 2);
  assert.equal((live.match(/class="bgr-stop"/g) ?? []).length, 1);
  assert.match(live, /aria-label="Detener subagent review"/);
  assert.doesNotMatch(live, /aria-label="Detener agent review"/);
  assert.match(live, /Subagente nativo · solo lectura/);
  assert.equal(renderToStaticMarkup(React.createElement(SpawnedWorkLive, { chat })), '', 'no second live row');

  store.state.spawnedWorkByChat['tab\u0000chat'] = [
    { ...native, status: 'completed', finishedAt: '2026-09-12T12:01:00Z', summary: 'Native result' },
    { ...managed, status: 'exited', finishedAt: '2026-09-12T12:02:00Z', resultExcerpt: 'Managed result' },
  ];
  const done = render();
  assert.equal((done.match(/<details class="r-sa"/g) ?? []).length, 2);
  assert.doesNotMatch(done, /class="bgr-stop"/);
  assert.match(done, /Native result/);
  assert.match(done, /Managed result/);
  assert.equal(renderToStaticMarkup(React.createElement(SpawnedWorkCard, { chat })), '', 'no duplicate terminal cards');
  assert.equal(mutations, 0, 'inspection never invokes a control');

  // A reload with no current transcript header still offers historical results.
  chat.messages = [];
  const history = renderToStaticMarkup(React.createElement(SpawnedWorkCard, { chat }));
  assert.equal((history.match(/<details class="r-sa"/g) ?? []).length, 2);
  assert.match(history, /Native result/);
  assert.doesNotMatch(history, /class="bgr-stop"/);
  assert.equal(mutations, 0);
});
