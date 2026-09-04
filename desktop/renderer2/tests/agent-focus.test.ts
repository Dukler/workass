import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
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
    at: `2026-08-18T12:${String(index % 60).padStart(2, '0')}:00Z`,
    events: [],
  })) as Msg[];
}

function chat(id: string, rows = messages(2), complete = true): Chat {
  return {
    id,
    chatId: `chat-${id}`,
    sessionId: null,
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-agent-focus',
    providerId: 'mock',
    currentModelId: null,
    currentModeId: null,
    pending: false,
    messages: rows,
    messageCount: complete ? rows.length : 80,
    historyComplete: complete,
    draft: '',
  } as Chat;
}

function subjectWith(chats: Chat[], activeId: string): any {
  const subject = new StoreCtor();
  subject.state.chats = chats;
  subject.state.activeId = activeId;
  subject.state.meta = { daemon: true, sessionSaveMode: 'lean-payload-v2' };
  subject.monitor = { probeNow() {} };
  subject.schedulePersist = () => {};
  return subject;
}

test('an exact one-shot agent focus changes the visible chat and a generic refresh does not replay it', () => {
  const first = chat('tab-first');
  const target = chat('tab-target');
  const subject = subjectWith([first, target], first.id);

  subject.onAgentApply({
    action: 'session-refresh', tabId: target.id, chatId: 'wrong-chat-id', focus: true,
  });
  assert.equal(subject.state.activeId, first.id, 'tab id alone must never address a focus target');

  subject.onAgentApply({
    action: 'session-refresh', tabId: target.id, chatId: target.chatId, focus: true,
  });
  assert.equal(subject.state.activeId, target.id);
  assert.equal(subject.pendingAgentFocus, null);

  subject.state.activeId = first.id;
  subject.onAgentApply({ action: 'session-refresh' });
  assert.equal(subject.state.activeId, first.id, 'the trailing generic refresh must not replay old focus');
});

test('a focus arriving before its actor row is applied after hydration without adopting ordinary server focus', () => {
  const first = chat('tab-first');
  const target = chat('tab-target');
  const authoritativeSource = subjectWith([first, target], target.id);
  const authoritative = authoritativeSource.toMirror(false);
  const subject = subjectWith([chat('tab-first')], first.id);

  subject.onAgentApply({
    action: 'session-refresh', tabId: target.id, chatId: target.chatId, focus: true,
  });
  assert.equal(subject.state.activeId, first.id, 'an unknown tab must wait for exact hydration');

  assert.equal(subject.restoreSessionSnapshot(authoritative), true);
  assert.equal(subject.state.activeId, target.id, 'the explicit focus intent must beat local-active preservation once');
  assert.equal(subject.pendingAgentFocus, null);

  subject.state.activeId = first.id;
  assert.equal(subject.restoreSessionSnapshot(authoritative), true);
  assert.equal(subject.state.activeId, first.id, 'later ordinary hydrations keep the local selection');
});

test('a newer local selection cancels an agent focus still waiting for hydration', () => {
  const first = chat('tab-first');
  const target = chat('tab-target');
  const authoritativeSource = subjectWith([first, target], target.id);
  const subject = subjectWith([chat('tab-first')], first.id);

  subject.onAgentApply({
    action: 'session-refresh', tabId: target.id, chatId: target.chatId, focus: true,
  });
  subject.switchChat(first.id);
  assert.equal(subject.pendingAgentFocus, null);

  assert.equal(subject.restoreSessionSnapshot(authoritativeSource.toMirror(false)), true);
  assert.equal(subject.state.activeId, first.id);
});

test('re-focusing a selected resident tail does not force a complete history load', async () => {
  const full = messages(80);
  const tail = full.slice(-60);
  const target = chat('tab-target', tail, false);
  const previousWindow = (globalThis as any).window;
  let archiveReads = 0;
  (globalThis as any).window = {
    api: { archiveLoad: async () => { archiveReads += 1; return full; } },
  };
  try {
    const subject = subjectWith([target], target.id);
    subject.onAgentApply({
      action: 'session-refresh', tabId: target.id, chatId: target.chatId, focus: true,
    });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(target.messages.length, 60);
    assert.equal(target.historyComplete, false);
    assert.equal(archiveReads, 0, 'focus is presentation intent, not a request to expand every historical attachment');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
