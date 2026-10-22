import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat, Msg } from '../src/store/types.ts';

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

function messages(count: number): Msg[] {
  return Array.from({ length: count }, (_, index) => ({
    id: `message-${index}`,
    role: index % 2 === 0 ? 'user' : 'assistant',
    content: `row ${index}`,
    status: 'done',
    at: `2026-08-12T12:${String(index % 60).padStart(2, '0')}:00Z`,
    events: [],
  })) as Msg[];
}

function chat(id: string, rows: Msg[]): Chat {
  return {
    id,
    chatId: `chat-${id}`,
    sessionId: null,
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-archive-residency',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: rows,
    messageCount: rows.length,
    historyComplete: true,
    draft: '',
  } as Chat;
}

test('opening a byte-bounded recent tail fetches the missing user image and steer row', async () => {
  const image = { mimeType: 'image/png', data: 'aGVsbG8=' };
  const complete = [
    { ...messages(1)[0], id: 'root-user' },
    { ...messages(2)[1], id: 'root-assistant' },
    { ...messages(1)[0], id: 'steer-user', content: 'see image', steerState: 'applied', images: [image] },
    { ...messages(2)[1], id: 'steer-assistant' },
  ] as Msg[];
  const previousWindow = (globalThis as any).window;
  const reads: Array<{ beforeMessageId?: string; tail?: number }> = [];
  (globalThis as any).window = {
    api: { archiveLoad: async (_tabId: string, options: { beforeMessageId?: string; tail?: number }) => {
      reads.push(options);
      if (options.beforeMessageId === 'steer-assistant') return [complete[2]];
      if (options.beforeMessageId === 'steer-user') return complete.slice(0, 2);
      return [complete[3]]; // 8 MiB expanded-image budget split the tail.
    } },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-image-tail', [complete[3]]);
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = null;
    subject.switchChat(target.id);
    await subject.recentHistoryLoads.get(target.id);
    assert.deepEqual(target.messages.map((row) => row.id), complete.map((row) => row.id));
    assert.deepEqual(target.messages[2].images, [image]);
    assert.equal(target.messages[2].steerState, 'applied');
    assert.deepEqual(reads.map((options) => options.beforeMessageId), [undefined, 'steer-assistant', 'steer-user']);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('switching chats keeps a bounded recent tail and loads the full ledger only on demand', async () => {
  const complete = messages(85);
  const previousWindow = (globalThis as any).window;
  const archiveCalls: Array<{ tabId: string; options: unknown }> = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string, options?: unknown) => {
        archiveCalls.push({ tabId, options });
        return tabId === 'tab-a' ? complete : [];
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-a', [...complete]);
    const second = chat('tab-b', messages(2));
    subject.state.chats = [first, second];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };
    subject.fullHistoriesLoaded.add(first.id);

    subject.switchChat(second.id);
    assert.equal(first.messages.length, 60);
    assert.equal(first.messageCount, 85);
    assert.equal(first.historyComplete, false);
    assert.equal(subject.fullHistoriesLoaded.has(first.id), false);

    subject.switchChat(first.id);
    assert.equal(subject.state.activeId, first.id);
    assert.equal(first.messages.length, 60);
    assert.equal(archiveCalls.length, 0, 'ordinary selection must not request the complete archive');

    assert.equal(await subject.loadFullHistory(first.id), true);
    assert.equal(first.messages.length, 85);
    assert.equal(first.historyComplete, true);
    assert.equal(subject.fullHistoriesLoaded.has(first.id), true);
    assert.deepEqual(archiveCalls, [
      { tabId: first.id, options: { tail: 60, deferImages: true, leanTools: true, leanTitles: true } },
      { tabId: first.id, options: { beforeMessageId: 'message-0', limit: 60, deferImages: true, leanTools: true, leanTitles: true } },
    ]);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('older history pages prepend by stable boundary until the canonical transcript is complete', async () => {
  const complete = messages(145);
  const previousWindow = (globalThis as any).window;
  const archiveCalls: Array<{ tabId: string; options: { beforeMessageId: string; limit: number } }> = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string, options: { beforeMessageId: string; limit: number }) => {
        archiveCalls.push({ tabId, options });
        const boundary = complete.findIndex((message) => message.id === options.beforeMessageId);
        return complete.slice(Math.max(0, boundary - options.limit), boundary);
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-paged', complete.slice(-60));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    assert.equal(await subject.loadOlderHistory(target.id, 40), true);
    assert.equal(target.messages.length, 100);
    assert.equal(target.messageCount, 145);
    assert.equal(target.historyComplete, false);

    assert.equal(await subject.loadOlderHistory(target.id, 40), true);
    assert.equal(target.messages.length, 140);
    assert.equal(target.historyComplete, false);

    assert.equal(await subject.loadOlderHistory(target.id, 40), true);
    assert.deepEqual(target.messages.map((message) => message.id), complete.map((message) => message.id));
    assert.equal(target.messageCount, 145);
    assert.equal(target.historyComplete, true);
    assert.deepEqual(archiveCalls, [
      { tabId: target.id, options: { beforeMessageId: 'message-85', limit: 40, deferImages: true, leanTools: true, leanTitles: true } },
      { tabId: target.id, options: { beforeMessageId: 'message-45', limit: 40, deferImages: true, leanTools: true, leanTitles: true } },
      { tabId: target.id, options: { beforeMessageId: 'message-5', limit: 40, deferImages: true, leanTools: true, leanTitles: true } },
    ]);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

// A failed read is not the start of the chat. Taking it for one stopped the
// automatic upward load of a 225 MB chat for good after one timeout.
test('an older page read says whether it loaded, found nothing, or failed', async () => {
  const complete = messages(100);
  const previousWindow = (globalThis as any).window;
  let mode: 'fail' | 'page' | 'nothing' = 'fail';
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options: { beforeMessageId: string; limit: number }) => {
        if (mode === 'fail') throw new Error('socket closed');
        if (mode === 'nothing') return [];
        const boundary = complete.findIndex((message) => message.id === options.beforeMessageId);
        return complete.slice(Math.max(0, boundary - options.limit), boundary);
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-outcome', complete.slice(-60));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    assert.equal(await subject.loadOlderHistoryPage(target.id, 20), 'failed');
    assert.equal(target.messages.length, 60, 'a failed read changes nothing');
    assert.equal(await subject.loadOlderHistory(target.id, 20), false, 'the boolean form stays as it was');
    mode = 'page';
    assert.equal(await subject.loadOlderHistoryPage(target.id, 20), 'loaded');
    assert.equal(target.messages.length, 80);
    mode = 'nothing';
    assert.equal(await subject.loadOlderHistoryPage(target.id, 20), 'empty');
    target.historyComplete = true;
    assert.equal(await subject.loadOlderHistoryPage(target.id, 20), 'empty', 'a complete history has nothing older');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('short image-rich pages keep advancing without marking history complete early', async () => {
  const complete = messages(9);
  const previousWindow = (globalThis as any).window;
  const boundaries: string[] = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options: { beforeMessageId: string; limit: number }) => {
        boundaries.push(options.beforeMessageId);
        const boundary = complete.findIndex((message) => message.id === options.beforeMessageId);
        return complete.slice(Math.max(0, boundary - 2), boundary);
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-short-pages', complete.slice(-1));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    for (let page = 0; page < 4; page += 1) {
      assert.equal(await subject.loadOlderHistory(target.id, 40), true);
      assert.equal(target.historyComplete, page === 3);
    }
    assert.deepEqual(boundaries, ['message-8', 'message-6', 'message-4', 'message-2']);
    assert.deepEqual(target.messages.map((message) => message.id), complete.map((message) => message.id));
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('full-history search assembles every short page and commits only after reaching the start', async () => {
  const complete = messages(9);
  const previousWindow = (globalThis as any).window;
  const boundaries: Array<string | undefined> = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options: { beforeMessageId?: string; tail?: number }) => {
        assert.ok(options, 'full history must not request one unbounded reply');
        boundaries.push(options.beforeMessageId);
        const end = options.beforeMessageId
          ? complete.findIndex((message) => message.id === options.beforeMessageId)
          : complete.length;
        return complete.slice(Math.max(0, end - 2), end);
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-full-short-pages', complete.slice(-1));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    assert.equal(await subject.loadFullHistory(target.id), true);
    assert.deepEqual(boundaries, [undefined, 'message-7', 'message-5', 'message-3', 'message-1', 'message-0']);
    assert.deepEqual(target.messages.map((message) => message.id), complete.map((message) => message.id));
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('older history paging remains compatible with a daemon that returns the full archive', async () => {
  const complete = messages(100);
  const previousWindow = (globalThis as any).window;
  const archiveCalls: unknown[] = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options?: unknown) => {
        archiveCalls.push(options);
        return complete;
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-old-daemon', complete.slice(-60));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    assert.equal(await subject.loadOlderHistory(target.id, 40), true);
    assert.deepEqual(target.messages.map((message) => message.id), complete.map((message) => message.id));
    assert.equal(target.historyComplete, true);
    assert.deepEqual(archiveCalls, [{ beforeMessageId: 'message-40', limit: 40, deferImages: true, leanTools: true, leanTitles: true }]);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('opening an incomplete chat with a resident tail activates synchronously without an archive read', () => {
  const firstRows = messages(85);
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let archiveCalls = 0;
  (globalThis as any).window = {
    api: {
      archiveLoad: async () => { archiveCalls += 1; return []; },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', [...firstRows]);
    const target = chat('tab-target', targetRows.slice(-60));
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };
    subject.fullHistoriesLoaded.add(first.id);

    subject.switchChat(target.id);

    assert.equal(subject.state.activeId, target.id);
    assert.equal(first.messages.length, 60);
    assert.equal(target.messages.length, 60);
    assert.equal(target.historyComplete, false);
    assert.equal(archiveCalls, 0);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('opening a metadata-only chat fetches ten recent rows before the no-blank handoff', async () => {
  const firstRows = messages(85);
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  const archiveCalls: Array<{ tabId: string; options: unknown }> = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string, options?: unknown) => {
        archiveCalls.push({ tabId, options });
        return tabId === 'tab-target' ? history : [];
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', firstRows);
    const target = chat('tab-target', []);
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    assert.equal(subject.state.activeId, first.id);
    assert.equal(first.messages.length, firstRows.length);
    // The first screen asks for the last exchange; this daemon predates the
    // option and answers the whole tail, which paints at once as before.
    assert.deepEqual(archiveCalls, [{ tabId: target.id, options: { tail: 10, lastExchange: true, deferImages: true, leanTools: true, leanTitles: true } }]);

    historyReleased = true;
    releaseHistory(targetRows.slice(-10));
    await subject.recentHistoryLoads.get(target.id);
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, target.id);
    assert.equal(target.messages.length, 10);
    assert.equal(target.historyComplete, false);
    assert.equal(first.messages.length, 60);
  } finally {
    if (!historyReleased) releaseHistory([]);
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('opening a metadata-only chat paints its last exchange, then pages in the rest of the slice', async () => {
  const firstRows = messages(85);
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releasePrefix!: () => void;
  const prefixGate = new Promise<void>((resolve) => { releasePrefix = resolve; });
  const archiveCalls: unknown[] = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string, options: { tail?: number; lastExchange?: boolean; beforeMessageId?: string; limit?: number }) => {
        if (tabId !== 'tab-target') return [];
        archiveCalls.push(options);
        // A daemon that knows lastExchange: the user's newest message on.
        if (options.lastExchange) return targetRows.slice(-2);
        if (options.beforeMessageId) {
          await prefixGate;
          const end = targetRows.findIndex((message) => message.id === options.beforeMessageId);
          return targetRows.slice(Math.max(0, end - (options.limit ?? 0)), end);
        }
        return targetRows.slice(-(options.tail ?? 0));
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', firstRows);
    const target = chat('tab-target', []);
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    await new Promise((resolve) => setTimeout(resolve, 0));

    // The handoff happened on the first screen, with the slice still loading.
    assert.equal(subject.state.activeId, target.id);
    assert.deepEqual(target.messages.map((message: Msg) => message.id), ['message-90', 'message-91']);
    assert.equal(target.historyComplete, false);
    assert.equal(target.messageCount, 92);
    assert.equal(subject.showsFirstScreenOnly(target.id), true);
    assert.deepEqual(archiveCalls, [
      { tail: 10, lastExchange: true, deferImages: true, leanTools: true, leanTitles: true },
      { beforeMessageId: 'message-90', limit: 8, deferImages: true, leanTools: true, leanTitles: true },
    ]);

    releasePrefix();
    await subject.recentHistoryLoads.get(target.id);
    await new Promise((resolve) => setTimeout(resolve, 0));

    // The rest of the slice is exactly the ten rows a whole tail paints.
    assert.deepEqual(target.messages.map((message: Msg) => message.id), targetRows.slice(-10).map((message) => message.id));
    assert.equal(target.historyComplete, false);
    assert.equal(target.messageCount, 92);
    assert.equal(subject.showsFirstScreenOnly(target.id), false);
    assert.equal(subject.state.activeId, target.id);
  } finally {
    releasePrefix();
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('the rest of an opened chat\'s slice is laid out after its first screen has painted', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  const previousFrame = (globalThis as any).requestAnimationFrame;
  const frames: Array<() => void> = [];
  (globalThis as any).requestAnimationFrame = (callback: () => void) => { frames.push(callback); return frames.length; };
  const runFrame = () => { for (const callback of frames.splice(0)) callback(); };
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options: { lastExchange?: boolean; beforeMessageId?: string; limit?: number }) => {
        if (options.lastExchange) return targetRows.slice(-2);
        const end = targetRows.findIndex((message) => message.id === options.beforeMessageId);
        return targetRows.slice(Math.max(0, end - (options.limit ?? 0)), end);
      },
    },
  };
  const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', messages(4));
    const target = chat('tab-target', []);
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    await tick();
    await tick();
    // The page came back at once; the first screen still owns this frame.
    assert.equal(subject.state.activeId, target.id);
    assert.equal(target.messages.length, 2);
    assert.equal(subject.showsFirstScreenOnly(target.id), true);

    runFrame(); // the frame that paints the first screen
    await tick();
    assert.equal(target.messages.length, 2);
    runFrame(); // the next one
    await subject.recentHistoryLoads.get(target.id);
    assert.deepEqual(target.messages.map((message: Msg) => message.id), targetRows.slice(-10).map((message) => message.id));
    assert.equal(subject.showsFirstScreenOnly(target.id), false);
  } finally {
    if (previousFrame === undefined) delete (globalThis as any).requestAnimationFrame;
    else (globalThis as any).requestAnimationFrame = previousFrame;
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a chat with rows on screen never asks for only its last exchange', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  const archiveCalls: unknown[] = [];
  (globalThis as any).window = {
    api: {
      archiveLoad: async (_tabId: string, options: { tail?: number }) => {
        archiveCalls.push(options);
        return targetRows.slice(-(options.tail ?? 0));
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const target = chat('tab-target', targetRows.slice(-3));
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    await subject.loadRecentHistory(target.id, 10);

    assert.deepEqual(archiveCalls, [{ tail: 10, deferImages: true, leanTools: true, leanTitles: true }]);
    assert.equal(target.messages.length, 10);
    assert.equal(subject.showsFirstScreenOnly(target.id), false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a completed metadata-only recent read cannot steal focus from a newer click', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  (globalThis as any).window = {
    api: {
      archiveLoad: async () => history,
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', messages(4));
    const target = chat('tab-target', []);
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    const latest = chat('tab-latest', messages(6));
    subject.state.chats = [first, target, latest];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    subject.switchChat(latest.id);
    assert.equal(subject.state.activeId, latest.id);

    historyReleased = true;
    releaseHistory(targetRows.slice(-10));
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, latest.id);
    assert.equal(target.messages.length, 10);
    assert.equal(target.historyComplete, false);
  } finally {
    if (!historyReleased) releaseHistory([]);
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('repeated clicks on one metadata-only chat share one recent read', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  let archiveCalls = 0;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  (globalThis as any).window = {
    api: {
      archiveLoad: async () => { archiveCalls += 1; return history; },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', messages(4));
    const target = chat('tab-target', []);
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    subject.switchChat(target.id);
    assert.equal(subject.state.activeId, first.id);
    assert.equal(archiveCalls, 1);

    historyReleased = true;
    releaseHistory(targetRows.slice(-10));
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, target.id);
    assert.equal(target.messages.length, 10);
    assert.equal(target.historyComplete, false);
    assert.equal(archiveCalls, 1);
  } finally {
    if (!historyReleased) releaseHistory([]);
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a genuinely empty new chat activates immediately without a history read', () => {
  const previousWindow = (globalThis as any).window;
  let archiveCalls = 0;
  (globalThis as any).window = { api: { archiveLoad: async () => { archiveCalls += 1; return []; } } };
  try {
    const subject = ownStore(new StoreCtor());
    const first = chat('tab-first', messages(4));
    const empty = chat('tab-empty', []);
    subject.state.chats = [first, empty];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(empty.id);

    assert.equal(subject.state.activeId, empty.id);
    assert.equal(archiveCalls, 0);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a daemon refresh releases every inactive complete history but keeps the active transcript visible', () => {
  const subject = ownStore(new StoreCtor());
  const active = chat('tab-active', messages(80));
  const inactive = chat('tab-inactive', messages(90));
  subject.state.chats = [active, inactive];
  subject.state.activeId = active.id;
  subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

  const server = subject.toMirror(false);
  server.chats[0].messages = active.messages;
  server.chats[0].messageCount = active.messages.length;
  server.chats[0].historyComplete = true;
  server.chats[1].messages = inactive.messages;
  server.chats[1].messageCount = inactive.messages.length;
  server.chats[1].historyComplete = true;
  const inactiveActivityAt = Date.parse('2026-08-12T12:59:00Z');
  server.chats[1].lastActivityAt = inactiveActivityAt;
  assert.equal(subject.restoreSessionSnapshot(server), true);

  assert.equal(subject.chat(active.id)?.messages.length, 80);
  assert.equal(subject.chat(inactive.id)?.messages.length, 60);
  assert.equal(subject.chat(inactive.id)?.messageCount, 90);
  assert.equal(subject.chat(inactive.id)?.historyComplete, false);
  assert.equal(subject.chat(inactive.id)?.lastActivityAt, inactiveActivityAt);
  assert.equal(subject.toMirror(false).chats[1].lastActivityAt, inactiveActivityAt);
});

test('a newer metadata-only refresh reconciles a bounded recent suffix without collapsing visible history', async () => {
  const complete = messages(14);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  let archiveReads = 0;
  let archiveOptions: unknown;
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string, options?: unknown) => {
        archiveReads += 1;
        archiveOptions = options;
        return tabId === 'tab-active' ? history : [];
      },
    },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const active = chat('tab-active', [...complete]);
    active.actorRevision = 7;
    subject.state.chats = [active];
    subject.state.activeId = active.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };
    subject.fullHistoriesLoaded.add(active.id);

    // Another surface may be the daemon-global active tab, so session:get can
    // legally return only metadata for the chat this renderer is still reading.
    // A newer actor revision makes the resident rows provisional. They stay on
    // screen until the small current tail arrives, rather than blinking empty
    // or forcing the complete archive through the renderer.
    const server = subject.toMirror(false);
    server.chats[0].actorRevision = 8;
    server.chats[0].messages = [];
    server.chats[0].messageCount = complete.length;
    server.chats[0].historyComplete = false;

    assert.equal(subject.restoreSessionSnapshot(server), true);
    assert.equal(subject.chat(active.id)?.messages.length, complete.length);
    const load = subject.recentHistoryLoads.get(active.id);
    assert.ok(load, 'visible stale history must start a bounded recent read');
    releaseHistory(complete.slice(-10));
    await load;

    assert.equal(archiveReads, 1);
    assert.deepEqual(archiveOptions, { tail: 10, deferImages: true, leanTools: true, leanTitles: true });
    assert.equal(subject.chat(active.id)?.messages.length, complete.length);
    assert.equal(subject.chat(active.id)?.historyComplete, true);
    assert.equal(subject.fullHistoriesLoaded.has(active.id), true);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a selected paged window survives an authoritative tail refresh', () => {
  const complete = messages(145);
  const subject = ownStore(new StoreCtor());
  const active = chat('tab-active-paged', complete.slice(-100));
  active.actorRevision = 7;
  active.messageCount = complete.length;
  active.historyComplete = false;
  subject.state.chats = [active];
  subject.state.activeId = active.id;
  subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

  const server = subject.toMirror(false);
  server.chats[0].actorRevision = 8;
  server.chats[0].messages = complete.slice(-60);
  server.chats[0].messageCount = complete.length;
  server.chats[0].historyComplete = false;

  assert.equal(subject.restoreSessionSnapshot(server), true);
  assert.deepEqual(
    subject.chat(active.id)?.messages.map((message: Msg) => message.id),
    complete.slice(-100).map((message) => message.id),
  );
  assert.equal(subject.chat(active.id)?.messageCount, complete.length);
  assert.equal(subject.chat(active.id)?.historyComplete, false);
});

test('an unchanged metadata-only refresh preserves a selected resident tail without another read', () => {
  const previousWindow = (globalThis as any).window;
  let archiveReads = 0;
  (globalThis as any).window = {
    api: { archiveLoad: async () => { archiveReads += 1; return []; } },
  };
  try {
    const subject = ownStore(new StoreCtor());
    const active = chat('tab-active', messages(60));
    active.actorRevision = 7;
    active.messageCount = 90;
    active.historyComplete = false;
    subject.state.chats = [active];
    subject.state.activeId = active.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    const server = subject.toMirror(false);
    server.chats[0].actorRevision = 7;
    server.chats[0].messages = [];
    server.chats[0].messageCount = 90;
    server.chats[0].historyComplete = false;

    assert.equal(subject.restoreSessionSnapshot(server), true);
    assert.equal(subject.chat(active.id)?.messages.length, 60);
    assert.equal(subject.chat(active.id)?.historyComplete, false);
    assert.equal(archiveReads, 0);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

// A refresh carries metadata only for inactive chats. A chat the user already
// opened keeps its recent tail, so opening it again paints at once instead of
// waiting on a history read (2026-10-13: about a second for a cold chat). An
// unchanged actor needs no read at all; a moved one refreshes behind the view.
test('a chat already opened keeps its recent tail across a refresh and opens without waiting', async () => {
  const previousWindow = (globalThis as any).window;
  const reads: string[] = [];
  (globalThis as any).window = { api: { archiveLoad: async (tabId: string) => { reads.push(tabId); return messages(10); } } };
  try {
    const subject = ownStore(new StoreCtor());
    const active = chat('tab-reading', messages(12));
    const quiet = chat('tab-quiet', messages(12));
    const moved = chat('tab-moved', messages(12));
    for (const item of [active, quiet, moved]) { item.actorRevision = 3; item.messageCount = 12; }
    subject.state.chats = [active, quiet, moved];
    subject.state.activeId = active.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    const server = subject.toMirror(false);
    for (const row of server.chats) {
      if (row.id === active.id) continue;
      row.messages = [];
      row.historyComplete = false;
      if (row.id === moved.id) { row.actorRevision = 4; row.messageCount = 13; }
    }
    assert.equal(subject.restoreSessionSnapshot(server), true);
    assert.equal(subject.chat(quiet.id)?.messages.length, 12, 'an unchanged chat keeps its tail');
    assert.equal(subject.chat(moved.id)?.messages.length, 12, 'a moved chat keeps its tail as a provisional view');

    subject.switchChat(quiet.id);
    assert.equal(subject.state.activeId, quiet.id, 'opening a resident chat paints at once');
    subject.switchChat(moved.id);
    assert.equal(subject.state.activeId, moved.id, 'a moved chat also paints at once');
    await subject.recentHistoryLoads.get(moved.id);
    assert.deepEqual(reads, [moved.id], 'only the moved chat reads its newest rows, behind the view');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
