import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat } from '../src/store/types.ts';

// Store-level contract for the message-scoped chat fork (D5): the renderer's
// toast wording is decided by the daemon's forkedFrom.atMessageId echo, the
// optimistic child row is fenced with overwriteChatCreates until the
// authoritative projection echoes it, and a refused fork releases that fence
// while keeping the retry anchor. The echo-into-toast branch had no test
// coverage (review follow-up wa-subagent-1792166920817-4).

let vite: ViteDevServer;
let StoreCtor: new () => any;
const fixtureStores: any[] = [];

function ownStore<T extends { clearToastTimers(): void }>(store: T): T {
  fixtureStores.push(store);
  return store;
}

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});

after(async () => {
  for (const store of fixtureStores) store.clearToastTimers();
  await vite.close();
});

function sourceChat(id: string): Chat {
  return {
    id,
    chatId: `chat-${id}`,
    sessionId: 'ses-source-1',
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-chat-fork-store',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

function seedStore(store: any, id: string): Chat {
  const owner = sourceChat(id);
  store.state.chats = [owner];
  store.state.activeId = owner.id;
  store.state.meta = { daemon: true };
  store.state.seq = 5;
  return owner;
}

function childRowOf(store: any, ownerId: string): Chat | undefined {
  return store.state.chats.find((chat: Chat) => chat.id !== ownerId);
}

async function withWindow(api: Record<string, unknown>, run: () => Promise<void>) {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api };
  try {
    await run();
  } finally {
    (globalThis as any).window = previousWindow;
  }
}

test('a message-scoped fork adopts the reply, clears the retry anchor, and toasts success', async () => {
  const calls: Array<Record<string, unknown>> = [];
  await withWindow({
    appChatFork: async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      return {
        sessionId: 'ses-child-1', providerId: 'mock',
        forkedFrom: { tabId: 'tab-fork-ok', atTurn: 1, atMessageId: 'msg-1' },
      };
    },
  }, async () => {
    const subject = ownStore(new StoreCtor());
    const owner = seedStore(subject, 'tab-fork-ok');

    await subject.forkFromMessage(owner.id, 'msg-1');

    assert.equal(calls.length, 1);
    assert.equal(calls[0].atMessageId, 'msg-1');
    assert.equal(calls[0].cwd, owner.cwd);
    assert.ok(String(calls[0].newTabId).startsWith('tab-'), 'renderer-minted child tab id');

    const child = childRowOf(subject, owner.id);
    assert.ok(child, 'optimistic child row present');
    assert.equal(child!.sessionId, 'ses-child-1');
    assert.equal(child!.pending, false);
    assert.equal((owner as any)._forkAttempt, undefined);
    // The create fence survives until the authoritative projection echoes the
    // committed child row.
    assert.equal((subject as any).pendingChatCreates.has(child!.id), true);

    assert.equal(subject.state.activeId, child!.id);
    const toast = subject.state.toasts.at(-1);
    assert.equal(toast.title, 'Bifurcación creada');
    assert.equal(toast.body, 'La conversación continúa en la copia nueva.');
  });
});

test('a reply without the atMessageId echo warns that the copy may include the whole history', async () => {
  await withWindow({
    appChatFork: async () => ({
      sessionId: 'ses-child-2', providerId: 'mock',
      forkedFrom: { tabId: 'tab-fork-echoless', atTurn: 1 },
    }),
  }, async () => {
    const subject = ownStore(new StoreCtor());
    const owner = seedStore(subject, 'tab-fork-echoless');

    await subject.forkFromMessage(owner.id, 'msg-2');

    // An older daemon ignores atMessageId and copies the whole ledger; a
    // binary that honored the id before the echo existed would copy the
    // hovered turn without echoing. "Puede incluir todo el historial" is the
    // only phrasing true in both version mixes.
    const toast = subject.state.toasts.at(-1);
    assert.equal(toast.title, 'Bifurcación creada');
    assert.equal(toast.body, 'El daemon no confirmó la bifurcación por mensaje: la copia puede incluir todo el historial.');
  });
});

test('a refused fork releases the create fence and keeps the retry anchor', async () => {
  const calls: Array<Record<string, unknown>> = [];
  await withWindow({
    appChatFork: async (opts: Record<string, unknown>) => {
      calls.push({ ...opts });
      throw new Error('fork source message "msg-3" is not in the chat ledger');
    },
  }, async () => {
    const subject = ownStore(new StoreCtor());
    const owner = seedStore(subject, 'tab-fork-refused');

    await subject.forkFromMessage(owner.id, 'msg-3');

    assert.equal(calls.length, 1);
    const child = childRowOf(subject, owner.id);
    assert.ok(child, 'optimistic child row still present until reconciliation');
    assert.equal(child!.sessionId, null);
    assert.ok(child!.sessionError, 'sessionError explains the refusal');
    // The create fence is released so the next authoritative projection drops
    // the ghost row; the anchor survives so one more click retries durably.
    assert.equal((subject as any).pendingChatCreates.has(child!.id), false);
    assert.equal((subject as any).remoteChatCreateFences.has(child!.id), false);
    const attempt = (owner as any)._forkAttempt;
    assert.ok(attempt, 'retry anchor kept');
    assert.equal(attempt.atMessageId, 'msg-3');
    assert.equal(attempt.cwd, owner.cwd);

    // A retry re-sends the exact tracked request (ids + hovered point + cwd).
    await subject.forkFromMessage(owner.id, 'msg-3');
    assert.equal(calls.length, 2);
    assert.equal(calls[1].operationId, calls[0].operationId);
    assert.equal(calls[1].cwd, calls[0].cwd);
    assert.equal(calls[1].atMessageId, 'msg-3');
  });
});
