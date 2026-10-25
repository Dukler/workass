// A reload paints the sidebar and the chat it was showing from what this window
// last read, before the socket answers anything, then revalidates by etag:
// unchanged rows stay, changed rows are replaced in place, a chat the daemon no
// longer lists disappears, and a chat this window deleted is never painted.
import assert from 'node:assert/strict';
import { after, afterEach, before, describe, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

let vite: ViteDevServer;
let StoreCtor: new () => any;
const stores: any[] = [];

before(async () => {
  vite = await createServer({ root: fileURLToPath(new URL('..', import.meta.url)), logLevel: 'silent', server: { middlewareMode: true }, appType: 'custom' });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});
after(async () => {
  for (const store of stores) { store.monitor?.stop(); store.clearToastTimers(); }
  restoreGlobals();
  await vite.close();
});

const saved = new Map<string, { had: boolean; value: unknown }>();
for (const key of ['window', 'sessionStorage', 'localStorage', 'document']) {
  saved.set(key, { had: Object.prototype.hasOwnProperty.call(globalThis, key), value: (globalThis as any)[key] });
}
function restoreGlobals() {
  for (const [key, { had, value }] of saved) {
    if (had) (globalThis as any)[key] = value; else delete (globalThis as any)[key];
  }
}
function memoryStorage(): Storage {
  const data = new Map<string, string>();
  return {
    get length() { return data.size; }, clear: () => data.clear(), key: (i: number) => [...data.keys()][i] ?? null,
    getItem: (k: string) => data.get(k) ?? null, setItem: (k: string, v: string) => { data.set(k, String(v)); }, removeItem: (k: string) => { data.delete(k); },
  } as Storage;
}
const flush = async () => { for (let i = 0; i < 50; i++) await new Promise((r) => setTimeout(r, 0)); };

const PREFIX = 'workass.renderer2.wire-cache.';
function keep(name: string, etag: string, value: unknown) {
  (globalThis as any).sessionStorage.setItem(PREFIX + name, `${etag}\n${JSON.stringify(value)}`);
}
function row(id: string, role: 'user' | 'assistant', content: string) {
  return { id, role, content, status: 'done', at: '2026-10-25T10:00:00Z', events: [] };
}
function shellChat(id: string, extra: Record<string, unknown> = {}) {
  return { id, chatId: `chat-${id}`, title: id, cwd: '/tmp/cache-first', providerId: 'claude', currentModelId: null, currentModeId: null,
    messages: [], messageCount: 4, historyComplete: false, actorRevision: 7, ...extra };
}
function shell(chats: unknown[], etag: string) {
  return { etag, chats, activeId: 'tab-a', seq: 0, globalRevision: 3 };
}
const firstRows = [row('m-3', 'user', 'question'), row('m-4', 'assistant', 'answer')];

function prime() {
  (globalThis as any).sessionStorage = memoryStorage();
  (globalThis as any).localStorage = memoryStorage();
  (globalThis as any).document = { documentElement: { setAttribute: () => {}, removeAttribute: () => {} }, addEventListener: () => {} };
  keep('session-shell', 'shell-1', shell([shellChat('tab-a'), shellChat('tab-b')], 'shell-1'));
  keep('first-screen', 'rows-1', { tabId: 'tab-a', chatId: 'chat-tab-a', text: JSON.stringify(firstRows) });
  keep('spawned-work-summary', 'sum-1', { etag: 'sum-1', chats: [{ tabId: 'tab-b', chatId: 'chat-tab-b', running: [], obligation: { state: 'needs_input' } }] });
}

function boot(api: Record<string, unknown>): any {
  (globalThis as any).window = { api, addEventListener: () => {} };
  const store = new StoreCtor();
  stores.push(store);
  store.schedulePersist = () => {};
  return store;
}

describe('cache-first reload', () => {
  afterEach(restoreGlobals);

  test('the sidebar and the open chat paint from the window cache before any read', () => {
    prime();
    let reads = 0;
    const store = boot({ getSession: async () => { reads++; return null; } });
    assert.equal(reads, 0);
    assert.deepEqual(store.state.chats.map((c: any) => c.id), ['tab-a', 'tab-b']);
    assert.equal(store.state.activeId, 'tab-a');
    assert.deepEqual(store.active().messages.map((m: any) => m.id), ['m-3', 'm-4']);
    assert.deepEqual(store.obligation(store.chat('tab-b')), { state: 'needs_input' });
  });

  test('an unchanged daemon confirms the painted rows with receipts', async () => {
    prime();
    const archive: unknown[] = [];
    const sessions: unknown[] = [];
    const store = boot({
      getSession: async (o: unknown) => { sessions.push(o); return { unchanged: true, etag: 'shell-1' }; },
      archiveLoad: async (tabId: string, o: any) => { archive.push(o); return o.ifNoneMatch === 'rows-1' ? { unchanged: true, etag: 'rows-1' } : []; },
    });
    await store.init();
    await flush();
    assert.equal((sessions[0] as any).ifNoneMatch, 'shell-1');
    assert.equal((archive[0] as any).ifNoneMatch, 'rows-1');
    assert.equal((archive[0] as any).lastExchange, true);
    assert.deepEqual(store.active().messages.slice(-2).map((m: any) => m.id), ['m-3', 'm-4']);
    assert.equal(store.provisionalFirstScreens.size, 0, 'the rows count as read once confirmed');
  });

  test('changed rows replace the painted ones in place', async () => {
    prime();
    const fresh = [row('m-5', 'user', 'new question'), row('m-6', 'assistant', 'new answer')];
    const store = boot({
      getSession: async () => shell([shellChat('tab-a', { messageCount: 6, actorRevision: 9 }), shellChat('tab-b')], 'shell-2'),
      archiveLoad: async (_tabId: string, o: any) => (o.lastExchange ? { etag: 'rows-2', messages: fresh } : []),
    });
    await store.init();
    await flush();
    assert.deepEqual(store.active().messages.slice(-2).map((m: any) => m.id), ['m-5', 'm-6']);
    assert.equal(store.active().messages.at(-1).content, 'new answer');
    assert.equal(store.provisionalFirstScreens.size, 0);
  });

  test('an older daemon answers plain arrays and nothing breaks', async () => {
    prime();
    const store = boot({
      getSession: async () => ({ chats: [shellChat('tab-a'), shellChat('tab-b')], activeId: 'tab-a', seq: 0 }),
      archiveLoad: async () => firstRows,
    });
    await store.init();
    await flush();
    assert.deepEqual(store.active().messages.slice(-2).map((m: any) => m.id), ['m-3', 'm-4']);
  });

  test('a chat the daemon no longer lists disappears', async () => {
    prime();
    const store = boot({
      getSession: async () => shell([shellChat('tab-b')], 'shell-3'),
      archiveLoad: async () => [],
    });
    await store.init();
    await flush();
    assert.deepEqual(store.state.chats.map((c: any) => c.id), ['tab-b']);
    assert.equal(store.provisionalFirstScreens.has('tab-a'), false);
  });

  test('a chat this window deleted forgets the cache, so a reload cannot paint it', () => {
    prime();
    const store = boot({});
    store.discardChatProjection(store.chat('tab-a'));
    assert.equal((globalThis as any).sessionStorage.getItem(PREFIX + 'session-shell'), null);
    assert.equal((globalThis as any).sessionStorage.getItem(PREFIX + 'first-screen'), null);
    const reloaded = boot({});
    assert.equal(reloaded.state.chats.some((c: any) => c.id === 'tab-a'), false);
  });
});
