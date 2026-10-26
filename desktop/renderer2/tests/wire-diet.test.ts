// A connect reads what the screen shows and no more, and a reload reuses what
// the window already read once the daemon confirms it is unchanged:
//   - one spawned-work:summary per machine instead of one spawned-work:list
//     per chat, with the old per-chat reads against a daemon without it;
//   - session:get as a shell (no selected tail), answered {unchanged} by etag.
// Nothing the user sees may differ: sidebar rows draw the same running rows
// and obligations, and the chat on screen still reads its full work list.
import assert from 'node:assert/strict';
import { after, afterEach, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let wireCache: any;
const fixtureStores: any[] = [];

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  wireCache = await vite.ssrLoadModule('/src/store/wire-cache.ts');
});

after(async () => {
  for (const store of fixtureStores) store.clearToastTimers();
  restoreGlobals();
  await vite.close();
});

const hadWindow = Object.prototype.hasOwnProperty.call(globalThis, 'window');
const previousWindow = (globalThis as any).window;
const hadSessionStorage = Object.prototype.hasOwnProperty.call(globalThis, 'sessionStorage');
const previousSessionStorage = (globalThis as any).sessionStorage;
function restoreGlobals() {
  if (hadWindow) (globalThis as any).window = previousWindow;
  else delete (globalThis as any).window;
  if (hadSessionStorage) (globalThis as any).sessionStorage = previousSessionStorage;
  else delete (globalThis as any).sessionStorage;
}

function memoryStorage(): Storage {
  const data = new Map<string, string>();
  return {
    get length() { return data.size; },
    clear: () => data.clear(),
    getItem: (key: string) => data.get(key) ?? null,
    key: (index: number) => [...data.keys()][index] ?? null,
    removeItem: (key: string) => { data.delete(key); },
    setItem: (key: string, value: string) => { data.set(key, String(value)); },
  } as Storage;
}

function message(id: string): Msg {
  return { id, role: 'assistant', content: '', result: '', events: [], status: 'done', at: null } as Msg;
}

function chat(id: string): Chat {
  return {
    id, chatId: `chat-${id}`, title: id, group: null, cwd: '/tmp/workass-wire-diet',
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, messages: [message(`m-${id}`)], draft: '',
  } as Chat;
}

function subject(chats: Chat[], api: Record<string, unknown>): any {
  (globalThis as any).window = { api };
  const store = new StoreCtor();
  fixtureStores.push(store);
  store.state.chats = chats;
  store.state.activeId = chats[0]?.id ?? null;
  store.state.hasSpawnedWorkChannels = true;
  store.schedulePersist = () => {};
  store.requireFullSave = () => {};
  return store;
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

function row(id: string, status: string, extra: Record<string, unknown> = {}) {
  return { id, taskId: `task-${id}`, kind: 'agent', label: id, status, startedAt: '2026-10-25T10:00:00Z', ...extra };
}

describe('spawned work at connect', () => {
  afterEach(restoreGlobals);

  test('one summary feeds every sidebar row; only the chat on screen reads its full list', async () => {
    const chats = [chat('tab-1'), chat('tab-2'), chat('tab-3')];
    const listed: string[] = [];
    let summaries = 0;
    const store = subject(chats, {
      spawnedWorkList: async (tabId: string) => {
        listed.push(tabId);
        return { items: [row('w-run', 'running'), row('w-done', 'done')] };
      },
      spawnedWorkSummary: async () => {
        summaries++;
        return {
          chats: [
            { tabId: 'tab-2', chatId: 'chat-tab-2', running: [row('w-2', 'running', { role: 'agent' })], counts: { total: 4, running: 1 },
              obligation: { state: 'needs_input', source: 'turn' } },
            { tabId: 'tab-3', chatId: 'chat-tab-3', running: [], counts: { total: 0, running: 0 }, obligation: { state: 'needs_input', source: 'permission' } },
          ],
        };
      },
    });
    // A stale obligation the daemon no longer reports is cleared.
    store.state.obligationByChat['tab-1\u0000chat-tab-1'] = { state: 'needs_input' };

    store.refreshAllSpawnedWork();
    await settle(); await settle();

    assert.equal(summaries, 1);
    assert.deepEqual(listed, ['tab-1'], 'only the chat on screen reads its full list');
    assert.deepEqual(store.spawnedWork(chats[0]).map((item: any) => item.id), ['w-run', 'w-done'], 'the open chat keeps terminal rows for its rail');
    assert.deepEqual(store.spawnedWork(chats[1]).map((item: any) => item.id), ['w-2']);
    assert.deepEqual(store.spawnedWork(chats[2]), []);
    assert.equal(store.obligation(chats[0]), undefined);
    assert.deepEqual(store.obligation(chats[1]), { state: 'needs_input', source: 'turn' });
    assert.deepEqual(store.obligation(chats[2]), { state: 'needs_input', source: 'permission' }, 'an obligation reaches a late client with no work');
  });

  test('a daemon without the summary gets one list per chat, as before', async () => {
    const chats = [chat('tab-1'), chat('tab-2'), chat('tab-3')];
    const listed: string[] = [];
    const store = subject(chats, {
      spawnedWorkList: async (tabId: string) => { listed.push(tabId); return { items: [], obligation: { state: 'needs_input' } }; },
    });
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    assert.deepEqual(listed.sort(), ['tab-1', 'tab-2', 'tab-3']);
    assert.deepEqual(store.obligation(chats[2]), { state: 'needs_input' });
  });

  test('a failed summary falls back to one list per chat', async () => {
    const chats = [chat('tab-1'), chat('tab-2')];
    const listed: string[] = [];
    const store = subject(chats, {
      spawnedWorkList: async (tabId: string) => { listed.push(tabId); return { items: [] }; },
      spawnedWorkSummary: async () => { throw new Error('unknown channel'); },
    });
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    assert.deepEqual(listed.sort(), ['tab-1', 'tab-2']);
  });

  test('a live event newer than the summary is never overwritten by it', async () => {
    const chats = [chat('tab-1'), chat('tab-2')];
    let release: (value: unknown) => void = () => {};
    const store = subject(chats, {
      spawnedWorkList: async () => ({ items: [] }),
      spawnedWorkSummary: () => new Promise((resolve) => { release = resolve; }),
    });
    store.refreshAllSpawnedWork();
    await settle();
    // The work started while the summary was in flight.
    store.onSpawnedWorkChanged({ tabId: 'tab-2', chatId: 'chat-tab-2', items: [row('w-new', 'running')], obligation: { state: 'needs_input' } });
    release({ chats: [] });
    await settle(); await settle();
    assert.deepEqual(store.spawnedWork(chats[1]).map((item: any) => item.id), ['w-new']);
    assert.deepEqual(store.obligation(chats[1]), { state: 'needs_input' });
  });

  test('another machine answers its own summary, tagged; one that predates it is read per chat', async () => {
    const remoteChats = [chat('M~m1~tab-r1'), chat('M~m1~tab-r2')];
    for (const remote of remoteChats) remote.chatId = remote.id.replace('M~m1~', 'M~m1~chat-');
    const local = chat('tab-1');
    const listed: string[] = [];
    const invoked: string[] = [];
    const store = subject([local, ...remoteChats], {
      spawnedWorkList: async (tabId: string) => { listed.push(tabId); return { items: [] }; },
      spawnedWorkSummary: async () => ({ chats: [] }),
    });
    store.machines = {
      linkFor: () => ({ invoke: async (channel: string) => {
        invoked.push(channel);
        return { chats: [{ tabId: 'tab-r2', chatId: 'chat-tab-r2', running: [row('w-r', 'running')], obligation: { state: 'needs_input' } }] };
      } }),
    };
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    assert.deepEqual(invoked, ['spawned-work:summary']);
    assert.deepEqual(store.spawnedWork(remoteChats[1]).map((item: any) => item.id), ['M~m1~w-r'], 'remote ids come back tagged');
    assert.deepEqual(store.obligation(remoteChats[1]), { state: 'needs_input' });

    invoked.length = 0;
    store.machines = { linkFor: () => ({ invoke: async (channel: string) => { invoked.push(channel); throw new Error('unknown channel'); } }) };
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    assert.deepEqual(invoked, ['spawned-work:summary']);
    assert.deepEqual(listed.filter((id) => id.startsWith('M~')).sort(), ['M~m1~tab-r1', 'M~m1~tab-r2']);
  });

  test('an unchanged summary is answered by its etag and applied from the kept text', async () => {
    const chats = [chat('tab-1'), chat('tab-2')];
    const sent: unknown[][] = [];
    const full = { etag: 'sum-1', chats: [{ tabId: 'tab-2', chatId: 'chat-tab-2', running: [row('w-2', 'running')], obligation: { state: 'needs_input' } }] };
    const replies: unknown[] = [full, { unchanged: true, etag: 'sum-1' }];
    const store = subject(chats, {
      spawnedWorkList: async () => ({ items: [] }),
      spawnedWorkSummary: async (...args: unknown[]) => { sent.push(args); return replies.shift(); },
    });
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    // Work settled locally in between; the receipt re-applies the daemon's
    // summary exactly, so the row and obligation come back.
    store.state.spawnedWorkByChat = {};
    store.state.obligationByChat = {};
    store.refreshAllSpawnedWork();
    await settle(); await settle();
    assert.deepEqual(sent, [[], [{ ifNoneMatch: 'sum-1' }]]);
    assert.deepEqual(store.spawnedWork(chats[1]).map((item: any) => item.id), ['w-2']);
    assert.deepEqual(store.obligation(chats[1]), { state: 'needs_input' });
  });

  test('this daemon\'s full list is read by etag; another machine\'s is read as before', async () => {
    const local = chat('tab-1');
    const remote = chat('M~m1~tab-r');
    remote.chatId = 'M~m1~chat-tab-r';
    const sent: unknown[][] = [];
    const full = { etag: 'list-1', items: [row('w-run', 'running'), row('w-done', 'done')], obligation: { state: 'needs_input' } };
    const replies: unknown[] = [full, { unchanged: true, etag: 'list-1' }, { items: [] }];
    const store = subject([local, remote], {
      spawnedWorkList: async (...args: unknown[]) => { sent.push(args); return replies.shift(); },
    });
    await store.refreshSpawnedWork(local);
    store.state.spawnedWorkByChat = {};
    await store.refreshSpawnedWork(local);
    assert.deepEqual(sent[0], ['tab-1', 'chat-tab-1', { ifNoneMatch: '' }]);
    assert.deepEqual(sent[1], ['tab-1', 'chat-tab-1', { ifNoneMatch: 'list-1' }]);
    assert.deepEqual(store.spawnedWork(local).map((item: any) => item.id), ['w-run', 'w-done'], 'the receipt re-applies the kept list');
    assert.deepEqual(store.obligation(local), { state: 'needs_input' });
    await store.refreshSpawnedWork(remote);
    assert.deepEqual(sent[2], ['M~m1~tab-r', 'M~m1~chat-tab-r']);
  });

  test('a digest that makes many chats look new reads one summary, not a list each', async () => {
    const chats = [chat('tab-1'), chat('tab-2'), chat('tab-3')];
    const listed: string[] = [];
    let summaries = 0;
    const store = subject(chats, {
      spawnedWorkList: async (tabId: string) => { listed.push(tabId); return { items: [] }; },
      spawnedWorkSummary: async () => { summaries++; return { chats: [] }; },
    });
    for (const c of chats) store.syncBackgroundPairs.set(`${c.id}\u0000${c.chatId}`, { tabId: c.id, chatId: c.chatId });
    await store.runScopedSync(new Set(['background']));
    await settle(); await settle();
    assert.equal(summaries, 1);
    assert.deepEqual(listed, ['tab-1'], 'only the chat on screen reads its full list');
  });
});

function shell(chats: Chat[], etag?: string) {
  return {
    ...(etag ? { etag } : {}),
    chats: chats.map((c) => ({ id: c.id, chatId: c.chatId, title: c.title, cwd: c.cwd, providerId: c.providerId, messages: [], messageCount: 1, historyComplete: false })),
    activeId: chats[0]?.id ?? null, seq: 0,
  };
}

describe('boot', () => {
  afterEach(restoreGlobals);

  test('a session refresh during boot is answered by one digest after boot, not one before it', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((resolve) => { release = resolve; });
    let refresh: (event: unknown) => void = () => {};
    let digests = 0;
    const store = subject([], {
      getSession: async () => { await gate; return shell([chat('tab-1')]); },
      stateDigest: async () => { digests++; return { chats: [], globalRevision: 0, catalogHash: {}, settingsRevision: '', procHash: '' }; },
      appMeta: async () => ({ rootDir: '/tmp', workspaceDir: '/tmp' }),
      onAgentApply: (cb: (event: unknown) => void) => { refresh = cb; },
    });
    (globalThis as any).window.addEventListener = () => {};
    const booted = store.init();
    await settle();
    refresh({ action: 'session-refresh' });
    await settle();
    assert.equal(digests, 0, 'a digest was read before the session it compares against');
    release();
    await booted;
    await settle(); await settle();
    assert.equal(digests, 1);
    store.monitor?.stop();
  });
});

describe('session shell by etag', () => {
  afterEach(restoreGlobals);

  test('the shell is read once, then answered unchanged and reused from the kept text', async () => {
    const chats = [chat('tab-1'), chat('tab-2')];
    const sent: unknown[] = [];
    const replies: unknown[] = [shell(chats, 'etag-1'), { unchanged: true, etag: 'etag-1' }];
    const store = subject(chats, { getSession: async (options: unknown) => { sent.push(options); return replies.shift(); } });

    const first = await store.readSessionShell('test');
    assert.equal(first.ok, true);
    assert.equal((sent[0] as any).shell, true);
    assert.equal((sent[0] as any).ifNoneMatch, undefined);
    const second = await store.readSessionShell('test');
    assert.equal((sent[1] as any).ifNoneMatch, 'etag-1');
    assert.deepEqual(second.value, first.value, 'the kept shell is exactly the shell the daemon sent');
    assert.notEqual(second.value, first.value, 'parsed afresh, never the object the store already hydrated from');
  });

  test('an older daemon answers the ordinary projection and nothing is kept', async () => {
    const chats = [chat('tab-1')];
    const sent: unknown[] = [];
    const store = subject(chats, { getSession: async (options: unknown) => { sent.push(options); return shell(chats); } });
    await store.readSessionShell('test');
    await store.readSessionShell('test');
    assert.equal(sent.length, 2);
    assert.equal((sent[1] as any).ifNoneMatch, undefined, 'no etag was named, so none is sent back');
  });

  test('a shell carrying a running chat\'s rows is not kept', async () => {
    const chats = [chat('tab-1')];
    const sent: unknown[] = [];
    const running = shell(chats, 'etag-live');
    (running.chats[0] as any).messages = [{ id: 'm-1', role: 'assistant', content: '', status: 'running', at: null, events: [] }];
    const store = subject(chats, { getSession: async (options: unknown) => { sent.push(options); return running; } });
    await store.readSessionShell('test');
    await store.readSessionShell('test');
    assert.equal((sent[1] as any).ifNoneMatch, undefined);
  });

  test('a receipt for another etag is not trusted: the shell is read again without one', async () => {
    const chats = [chat('tab-1')];
    const sent: unknown[] = [];
    const replies: unknown[] = [shell(chats, 'etag-1'), { unchanged: true, etag: 'etag-other' }, shell(chats, 'etag-2')];
    const store = subject(chats, { getSession: async (options: unknown) => { sent.push(options); return replies.shift(); } });
    await store.readSessionShell('test');
    const again = await store.readSessionShell('test');
    assert.equal(sent.length, 3);
    assert.equal((sent[2] as any).ifNoneMatch, undefined);
    assert.equal((again.value as any).etag, 'etag-2');
  });

  test('a reload reuses the shell and digest the previous load kept', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const chats = [chat('tab-1')];
    const digest = { chats: [], globalRevision: 0, catalogHash: {}, settingsRevision: '', procHash: '', etag: 'digest-1' };
    const before = subject(chats, { getSession: async () => shell(chats, 'etag-1') });
    await before.readSessionShell('test');
    before.lastFullDigest = digest;
    before.persistWireCache();

    const sent: unknown[] = [];
    const digestArgs: unknown[] = [];
    const after = subject(chats, {
      getSession: async (options: unknown) => { sent.push(options); return { unchanged: true, etag: 'etag-1' }; },
      stateDigest: async (...args: unknown[]) => { digestArgs.push(args); return { unchanged: true, etag: 'digest-1' }; },
    });
    (globalThis as any).window.addEventListener = () => {};
    after.installWireCachePersistence();
    const read = await after.readSessionShell('test');
    assert.equal((sent[0] as any).ifNoneMatch, 'etag-1');
    assert.deepEqual(read.value, shell(chats, 'etag-1'));
    assert.deepEqual(await after.readStateDigest((globalThis as any).window.api), digest);
    assert.deepEqual(digestArgs, [[{ ifNoneMatch: 'digest-1' }]]);
  });

  test('wire cache entries round-trip and refuse what they cannot keep', () => {
    (globalThis as any).sessionStorage = memoryStorage();
    wireCache.writeWireCache('x', 'etag-1', '{"a":1}');
    assert.deepEqual(wireCache.readWireCache('x'), { etag: 'etag-1', text: '{"a":1}' });
    assert.deepEqual(wireCache.parseWireCache(wireCache.readWireCache('x')), { a: 1 });
    wireCache.writeWireCache('x', '', '');
    assert.equal(wireCache.readWireCache('x'), null);
    wireCache.writeWireCache('x', 'etag-2', 'y'.repeat(4_000_001));
    assert.equal(wireCache.readWireCache('x'), null, 'an oversized entry is dropped, never left stale');
    assert.equal(wireCache.isUnchangedReply({ unchanged: true, etag: 'a' }, 'a'), true);
    assert.equal(wireCache.isUnchangedReply({ unchanged: true, etag: 'a' }, 'b'), false);
    assert.equal(wireCache.isUnchangedReply({ unchanged: true, etag: 'a' }, ''), false);
  });
});
