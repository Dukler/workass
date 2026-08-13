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
    draft: '',
  } as Chat;
}

test('switching chats evicts an idle archive tail and reloads the complete ledger on return', async () => {
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
    subject.archivesLoaded.add(first.id);

    subject.switchChat(second.id);
    assert.equal(first.messages.length, 60);
    assert.equal(first._archivedCount, 85);
    assert.equal(subject.archivesLoaded.has(first.id), false);

    subject.switchChat(first.id);
    await subject.archiveLoads.get(first.id);
    assert.equal(first.messages.length, 85);
    assert.equal(subject.archivesLoaded.has(first.id), true);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a daemon refresh compacts every inactive complete history but keeps the active transcript visible', () => {
  const subject = new StoreCtor();
  const active = chat('tab-active', messages(80));
  const inactive = chat('tab-inactive', messages(90));
  subject.state.chats = [active, inactive];
  subject.state.activeId = active.id;
  subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };

  const server = subject.toMirror(false, false);
  server.chats[0].messages = active.messages;
  server.chats[1].messages = inactive.messages;
  assert.equal(subject.restoreSessionSnapshot(server), true);

  assert.equal(subject.chat(active.id)?.messages.length, 80);
  assert.equal(subject.chat(inactive.id)?.messages.length, 60);
  assert.equal(subject.chat(inactive.id)?._archivedCount, 90);
});
