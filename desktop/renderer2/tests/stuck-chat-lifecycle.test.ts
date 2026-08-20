import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat, Msg, QueuedMsg } from '../src/store/types.ts';
import type { PublicJob, StartJobReply, StateDigest } from '../src/wire/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  const loaded = await vite.ssrLoadModule('/src/store/store.ts');
  StoreCtor = loaded.Store;
});

after(async () => { await vite.close(); });

function chat(id = 'tab-1', chatId = 'chat-1'): Chat {
  return {
    id,
    chatId,
    sessionId: 'session-1',
    sessionProviderId: 'codex',
    title: 'Lifecycle',
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
    id: 'job-1',
    kind: 'app-chat',
    key: null,
    title: 'Lifecycle',
    status: 'running',
    startedAt: '2026-07-24T12:00:00Z',
    finishedAt: null,
    code: null,
    permissionMode: 'agent',
    chatId: 'chat-1',
    tabId: 'tab-1',
    sessionId: 'session-1',
    providerId: 'codex',
    userMessageId: 'user-canonical',
    assistantMessageId: 'assistant-canonical',
    promptText: 'canonical prompt',
    result: null,
    error: null,
    stopReason: null,
    crashInterrupted: false,
    ...overrides,
  };
}

function subjectWithChat(): { subject: any; owner: Chat } {
  const subject = new StoreCtor();
  const owner = chat();
  subject.state.chats = [owner];
  subject.state.activeId = owner.id;
  return { subject, owner };
}

test('an unanchored job:start synthesizes canonical rows and streams into the assistant', () => {
  const { subject, owner } = subjectWithChat();

  (subject as any).onJobEvent({ type: 'start', job: job() });
  assert.deepEqual(owner.messages.map((message) => message.id), ['user-canonical', 'assistant-canonical']);
  assert.equal(owner.messages[0].content, 'canonical prompt');
  assert.equal(owner.messages[1].status, 'running');
  assert.equal(owner.messages[1].jobId, 'job-1');

  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'hello' });
  assert.equal(owner.messages[1].content, 'hello');
  assert.equal(owner.messages[1].status, 'running');
});

test('an unanchored job:end paints a terminal canonical row from the public job', () => {
  const { subject, owner } = subjectWithChat();
  const terminal = job({
    status: 'done',
    finishedAt: '2026-07-24T12:00:02Z',
    result: 'recovered final answer',
    images: [{ mimeType: 'image/png', data: 'aGVsbG8=', name: 'result.png' }],
  });

  (subject as any).onJobEvent({ type: 'end', job: terminal });
  const assistant = owner.messages.find((message) => message.id === 'assistant-canonical');
  assert.equal(assistant?.status, 'done');
  assert.equal(assistant?.result, 'recovered final answer');
  assert.equal(assistant?.images?.[0].name, 'result.png');
});

test('a stop or steer never reprints the streamed turn under itself', () => {
  const { subject, owner } = subjectWithChat();
  (subject as any).onJobEvent({ type: 'start', job: job() });
  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'narración A.', phase: 'commentary' });
  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'narración B.', phase: 'commentary' });

  // Job.Result is the daemon's combined whole-turn text; a stop before any
  // final_answer chunk used to paste all of it back as a second surface.
  (subject as any).onJobEvent({
    type: 'end',
    job: job({ status: 'failed', code: 130, stopReason: 'cancelled', finishedAt: '2026-07-24T12:00:02Z', result: 'narración A.narración B.' }),
  });
  const assistant = owner.messages.find((message) => message.id === 'assistant-canonical');
  assert.equal(assistant?.content, 'narración A.narración B.');
  assert.equal(assistant?.result, undefined);
  assert.equal(assistant?.status, 'cancelled');
});

test('a typed turn keeps its own commentary/final-answer split at job:end', () => {
  const { subject, owner } = subjectWithChat();
  (subject as any).onJobEvent({ type: 'start', job: job() });
  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'narración A.', phase: 'commentary' });
  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'respuesta final', phase: 'final_answer' });

  (subject as any).onJobEvent({
    type: 'end',
    job: job({ status: 'done', code: 0, finishedAt: '2026-07-24T12:00:02Z', result: 'narración A.respuesta final' }),
  });
  const assistant = owner.messages.find((message) => message.id === 'assistant-canonical');
  assert.equal(assistant?.content, 'narración A.');
  assert.equal(assistant?.result, 'respuesta final');
});

test('a steer continuation never inherits the pre-steer text as a result', () => {
  const { subject, owner } = subjectWithChat();
  (subject as any).onJobEvent({ type: 'start', job: job() });
  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'narración previa.', phase: 'commentary' });
  const head = owner.messages.find((message) => message.id === 'assistant-canonical')!;
  head.status = 'done';
  head.turnRootId = head.id;
  head.turnTerminal = false;
  owner.messages.push(
    { id: 'steer-1', role: 'user', content: 'pará', status: 'done', at: null, events: [], turnRootId: head.id } as Msg,
    { id: 'assistant-cont', role: 'assistant', content: '', status: 'running', at: null, events: [], jobId: 'job-1', turnRootId: head.id, turnTerminal: true } as Msg,
  );
  (subject as any).rebuildJobRefs();

  (subject as any).onJobEvent({
    type: 'end',
    job: job({ status: 'failed', code: 130, stopReason: 'cancelled', finishedAt: '2026-07-24T12:00:02Z', result: 'narración previa.' }),
  });
  const continuation = owner.messages.find((message) => message.id === 'assistant-cont');
  assert.equal(head.content, 'narración previa.');
  assert.equal(continuation?.content, '');
  assert.equal(continuation?.result, undefined);
});

test('an unanchored old-daemon job without canonical ids schedules reconciliation', () => {
  const { subject, owner } = subjectWithChat();
  const scheduled: Set<string>[] = [];
  (subject as any).scheduleScopedSync = (scopes: Iterable<string>) => scheduled.push(new Set(scopes));

  (subject as any).onJobEvent({
    type: 'start',
    job: job({ userMessageId: undefined, assistantMessageId: undefined, promptText: undefined }),
  });

  assert.equal(owner.messages.length, 0);
  assert.equal(scheduled.length, 1);
  assert.deepEqual([...scheduled[0]].sort(), ['permissions', 'session']);
});

test('late data after local cancellation never resurrects the terminal row', () => {
  const { subject, owner } = subjectWithChat();
  const assistant: Msg = {
    id: 'assistant-canonical',
    role: 'assistant',
    content: '',
    status: 'cancelled',
    at: '2026-07-24T12:00:02Z',
    jobId: 'job-1',
    events: [],
  };
  owner.messages = [assistant];
  (subject as any).jobRef.set('job-1', { tabId: owner.id, msgId: assistant.id });

  (subject as any).onJobEvent({ type: 'data', id: 'job-1', stream: 'stdout', chunk: 'late bytes' });
  assert.equal(assistant.content, 'late bytes');
  assert.equal(assistant.status, 'cancelled');
});

test('cancel false finalizes locally, clears anchors, and keeps the FIFO paused', async () => {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api: { cancelJob: async () => ({ cancelled: false, reason: 'idle' }) } };
  try {
    const { subject, owner } = subjectWithChat();
    const assistant: Msg = {
      id: 'assistant-canonical',
      role: 'assistant',
      content: '',
      status: 'running',
      at: null,
      jobId: 'job-1',
      events: [],
    };
    owner.messages = [assistant];
    owner.queue = [{ id: 'queued-next', text: 'continue' }];
    (subject as any).jobRef.set('job-1', { tabId: owner.id, msgId: assistant.id });
    (subject as any).chatJobs.set(owner.chatId, { tabId: owner.id, msgId: assistant.id });
    let recovered = 0;
    (subject as any).recoverIdleQueues = () => { recovered += 1; };
    (subject as any).scheduleScopedSync = () => {};

    await subject.cancelChatTurn(owner.id);
    assert.equal(assistant.status, 'cancelled');
    assert.equal((subject as any).jobRef.has('job-1'), false);
    assert.equal((subject as any).chatJobs.has(owner.chatId), false);
    assert.equal(recovered, 0);
    assert.equal(owner.queuePaused, true);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('an in-flight cancel receipt never manufactures a local terminal turn', async () => {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: { cancelJob: async () => ({ cancelled: false, reason: 'pending', queuePaused: true, queuePauseRevision: 2 }) },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    const assistant: Msg = {
      id: 'assistant-canonical', role: 'assistant', content: 'partial', status: 'running',
      at: null, jobId: 'job-1', events: [],
    };
    owner.messages = [assistant];
    let syncs = 0;
    (subject as any).scheduleScopedSync = () => { syncs += 1; };

    await subject.cancelChatTurn(owner.id);
    assert.equal(assistant.status, 'running');
    assert.equal(owner.queuePaused, true);
    assert.equal(owner.queuePauseRevision, 2);
    assert.equal(syncs, 1);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('Stop before durable chat creation completes cancels the renderer-owned send without creating provider work', async () => {
  const previousWindow = (globalThis as any).window;
  let starts = 0;
  (globalThis as any).window = {
    api: {
      startJob: async () => {
        starts += 1;
        return job({ id: 'must-not-start' });
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    let releaseCreation!: () => void;
    const creationReady = new Promise<void>((resolve) => { releaseCreation = resolve; });
    (subject as any).ensureChatCreated = async () => { await creationReady; return true; };

    const sending = subject.sendTo(owner.id, 'cancel before admission');
    await new Promise((resolve) => setTimeout(resolve, 0));
    const assistant = owner.messages.find((message) => message.role === 'assistant')!;
    assert.equal(assistant.status, 'running');

    await subject.cancelChatTurn(owner.id);
    releaseCreation();
    assert.equal(await sending, false);
    assert.equal(starts, 0);
    assert.equal(assistant.status, 'cancelled');
    assert.equal(assistant.jobId, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('Stop during job:start admission cancels the exact accepted job once', async () => {
  const previousWindow = (globalThis as any).window;
  let releaseStart!: (value: PublicJob) => void;
  const cancelIDs: string[] = [];
  (globalThis as any).window = {
    api: {
      startJob: async () => await new Promise<PublicJob>((resolve) => { releaseStart = resolve; }),
      cancelJob: async (id: string) => {
        cancelIDs.push(id);
        return { cancelled: true, reason: 'cancelled', queuePaused: true, queuePauseRevision: 3 };
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    const sending = subject.sendTo(owner.id, 'cancel across admission');
    while (!releaseStart) await new Promise((resolve) => setTimeout(resolve, 0));
    const assistant = owner.messages.find((message) => message.role === 'assistant')!;
    assert.equal(assistant.jobId, undefined);

    await subject.cancelChatTurn(owner.id);
    releaseStart(job({ id: 'job-admitted-after-stop' }));
    assert.equal(await sending, true);
    assert.deepEqual(cancelIDs, ['job-admitted-after-stop']);
    assert.equal(assistant.jobId, 'job-admitted-after-stop');
    assert.equal(assistant.status, 'running', 'the provider terminal event remains authoritative');
    assert.equal(owner.queuePaused, true);
    assert.equal(owner.queuePauseRevision, 3);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('an early job:start event and its later reply cannot deliver Stop twice', async () => {
  const previousWindow = (globalThis as any).window;
  let releaseStart!: (value: PublicJob) => void;
  const cancelIDs: string[] = [];
  (globalThis as any).window = {
    api: {
      startJob: async () => await new Promise<PublicJob>((resolve) => { releaseStart = resolve; }),
      cancelJob: async (id: string) => {
        cancelIDs.push(id);
        return { cancelled: true, reason: 'cancelled', queuePaused: true, queuePauseRevision: 5 };
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    const sending = subject.sendTo(owner.id, 'one physical Stop');
    while (!releaseStart) await new Promise((resolve) => setTimeout(resolve, 0));
    const admitted = job({ id: 'job-event-before-reply' });
    (subject as any).onJobEvent({ type: 'start', job: admitted });

    await subject.cancelChatTurn(owner.id);
    releaseStart(admitted);
    assert.equal(await sending, true);
    assert.deepEqual(cancelIDs, ['job-event-before-reply']);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a Stop requested before job:start transfers at the event and rejects a stale busy receipt', async () => {
  const previousWindow = (globalThis as any).window;
  let releaseStart!: (value: StartJobReply) => void;
  const cancelIDs: string[] = [];
  (globalThis as any).window = {
    api: {
      startJob: async () => await new Promise<StartJobReply>((resolve) => { releaseStart = resolve; }),
      cancelJob: async (id: string) => {
        cancelIDs.push(id);
        return { cancelled: true, reason: 'cancelled', queuePaused: true, queuePauseRevision: 6 };
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    const sending = subject.sendTo(owner.id, 'stop before the admission event');
    while (!releaseStart) await new Promise((resolve) => setTimeout(resolve, 0));

    await subject.cancelChatTurn(owner.id);
    const admitted = job({ id: 'job-admitted-by-event' });
    (subject as any).onJobEvent({ type: 'start', job: admitted });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.deepEqual(cancelIDs, ['job-admitted-by-event']);

    releaseStart({
      queued: true, queueId: 'stale-queue-receipt', position: 1, delivery: 'queue',
      queuedAt: '2026-07-24T12:00:01Z', agentQueueRevision: 1,
    });
    assert.equal(await sending, true);
    assert.deepEqual(cancelIDs, ['job-admitted-by-event']);
    assert.equal(owner.queue, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('Stop during durable busy admission removes the exact FIFO owner before send settles', async () => {
  const previousWindow = (globalThis as any).window;
  let releaseStart!: (value: StartJobReply) => void;
  (globalThis as any).window = {
    api: {
      startJob: async () => await new Promise<StartJobReply>((resolve) => { releaseStart = resolve; }),
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    let releaseFlush!: () => void;
    const flushGate = new Promise<void>((resolve) => { releaseFlush = resolve; });
    let flushes = 0;
    (subject as any).flushSession = async () => { flushes += 1; await flushGate; };

    const sending = subject.sendTo(owner.id, 'cancel exact busy input');
    while (!releaseStart) await new Promise((resolve) => setTimeout(resolve, 0));
    await subject.cancelChatTurn(owner.id);
    releaseStart({
      queued: true, queueId: 'busy-owner', position: 1, delivery: 'queue',
      queuedAt: '2026-07-24T12:00:01Z', agentQueueRevision: 1,
    });

    let settled = false;
    void sending.then(() => { settled = true; });
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(settled, false, 'the send waits for durable FIFO removal');
    assert.equal(flushes, 1);
    assert.equal(owner.queue, undefined);
    assert.deepEqual(owner.messages, []);

    releaseFlush();
    assert.equal(await sending, true);
    assert.equal((subject as any).pendingTurnStarts.size, 0);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a rejected job start terminates its optimistic row and the next send can start', async () => {
  const previousWindow = (globalThis as any).window;
  let starts = 0;
  (globalThis as any).window = {
    api: {
      appChatNewSession: async () => ({
        sessionId: 'session-2', providerId: 'codex', cwd: '/tmp/workass-test',
        currentModelId: 'model-1', currentModeId: 'agent',
      }),
      startJob: async () => {
        starts += 1;
        if (starts === 1) return { error: 'provider lane rejected the turn' };
        return job({ id: 'job-2', sessionId: 'session-2' });
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    (subject as any).scheduleScopedSync = () => {};

    assert.equal(await (subject as any)._send(owner, 'first attempt'), false);
    const failed = owner.messages.at(-1)!;
    assert.equal(failed.status, 'failed');
    assert.match(failed.content, /provider lane rejected the turn/);
    assert.equal(failed.retryPrompt, 'first attempt');
    assert.equal(owner.sessionId, null);
    assert.equal((subject as any).chatJobs.size, 0);
    assert.equal(subject.isChatRunning(owner.id), false);

    assert.equal(await (subject as any)._send(owner, 'second attempt'), true);
    const running = owner.messages.at(-1)!;
    assert.equal(running.status, 'running');
    assert.equal(running.jobId, 'job-2');
    assert.equal(owner.sessionId, 'session-2');
    assert.equal(starts, 2);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('an accepted stop reaches a terminal row and does not block the next send', async () => {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      cancelJob: async () => ({ cancelled: true, reason: 'cancelled' }),
      startJob: async () => job({ id: 'job-after-stop' }),
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    const assistant: Msg = {
      id: 'assistant-canonical', role: 'assistant', content: 'partial', status: 'running',
      at: null, jobId: 'job-1', events: [],
    };
    owner.messages = [assistant];
    (subject as any).jobRef.set('job-1', { tabId: owner.id, msgId: assistant.id });
    (subject as any).chatJobs.set(owner.chatId, { tabId: owner.id, msgId: assistant.id });

    await subject.cancelChatTurn(owner.id);
    assert.equal(assistant.status, 'running', 'accepted cancellation waits for the durable terminal event');
    (subject as any).onJobEvent({
      type: 'end',
      job: job({ status: 'failed', code: 130, stopReason: 'cancelled', finishedAt: '2026-07-24T12:00:02Z', result: 'partial' }),
    });
    assert.equal(assistant.status, 'cancelled');
    assert.equal(subject.isChatRunning(owner.id), false);
    assert.equal((subject as any).chatJobs.size, 0);
    assert.equal((subject as any).jobRef.has('job-1'), false);

    assert.equal(await (subject as any)._send(owner, 'message after stop'), true);
    const next = owner.messages.at(-1)!;
    assert.equal(next.status, 'running');
    assert.equal(next.jobId, 'job-after-stop');
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('explicit stop leaves queued follow-ups paused after the terminal event', async () => {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      cancelJob: async () => ({ cancelled: true, reason: 'cancelled', queuePaused: true, queuePauseRevision: 4 }),
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    owner.queue = [{ id: 'queued-next', text: 'do not auto-start' }];
    owner.queuePauseRevision = 3;
    const assistant: Msg = {
      id: 'assistant-canonical', role: 'assistant', content: 'partial', status: 'running',
      at: null, jobId: 'job-1', events: [],
    };
    owner.messages = [assistant];
    (subject as any).jobRef.set('job-1', { tabId: owner.id, msgId: assistant.id });
    (subject as any).chatJobs.set(owner.chatId, { tabId: owner.id, msgId: assistant.id });
    let drains = 0;
    (subject as any).flushNextQueued = async () => { drains += 1; };

    await subject.cancelChatTurn(owner.id);
    assert.equal(owner.queuePaused, true);
    assert.equal(owner.queuePauseRevision, 4);
    (subject as any).onJobEvent({
      type: 'end',
      job: job({ status: 'failed', code: 130, stopReason: 'cancelled', finishedAt: '2026-07-24T12:00:02Z' }),
    });
    assert.equal(subject.isChatRunning(owner.id), false);
    assert.equal(owner.queue?.[0].id, 'queued-next');
    assert.equal(drains, 0);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('resuming a paused queue is revision-fenced before it drains', async () => {
  const previousWindow = (globalThis as any).window;
  const requests: Record<string, unknown>[] = [];
  (globalThis as any).window = {
    api: {
      chatQueueResume: async (request: Record<string, unknown>) => {
        requests.push(request);
        return {
          ok: true, operationId: request.operationId, queuePaused: false,
          queuePauseRevision: 7, actorRevision: 12,
        };
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    owner.queue = [{ id: 'queued-next', text: 'continue explicitly' }];
    owner.queuePaused = true;
    owner.queuePauseRevision = 7;
    let drains = 0;
    (subject as any).flushNextQueued = async () => { drains += 1; };

    assert.equal(await subject.resumeQueued(owner.id), true);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].expectedRevision, 7);
    assert.equal(requests[0].tabId, owner.id);
    assert.equal(requests[0].chatId, owner.chatId);
    assert.equal(owner.queuePaused, false);
    assert.equal(owner.actorRevision, 12);
    assert.equal(drains, 1);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('resuming after the last queued row is removed persists the empty queue first', async () => {
  const previousWindow = (globalThis as any).window;
  const order: string[] = [];
  (globalThis as any).window = {
    api: {
      chatQueueResume: async (request: Record<string, unknown>) => {
        order.push('resume');
        return {
          ok: true, operationId: request.operationId, queuePaused: false,
          queuePauseRevision: 9, actorRevision: 15,
        };
      },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    owner.queue = [];
    owner.queuePaused = true;
    owner.queuePauseRevision = 9;
    (subject as any).flushSession = async () => { order.push('persist'); };

    assert.equal(await subject.resumeQueued(owner.id), true);
    assert.deepEqual(order, ['persist', 'resume']);
    assert.equal(owner.queuePaused, false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('a failed queued flush preserves the exact FIFO head object', async () => {
  const { subject, owner } = subjectWithChat();
  owner.sessionId = null;
  const head: QueuedMsg = { id: 'queue-head', text: 'do not lose me' };
  owner.queue = [head, { id: 'queue-second', text: 'second' }];
  let queueId: string | undefined;
  (subject as any)._send = async (_chat: Chat, _prompt: string, _images: unknown, id: string) => {
    queueId = id;
    return false;
  };

  await (subject as any).flushNextQueued(owner);
  assert.equal(queueId, head.id);
  assert.equal(owner.queue?.length, 2);
  assert.strictEqual(owner.queue?.[0], head);
});

test('a digest cannot erase a queue mutation while its save is in flight', () => {
  const { subject, owner } = subjectWithChat();
  owner.messages.push({
    id: 'assistant-running', role: 'assistant', content: 'working', status: 'running',
    at: null, events: [], jobId: 'job-running',
  } as Msg);
  subject.state.connection = 'connected';
  subject.schedulePersist = () => {};

  assert.equal(subject.queueDraftMessage(owner.id, 'must survive refresh', []), true);
  assert.equal(owner.queue?.[0].text, 'must survive refresh');

  // The server still has the pre-queue snapshot when digest sync lands.
  // The pending queue fence must keep the renderer's exact FIFO projection.
  const staleServer = subject.toMirror(false);
  staleServer.chats[0].queue = undefined;
  assert.equal(subject.restoreSessionSnapshot(staleServer), true);
  assert.equal(subject.chat(owner.id)?.queue?.[0].text, 'must survive refresh');
});

test('a submitted draft is released before async send work and cannot reappear on refresh', async () => {
  const { subject, owner } = subjectWithChat();
  owner.draft = 'already sent';
  subject.schedulePersist = () => {};
  let release!: (accepted: boolean) => void;
  subject._send = () => new Promise<boolean>((resolve) => { release = resolve; });

  const delivery = subject.sendTo(owner.id, 'already sent', undefined, 'already sent');
  assert.equal(owner.draft, '', 'the next tab mount must see the draft already released');

  // Model the periodic digest arriving before the draft-clear save. It still
  // carries the pre-send value and must not re-own text now represented by the
  // submitted turn.
  const staleServer = subject.toMirror(false);
  staleServer.chats[0].draft = 'already sent';
  assert.equal(subject.restoreSessionSnapshot(staleServer), true);
  assert.equal(subject.chat(owner.id)?.draft, '');

  release(true);
  assert.equal(await delivery, true);
});

test('submission does not clear text typed after attachment preparation began', async () => {
  const { subject, owner } = subjectWithChat();
  owner.draft = 'newer text';
  subject.schedulePersist = () => {};
  subject._send = async () => true;

  assert.equal(await subject.sendTo(owner.id, 'older text', undefined, 'older text'), true);
  assert.equal(owner.draft, 'newer text');
});

test('digest heartbeat falls back to app:meta after an old daemon rejects state:digest', async () => {
  const previousWindow = (globalThis as any).window;
  let digestCalls = 0;
  let metaCalls = 0;
  (globalThis as any).window = {
    api: {
      stateDigest: async () => {
        digestCalls += 1;
        throw new Error('unknown channel state:digest');
      },
      appMeta: async () => {
        metaCalls += 1;
        return { version: 'old-daemon' };
      },
    },
  };
  try {
    const subject = new StoreCtor();
    await (subject as any).pingConnection();
    await (subject as any).pingConnection();
    assert.equal(digestCalls, 1);
    assert.equal(metaCalls, 2);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('digest divergence schedules only the relevant scoped sync', () => {
  const { subject, owner } = subjectWithChat();
  const scheduled: Set<string>[] = [];
  (subject as any).scheduleScopedSync = (scopes: Set<string>) => scheduled.push(new Set(scopes));
  const digest: StateDigest = {
    chats: [{
      tabId: owner.id,
      chatId: owner.chatId!,
      runningJobId: 'daemon-job',
      lastMessageId: null,
      messageCount: 0,
      queueLen: 0,
      queueHeadId: null,
      agentQueueRevision: 0,
      runtimeControlRevision: 0,
      providerId: 'codex',
      currentModelId: 'model-1',
      currentModeId: 'agent',
      pendingPermissionIds: [],
    }],
    catalogHash: {},
    settingsRevision: '',
    procHash: '',
  };

  (subject as any).handleStateDigest(digest);
  assert.equal(scheduled.length, 1);
  assert.equal(scheduled[0].has('session'), true);
  assert.equal(scheduled[0].has('permissions'), false);
});

test('mirror restore keeps an explicit local pick unless daemon control revision is strictly higher', () => {
  const { subject, owner } = subjectWithChat();
  owner.providerId = 'codex';
  owner.currentModelId = 'local-newer';
  owner.currentModeId = 'agent-full-access';
  owner.runtimeControlRevision = 4;
  owner._controlRevision = 3;
  const restored = chat();
  restored.providerId = 'claude';
  restored.currentModelId = 'daemon-stale';
  restored.currentModeId = 'ask';
  restored.runtimeControlRevision = 4;

  (subject as any).preserveNewerLocalControls([owner], [restored]);
  assert.equal(restored.providerId, 'codex');
  assert.equal(restored.currentModelId, 'local-newer');
  assert.equal(restored.currentModeId, 'agent-full-access');

  const newerDaemon = chat();
  newerDaemon.providerId = 'claude';
  newerDaemon.currentModelId = 'daemon-newer';
  newerDaemon.runtimeControlRevision = 5;
  (subject as any).preserveNewerLocalControls([owner], [newerDaemon]);
  assert.equal(newerDaemon.currentModelId, 'daemon-newer');
});

test('plan-usage failure always releases the provider loading latch', async () => {
  const previousWindow = (globalThis as any).window;
  let metadataCalls = 0;
  let sessionCalls = 0;
  (globalThis as any).window = {
    api: {
      appChatRefreshPlanUsage: async () => { metadataCalls += 1; throw new Error('fixture usage failure'); },
      appChatNewSession: async () => { sessionCalls += 1; throw new Error('visible session must not be acquired'); },
    },
  };
  try {
    const { subject, owner } = subjectWithChat();
    subject.state.connection = 'connected';
    owner.sessionProviderId = owner.providerId;
    owner.planUsageSupported = true;
    subject.refreshPlanUsage(owner.id);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(metadataCalls, 1);
    assert.equal(sessionCalls, 0);
    assert.equal(subject.state.planUsageLoadingByProvider.codex, false);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
