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
  (globalThis as any).window = {
    api: {
      startJob,
    },
  };
  return subject;
}

function possibleDeliveryError(): Error & { mayHaveBeenAccepted: true } {
  const error = new Error('socket closed after dispatch') as Error & { mayHaveBeenAccepted: true };
  error.name = 'WorkassInvokeError';
  error.mayHaveBeenAccepted = true;
  return error;
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
    assert.equal(Object.hasOwn(calls[0], 'history'), false, 'the actor owns provider history');
    assert.equal(Object.hasOwn(calls[1], 'history'), false, 'a retry must not replay renderer transcript state');
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

test('a sessionless first send creates and admits only through the selected job:start operation', async () => {
  const previousWindow = (globalThis as any).window;
  try {
    const owner = chat('tab-sessionless-send');
    owner.sessionId = null;
    let sessionCreates = 0;
    const calls: Array<Record<string, unknown>> = [];
    const subject = setup(owner, async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      return { id: 'job-sessionless' };
    });
    (globalThis as any).window.api.appChatNewSession = async () => {
      sessionCreates += 1;
      return { sessionId: 'unwanted-preflight', providerId: owner.providerId };
    };

    assert.equal(await subject._send(owner, 'one atomic send'), true);
    assert.equal(sessionCreates, 0, 'send must not create a separate provider session before actor admission');
    assert.equal(calls.length, 1);
    assert.equal(calls[0].providerId, owner.providerId);
    assert.equal(calls[0].operationId, calls[0].userMessageId);
    assert.equal(calls[0].sessionId, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('possible remote acceptance keeps one running owner until the late actor event binds it', async () => {
  const previousWindow = (globalThis as any).window;
  try {
    let submitted!: Record<string, unknown>;
    const owner = chat('tab-possibly-accepted');
    const subject = setup(owner, async (opts: Record<string, unknown>) => {
      submitted = { ...opts };
      throw possibleDeliveryError();
    });

    assert.equal(await subject._send(owner, 'keep exact ownership'), true);
    assert.deepEqual(owner.messages.map((message) => message.id), [submitted.userMessageId, submitted.assistantMessageId]);
    const assistant = owner.messages[1];
    assert.equal(assistant.status, 'running');
    assert.equal(assistant.retryPrompt, undefined);
    assert.ok(subject.pendingTurnStarts.has(owner.id));

    subject.onJobEvent({
      type: 'start',
      job: {
        id: 'job-late-admission', kind: 'app-chat', key: null, title: owner.title, status: 'running',
        startedAt: '2026-08-20T12:00:00Z', finishedAt: null, code: null, permissionMode: 'agent',
        chatId: owner.chatId, tabId: owner.id, sessionId: 'session-late', providerId: owner.providerId,
        userMessageId: submitted.userMessageId, assistantMessageId: submitted.assistantMessageId,
        promptText: 'keep exact ownership', result: null, error: null, stopReason: null, crashInterrupted: false,
      },
    });
    assert.equal(subject.pendingTurnStarts.has(owner.id), false);
    assert.equal(assistant.jobId, 'job-late-admission');

    subject.onJobEvent({ type: 'data', id: 'job-late-admission', stream: 'stdout', chunk: 'arrived once' });
    assert.equal(assistant.content, 'arrived once');
    assert.equal(new Set(owner.messages.map((message) => message.id)).size, 2);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
