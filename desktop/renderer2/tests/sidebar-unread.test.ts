import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';

// The sidebar's finished cue (v2's "Listo" pill, v1's dot) reads `chat.unread`.
// Nothing ever SET that flag — switchChat only cleared it — so the state was
// unreachable in a real session. These tests pin the setter and its meaning:
// unseen, not merely finished.

let vite: ViteDevServer;
let StoreCtor: new () => any;
let resolveStatus: (chat: unknown, live: boolean, active: boolean) => string;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  resolveStatus = (await vite.ssrLoadModule('/src/components/SidebarV2.tsx')).resolveStatus;
});

after(async () => { await vite.close(); });

function chat(id: string, chatId: string): Chat {
  return {
    id,
    chatId,
    sessionId: `session-${id}`,
    sessionProviderId: 'codex',
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-test',
    currentModelId: 'model-1',
    currentModeId: 'agent',
    providerId: 'codex',
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

function job(overrides: Partial<PublicJob> = {}): PublicJob {
  return {
    id: 'job-bg',
    kind: 'app-chat',
    key: null,
    title: 'background turn',
    status: 'done',
    startedAt: '2026-07-25T12:00:00Z',
    finishedAt: '2026-07-25T12:00:09Z',
    code: 0,
    permissionMode: 'agent',
    chatId: 'chat-2',
    tabId: 'tab-2',
    sessionId: 'session-tab-2',
    providerId: 'codex',
    userMessageId: 'user-bg',
    assistantMessageId: 'assistant-bg',
    promptText: 'background prompt',
    result: 'background answer',
    error: null,
    stopReason: null,
    crashInterrupted: false,
    ...overrides,
  };
}

function subject(): { store: any; watched: Chat; background: Chat } {
  const store = new StoreCtor();
  const watched = chat('tab-1', 'chat-1');
  const background = chat('tab-2', 'chat-2');
  store.state.chats = [watched, background];
  store.state.activeId = watched.id;
  return { store, watched, background };
}

test('a turn that ends in a chat you are not watching becomes the finished cue', () => {
  const { store, background } = subject();

  store.onJobEvent({ type: 'end', job: job() });

  assert.equal(background.unread, true);
  assert.equal(resolveStatus(background, false, false), 'done');
});

test('a turn that ends in the chat you are watching is not news', () => {
  const { store, watched } = subject();

  store.onJobEvent({ type: 'end', job: job({ chatId: 'chat-1', tabId: 'tab-1', sessionId: 'session-tab-1' }) });

  assert.equal(watched.unread, undefined);
  assert.equal(resolveStatus(watched, false, true), 'ready');
});

test('a cancel is your own doing, so it never waits for you', () => {
  const { store, background } = subject();

  store.onJobEvent({ type: 'end', job: job({ status: 'failed', code: 130, stopReason: 'cancelled' }) });

  assert.equal(background.unread, undefined);
});

test('a background failure waits as a failure, and opening the chat clears it', () => {
  const { store, background } = subject();

  store.onJobEvent({ type: 'end', job: job({ status: 'failed', code: 1, error: 'boom', result: null }) });
  assert.equal(background.unread, true);
  // failed outranks done: an unseen break is louder than an unseen success.
  assert.equal(resolveStatus(background, false, false), 'failed');

  store.switchChat(background.id);
  assert.equal(background.unread, false);
  // Reading the broken chat is what clears it: the error is on screen in the
  // transcript, so the row reports nothing you cannot already see.
  assert.equal(resolveStatus(background, false, true), 'ready');
  assert.equal(resolveStatus(background, false, false), 'ready');
});

test('a daemon restart is not an agent error', () => {
  const { store, background } = subject();

  store.onJobEvent({ type: 'end', job: job({ status: 'failed', code: 1, error: 'boom', result: null }) });
  assert.equal(resolveStatus(background, false, false), 'failed');

  // What the daemon's orphan sweep stamps on every turn it kills at restart.
  const last = background.messages[background.messages.length - 1];
  last.interrupted = true;
  assert.equal(resolveStatus(background, false, false), 'done');
});

test('the daemon reports its own interruption on the turn it ends', () => {
  const { store, background } = subject();

  // A restart closes every bridge, so the in-flight prompt fails and the job is
  // finalized before the process exits. Nothing is left running for the next
  // boot's sweep to stamp, so the interruption has to ride this event.
  store.onJobEvent({
    type: 'end',
    job: job({
      status: 'failed', code: 1, result: null,
      error: 'ACP session reset', stopReason: 'daemon-restart', interrupted: true,
    }),
  });

  const last = background.messages[background.messages.length - 1];
  assert.equal(last.interrupted, true);
  // Not unseen news either: otherwise every background chat comes back from a
  // restart wearing a "Listo" cue for a turn that never finished.
  assert.equal(background.unread, undefined);
  assert.equal(resolveStatus(background, false, false), 'ready');
});
