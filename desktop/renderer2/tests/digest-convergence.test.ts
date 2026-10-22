// An idle client must stay quiet. Every heartbeat reads state:digest, and a
// divergence there pulls session:get (the whole mirror) and, before this, one
// spawned-work read for every chat. Three disagreements could never converge,
// so a quiet client re-read everything every five seconds:
//   - a chat handed over from another machine keeps that machine's tag in its
//     stored id, so it was left out of this daemon's chat count;
//   - a live session's reported mode was compared with the actor's saved mode;
//   - a daemon with no processes answers null, and that answer was never hashed.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { after, afterEach, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import type { StateDigest, StateDigestChat } from '../src/wire/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let goJSON: (value: unknown) => string;
const fixtureStores: any[] = [];

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  goJSON = (await vite.ssrLoadModule('/src/store/digest-hash.ts')).goJSON;
});

after(async () => {
  for (const store of fixtureStores) store.clearToastTimers();
  restoreWindow();
  await vite.close();
});

function message(id: string): Msg {
  return { id, role: 'assistant', content: '', result: '', events: [], status: 'done', at: null } as Msg;
}

function chat(id: string, extra: Partial<Chat> = {}): Chat {
  return {
    id, chatId: `chat-${id}`, title: id, group: null, cwd: '/tmp/workass-digest-test',
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, messages: [message(`m-${id}`)], draft: '', ...extra,
  } as Chat;
}

function digestChat(c: Chat, extra: Partial<StateDigestChat> = {}): StateDigestChat {
  return {
    tabId: c.id, chatId: c.chatId!, actorRevision: 0, runningJobId: null,
    lastMessageId: c.messages.at(-1)?.id ?? null, messageCount: c.messages.length,
    queueLen: 0, queueHeadId: null, agentQueueRevision: 0, runtimeControlRevision: 0,
    providerId: c.providerId ?? null, currentModelId: null, currentModeId: null,
    pendingPermissionIds: [], ...extra,
  };
}

function digestOf(chats: StateDigestChat[], extra: Partial<StateDigest> = {}): StateDigest {
  return { chats, globalRevision: 0, catalogHash: {}, settingsRevision: '', procHash: '', ...extra };
}

function subject(chats: Chat[], api: Record<string, unknown> = {}): any {
  (globalThis as any).window = { api };
  const store = new StoreCtor();
  fixtureStores.push(store);
  store.state.chats = chats;
  store.state.activeId = chats[0]?.id ?? null;
  store.schedulePersist = () => {};
  store.requireFullSave = () => {};
  store.localProcHash = '';
  store.localSettingsRevision = '';
  return store;
}

function mirrorOf(chats: Chat[]) {
  return {
    chats: chats.map((c) => ({
      id: c.id, chatId: c.chatId, title: c.title, cwd: c.cwd, providerId: c.providerId,
      messages: c.messages.map((m) => ({ id: m.id, role: m.role, content: m.content, result: m.result, status: m.status, at: m.at, events: [] })),
    })),
    activeId: chats[0]?.id ?? null, seq: 0,
  };
}

// The gate runs renderer files in one process (--test-isolation=none), where a
// top-level after() only runs once every file has finished: restore the global
// window after each test here, or later files see this fixture's window.
const hadWindow = Object.prototype.hasOwnProperty.call(globalThis, 'window');
const previousWindow = (globalThis as any).window;
function restoreWindow() {
  if (hadWindow) (globalThis as any).window = previousWindow;
  else delete (globalThis as any).window;
}

describe('state digest convergence', () => {
afterEach(restoreWindow);

test('a chat handed over from another machine still counts as this daemon\'s chat', () => {
  const local = chat('tab-1');
  const handedOver = chat('M~m-old~tab-2');
  const store = subject([local, handedOver]);
  assert.equal(store.restoreSessionSnapshot(mirrorOf([local, handedOver])), true);

  const scopes = store.digestSyncScopes(digestOf([digestChat(local), digestChat(handedOver)]));

  assert.equal(scopes.has('session'), false, 'a tagged id this daemon lists was counted as another machine\'s chat');
  assert.equal(store.digestSyncScopes(digestOf([digestChat(local)])).has('session'), true, 'a chat the daemon dropped still syncs');
});

test('a live session\'s reported mode is not compared with the saved mode', () => {
  const store = subject([]);
  const restored = store.fromMirror({
    version: 1, activeId: 'tab-1', chats: [{
      id: 'tab-1', chatId: 'chat-tab-1', title: 'live', providerId: 'custom', currentModelId: null, currentModeId: null,
      messages: [], messageCount: 0,
      liveSession: { sessionId: 's-1', providerId: 'custom', currentModeId: 'ask', currentModelId: null },
    }],
  }).chats[0];
  assert.equal(restored.currentModeId, 'ask', 'the composer shows the live mode');
  store.state.chats = [restored];

  const scopes = store.digestSyncScopes(digestOf([digestChat(restored, { providerId: 'custom', lastMessageId: null, messageCount: 0 })]));

  assert.equal(scopes.has('session'), false, 'the live mode looked like a session divergence on every heartbeat');
});

test('an unchanged divergence is read once, then again only when either side changes', async () => {
  const local = chat('tab-1');
  let reads = 0;
  // The daemon's projection keeps answering the same mirror, which still
  // disagrees with its own digest.
  const store = subject([local], { getSession: async () => { reads++; return mirrorOf([local]); } });
  const stuck = digestOf([digestChat(local, { lastMessageId: 'm-elsewhere' })]);

  assert.equal(store.digestSyncScopes(stuck).has('session'), true);
  await store.runScopedSync(new Set(['session']));
  assert.equal(reads, 1);
  for (let heartbeat = 0; heartbeat < 5; heartbeat++) {
    assert.equal(store.digestSyncScopes(stuck).has('session'), false, 'the same stuck state was re-read on a heartbeat');
  }

  const changed = digestOf([digestChat(local, { lastMessageId: 'm-newer' })]);
  assert.equal(store.digestSyncScopes(changed).has('session'), true, 'a new daemon state is read');

  store.digestSyncScopes(stuck);
  store.sessionSyncedAt = Date.now() - 61_000;
  assert.equal(store.digestSyncScopes(stuck).has('session'), true, 'a stuck state is still retried after a minute');
});

test('a local change re-arms the read of an unchanged daemon state', async () => {
  const local = chat('tab-1');
  const store = subject([local], { getSession: async () => mirrorOf([local]) });
  const stuck = digestOf([digestChat(local, { lastMessageId: 'm-elsewhere' })]);
  store.digestSyncScopes(stuck);
  await store.runScopedSync(new Set(['session']));
  assert.equal(store.digestSyncScopes(stuck).has('session'), false);

  store.state.chats[0].messages.push(message('m-optimistic'));
  assert.equal(store.digestSyncScopes(stuck).has('session'), true);
});

test('background work is re-read only for the chats whose digest row changed', async () => {
  const chats = [chat('tab-1'), chat('tab-2'), chat('tab-3')];
  const listed: string[] = [];
  const store = subject(chats, {
    getSession: async () => mirrorOf(chats),
    spawnedWorkList: async (tabId: string) => { listed.push(tabId); return { items: [] }; },
  });
  store.state.hasSpawnedWorkChannels = true;
  const digest = digestOf([digestChat(chats[0]), digestChat(chats[1], { runningJobId: 'job-2' }), digestChat(chats[2])]);

  const scopes = store.digestSyncScopes(digest);
  assert.equal(scopes.has('background'), true);
  await store.runScopedSync(scopes);
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.deepEqual(listed, ['tab-2']);
});

test('a daemon with no processes converges on its null process list', async () => {
  const store = subject([], { procList: async () => ({ processes: null }) });
  store.localProcHash = null;
  const procHash = createHash('sha256').update(goJSON(null)).digest('hex');
  const digest = digestOf([], { procHash });
  assert.equal(store.digestSyncScopes(digest).has('processes'), true);

  await store.runScopedSync(new Set(['processes']));
  for (let i = 0; i < 50 && store.localProcHash !== procHash; i++) await new Promise((resolve) => setTimeout(resolve, 5));

  assert.deepEqual(store.state.processes, []);
  assert.equal(store.digestSyncScopes(digest).has('processes'), false, 'the process list was re-read on every heartbeat');
});

test('an unchanged digest is answered by its etag, and an older daemon still gets plain reads', async () => {
  const local = chat('tab-1');
  const full = { ...digestOf([digestChat(local)]), etag: 'etag-1' };
  const sent: unknown[][] = [];
  const replies: unknown[] = [full, { unchanged: true, etag: 'etag-1' }, { unchanged: true, etag: 'etag-1' }];
  const store = subject([local], { stateDigest: async (...args: unknown[]) => { sent.push(args); return replies.shift(); } });
  const bridge = (globalThis as any).window.api;

  assert.equal(await store.readStateDigest(bridge), full);
  assert.equal(await store.readStateDigest(bridge), full, 'the unchanged reply reuses the last full digest');
  assert.equal(await store.readStateDigest(bridge), full);
  assert.deepEqual(sent, [[], [{ ifNoneMatch: 'etag-1' }], [{ ifNoneMatch: 'etag-1' }]]);
  await assert.rejects(store.readStateDigest({ stateDigest: async () => ({ unchanged: true, etag: 'other' }) }));

  const plain = digestOf([digestChat(local)]);
  const old: unknown[][] = [];
  const older = subject([local], { stateDigest: async (...args: unknown[]) => { old.push(args); return plain; } });
  await older.readStateDigest((globalThis as any).window.api);
  await older.readStateDigest((globalThis as any).window.api);
  assert.deepEqual(old, [[], []], 'a digest without an etag is never read conditionally');
});
});
