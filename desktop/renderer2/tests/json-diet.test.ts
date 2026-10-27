// A connect parses what the sidebar and the chat on screen show, and no more
// (store/session-page.ts, daemon cmd/workass/session_page.go):
//   - a paged session shell leaves the filed archive on the daemon, counted by
//     folder, and a search or an open asks the daemon for what it needs;
//   - a changed shell comes back as a delta against the kept one;
//   - the paged chat order is partial, and saves say so;
//   - the provider list a window kept is reused on its next load.
// Every chat stays reachable: by the sidebar, by search, by a notification.
import assert from 'node:assert/strict';
import { after, afterEach, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let page: any;
let workspaces: any;
let wireCache: any;
let digestHash: any;
const fixtureStores: any[] = [];

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  page = await vite.ssrLoadModule('/src/store/session-page.ts');
  workspaces = await vite.ssrLoadModule('/src/workspaces.ts');
  wireCache = await vite.ssrLoadModule('/src/store/wire-cache.ts');
  digestHash = await vite.ssrLoadModule('/src/store/digest-hash.ts');
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

function chat(id: string, cwd = '/work/alpha'): Chat {
  return {
    id, chatId: `chat-${id}`, title: id, group: null, cwd,
    providerId: 'claude', currentModelId: null, currentModeId: null,
    pending: false, messages: [message(`m-${id}`)], draft: '',
  } as Chat;
}

function row(id: string, cwd = '/work/alpha') {
  return { id, chatId: `chat-${id}`, title: id, cwd, providerId: 'claude', messages: [], messageCount: 1, historyComplete: false };
}

function pagedShell(ids: string[], etag: string, extra: Record<string, unknown> = {}) {
  return {
    etag, chats: ids.map((id) => row(id)), activeId: ids[0] ?? null, seq: 0, chatOrder: ids,
    chatOrderPartial: true, deferredChats: { count: 2, cwds: [['/work/beta', 2]] }, ...extra,
  };
}

function subject(chats: Chat[], api: Record<string, unknown>): any {
  (globalThis as any).window = { api };
  const store = new StoreCtor();
  fixtureStores.push(store);
  store.state.chats = chats;
  store.state.activeId = chats[0]?.id ?? null;
  store.schedulePersist = () => {};
  store.requireFullSave = () => {};
  return store;
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));
const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

describe('session shell delta', () => {
  test('a delta rebuilds exactly the shell it names', () => {
    const base = {
      etag: 'e1', activeId: 'a', theme: 'dark', chatOrder: ['a', 'M~m1~r', 'b'],
      chats: [{ id: 'a', title: 'A', unread: true }, { id: 'b', title: 'B' }],
    };
    const reply = {
      etag: 'e2',
      delta: {
        base: 'e1', set: { activeId: 'c' }, unset: ['theme'],
        lists: { chatOrder: ['c', [0, 3]] },
        patch: [{ id: 'a', set: { title: 'A2' }, unset: ['unread'] }],
        chats: [{ id: 'c', title: 'C' }],
        ids: ['c', 'a', 'b'],
      },
    };
    const rebuilt = page.applyShellDelta(base, 'e1', reply);
    assert.deepEqual(rebuilt, {
      etag: 'e2', activeId: 'c', chatOrder: ['c', 'a', 'M~m1~r', 'b'],
      chats: [{ id: 'c', title: 'C' }, { id: 'a', title: 'A2' }, { id: 'b', title: 'B' }],
    });
    assert.deepEqual(base.chats[0], { id: 'a', title: 'A', unread: true }, 'the kept shell is not modified');
    // Without ids the rows keep their order.
    assert.deepEqual(page.applyShellDelta(base, 'e1', { etag: 'e3', delta: { base: 'e1', patch: [{ id: 'b', set: { title: 'B2' } }] } }).chats,
      [{ id: 'a', title: 'A', unread: true }, { id: 'b', title: 'B2' }]);
    // A delta that does not apply is refused, and the shell is read whole.
    assert.equal(page.applyShellDelta(base, 'e0', reply), null, 'another base');
    assert.equal(page.applyShellDelta(base, 'e1', { etag: 'e2', delta: { base: 'e1', patch: [{ id: 'zz', set: {} }] } }), null, 'a patch of a row it does not hold');
    assert.equal(page.applyShellDelta(base, 'e1', { etag: 'e2', delta: { base: 'e1', ids: ['a', 'zz'] } }), null, 'an id it was never sent');
    assert.equal(page.applyShellDelta(base, 'e1', { etag: 'e2', delta: { base: 'e1', lists: { chatOrder: [[2, 5]] } } }), null, 'a run past the kept list');
    assert.equal(page.isShellDelta(reply), true);
    assert.equal(page.isShellDelta({ etag: 'e1', unchanged: true }), false);
  });

  test('deferredChats is read defensively, and a paged read leaves the archive behind its margin', () => {
    assert.deepEqual(page.parseDeferredChats({ count: 3, cwds: [['/a', 2], ['/b', 1], ['bad'], ['/c', 0]] }), { count: 3, cwds: [['/a', 2], ['/b', 1]] });
    assert.equal(page.parseDeferredChats(undefined), null);
    assert.equal(page.parseDeferredChats({ count: -1 }), null);
    const now = Date.parse('2026-10-25T12:00:00Z');
    const before = page.pageSettledBefore(now);
    assert.ok(now - before > page.ARCHIVE_AFTER_SETTLED_MS + page.SIDEBAR_LIFECYCLE_TICK_MS, 'the margin covers the sidebar clock lag');
  });
});

describe('folders of chats the daemon did not send', () => {
  test('they still make their folder, are counted on it, and carry no machine', () => {
    const groups = workspaces.buildWorkspaceGroups(
      [{ path: '/work/alpha', name: 'alpha' }],
      [chat('a', '/work/alpha'), { ...chat('r', '/work/remote'), machineId: 'm1' }],
      ['/work/removed'],
      [['/work/beta', 2], ['/work/remote', 1], ['/work/removed', 4], ['', 1]],
    );
    const byPath = new Map(groups.map((group: any) => [group.path, group]));
    assert.equal(byPath.get('/work/beta').deferred, 2);
    assert.equal(byPath.get('/work/beta').chats.length, 0);
    assert.equal(byPath.get('/work/remote').machineId, undefined, 'a folder shared with local filed chats is not one machine\'s');
    assert.equal(byPath.get('').deferred, 5, 'a removed folder and no folder fall to "Chats"');
    assert.equal(byPath.get('/work/removed'), undefined);
    const labels = workspaces.deferredSearchLabels(groups, [['/work/beta', 2], ['/work/removed', 4], ['/work/alpha', 1]]);
    assert.deepEqual(labels, [['/work/beta', 'beta'], ['/work/removed', 'removed'], ['/work/alpha', 'alpha']]);
  });
});

describe('paged session reads', () => {
  afterEach(restoreGlobals);

  test('the shell, the digest and the summary ask for the same page', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const sent: any[] = [];
    const store = subject([chat('tab-1')], {
      getSession: async (options: any) => { sent.push(['session', options]); return pagedShell(['tab-1'], 'e1'); },
      stateDigest: async (options: any) => { sent.push(['digest', options]); return { chats: [], globalRevision: 0, catalogHash: {}, settingsRevision: '', procHash: '' }; },
      spawnedWorkSummary: async (options: any) => { sent.push(['summary', options]); return { chats: [] }; },
    });
    store.pendingAgentFocus = { tabId: 'tab-focus', chatId: 'chat-focus' };
    store.pageIncludes.add('tab-open');
    await store.readSessionShell('test');
    await store.readStateDigest((globalThis as any).window.api);
    await store.readSpawnedWorkSummary('');
    assert.equal(sent.length, 3);
    for (const [, options] of sent) {
      assert.deepEqual([...options.page.include].sort(), ['tab-1', 'tab-focus', 'tab-open']);
      assert.equal(typeof options.page.settledBefore, 'number');
    }
    assert.equal(sent[0][1].shell, true);
    assert.equal(sent[0][1].delta, true);
  });

  test('a changed shell read by etag comes back as a delta and is kept rebuilt', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const replies: unknown[] = [
      pagedShell(['tab-1'], 'e1'),
      { etag: 'e2', delta: { base: 'e1', patch: [{ id: 'tab-1', set: { title: 'renamed' } }] } },
    ];
    const sent: any[] = [];
    const store = subject([chat('tab-1')], { getSession: async (options: any) => { sent.push(options); return replies.shift(); } });
    await store.readSessionShell('test');
    const second = await store.readSessionShell('test');
    assert.equal(sent[1].ifNoneMatch, 'e1');
    assert.equal(second.value.etag, 'e2');
    assert.equal(second.value.chats[0].title, 'renamed');
    assert.equal(JSON.parse(store.sessionShell.text).chats[0].title, 'renamed');
    assert.equal(store.sessionShell.etag, 'e2');
  });

  test('a search asks the daemon only when it holds filed chats, and clearing it drops them again', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const searches: unknown[] = [];
    const store = subject([chat('tab-1')], {
      getSession: async (options: any) => { searches.push(options.page.search ?? null); return pagedShell(['tab-1', 'tab-hit'], 'e-search'); },
    });
    store.state.deferredChats = null;
    store.setSidebarSearch('hit', []);
    await wait(200);
    // A late refresh may read the page (search null); what must not happen is a search.
    assert.deepEqual(searches.filter((search) => search !== null), [], 'a whole shell needs no daemon search');

    store.state.deferredChats = { count: 2, cwds: [['/work/beta', 2]] };
    store.setSidebarSearch('H', []);
    store.setSidebarSearch('Hi', []);
    store.setSidebarSearch('Hit', [['/work/beta', 'beta']]);
    assert.equal(store.state.sidebarSearchPending, true);
    await wait(250);
    await store.agentRefresh;
    // Another fixture's late refresh may read the page without a search; this
    // store asked for exactly one.
    assert.deepEqual(searches.filter((search) => search !== null), ['hit'], 'keystrokes are coalesced into one search');
    assert.ok(store.chat('tab-hit'), 'the match is a row of the window');
    assert.equal(store.state.sidebarSearchPending, false);
    store.setSidebarSearch('', []);
    await wait(50);
    await store.agentRefresh;
    assert.equal(searches[searches.length - 1], null, 'clearing the search reads the page without it');
  });

  test('opening a chat the page left out reads its row, then opens it', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const includes: string[][] = [];
    const store = subject([chat('tab-1')], {
      getSession: async (options: any) => { includes.push([...options.page.include].sort()); return pagedShell(['tab-1', 'tab-filed'], 'e-open'); },
      archiveLoad: async () => [{ id: 'm-filed', role: 'assistant', content: 'filed', status: 'done', at: null, events: [] }],
    });
    store.state.deferredChats = { count: 1, cwds: [['/work/beta', 1]] };
    store.switchChat('tab-filed');
    await wait(20);
    await store.agentRefresh;
    await wait(20);
    // The open's own read asks for its row first; a later refresh may read the page again.
    assert.deepEqual(includes[0], ['tab-1', 'tab-filed']);
    assert.equal(store.state.activeId, 'tab-filed');
    assert.equal(store.pageIncludes.has('tab-filed'), false, 'once on screen, the page lists it on its own');
    assert.equal(await store.ensureChatLoaded('M~m1~tab-x'), false, 'another machine\'s chat is not this page\'s');
  });
});

describe('a partial chat order', () => {
  afterEach(restoreGlobals);

  test('saves say so to a daemon that folds them, and wait for any other', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const saves: any[] = [];
    const store = subject([chat('tab-1')], { saveSession: async (snapshot: any) => { saves.push(snapshot); return { ok: true, globalRevision: 1 }; } });
    store.adoptLocalSnapshotOrder(pagedShell(['tab-1'], 'e1'), store.state.chats);
    assert.equal(store.localChatOrderPartial, true);
    store.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2', chatOrderMode: 'partial-v1' };
    store.committedGlobalPresentationFingerprint = 'stale';
    await store.flushSession();
    assert.equal(saves.length, 1);
    assert.equal(saves[0].chatOrderPartial, true);

    store.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };
    store.committedGlobalPresentationFingerprint = 'stale';
    store.pendingGlobalPresentationOperation = null;
    await store.flushSession();
    assert.equal(saves.length, 1, 'a partial order is never sent to a daemon that would keep it as the whole order');

    store.adoptLocalSnapshotOrder({ chats: [row('tab-1')], chatOrder: ['tab-1'], activeId: 'tab-1', seq: 0 }, store.state.chats);
    assert.equal(store.localChatOrderPartial, false);
    await store.flushSession();
    assert.equal(saves.length, 2);
    assert.equal(saves[1].chatOrderPartial, undefined);
  });
});

describe('the provider list a window kept', () => {
  afterEach(restoreGlobals);

  test('is named by the daemon\'s hash, kept for the bridge, and painted on the next load', async () => {
    (globalThis as any).sessionStorage = memoryStorage();
    const list = [{ id: 'mock', name: 'Mock <dev> & co', status: 'ready', enabled: true }];
    const first = subject([], {});
    first.rememberProvidersList(list);
    await settle(); await settle();
    const kept = wireCache.readWireCache('providers-list');
    assert.equal(kept.etag, await digestHash.stateDigestHash(list));
    assert.match(kept.etag, /^[0-9a-f]{64}$/);
    const second = subject([], {});
    assert.deepEqual(second.state.providers.map((provider: any) => provider.id), ['mock']);
    assert.equal(second.providersCacheEtag, kept.etag);
  });
});
