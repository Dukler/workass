import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import type { DeliveryCapabilities } from '../src/wire/types.ts';

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

const receiptDelivery: DeliveryCapabilities = {
  stableInputIdentity: true,
  liveSteer: true,
  steerConsumptionReceipt: true,
  consumptionReceipt: true,
  turnReadback: true,
};

const genericLiveDelivery: DeliveryCapabilities = {
  ...receiptDelivery,
  steerConsumptionReceipt: false,
};

const queuedDelivery: DeliveryCapabilities = {
  ...genericLiveDelivery,
  liveSteer: false,
};

function chat(providerId = 'provider-receipt-fixture', deliveryCapabilities: DeliveryCapabilities | undefined = receiptDelivery): Chat {
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
    deliveryCapabilities,
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
function subject(
  api: Record<string, unknown> = {},
  providerId = 'provider-receipt-fixture',
  deliveryCapabilities: DeliveryCapabilities | undefined = receiptDelivery,
): { store: any; owner: Chat } {
  (globalThis as any).window = { api: { appChatSteer: async () => ({ ok: true }), ...api } };
  const store = new StoreCtor();
  const owner = chat(providerId, deliveryCapabilities);
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

test('a receipt-capable live steer stays staged and never bounces through the queue', async () => {
  const steerCalls: unknown[][] = [];
  const { store, owner } = subject({
    appChatSteer: async (...args: unknown[]) => {
      steerCalls.push(args);
      return { ok: true, live: true, strategy: 'receipt-live', turnId: 'provider-turn-1', receipt: true };
    },
  });
  running(owner);

  assert.equal(await store.steerRunning(owner.id, 'pará, hacé lo otro'), true);

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

test('a typed live-steer rejection restores composer ownership without creating FIFO work', async () => {
  const { store, owner } = subject({
    appChatSteer: async () => ({
      ok: false, live: false, interrupted: true, strategy: 'interrupt-queue',
      error: 'the delivery strategy rejected live steering',
    }),
  });
  running(owner);

  assert.equal(await store.steerRunning(owner.id, 'redirigí esto'), false);
  assert.equal(owner.queue, undefined, 'explicit steering must never create a queued follow-up');
  assert.equal(
    owner.messages.filter((message) => message.role === 'user' && message.content === 'redirigí esto').length,
    0,
    'the rejected temporary row is removed so the composer can reclaim ownership',
  );
});

test('provider identity cannot change receipt-boundary placement', async () => {
  for (const providerId of ['fixture-a', 'fixture-b']) {
    const calls: unknown[][] = [];
    const { store, owner } = subject({
      appChatSteer: async (...args: unknown[]) => {
        calls.push(args);
        return { ok: true, live: true, strategy: 'receipt-live', turnId: `${providerId}-turn`, receipt: true };
      },
    }, providerId, receiptDelivery);
    running(owner, `${providerId}-job`);

    assert.equal(await store.steerRunning(owner.id, `steer ${providerId}`), true);
    const steer = owner.messages.find((message) => message.role === 'user' && message.content === `steer ${providerId}`);
    assert.ok(steer);
    assert.equal(steer!.steerBoundary, 'waiting');
    assert.equal((calls[0][5] as { deferUntilConsumed?: boolean }).deferUntilConsumed, true);
  }
});

test('generic live steering uses its pending transcript row without a receipt boundary', async () => {
  let boundary: { deferUntilConsumed?: boolean } | undefined;
  const { store, owner } = subject({
    appChatSteer: async (...args: unknown[]) => {
      boundary = args[5] as { deferUntilConsumed?: boolean };
      return { ok: true, live: true, strategy: 'generic-live', turnId: 'generic-turn' };
    },
  }, 'arbitrary-generic-provider', genericLiveDelivery);
  running(owner);

  assert.equal(await store.steerRunning(owner.id, 'generic steer'), true);
  const steer = owner.messages.find((message) => message.role === 'user' && message.content === 'generic steer');
  assert.ok(steer);
  assert.equal(steer!.steerBoundary, undefined);
  assert.equal(boundary?.deferUntilConsumed, undefined);
  assert.equal(owner.queue, undefined);
});

test('a lane without live-steer capability rejects visibly without invoking steer or FIFO', async () => {
  let steerCalls = 0;
  const { store, owner } = subject({
    appChatSteer: async () => { steerCalls += 1; return { ok: true }; },
  }, 'arbitrary-queued-provider', queuedDelivery);
  running(owner);

  assert.equal(await store.steerRunning(owner.id, 'queue by capability'), false);
  assert.equal(steerCalls, 0);
  assert.equal(owner.queue, undefined);
  assert.equal(owner.messages.some((message) => message.content === 'queue by capability'), false);
	assert.equal(store.state.toasts.at(-1)?.title, 'No se pudo dirigir');
});

test('an explicit steer whose active turn just ended restores the composer without starting a new turn', async () => {
  let steerCalls = 0;
  let startCalls = 0;
  const { store, owner } = subject({
    appChatSteer: async () => { steerCalls += 1; return { ok: true }; },
    startJob: async () => { startCalls += 1; return { id: 'unexpected-job' }; },
  });
  running(owner);
  owner.messages[1].status = 'done';

  assert.equal(await store.steerRunning(owner.id, 'do not turn this into a new turn'), false);
  assert.equal(steerCalls, 0);
  assert.equal(startCalls, 0);
  assert.equal(owner.queue, undefined);
  assert.equal(owner.messages.some((message) => message.content === 'do not turn this into a new turn'), false);
  assert.equal(store.state.toasts.at(-1)?.title, 'No se pudo dirigir');
});

test('an old queue-shaped rejection cannot transfer explicit steer intent into renderer FIFO', async () => {
  const { store, owner } = subject({
    appChatSteer: async () => ({
      ok: false, live: false, interrupted: true, strategy: 'interrupt-queue',
      daemonQueued: true, error: 'the durable chat actor owns the follow-up',
    }),
  });
  running(owner);

  assert.equal(await store.steerRunning(owner.id, 'queue this once'), false);
  assert.equal(owner.queue, undefined, 'a queue-shaped rejection must not create renderer FIFO state');
  assert.equal(
    owner.messages.filter((message) => message.role === 'user' && message.content === 'queue this once').length,
    0,
    'the rejected temporary transcript owner was released',
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
  let hydrateOnSend: (() => void) | null = null;
  const { store, owner } = subject({
    startJob: async () => { hydrateOnSend?.(); return { id: 'job-3' }; },
  });
  owner.queue = [{ id: 'q-1', text: 'mandá esto' }];

  // The daemon snapshot predates the optimistic turn pair but still owns the
  // queued row. Exercise the real hydration boundary: it replaces the Chat
  // object and must carry the exact in-flight pair before job:start can accept.
  const staleMirror = store.toMirror(false);
  let replacement: Chat | null = null;
  hydrateOnSend = () => {
    assert.equal(store.restoreSessionSnapshot(staleMirror), true);
    replacement = store.chat(owner.id);
  };

  await store.flushNextQueued(owner);

  assert.ok(replacement, 'the stale daemon snapshot hydrated a replacement chat');
  assert.notStrictEqual(replacement, owner);
  assert.equal(store.chat('tab-1'), replacement);
  assert.deepEqual(replacement!.messages.map((message) => message.id), ['q-1', 'queue-assistant-q-1']);
  assert.equal(replacement!.queue, undefined, 'the accepted row leaves the live chat, not the orphan');
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
      return { ok: true, live: true, strategy: 'receipt-live', turnId: 'turn-1', receipt: true };
    },
  }, 'another-receipt-provider', receiptDelivery);
  running(owner);

  assert.equal(store.queueDraftMessage(owner.id, 'first, keep this queued', []), true);
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.equal(await store.steerRunning(owner.id, 'now steer the running turn'), true);
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
