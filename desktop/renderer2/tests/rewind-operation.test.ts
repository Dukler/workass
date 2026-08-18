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

function chat(): Chat {
  return {
    id: 'tab-rewind-retry',
    chatId: 'chat-rewind-retry',
    sessionId: 'session-rewind-retry',
    title: 'Rewind retry',
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-rewind-retry',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

test('a failed rewind reply keeps its OperationID until the authoritative receipt', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      chatRewind: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) throw new Error('transport lost');
        return { ok: true, chatId: opts.chatId, turnSeq: opts.turnSeq, repos: [] };
      },
    },
  };
  try {
    const subject = new StoreCtor();
    const owner = chat();
    subject.state.chats = [owner];
    subject.state.activeId = owner.id;
    subject.state.rewind = {
      open: true,
      tabId: owner.id,
      chatId: owner.chatId,
      loading: false,
      items: [],
    };

    await subject.rewindTo(4);
    const operationId = subject.state.rewind.operationId;
    assert.match(operationId, /^checkpoint-restore-/);
    assert.equal(subject.state.rewind.operationTurn, 4);
    assert.equal(subject.state.rewind.busyTurn, undefined);

    await subject.rewindTo(4);
    assert.equal(calls.length, 2);
    assert.equal(calls[0].tabId, owner.id);
    assert.equal(calls[0].chatId, owner.chatId);
    assert.equal(calls[1].tabId, owner.id);
    assert.equal(calls[1].chatId, owner.chatId);
    assert.equal(calls[1].operationId, operationId);
    assert.equal(subject.state.rewind.operationId, undefined);
    assert.equal(subject.state.rewind.operationTurn, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a checkpoint receipt cannot close another chat or machine rewind panel', () => {
  const subject = new StoreCtor();
  const current = chat();
  const other = {
    ...chat(),
    id: 'M~m-remote~tab-other',
    chatId: 'M~m-remote~chat-other',
    sessionId: 'M~m-remote~session-other',
    machineId: 'm-remote',
    title: 'Remote rewind',
  } as Chat;
  subject.state.chats = [current, other];
  subject.state.activeId = current.id;
  subject.state.rewind = {
    open: true,
    tabId: current.id,
    chatId: current.chatId,
    loading: false,
    items: [],
    busyTurn: 4,
  };

  subject.onCheckpointRestored({ chatId: other.chatId, turnSeq: 2, repos: [] });
  assert.equal(subject.state.rewind.open, true);
  assert.equal(subject.state.rewind.busyTurn, 4);
  assert.equal(subject.state.rewind.tabId, current.id);

  subject.onCheckpointRestored({ chatId: current.chatId, turnSeq: 4, repos: [] });
  assert.equal(subject.state.rewind.open, false);
  assert.equal(subject.state.rewind.busyTurn, undefined);
});
