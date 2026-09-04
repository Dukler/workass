import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat, Msg } from '../src/store/types.ts';

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
    const subject = new StoreCtor();
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
    assert.deepEqual(archiveCalls, [{ tabId: first.id, options: undefined }]);
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
    const subject = new StoreCtor();
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
      { tabId: target.id, options: { beforeMessageId: 'message-85', limit: 40 } },
      { tabId: target.id, options: { beforeMessageId: 'message-45', limit: 40 } },
      { tabId: target.id, options: { beforeMessageId: 'message-5', limit: 40 } },
    ]);
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
    const subject = new StoreCtor();
    const target = chat('tab-old-daemon', complete.slice(-60));
    target.messageCount = complete.length;
    target.historyComplete = false;
    subject.state.chats = [target];
    subject.state.activeId = target.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    assert.equal(await subject.loadOlderHistory(target.id, 40), true);
    assert.deepEqual(target.messages.map((message) => message.id), complete.map((message) => message.id));
    assert.equal(target.historyComplete, true);
    assert.deepEqual(archiveCalls, [{ beforeMessageId: 'message-40', limit: 40 }]);
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
    const subject = new StoreCtor();
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
    const subject = new StoreCtor();
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
    assert.deepEqual(archiveCalls, [{ tabId: target.id, options: { tail: 10 } }]);

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
    const subject = new StoreCtor();
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
    const subject = new StoreCtor();
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
    const subject = new StoreCtor();
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
  const subject = new StoreCtor();
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
    const subject = new StoreCtor();
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
    assert.deepEqual(archiveOptions, { tail: 10 });
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
  const subject = new StoreCtor();
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
    const subject = new StoreCtor();
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
