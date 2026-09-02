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

test('switching chats releases an idle full history and reloads the actor ledger on return', async () => {
  const complete = messages(85);
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string) => tabId === 'tab-a' ? complete : [],
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
    await subject.fullHistoryLoads.get(first.id);
    assert.equal(first.messages.length, 85);
    assert.equal(first.historyComplete, true);
    assert.equal(subject.fullHistoriesLoaded.has(first.id), true);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('opening an incomplete chat keeps the current transcript visible until history is ready', async () => {
  const firstRows = messages(85);
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string) => tabId === 'tab-target' ? history : [],
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

    assert.equal(subject.state.activeId, first.id);
    assert.equal(first.messages.length, firstRows.length);
    assert.equal(target.messages.length, 60);

    historyReleased = true;
    releaseHistory(targetRows);
    await subject.fullHistoryLoads.get(target.id);
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, target.id);
    assert.equal(target.messages.length, targetRows.length);
    assert.equal(target.historyComplete, true);
    assert.equal(first.messages.length, 60);
    assert.equal(first.historyComplete, false);
  } finally {
    if (!historyReleased) releaseHistory([]);
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a completed older history read cannot steal focus from a newer click', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string) => tabId === 'tab-target' ? history : [],
    },
  };
  try {
    const subject = new StoreCtor();
    const first = chat('tab-first', messages(4));
    const target = chat('tab-target', targetRows.slice(-60));
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
    releaseHistory(targetRows);
    await subject.fullHistoryLoads.get(target.id);
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, latest.id);
    assert.equal(target.messages.length, 60);
    assert.equal(target.historyComplete, false);
    assert.equal(subject.fullHistoriesLoaded.has(target.id), false);
  } finally {
    if (!historyReleased) releaseHistory([]);
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('repeated clicks on one incomplete chat share its completed history without an eviction flash', async () => {
  const targetRows = messages(92);
  const previousWindow = (globalThis as any).window;
  let releaseHistory!: (rows: Msg[]) => void;
  let historyReleased = false;
  let archiveCalls = 0;
  const history = new Promise<Msg[]>((resolve) => { releaseHistory = resolve; });
  const blockedRepeat = new Promise<Msg[]>(() => {});
  (globalThis as any).window = {
    api: {
      archiveLoad: async () => (++archiveCalls === 1 ? history : blockedRepeat),
    },
  };
  try {
    const subject = new StoreCtor();
    const first = chat('tab-first', messages(4));
    const target = chat('tab-target', targetRows.slice(-60));
    target.messageCount = targetRows.length;
    target.historyComplete = false;
    subject.state.chats = [first, target];
    subject.state.activeId = first.id;
    subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

    subject.switchChat(target.id);
    subject.switchChat(target.id);
    assert.equal(subject.state.activeId, first.id);

    historyReleased = true;
    releaseHistory(targetRows);
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(subject.state.activeId, target.id);
    assert.equal(target.messages.length, targetRows.length);
    assert.equal(target.historyComplete, true);
    assert.equal(archiveCalls, 1);
  } finally {
    if (!historyReleased) releaseHistory([]);
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

test('a metadata-only refresh invalidates a stale loaded marker and reloads the visible chat', async () => {
  const complete = messages(14);
  const previousWindow = (globalThis as any).window;
  let archiveReads = 0;
  (globalThis as any).window = {
    api: {
      archiveLoad: async (tabId: string) => {
        archiveReads += 1;
        return tabId === 'tab-active' ? complete : [];
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
    // A newer actor revision prevents suffix preservation when that projection
    // has no rows; the renderer must clear its old residency marker and pull
    // the canonical actor ledger instead of painting an empty new chat.
    const server = subject.toMirror(false);
    server.chats[0].actorRevision = 8;
    server.chats[0].messages = [];
    server.chats[0].messageCount = complete.length;
    server.chats[0].historyComplete = false;

    assert.equal(subject.restoreSessionSnapshot(server), true);
    const load = subject.fullHistoryLoads.get(active.id);
    assert.ok(load, 'visible incomplete history must start an actor archive read');
    await load;

    assert.equal(archiveReads, 1);
    assert.equal(subject.chat(active.id)?.messages.length, complete.length);
    assert.equal(subject.chat(active.id)?.historyComplete, true);
    assert.equal(subject.fullHistoriesLoaded.has(active.id), true);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
