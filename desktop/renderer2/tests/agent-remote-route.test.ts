import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { createServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { AgentRouteRequest } from '../src/wire/types.ts';
import { tagId } from '../src/wire/machineIds.ts';

const machineId = 'm-san';
const tabId = tagId(machineId, 'tab-hello');
const chatId = tagId(machineId, 'chat-hello');

function remoteChat(overrides: Partial<Chat> = {}): Chat {
  return {
    id: tabId, chatId, machineId, sessionId: 'session-hello',
    title: 'hello', titleLocked: true, group: 'workass', cwd: 'C:\\Users\\dev\\workass',
    currentModelId: 'gpt-5.5[high]', currentModeId: 'agent-full-access',
    providerId: 'devin', providerName: 'Devin', pending: false, draft: '',
    messages: [], messageCount: 0, historyComplete: true,
    ...overrides,
  };
}

function request(method: string, params: Record<string, unknown> = {}): AgentRouteRequest {
  return { requestId: `request-${method}`, method, params, expiresAt: Date.now() + 10_000 };
}

async function loadStore(t: { after(fn: () => void | Promise<void>): void }) {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
  });
  t.after(async () => { await server.close(); });
  const storeModule = await server.ssrLoadModule('/src/store/store.ts');
  const apiModule = await server.ssrLoadModule('/src/wire/api.ts');
  return {
    Store: storeModule.Store as new () => StoreShape,
    setMachineRouter: apiModule.setMachineRouter as (api: unknown) => void,
  };
}

interface StoreShape {
  state: { chats: Chat[]; activeId: string | null; connection: string };
  routeAgentRequest(request: AgentRouteRequest): Promise<unknown>;
}

test('agent MCP projection lists and reads a mounted remote chat without focusing it', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat({
    messages: [{ id: tagId(machineId, 'a-1'), role: 'assistant', content: 'remote answer', result: 'done', status: 'done', at: '2026-09-03T00:00:00Z', events: [] }],
    messageCount: 1,
  })];
  subject.state.activeId = null;

  const listed = await subject.routeAgentRequest(request('chat.list')) as { chats: Array<Record<string, unknown>> };
  assert.equal(listed.chats.length, 1);
  assert.equal(listed.chats[0].tabId, tabId);
  assert.equal(listed.chats[0].chatId, chatId);
  assert.equal(listed.chats[0].machineId, machineId);
  assert.equal(subject.state.activeId, null, 'an MCP read must not select the remote row');

  const read = await subject.routeAgentRequest(request('chat.read', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, limit: 40,
  })) as Record<string, unknown>;
  assert.equal(read.running, false);
  assert.equal((read.messages as Array<Record<string, unknown>>)[0].content, 'remote answer');
  assert.equal(subject.state.activeId, null);
});

test('agent MCP remote auto-send becomes one stable FIFO owner while the chat runs', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  subject.state.connection = 'connected';
  subject.state.chats = [remoteChat({
    messages: [{ id: tagId(machineId, 'a-running'), role: 'assistant', content: '', status: 'running', at: null, events: [], jobId: tagId(machineId, 'job-1') }],
  })];
  const queueWrites: unknown[] = [];
  setMachineRouter({
    chatQueueReplace: async (opts) => {
      queueWrites.push(opts);
      return { ok: true, operationId: opts.operationId, agentQueueRevision: 1, actorRevision: 2 };
    },
  });
  try {
    const params = {
      tab_id: tabId, chat_id: chatId, machine_id: machineId,
      operation_id: 'diagnose-update-once', message: 'why did the update fail?', delivery: 'auto',
    };
    const first = await subject.routeAgentRequest(request('chat.send', params)) as Record<string, unknown>;
    const second = await subject.routeAgentRequest(request('chat.send', params)) as Record<string, unknown>;
    assert.equal(first.queued, true);
    assert.equal(second.queueId, first.queueId);
    assert.equal(subject.state.chats[0].queue?.length, 1, 'a retried MCP call must not duplicate the prompt');
    assert.equal(queueWrites.length, 1, 'the stable duplicate is answered without a second durable queue mutation');
  } finally {
    setMachineRouter(undefined);
  }
});

test('agent MCP remote routes fail closed on a mismatched machine tag', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat()];
  await assert.rejects(
    subject.routeAgentRequest(request('chat.read', {
      tab_id: tabId, chat_id: chatId, machine_id: 'm-other',
    })),
    /exact tab_id \+ chat_id \+ machine_id/,
  );
});

test('agent MCP remote reads drop an indivisible oversized message instead of overflowing transport', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat({
    messages: [{
      id: tagId(machineId, 'a-huge'), role: 'assistant', content: '🚀'.repeat(400_000),
      status: 'done', at: '2026-09-03T00:00:00Z', events: [],
    }],
    messageCount: 1,
  })];
  const read = await subject.routeAgentRequest(request('chat.read', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, limit: 1,
  })) as { messages: unknown[]; truncated: boolean };
  assert.equal(read.truncated, true);
  assert.deepEqual(read.messages, []);
  assert.ok(new TextEncoder().encode(JSON.stringify(read)).byteLength < 768 * 1024);
});
