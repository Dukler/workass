import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg } from '../src/store/types.ts';
import type { DeliveryCapabilities } from '../src/wire/types.ts';
import { composerKeyAction } from '../src/composer-submit.ts';
import { normalizeDeliveryCapabilities, stopAndSendSupported } from '../src/steering.ts';

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

test('one keyboard submission calls live steering directly across native and ACP lanes', async () => {
  for (const providerId of ['codex', 'claude', 'omp', 'pi', 'devin', 'mock', 'custom-acp']) {
    for (const modifier of ['ctrlKey', 'metaKey']) {
      let calls = 0;
      const { store, owner } = subject({
        appChatSteer: async () => { calls++; return { ok: true, live: true, strategy: 'generic-live' }; },
      }, providerId, genericLiveDelivery);
      running(owner);
      store.setDraft(owner.id, 'use this direction @agent');
      const action = composerKeyAction(true, { key: 'Enter', shiftKey: false, ctrlKey: false, metaKey: false, [modifier]: true }, true);
      assert.equal(action, 'steer');
      const submission = store.captureDraftSubmission(owner.id, owner.draft);
      assert.equal(await store.steerRunning(owner.id, owner.draft, undefined, submission), true);
      assert.equal(calls, 1, providerId);
      assert.equal(owner.queue, undefined, providerId);
      assert.equal(owner.draft, '');
      assert.equal(owner.messages.filter(m => m.content === 'use this direction @agent').length, 1);
    }
  }
});

test('stop-and-send is a separate typed action; a real live-steer capability always wins', () => {
  assert.equal(stopAndSendSupported(normalizeDeliveryCapabilities({ stopAndSend: true, liveSteer: false })), true);
  assert.equal(stopAndSendSupported(normalizeDeliveryCapabilities({ stopAndSend: true, liveSteer: true })), false);
  assert.equal(stopAndSendSupported(normalizeDeliveryCapabilities({})), false);
  assert.equal(stopAndSendSupported(undefined), false);
});

test('one stop-and-send shortcut durably queues attachments before cancelling its exact turn', async () => {
  const calls: string[] = [];
  let queueRequest: any;
  let acceptQueue!: (value: unknown) => void;
  const { store, owner } = subject({
    appChatSteer: async () => { assert.fail('stop-and-send is not ACP live steering'); },
    chatQueueReplace: (request: any) => {
      calls.push('queue'); queueRequest = request;
      return new Promise((resolve) => { acceptQueue = resolve; });
    },
    cancelJob: async (jobId: string) => { calls.push(`cancel:${jobId}`); return { cancelled: true, reason: 'pending' }; },
  }, 'capability-fixture', { ...queuedDelivery, stopAndSend: true });
  running(owner);
  owner.queue = [{ id: 'earlier', text: 'earlier FIFO message' }];
  store.setDraft(owner.id, 'new direction');
  const images = [{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'fixture.png' }];
  const action = composerKeyAction(true, { key: 'Enter', shiftKey: false, ctrlKey: true, metaKey: false }, true);
  assert.equal(action, 'steer');
  const pending = store.steerRunning(owner.id, owner.draft, images);
  assert.deepEqual(calls, ['queue']);
  assert.equal(owner.draft, '');
  assert.equal(queueRequest.tabId, owner.id);
  assert.equal(queueRequest.chatId, owner.chatId);
  assert.deepEqual(queueRequest.queue.map((q: any) => q.text), ['earlier FIFO message', 'new direction']);
  assert.deepEqual(queueRequest.queue[1].images, images);
  // Hydration may replace objects while the durable receipt is in flight.
  const hydrated = { ...owner, messages: owner.messages.map(m => ({ ...m })), queue: owner.queue.map(q => ({ ...q })) };
  store.state.chats = [hydrated];
  store.setDraft(owner.id, 'next local draft');
  acceptQueue({ ok: true, operationId: queueRequest.operationId, agentQueueRevision: 1, actorRevision: 2 });
  assert.equal(await pending, true);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  assert.equal(hydrated.draft, 'next local draft');
  assert.equal(hydrated.queue.length, 2);
  assert.equal(hydrated.messages.filter(m => m.role === 'user' && m.content === 'new direction').length, 0);
});

test('one Devin steering shortcut owns a fresh message with an empty FIFO before stopping the exact turn', async () => {
  for (const modifier of ['ctrlKey', 'metaKey']) {
    const calls: string[] = [];
    let request: any;
    const { store, owner } = subject({
      appChatSteer: async () => { assert.fail('Devin stop-and-send must not claim native live steering'); },
      chatQueueReplace: async (value: any) => {
        calls.push('queue'); request = value;
        return { ok: true, operationId: value.operationId, agentQueueRevision: 1, actorRevision: 2 };
      },
      cancelJob: async (jobId: string) => { calls.push(`cancel:${jobId}`); return { cancelled: true }; },
    }, 'devin', { ...queuedDelivery, stopAndSend: true });
    running(owner);
    assert.equal(owner.queue, undefined);
    store.setDraft(owner.id, 'send this direction once');
    const action = composerKeyAction(true, { key: 'Enter', shiftKey: false, ctrlKey: false, metaKey: false, [modifier]: true }, true);
    assert.equal(action, 'steer');
    assert.equal(await store.steerRunning(owner.id, owner.draft), true);
    assert.deepEqual(calls, ['queue', 'cancel:job-1']);
    assert.equal(request.queue.length, 1);
    assert.equal(request.queue[0].text, 'send this direction once');
    assert.ok(request.queue[0].id);
    assert.ok(request.operationId);
    assert.equal(owner.draft, '');
  }
});

test('a repeated Devin steering shortcut reuses the same queue identity and exact cancellation', async () => {
  const calls: string[] = [];
  let request: any;
  let acceptQueue!: (value: unknown) => void;
  let acceptCancel!: (value: unknown) => void;
  const { store, owner } = subject({
    chatQueueReplace: (value: any) => {
      calls.push('queue'); request = value;
      return new Promise((resolve) => { acceptQueue = resolve; });
    },
    cancelJob: (jobId: string) => {
      calls.push(`cancel:${jobId}`);
      return new Promise((resolve) => { acceptCancel = resolve; });
    },
  }, 'devin', { ...queuedDelivery, stopAndSend: true });
  running(owner);
  store.setDraft(owner.id, 'one immutable action');
  const submission = store.captureDraftSubmission(owner.id, owner.draft);
  const first = store.steerRunning(owner.id, owner.draft, undefined, submission);
  const repeated = store.steerRunning(owner.id, 'one immutable action', undefined, submission);
  assert.deepEqual(calls, ['queue']);
  assert.equal(owner.queue?.length, 1);
  assert.equal(request.queue.length, 1);
  const queueId = request.queue[0].id;
  const operationId = request.operationId;
  acceptQueue({ ok: true, operationId, agentQueueRevision: 1, actorRevision: 2 });
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  const repeatedAfterReceipt = store.steerRunning(owner.id, 'one immutable action', undefined, submission);
  assert.equal(owner.queue?.length, 1);
  assert.equal(owner.queue?.[0].id, queueId);
  acceptCancel({ cancelled: true });
  assert.deepEqual(await Promise.all([first, repeated, repeatedAfterReceipt]), [true, true, true]);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
});

test('a pre-ownership Devin rejection releases the same captured edit for retry', async () => {
  const calls: string[] = [];
  const { store, owner } = subject({}, 'devin', { ...queuedDelivery, stopAndSend: true });
  running(owner);
  store.setDraft(owner.id, 'retry this exact direction');
  const submission = store.captureDraftSubmission(owner.id, owner.draft);

  assert.equal(typeof submission.edit, 'object');
  assert.equal(await store.steerRunning(owner.id, owner.draft, undefined, submission), false);
  assert.equal(owner.queue, undefined);
  assert.equal(owner.draft, 'retry this exact direction');
  assert.deepEqual(calls, []);

  const api = (globalThis as any).window.api;
  api.chatQueueReplace = async (request: any) => {
    calls.push('queue');
    return { ok: true, operationId: request.operationId, agentQueueRevision: 1, actorRevision: 2 };
  };
  api.cancelJob = async (jobId: string) => { calls.push(`cancel:${jobId}`); return { cancelled: true }; };

  assert.equal(await store.steerRunning(owner.id, owner.draft, undefined, submission), true);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  assert.equal(owner.queue?.length, 1);
  assert.equal(owner.queue?.[0].text, 'retry this exact direction');
  assert.equal(owner.draft, '');

  // Once queue ownership transfers, a retry of the same captured edit reuses
  // that action even after its durable queue and cancellation receipts settle.
  assert.equal(await store.steerRunning(owner.id, 'retry this exact direction', undefined, submission), true);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  assert.equal(owner.queue?.length, 1);
});

test('an absent runtime edit identity uses one stable captured-submission fallback', async () => {
  const calls: string[] = [];
  let request: any;
  let acceptQueue!: (value: unknown) => void;
  const { store, owner } = subject({
    chatQueueReplace: (value: any) => {
      calls.push('queue'); request = value;
      return new Promise((resolve) => { acceptQueue = resolve; });
    },
    cancelJob: async (jobId: string) => { calls.push(`cancel:${jobId}`); return { cancelled: true }; },
  }, 'devin', { ...queuedDelivery, stopAndSend: true });
  running(owner);
  store.setDraft(owner.id, 'stable fallback identity');
  const submission = store.captureDraftSubmission(owner.id, owner.draft) as any;
  submission.edit = undefined;

  const first = store.steerRunning(owner.id, owner.draft, undefined, submission);
  assert.equal(owner.queue?.length, 1);
  assert.equal(owner.draft, '');
  const repeatedInFlight = store.steerRunning(owner.id, 'stable fallback identity', undefined, submission);
  assert.equal(owner.queue?.length, 1);
  assert.deepEqual(calls, ['queue']);

  acceptQueue({ ok: true, operationId: request.operationId, agentQueueRevision: 1, actorRevision: 2 });
  assert.deepEqual(await Promise.all([first, repeatedInFlight]), [true, true]);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  assert.equal(await store.steerRunning(owner.id, 'stable fallback identity', undefined, submission), true);
  assert.deepEqual(calls, ['queue', 'cancel:job-1']);
  assert.equal(owner.queue?.length, 1);
});

test('failed or mismatched queue admission never stops and retains the same pending operation', async () => {
  for (const failure of ['missing', 'rejected', 'wrong-operation', 'transport']) {
    let cancels = 0;
    let operationId = '';
    const { store, owner } = subject({
      chatQueueReplace: async (request: any) => {
        operationId = request.operationId;
        if (failure === 'transport') throw new Error('offline');
        if (failure === 'missing') return undefined;
        return { ok: failure !== 'rejected', operationId: failure === 'wrong-operation' ? 'other-operation' : request.operationId };
      },
      cancelJob: async () => { cancels++; return true; },
    }, 'capability-fixture', { ...queuedDelivery, stopAndSend: true });
    store.requestChatReadback = () => {};
    running(owner);
    store.setDraft(owner.id, 'keep this input');
    assert.equal(await store.steerRunning(owner.id, owner.draft), true);
    assert.equal(cancels, 0, failure);
    assert.equal(owner.queue?.length, 1, failure);
    assert.equal(owner.queue?.[0].text, 'keep this input');
    assert.equal(store.pendingQueueOperationIds.get(owner.id), operationId);
  }
});

test('queue receipt cannot cancel a replacement turn, replacement chat, or removed input', async () => {
  for (const race of ['turn', 'job', 'chat', 'removed']) {
    let cancels = 0;
    const { store, owner } = subject({
      chatQueueReplace: async (request: any) => {
        if (race === 'turn') { owner.messages[1].status = 'done'; owner.messages.push({ id: 'replacement', role: 'assistant', status: 'running', content: '', events: [], at: null, jobId: 'job-2' }); }
        if (race === 'job') owner.messages[1].jobId = 'job-2';
        if (race === 'chat') owner.chatId = 'replacement-chat';
        if (race === 'removed') store.removeQueued(owner.id, owner.queue![0].id);
        return { ok: true, operationId: request.operationId, agentQueueRevision: 1, actorRevision: 2 };
      },
      cancelJob: async () => { cancels++; return true; },
    }, 'capability-fixture', { ...queuedDelivery, stopAndSend: true });
    running(owner);
    assert.equal(await store.steerRunning(owner.id, 'direction'), true);
    assert.equal(cancels, 0, race);
  }
});

test('a failed stop leaves one durable FIFO owner without automatic cancellation retry', async () => {
  let cancels = 0;
  const { store, owner } = subject({
    chatQueueReplace: async (request: any) => ({ ok: true, operationId: request.operationId, agentQueueRevision: 1, actorRevision: 2 }),
    cancelJob: async () => { cancels++; throw new Error('offline'); },
  }, 'capability-fixture', { ...queuedDelivery, stopAndSend: true });
  store.requestChatReadback = () => {};
  running(owner);
  assert.equal(await store.steerRunning(owner.id, 'direction'), true);
  assert.equal(cancels, 1);
  assert.equal(owner.queue?.length, 1);
  assert.equal(store.pendingQueueOperationIds.has(owner.id), false);
  assert.equal(owner.messages[1].status, 'running');
});

test('a terminal event during queue admission drains the existing FIFO without cancelling', async () => {
  let cancels = 0;
  const { store, owner } = subject({
    chatQueueReplace: async (request: any) => {
      owner.messages[1].status = 'done';
      return { ok: true, operationId: request.operationId, agentQueueRevision: 1, actorRevision: 2 };
    },
    cancelJob: async () => { cancels++; return true; },
  }, 'capability-fixture', { ...queuedDelivery, stopAndSend: true });
  running(owner);
  owner.queue = [{ id: 'earlier', text: 'earlier FIFO message' }];
  let drained: Chat | undefined;
  store.flushNextQueued = async (chat: Chat) => { drained = chat; };
  assert.equal(await store.steerRunning(owner.id, 'direction'), true);
  assert.equal(cancels, 0);
  assert.equal(drained, owner);
  assert.deepEqual(owner.queue?.map(item => item.text), ['earlier FIFO message', 'direction']);
});

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
  store.setDraft(owner.id, 'keep on unsupported provider');

  assert.equal(await store.steerRunning(owner.id, owner.draft), false);
  assert.equal(steerCalls, 0);
  assert.equal(owner.queue, undefined);
  assert.equal(owner.draft, 'keep on unsupported provider');
  assert.equal(owner.messages.some((message) => message.content === 'keep on unsupported provider'), false);
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
  store.setDraft(owner.id, 'retain after turn end');

  assert.equal(await store.steerRunning(owner.id, owner.draft), false);
  assert.equal(steerCalls, 0);
  assert.equal(startCalls, 0);
  assert.equal(owner.queue, undefined);
  assert.equal(owner.draft, 'retain after turn end');
  assert.equal(owner.messages.some((message) => message.content === 'retain after turn end'), false);
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


test('steering after attachment preparation preserves a newer draft through rejection', async () => {
  let reject!: (value: unknown) => void;
  const { store, owner } = subject({ appChatSteer: () => new Promise((resolve) => { reject = resolve; }) });
  running(owner);
  store.setDraft(owner.id, 'direction');
  const submission = store.captureDraftSubmission(owner.id, 'direction');
  store.setDraft(owner.id, 'new typing during encoding');
  const delivery = store.steerRunning(owner.id, 'direction', undefined, submission);
  assert.equal(owner.draft, 'new typing during encoding');
  await new Promise((resolve) => setImmediate(resolve));
  reject({ ok: false, strategy: 'rejected' });
  await delivery;
  assert.equal(owner.draft, 'new typing during encoding');
});
