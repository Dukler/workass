import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat } from '../src/store/types.ts';
import { LEAN_SESSION_SAVE_MODE } from '../src/store/persistence.ts';

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
    sessionId: 'session-delta-operation',
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-delta-operation',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

function setup(owner: Chat): any {
  const subject = new StoreCtor();
  subject.state.chats = [owner];
  subject.state.activeId = owner.id;
  subject.state.meta = { daemon: true, sessionSaveMode: LEAN_SESSION_SAVE_MODE };
  subject.dirtyChats?.clear();
  subject.dirtyChatVersions?.clear();
  subject.fullSavePending = false;
  subject.schedulePersist = () => {};
  return subject;
}

test('a queue save retries one stable operation after an undefined reply and preserves its dirty fence', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      chatQueueReplace: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) return undefined;
        return { ok: true, operationId: opts.operationId, agentQueueRevision: 1, actorRevision: 1 };
      },
      chatPresentationSave: async (opts: Record<string, unknown>) => ({
        ok: true, operationId: opts.operationId, presentationRevision: 1, actorRevision: 1,
      }),
      saveSession: async () => ({ ok: true, globalRevision: 1 }),
    },
  };
  try {
    const owner = chat('tab-queue-operation');
    const subject = setup(owner);
	owner.queue = [{ id: 'queued-once', text: 'queued once' }];
	subject.markQueueMutation(owner);

    await subject.flushSession();
    assert.equal(subject.dirtyChats.has(owner.id), true);
    await subject.flushSession();

    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(subject.dirtyChats.has(owner.id), false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a presentation save retries one stable operation and refuses a mismatched receipt', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      chatPresentationSave: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) return { ok: true, operationId: 'wrong-operation', presentationRevision: 1, actorRevision: 1 };
        return { ok: true, operationId: opts.operationId, presentationRevision: 1, actorRevision: 1 };
      },
      saveSession: async () => ({ ok: true, globalRevision: 1 }),
    },
  };
  try {
    const owner = chat('tab-presentation-operation');
    const subject = setup(owner);
    owner.title = 'renamed';
    subject.bumpChat(owner);

    await subject.flushSession();
    assert.equal(subject.dirtyChats.has(owner.id), true);
    await subject.flushSession();

    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(subject.dirtyChats.has(owner.id), false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a failed settlement save survives a stale digest and retries the same operation', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      chatPresentationSave: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) {
          return { ok: true, operationId: 'wrong-operation', presentationRevision: 1, actorRevision: 1 };
        }
        return { ok: true, operationId: opts.operationId, presentationRevision: 1, actorRevision: 1 };
      },
      saveSession: async () => ({ ok: true, globalRevision: 1 }),
    },
  };
  try {
    const owner = chat('tab-settlement-fence');
    owner.unread = true;
    const subject = setup(owner);
    const flush = subject.flushSession.bind(subject);
    subject.flushSession = async () => {};

    subject.settleChat(owner.id, true);
    const settledAt = owner.settledAt;
    assert.equal(typeof settledAt, 'number');
    await flush(true);
    assert.equal(subject.dirtyChats.has(owner.id), true);

    const staleServer = subject.toMirror(false, false);
    staleServer.chats[0].settled = undefined;
    staleServer.chats[0].settledAt = undefined;
    staleServer.chats[0].unread = true;
    assert.equal(subject.restoreSessionSnapshot(staleServer), true);
    assert.equal(subject.chat(owner.id)?.settled, 'settled');
    assert.equal(subject.chat(owner.id)?.settledAt, settledAt);
    assert.equal(subject.chat(owner.id)?.unread, false);

    await flush(true);
    assert.equal(calls.length, 2);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(calls[0].settledAt, settledAt);
    assert.equal(calls[1].settledAt, settledAt);
    assert.equal(subject.chat(owner.id)?.settled, 'settled');
    assert.equal(subject.dirtyChats.has(owner.id), false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
