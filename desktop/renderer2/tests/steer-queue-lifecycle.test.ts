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

after(async () => {
  await vite.close();
  delete (globalThis as any).window;
});

function chat(providerId = 'claude'): Chat {
  return {
    id: 'tab-1',
    chatId: 'chat-1',
    sessionId: 'session-1',
    sessionProviderId: providerId,
    title: 'Steering',
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-steer-test',
    currentModelId: 'model-1',
    currentModeId: 'agent',
    providerId,
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

function running(owner: Chat, jobId = 'job-1') {
  owner.messages.push(
    { id: 'u-0', role: 'user', content: 'arrancá', status: 'done', at: null, events: [] } as Msg,
    { id: 'a-0', role: 'assistant', content: 'trabajando…', status: 'running', at: null, events: [], jobId } as Msg,
  );
}

// window.api is the only bridge the store talks to; `has()` feature-detects on it.
function subject(api: Record<string, unknown> = {}, providerId = 'claude'): { store: any; owner: Chat } {
  (globalThis as any).window = { api: { appChatSteer: async () => ({ ok: true }), ...api } };
  const store = new StoreCtor();
  const owner = chat(providerId);
  store.state.chats = [owner];
  store.state.activeId = owner.id;
  store.state.connection = 'connected';
  store.flushSession = async () => {};
  store.archive = async () => {};
  store.ensureArchive = async () => {};
  store.schedulePersist = () => {};
  store.writeLocalMirrorNow = () => {};
  return { store, owner };
}

test('a live Claude steer lands in the transcript and never bounces through the queue', async () => {
  const steerCalls: unknown[][] = [];
  const { store, owner } = subject({
    appChatSteer: async (...args: unknown[]) => {
      steerCalls.push(args);
      return { ok: true, live: true, strategy: 'claude-live', turnId: 'sdk-uuid-1', receipt: true };
    },
  });
  running(owner);

  assert.equal(await store.steerOrQueue(owner.id, 'pará, hacé lo otro'), true);

  // The bug: Claude used to enqueue a FIFO copy before asking the daemon to
  // interrupt, so every steer visibly bounced into the queue list and the copy
  // was re-sent as a duplicate turn at the next job:end.
  assert.equal(owner.queue, undefined, 'a live steer must never create a FIFO row');
  const steer = owner.messages.find((message) => message.role === 'user' && message.content === 'pará, hacé lo otro');
  assert.ok(steer, 'the steer owns exactly one transcript row');
  assert.equal(steer!.steerState, 'accepted');

  // The client ids ride the request so the daemon can persist the same staged
  // ownership and commit it on the consumption receipt.
  assert.equal(steerCalls.length, 1);
  assert.equal(steerCalls[0][0], 'session-1');
  assert.equal(steerCalls[0][3], steer!.id);
  assert.equal((steerCalls[0][5] as { deferUntilConsumed?: boolean }).deferUntilConsumed, true);
});

test('only an adapter rejection moves a Claude steer into the FIFO, exactly once', async () => {
  const { store, owner } = subject({
    appChatSteer: async () => ({
      ok: false, live: false, interrupted: true, strategy: 'interrupt-queue',
      error: 'adapter viejo sin steering nativo',
    }),
  });
  running(owner);

  assert.equal(await store.steerOrQueue(owner.id, 'redirigí esto'), true);
  assert.equal(owner.queue?.length, 1, 'an interrupted steer becomes one queued follow-up');
  assert.equal(owner.queue?.[0].text, 'redirigí esto');
  assert.equal(
    owner.messages.filter((message) => message.role === 'user' && message.content === 'redirigí esto').length,
    0,
    'the rejected transcript row is removed so the queue row is the only owner',
  );
});

test('an actor-owned rejected steer does not create a second renderer FIFO row', async () => {
  const { store, owner } = subject({
    appChatSteer: async () => ({
      ok: false, live: false, interrupted: true, strategy: 'interrupt-queue',
      daemonQueued: true, error: 'the durable chat actor owns the follow-up',
    }),
  });
  running(owner);

  assert.equal(await store.steerOrQueue(owner.id, 'queue this once'), true);
  assert.equal(owner.queue, undefined, 'the daemon-owned FIFO must not be mirrored a second time in renderer state');
  assert.equal(
    owner.messages.filter((message) => message.role === 'user' && message.content === 'queue this once').length,
    0,
    'ownership transferred out of the transcript exactly once',
  );
});

test('a follow-up queued as the turn ends still drains instead of parking', async () => {
  const started: string[] = [];
  const { store, owner } = subject({
    startJob: async (opts: { prompt: string }) => { started.push(opts.prompt); return { id: 'job-2' }; },
  });
  running(owner);

  // The composer read `running` while the turn was alive; the terminal lands
  // between that check and the push, so job:end already drained an empty FIFO.
  const chatRef = store.chat(owner.id);
  assert.equal(store.queueDraftMessage(owner.id, 'seguí con esto', []), true);
  chatRef.messages[1].status = 'done';
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(store.queueDraftMessage(owner.id, 'y después esto', []), false, 'an idle chat sends, never queues');
  const queued = store.chat(owner.id)?.queue ?? [];
  assert.equal(queued.length, 1, 'the boundary row is still owned by exactly one surface');
  assert.equal(queued[0].text, 'seguí con esto');
});

test('a hydration during the drain cannot strand the accepted row in the live queue', async () => {
  let replaceOnSend: (() => void) | null = null;
  const { store, owner } = subject({
    startJob: async () => { replaceOnSend?.(); return { id: 'job-3' }; },
  });
  owner.queue = [{ id: 'q-1', text: 'mandá esto' }];

  // restoreSessionSnapshot replaces every chat object wholesale. The drain used
  // to remove the accepted row from its captured object, leaving the live chat
  // with a row nothing would ever drain again.
  const replacement = chat();
  replacement.queue = [{ id: 'q-1', text: 'mandá esto' }];
  replaceOnSend = () => { store.state.chats = [replacement]; };

  await store.flushNextQueued(owner);

  assert.equal(store.chat('tab-1'), replacement);
  assert.equal(replacement.queue, undefined, 'the accepted row leaves the live chat, not the orphan');
});

test('a delayed queue save cannot strand a later accepted steer beside the composer', async () => {
  let replacement: Chat | null = null;
  const { store, owner } = subject({
    appChatSteer: async () => {
      // A persisted queue mutation is followed by an actor/session digest. That
      // hydration replaces the renderer Chat object while the steer request is
      // awaiting its native acknowledgement, but preserves the daemon-owned
      // pending pair and the older FIFO row by stable id.
      replacement = {
        ...owner,
        queue: owner.queue?.map((item) => ({ ...item })),
        messages: owner.messages.map((message) => ({ ...message, events: [...message.events] })),
      };
      store.state.chats = [replacement];
      await Promise.resolve();
      return { ok: true, live: true, strategy: 'codex-live', turnId: 'turn-1', receipt: true };
    },
  }, 'codex');
  running(owner);

  assert.equal(store.queueDraftMessage(owner.id, 'first, keep this queued', []), true);
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(await store.steerOrQueue(owner.id, 'now steer the running turn'), true);
  assert.ok(replacement, 'the queue digest replaced the renderer chat');
  assert.equal(store.chat(owner.id), replacement);
  assert.deepEqual(replacement!.queue?.map((item) => item.text), ['first, keep this queued']);
  const steer = replacement!.messages.find((message) => message.role === 'user' && message.content === 'now steer the running turn');
  assert.ok(steer, 'the accepted steer keeps its stable owner after hydration');
  assert.equal(steer!.steerState, 'accepted');
  assert.equal(steer!.steerBoundary, 'waiting', 'only the native consumption receipt may commit it into chronology');

  store.rebuildJobRefs();
  store.onJobEvent({
    type: 'acp', id: 'job-1',
    event: { kind: 'steer-consumed', clientUserMessageId: steer!.id },
  });
  assert.equal(steer!.steerState, 'applied');
  assert.equal(steer!.steerBoundary, undefined, 'the receipt moves the same row out of the composer-adjacent preview');
  assert.deepEqual(replacement!.queue?.map((item) => item.text), ['first, keep this queued'], 'the older FIFO owner remains independent');
});
