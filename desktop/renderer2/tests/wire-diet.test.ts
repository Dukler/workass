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
// Paged reads (store/session-page.ts) add {page} to every local read; these
// tests are about the rest of the arguments.
function withoutPage(calls: unknown[][]): unknown[][] {
  return calls.map((args) => args.map((arg) => {
    if (!arg || typeof arg !== 'object' || Array.isArray(arg)) return arg;
    const { page: _page, ...rest } = arg as Record<string, unknown>;
    return rest;
  }));
}


before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true, ws: false },
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
    assert.deepEqual(withoutPage(sent), [[{}], [{ ifNoneMatch: 'sum-1' }]]);
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
    assert.deepEqual(sent[0], ['tab-1', 'chat-tab-1', { ifNoneMatch: '', brief: true }]);
    assert.deepEqual(sent[1], ['tab-1', 'chat-tab-1', { ifNoneMatch: 'list-1', brief: true }]);
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
    assert.deepEqual(withoutPage(digestArgs as unknown[][]), [[{ ifNoneMatch: 'digest-1' }]]);
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

describe('long chats: brief spawned work and work groups', () => {
  afterEach(restoreGlobals);

  test('a brief list keeps settled agents as card statuses and reads rows whole when shown', async () => {
    const local = chat('tab-1');
    const sent: any[] = [];
    const replies: unknown[] = [
      { brief: true, items: [row('w-run', 'running')], settled: [['call-old', 'failed']], omitted: 3 },
      { brief: true, items: [row('w-run', 'running'), row('old', 'failed', { toolCallId: 'call-old', summary: 'whole' })], settled: [], omitted: 2 },
      { items: [row('w-run', 'running'), row('old', 'failed', { toolCallId: 'call-old' }), row('older', 'done')] },
    ];
    const store = subject([local], { spawnedWorkList: async (...args: unknown[]) => { sent.push(args[2]); return replies.shift(); } });
    await store.refreshSpawnedWork(local);
    assert.deepEqual(sent[0], { ifNoneMatch: '', brief: true });
    const items = store.spawnedWork(local);
    assert.deepEqual(items.map((item: any) => [item.id, item.status, item.brief === true]), [['w-run', 'running', false], ['call-old', 'failed', true]]);
    // The rail draws the settled agent: its row comes whole.
    store.wantSpawnedWorkRows(local, ['call-old']);
    await settle(); await settle();
    assert.deepEqual(sent[1], { ifNoneMatch: '', brief: true, keep: ['call-old'] });
    assert.equal(store.spawnedWork(local).find((item: any) => item.id === 'old')?.summary, 'whole');
    // Older finished work on screen: the whole list.
    store.wantFullSpawnedWork(local);
    await settle(); await settle();
    assert.deepEqual(sent[2], { ifNoneMatch: '' });
    assert.equal(store.spawnedWork(local).length, 3);
  });

  test('a folded row reads its calls page by page when shown, and keeps them', async () => {
    const local = chat('tab-1');
    const folded = message('m-big');
    folded.workSummary = { tools: 3, subagentCalls: 0, failed: 0, events: 3 };
    folded.events = [{ kind: 'tool', key: 'k0', at: 0, id: null, title: '', status: '', workStub: true } as any];
    local.messages = [folded];
    const calls: any[] = [];
    const tool = (index: number) => ({ kind: 'tool', key: `k${index}`, at: 0, id: `c${index}`, title: `call ${index}`, status: 'completed', toolKind: 'read', command: null, terminalId: null, input: '', inputDeferred: true, output: 'o', location: null });
    const pages: unknown[] = [
      { messageId: 'm-big', offset: 0, next: 2, total: 3, events: [tool(0), tool(1)] },
      { messageId: 'm-big', offset: 2, next: 3, total: 3, events: [tool(2)] },
    ];
    const store = subject([local], { archiveLoad: async (_tab: string, options: any) => { calls.push(options); return pages.shift(); } });
    assert.equal(await store.loadWorkGroup('tab-1', folded), true);
    // The first page is the list's first screen; the rest is read behind
    // anything a reader waits for.
    assert.deepEqual(calls.map((options) => [options.tail, options.workGroup, options.toolSummaries, options.workGroups, options.background]), [
      [1, { messageId: 'm-big', offset: 0, limit: 80, compact: true, ifNoneMatch: '' }, true, undefined, undefined],
      [1, { messageId: 'm-big', offset: 2, compact: true }, true, undefined, true],
    ]);
    assert.equal(folded.workSummary, undefined);
    assert.deepEqual(folded.events.map((event: any) => event.key), ['k0', 'k1', 'k2']);
    assert.deepEqual((folded.events[0] as any).detail, { tabId: 'tab-1', messageId: 'm-big' });
    // A refresh brings the summary back: the kept calls apply without a read.
    const again = message('m-big');
    again.workSummary = folded.workSummary ?? { tools: 3, subagentCalls: 0, failed: 0, events: 3 };
    local.messages = [again];
    assert.equal(await store.loadWorkGroup('tab-1', again), true);
    assert.equal(calls.length, 2);
    assert.equal(again.events.length, 3);
  });

  test('compact pages restore every call exactly; each page is drawn as it arrives', async () => {
    const local = chat('tab-3');
    const folded = message('m-c');
    folded.workSummary = { tools: 3, subagentCalls: 0, failed: 0, events: 3 };
    local.messages = [folded];
    const full = (index: number) => ({ at: 0, command: index === 1 ? 'ls -la' : null, id: `c${index}`, input: '', inputDeferred: true, key: `timeline:tool:c${index}`, kind: 'tool', location: null, output: 'o', status: 'completed', terminalId: null, title: index === 1 ? 'ls -la' : `call ${index}`, toolKind: 'read' });
    const compact = (index: number) => {
      const event: any = { ...full(index) };
      for (const field of ['command', 'location', 'terminalId']) if (event[field] === null) delete event[field];
      delete event.key;
      if (event.title === event.command) { delete event.title; event.titleIs = 'command'; }
      return event;
    };
    const pages: unknown[] = [
      { etag: 'e0', messageId: 'm-c', offset: 0, next: 2, total: 3, events: [compact(0), compact(1)] },
      { etag: 'e2', messageId: 'm-c', offset: 2, next: 3, total: 3, events: [compact(2)] },
    ];
    let partialSeen: number[] = [];
    const store = subject([local], { archiveLoad: async () => { partialSeen.push(folded.workPartial?.events.length ?? 0); return pages.shift(); } });
    assert.equal(await store.loadWorkGroup('tab-3', folded), true);
    assert.deepEqual(partialSeen, [0, 2], 'the first page is on screen while the second is read');
    assert.equal(folded.workPartial, undefined);
    for (let index = 0; index < 3; index++) {
      const { detail, ...event } = folded.events[index] as any;
      assert.deepEqual(event, full(index));
    }
  });

  test('a first page read ahead of an open, or kept from before a reload, is not read again', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const local = chat('tab-4');
    const folded = message('m-p');
    folded.workSummary = { tools: 1, subagentCalls: 0, failed: 0, events: 1 };
    local.messages = [folded];
    const page = { etag: 'p0', messageId: 'm-p', offset: 0, next: 1, total: 1, events: [{ at: 0, id: 'c0', kind: 'tool', status: 'completed', title: 't', toolKind: 'read', input: '', output: 'o' }] };
    const calls: any[] = [];
    const store = subject([local], { archiveLoad: async (_tab: string, options: any) => { calls.push(options); return calls.length === 1 ? JSON.parse(JSON.stringify(page)) : { unchanged: true, etag: 'p0' }; } });
    await store.prefetchWorkGroup('tab-4', folded);
    assert.equal(calls[0].background, true, 'a read ahead waits behind interactive reads');
    assert.equal(await store.loadWorkGroup('tab-4', folded), true);
    assert.equal(calls.length, 1, 'the open used the page read ahead');
    // A reload: the kept page is asked for by its etag and answered by a receipt.
    const again = message('m-p');
    again.workSummary = { tools: 1, subagentCalls: 0, failed: 0, events: 1 };
    local.messages = [again];
    (await vite.ssrLoadModule('/src/chat/work-group.ts')).forgetWorkGroups();
    assert.equal(await store.loadWorkGroup('tab-4', again), true);
    assert.equal(calls[1].workGroup.ifNoneMatch, 'p0');
    assert.equal((again.events[0] as any).key, 'timeline:tool:c0');
  });

  test('the next older page read ahead is the one the top then loads, and goes with a chat switch', async () => {
    const local = chat('tab-5');
    local.messages = [message('m-10')];
    local.messageCount = 30;
    const other = chat('tab-6');
    const calls: any[] = [];
    const pages = [[message('m-8'), message('m-9')], [message('m-6'), message('m-7')], [message('m-6'), message('m-7')]];
    const store = subject([local, other], { archiveLoad: async (_tab: string, options: any) => { calls.push(options); return pages.shift(); } });
    store.prefetchOlderHistory('tab-5', 40);
    store.prefetchOlderHistory('tab-5', 40);
    assert.equal(calls.length, 1, 'one page is read ahead once');
    assert.equal(calls[0].background, true);
    assert.equal(calls[0].beforeMessageId, 'm-10');
    assert.equal(await store.loadOlderHistoryPage('tab-5', 40), 'loaded');
    assert.equal(calls.length, 1, 'the top used the page read ahead');
    assert.deepEqual(local.messages.map((m: any) => m.id), ['m-8', 'm-9', 'm-10']);
    // Read ahead, then left: the page is not used for the chat later.
    store.prefetchOlderHistory('tab-5', 40);
    store.switchChat('tab-6');
    assert.equal(await store.loadOlderHistoryPage('tab-5', 40), 'loaded');
    assert.equal(calls.length, 3);
    assert.equal(calls[2].background, undefined);
  });

  test('a long reply arrives as its last part and puts itself back together part by part', async () => {
    const history = await vite.ssrLoadModule('/src/chat/history.ts');
    const whole = 'aaaa\n\nbbbb\n\ncccc';
    const image = { deferredImageRef: 'images/x', width: 10, height: 20, name: 'n' };
    const [tail] = history.actorMessages([{ id: 'm-long', role: 'assistant', status: 'done', content: 'cccc', contentFrom: 12, contentLength: 16,
      events: [{ kind: 'tool', key: 'k2', at: 12, workStub: true, images: [{ ...image }] }] }], 'tab-7');
    assert.equal(tail.contentFrom, 12);
    assert.equal(tail.events[0].at, 0, 'events count from the part');
    assert.deepEqual(tail.events[0].images[0], { ...image, data: '', mimeType: 'image/png' }, 'by-reference images are whole again');
    const local = chat('tab-7');
    local.messages = [tail];
    const parts = [
      { messageId: 'm-long', from: 6, to: 12, content: 'bbbb\n\n', events: [{ kind: 'tool', key: 'k1', at: 6, workStub: true, images: [{ ...image }] }] },
      { messageId: 'm-long', from: 0, to: 6, content: 'aaaa\n\n', events: [] },
    ];
    const calls: any[] = [];
    const store = subject([local], { archiveLoad: async (_tab: string, options: any) => { calls.push(options.contentHead); return parts.shift(); } });
    assert.equal(await store.loadContentHead('tab-7', tail), true);
    assert.deepEqual(calls[0], { messageId: 'm-long', before: 12 });
    assert.equal(tail.contentFrom, 6);
    assert.deepEqual(tail.events.map((event: any) => [event.key, event.at]), [['k1', 0], ['k2', 6]]);
    await store.completeLongRows('tab-7');
    assert.equal(tail.content, whole);
    assert.equal(tail.contentFrom, undefined);
    assert.deepEqual(tail.events.map((event: any) => [event.key, event.at]), [['k1', 6], ['k2', 12]]);
    assert.deepEqual(calls[1], { messageId: 'm-long', before: 6, all: true });
  });

  test('copying a long reply read in parts copies the whole reply', async () => {
    const history = await vite.ssrLoadModule('/src/chat/history.ts');
    const { copyWholeReply } = await vite.ssrLoadModule('/src/components/AssistantMessage.tsx');
    const [tail] = history.actorMessages([{ id: 'm-copy', role: 'assistant', status: 'done', content: 'end', contentFrom: 6, contentLength: 9, events: [] }], 'tab-8');
    const local = chat('tab-8');
    local.messages = [tail];
    subject([local], { archiveLoad: async () => ({ messageId: 'm-copy', from: 0, to: 6, content: 'start ', events: [] }) });
    let copied: string | null = null;
    const previous = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
    Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { clipboard: { writeText: async (text: string) => { copied = text; } } } });
    try {
      await copyWholeReply('tab-8', tail);
    } finally {
      if (previous) Object.defineProperty(globalThis, 'navigator', previous);
      else delete (globalThis as any).navigator;
    }
    assert.equal(copied, 'start end');
  });

  test('a daemon that predates work groups answers a row, which is not a group', async () => {
    const local = chat('tab-2');
    const folded = message('m-old');
    folded.workSummary = { tools: 9, subagentCalls: 0, failed: 0, events: 9 };
    local.messages = [folded];
    const store = subject([local], { archiveLoad: async () => [{ id: 'm-old', role: 'assistant', status: 'done', events: [] }] });
    assert.equal(await store.loadWorkGroup('tab-2', folded), false);
    assert.ok(folded.workSummary, 'the summary stays until the calls are read');
  });
});
