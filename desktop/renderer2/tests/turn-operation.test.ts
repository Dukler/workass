import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat } from '../src/store/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});

after(async () => { await vite.close(); });

function chat(id: string): Chat {
  return {
    id,
    chatId: `chat-${id}`,
    sessionId: `session-${id}`,
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-turn-operation',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

function setup(owner: Chat, startJob: (...args: any[]) => Promise<unknown>): any {
  const subject = new StoreCtor();
  subject.state.chats = [owner];
  subject.state.activeId = owner.id;
  subject.state.connection = 'connected';
  subject.schedulePersist = () => {};
  (globalThis as any).window = { api: { startJob } };
  return subject;
}

test('retryTurn reuses the original job:start OperationID and exact message pair', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  try {
    const subject = setup(chat('tab-turn-retry'), async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      attempts += 1;
      return attempts === 1 ? undefined : { id: 'job-retried' };
    });
    const owner = subject.chat('tab-turn-retry');
    owner.messages.push(
      { id: 'prior-user', role: 'user', content: 'earlier prompt', status: 'done', at: '2026-08-12T00:00:00Z', events: [] },
      { id: 'prior-assistant', role: 'assistant', content: 'earlier answer', status: 'done', at: '2026-08-12T00:00:01Z', events: [] },
    );

    assert.equal(await subject._send(owner, 'retry this'), false);
    const failed = [...owner.messages].reverse().find((message: any) => message.role === 'assistant');
    assert.ok(failed?.retryPrompt);
    const userID = [...owner.messages].reverse().find((message: any) => message.role === 'user')?.id;
    const assistantID = failed.id;

    await subject.retryTurn(owner.id, assistantID);
    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(calls[1].operationId, userID);
    assert.equal(calls[1].userMessageId, userID);
    assert.equal(calls[1].assistantMessageId, assistantID);
    assert.deepEqual(calls[1].history, calls[0].history);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a queued head reuses its operation and assistant identities after a lost job:start reply', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  try {
    const owner = chat('tab-queued-turn-retry');
    owner.queue = [{ id: 'queue-head-retry', text: 'queued once' }];
    const subject = setup(owner, async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      attempts += 1;
      return attempts === 1 ? undefined : { id: 'job-queued-retried' };
    });

    await subject.flushNextQueued(owner);
    await subject.flushNextQueued(owner);

    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(calls[0].operationId, 'queue-head-retry');
    assert.equal(calls[1].userMessageId, calls[0].userMessageId);
    assert.equal(calls[1].assistantMessageId, calls[0].assistantMessageId);
    assert.equal(owner.queue, undefined);
    assert.equal(new Set(owner.messages.map((message: any) => message.id)).size, owner.messages.length);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a definite queued rejection does not feed the current operation back as provider history', async () => {
  const calls: Array<Record<string, any>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  try {
    const owner = chat('tab-queued-history-retry');
    owner.queue = [{ id: 'queue-history-retry', text: 'do not duplicate this' }];
    const subject = setup(owner, async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      attempts += 1;
      // null models a definite start rejection after the renderer has created
      // its optimistic pair; the next FIFO attempt is the same operation.
      return attempts === 1 ? null : { id: 'job-history-retried' };
    });

    await subject.flushNextQueued(owner);
    await subject.flushNextQueued(owner);

    assert.equal(calls.length, 2);
    const history = Array.isArray(calls[1].history) ? calls[1].history : [];
    assert.equal(history.some((row: any) => row.id === 'queue-history-retry'), false);
    assert.equal(history.some((row: any) => row.id === 'queue-assistant-queue-history-retry'), false);
    assert.equal(calls[1].operationId, 'queue-history-retry');
    assert.equal(owner.queue, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
