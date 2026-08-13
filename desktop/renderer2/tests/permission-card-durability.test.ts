// An open permission/question card is actor-owned semantic state. The daemon's
// session projection must carry it, and a wholesale restore must neither drop
// an authoritative card nor resurrect a stale renderer-local one.
//
// The trigger was the count on the other side of the same digest: another machine's
// chats are carried across every restore but appear in no digest THIS daemon can
// produce, so `digest.chats.length !== state.chats.length` was permanently true and
// declared the session diverged forever.
import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import type { StateDigest, StateDigestChat } from '../src/wire/types.ts';

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

function message(id: string, extra: Partial<Msg> = {}): Msg {
  return { id, role: 'assistant', content: '', result: '', events: [], status: 'running', at: null, ...extra } as Msg;
}

function chat(id: string, messages: Msg[], extra: Partial<Chat> = {}): Chat {
  return {
    id, chatId: `chat-${id}`, title: id, group: null, cwd: '/tmp/workass-perm-test',
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, messages, draft: '', ...extra,
  } as Chat;
}

function mirrorOf(chats: Chat[]) {
  return {
    chats: chats.map((c) => ({
      id: c.id, chatId: c.chatId, title: c.title, cwd: c.cwd, providerId: c.providerId,
      messages: c.messages.map((m) => ({
        id: m.id, role: m.role, content: m.content, result: m.result, status: m.status, at: m.at,
        jobId: m.jobId, permission: m.permission, turnStartedAt: m.turnStartedAt,
        interrupted: m.interrupted, retryPrompt: m.retryPrompt, events: m.events,
      })),
    })),
    activeId: chats[0]?.id ?? null, seq: 0,
  };
}

function subject(chats: Chat[]): any {
  const store = new StoreCtor();
  store.state.chats = chats;
  store.state.activeId = chats[0]?.id ?? null;
  store.schedulePersist = () => {};
  store.requireFullSave = () => {};
  return store;
}

const ASK = {
  id: 'perm-ask-1', title: 'AskUserQuestion', kind: 'ask', options: [{ optionId: 'a', name: 'Full', kind: 'answer' }],
  question: { question: '¿Qué alcance?', header: 'Alcance', options: [{ label: 'Full', description: 'queue → stable' }] },
};

test('a session restore keeps the question card the user is reading', () => {
  const open = chat('tab-1', [message('m-1', { permission: ASK as never })]);
  const store = subject([open]);

  assert.equal(store.restoreSessionSnapshot(mirrorOf([open])), true);

  const restored = store.state.chats[0].messages[0];
  assert.equal(restored.permission?.id, 'perm-ask-1', 'the open card was dropped by the restore');
  assert.equal(restored.permission?.question?.question, '¿Qué alcance?', 'the question lost its body and falls back to a permission card');
});

test('a restore does not resurrect a card on a message that no longer holds one', () => {
  const local = chat('tab-1', [message('m-1', { permission: ASK as never })]);
  const authoritative = chat('tab-1', [message('m-1')]);
  const store = subject([local]);

  assert.equal(store.restoreSessionSnapshot(mirrorOf([authoritative])), true);

	assert.equal(store.state.chats[0].messages[0].permission, undefined);
});

test('actor hydration preserves terminal recovery and running-start fields', () => {
  const authoritative = chat('tab-1', [message('m-1', {
    status: 'failed', interrupted: true, retryPrompt: 'try again', turnStartedAt: 1234,
  })]);
  const store = subject([]);

  assert.equal(store.restoreSessionSnapshot(mirrorOf([authoritative])), true);
  const restored = store.state.chats[0].messages[0];
  assert.equal(restored.interrupted, true);
  assert.equal(restored.retryPrompt, 'try again');
  assert.equal(restored.turnStartedAt, 1234);
});

function digestChat(c: Chat, extra: Partial<StateDigestChat> = {}): StateDigestChat {
  return {
    tabId: c.id, chatId: c.chatId!, actorRevision: c.actorRevision ?? 0, runningJobId: null, lastMessageId: c.messages.at(-1)?.id ?? null,
    messageCount: c.messages.length, queueLen: 0, queueHeadId: null,
    agentQueueRevision: 0, runtimeControlRevision: 0,
    providerId: c.providerId ?? null, currentModelId: null, currentModeId: null,
    pendingPermissionIds: [], ...extra,
  };
}

function digestOf(chats: StateDigestChat[]): StateDigest {
  return { chats, globalRevision: 0, catalogHash: {}, settingsRevision: '', procHash: '' };
}

test('a mounted machine does not make this daemon look permanently diverged', () => {
  const local = chat('tab-1', [message('m-1', { status: 'done' })]);
  const remote = chat('m~builder~tab-9', [message('m-9', { status: 'done' })], { machineId: 'm-builder' } as Partial<Chat>);
  const store = subject([local, remote]);
  store.localProcHash = '';
  store.localSettingsRevision = '';

  const scopes = store.digestSyncScopes(digestOf([digestChat(local)]));

  assert.equal(scopes.has('session'), false, 'another machine\'s chats count against this daemon\'s digest');
});

test('a chat this daemon really lost still forces a session sync', () => {
  const kept = chat('tab-1', [message('m-1', { status: 'done' })]);
  const dropped = chat('tab-2', [message('m-2', { status: 'done' })]);
  const store = subject([kept, dropped]);
  store.localProcHash = '';
  store.localSettingsRevision = '';

  const scopes = store.digestSyncScopes(digestOf([digestChat(kept)]));

  assert.equal(scopes.has('session'), true, 'a real chat-count divergence no longer synchronizes');
});

test('bounded history count does not force sync while a changed tail identity does', () => {
  const local = chat('tab-1', [message('m-1', { status: 'done' })]);
  const store = subject([local]);
  store.localProcHash = '';
  store.localSettingsRevision = '';

  assert.equal(store.digestSyncScopes(digestOf([digestChat(local, { messageCount: 2 })])).has('session'), false);
  assert.equal(store.digestSyncScopes(digestOf([digestChat(local, { lastMessageId: 'different' })])).has('session'), true);
});

test('an inactive bounded history relies on actor revision and tail identity', () => {
  const complete = [message('m-1', { status: 'done' }), message('m-2', { status: 'done' })];
  const local = chat('tab-1', complete.slice(-1));
  local.messageCount = complete.length;
  local.historyComplete = false;
  const store = subject([local]);
  store.localProcHash = '';
  store.localSettingsRevision = '';

  assert.equal(store.digestSyncScopes(digestOf([digestChat(local, {
    messageCount: complete.length,
    lastMessageId: 'm-2',
  })])).has('session'), false);
});
