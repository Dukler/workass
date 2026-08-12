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
    sessionId: null,
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-delete-retry',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

test('a lost delete reply retries the exact OperationID until the actor receipt arrives', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      chatDelete: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) return undefined;
        return { ok: true, operationId: opts.operationId };
      },
    },
  };
  try {
    const subject = new StoreCtor();
    const owner = chat('tab-delete-retry');
    const other = chat('tab-delete-survivor');
    subject.state.chats = [owner, other];
    subject.state.activeId = owner.id;
    subject.state.meta = { daemon: true };

    await subject.closeChatDurably(owner.id);
    assert.equal(subject.state.chats.some((candidate: Chat) => candidate.id === owner.id), true);

    await subject.closeChatDurably(owner.id);
    assert.equal(subject.state.chats.some((candidate: Chat) => candidate.id === owner.id), false);
    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, calls[0].operationId);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
