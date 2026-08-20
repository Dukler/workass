import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import { stageChronologicalSteer } from '../src/steering.ts';
import { LEAN_SESSION_SAVE_MODE, type Mirror, type MirrorMsg } from '../src/store/persistence.ts';
import type { Chat, Msg } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';
import { localId, machineOf, tagId, tagPayload } from '../src/wire/machineIds.ts';

const MACHINE = 'm-lagpc';
const TAB = tagId(MACHINE, 'tab-remote');
const CHAT = tagId(MACHINE, 'chat-remote');

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

function mirror(messages: MirrorMsg[] = [], overrides: Record<string, unknown> = {}): Mirror {
  return {
    v: 1,
    activeId: 'tab-remote',
    seq: 1,
    globalRevision: 0,
    workspaces: [],
    collapsedWorkspaces: [],
    removedWorkspaces: [],
    theme: 'dark',
    themePref: 'dark',
    density: 'compact',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    mode: 'chats',
    chats: [{
      id: 'tab-remote',
      chatId: 'chat-remote',
      actorRevision: 1,
      title: 'Remote',
      titleLocked: true,
      group: null,
      cwd: 'C:\\workass',
      workspaceRevision: 0,
      presentationRevision: 1,
      agentQueueRevision: 0,
      runtimeControlRevision: 0,
      currentModelId: 'gpt-test',
      currentModeId: 'agent',
      providerId: 'codex',
      draft: '',
      liveSession: {
        sessionId: 'session-remote',
        cwd: 'C:\\workass',
        providerId: 'codex',
        providerName: 'Codex',
        models: [],
        currentModelId: 'gpt-test',
        modes: [],
        currentModeId: 'agent',
      },
      messageCount: messages.length,
      historyComplete: true,
      messages,
      ...overrides,
    }],
  } as Mirror;
}

function remoteSubject(nextMirror: () => Mirror): any {
  const subject = new StoreCtor();
  subject.schedulePersist = () => {};
  subject.state.meta = { daemon: true, sessionSaveMode: LEAN_SESSION_SAVE_MODE, workspaceRebindMode: 'transactional-v1' };
  subject.state.connection = 'connected';
  subject.machines = {
    linkFor: (machineId: string) => machineId === MACHINE ? {
      invoke: async (channel: string) => {
        assert.equal(channel, 'session:get');
        return nextMirror();
      },
    } : null,
    setReason: () => {},
  };
  return subject;
}

async function withWindowApi<T>(api: Record<string, unknown>, task: () => Promise<T>): Promise<T> {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api };
  try {
    return await task();
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
}

function terminalJob(opts: Record<string, unknown>): PublicJob {
  return {
    id: localId(String(opts.id)),
    kind: 'app-chat',
    key: null,
    title: 'Remote',
    status: 'done',
    startedAt: '2026-08-19T12:00:00Z',
    finishedAt: '2026-08-19T12:00:01Z',
    code: 0,
    permissionMode: 'agent',
    chatId: 'chat-remote',
    tabId: 'tab-remote',
    sessionId: 'session-remote',
    providerId: 'codex',
    userMessageId: localId(String(opts.userMessageId)),
    assistantMessageId: localId(String(opts.assistantMessageId)),
    promptText: 'hello remote',
    result: 'done',
    error: null,
    stopReason: 'end_turn',
    crashInterrupted: false,
  } as PublicJob;
}

test('local session hydration preserves the exact pre-admission owner and Stop intent', () => {
  const subject = new StoreCtor();
  subject.schedulePersist = () => {};
  const owner = {
    id: 'tab-local', chatId: 'chat-local', sessionId: null,
    title: 'Local', titleLocked: true, group: null, cwd: '/tmp/workass',
    currentModelId: null, currentModeId: null, providerId: 'codex', pending: true,
    messages: [
      { id: 'user-local', role: 'user', content: 'hello', status: 'done', at: '2026-08-19T12:00:00Z', events: [] },
      { id: 'assistant-local', role: 'assistant', content: '', status: 'running', at: null, events: [] },
    ],
    draft: '',
  } as Chat;
  subject.state.chats = [owner];
  subject.state.activeId = owner.id;
  subject.pendingTurnStarts.set(owner.id, {
    chatId: owner.chatId,
    userId: 'user-local',
    assistantId: 'assistant-local',
    cancelRequested: false,
  });

  const stale = subject.toMirror(false) as Mirror;
  stale.chats[0].messages = [];
  stale.chats[0].messageCount = 0;
  assert.equal(subject.restoreSessionSnapshot(stale), true);

  const restored = subject.chat(owner.id) as Chat;
  assert.deepEqual(restored.messages.map((message) => message.id), ['user-local', 'assistant-local']);
  void subject.cancelChatTurn(owner.id);
  assert.equal(subject.pendingTurnStarts.get(owner.id).cancelRequested, true);
});

test('remote hydration during start keeps one owned pair, transfers Stop, and terminal replay cannot duplicate it', async () => {
  let currentMirror = mirror();
  const subject = remoteSubject(() => currentMirror);
  await subject.hydrateMachine(MACHINE);
  subject.dirtyChats.clear();
  subject.dirtyChatVersions.clear();
  subject.fullSavePending = false;

  let startOptions!: Record<string, unknown>;
  let releaseStart!: (value: unknown) => void;
  const startReply = new Promise((resolve) => { releaseStart = resolve; });
  const cancelledJobs: string[] = [];

  await withWindowApi({
    startJob: async (opts: Record<string, unknown>) => { startOptions = opts; return startReply; },
    cancelJob: async (jobId: string) => { cancelledJobs.push(jobId); return { cancelled: true }; },
  }, async () => {
    const send = subject._send(subject.chat(TAB), 'hello remote');
    await new Promise((resolve) => setTimeout(resolve, 0));

    assert.equal(machineOf(String(startOptions.userMessageId)), MACHINE);
    assert.equal(machineOf(String(startOptions.assistantMessageId)), MACHINE);
    assert.equal(startOptions.operationId, startOptions.userMessageId);
    const pending = subject.pendingTurnStarts.get(TAB);
    assert.equal(pending.chatId, CHAT);

    // This snapshot was read before job:start admission and therefore lacks the
    // renderer-owned pair. Hydration must carry that exact pair, not synthesize
    // another one and not disturb chats owned by another machine.
    currentMirror = mirror();
    await subject.hydrateMachine(MACHINE);
    assert.deepEqual(subject.chat(TAB).messages.map((message: Msg) => message.id), [
      startOptions.userMessageId,
      startOptions.assistantMessageId,
    ]);

    await subject.cancelChatTurn(TAB);
    assert.equal(subject.pendingTurnStarts.get(TAB).cancelRequested, true);

    const jobId = tagId(MACHINE, 'job-started');
    releaseStart({ id: jobId });
    assert.equal(await send, true);
    assert.deepEqual(cancelledJobs, [jobId]);

    const end = tagPayload(MACHINE, {
      type: 'end',
      job: terminalJob({
        id: jobId,
        userMessageId: startOptions.userMessageId,
        assistantMessageId: startOptions.assistantMessageId,
      }),
    });
    subject.onJobEvent(end);
    const live = subject.chat(TAB) as Chat;
    assert.deepEqual(live.messages.map((message) => message.id), [
      startOptions.userMessageId,
      startOptions.assistantMessageId,
    ]);
    assert.equal(live.messages[1].status, 'done');
  });
});

test('remote busy admission after hydration reuses the tagged actor queue row exactly once', async () => {
  let currentMirror = mirror();
  const subject = remoteSubject(() => currentMirror);
  await subject.hydrateMachine(MACHINE);
  subject.dirtyChats.clear();
  subject.dirtyChatVersions.clear();
  subject.fullSavePending = false;

  let releaseStart!: (value: unknown) => void;
  const startReply = new Promise((resolve) => { releaseStart = resolve; });
  await withWindowApi({ startJob: async () => startReply }, async () => {
    const send = subject._send(subject.chat(TAB), 'queue me once');
    await new Promise((resolve) => setTimeout(resolve, 0));

    // The actor has already accepted the FIFO row, but this renderer still owns
    // its optimistic pair until the busy receipt returns.
    currentMirror = mirror([], {
      queue: [{ id: 'host-q', text: 'queue me once', source: 'host', delivery: 'queue' }],
      agentQueueRevision: 1,
    });
    await subject.hydrateMachine(MACHINE);
    releaseStart(tagPayload(MACHINE, {
      queued: true,
      queueId: 'host-q',
      position: 1,
      delivery: 'queue',
      queuedAt: '2026-08-19T12:00:01Z',
      agentQueueRevision: 1,
    }));

    assert.equal(await send, true);
    const live = subject.chat(TAB) as Chat;
    assert.deepEqual(live.queue?.map((item) => item.id), [tagId(MACHINE, 'host-q')]);
    assert.equal(live.messages.length, 0, 'the optimistic pair transfers to the existing actor row');
  });
});

test('remote steer rows survive hydration and a tagged consumption receipt settles the same pair', async () => {
  const activeMessages: MirrorMsg[] = [
    { id: 'user-root', role: 'user', content: 'start', status: 'done', at: '2026-08-19T12:00:00Z', events: [] },
    { id: 'assistant-root', role: 'assistant', content: 'working', status: 'running', at: null, jobId: 'job-root', events: [] },
  ];
  let currentMirror = mirror(activeMessages);
  const subject = remoteSubject(() => currentMirror);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);

  const live = subject.chat(TAB) as Chat;
  const steer = {
    id: tagId(MACHINE, 'steer-user'), role: 'user', content: 'change direction', status: 'pending',
    at: '2026-08-19T12:00:01Z', events: [],
  } as Msg;
  const continuation = {
    id: tagId(MACHINE, 'steer-assistant'), role: 'assistant', content: '', status: 'running',
    at: null, jobId: tagId(MACHINE, 'job-root'), events: [],
  } as Msg;
  assert.ok(stageChronologicalSteer(live.messages, steer, continuation));
  const pending = { tabId: TAB, chatId: CHAT, userId: steer.id, continuationId: continuation.id };
  subject.pendingSteers.set(JSON.stringify([TAB, CHAT, steer.id]), pending);

  currentMirror = mirror(activeMessages);
  await subject.hydrateMachine(MACHINE);
  assert.equal(subject.chat(TAB).messages.filter((message: Msg) => message.id === steer.id).length, 1);
  assert.equal(subject.chat(TAB).messages.filter((message: Msg) => message.id === continuation.id).length, 1);

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'acp',
    id: 'job-root',
    event: { kind: 'steer-consumed', clientUserMessageId: 'steer-user' },
  }));
  const settled = subject.chat(TAB) as Chat;
  assert.equal(settled.messages.find((message) => message.id === steer.id)?.steerState, 'applied');
  assert.equal(settled.messages.find((message) => message.id === continuation.id)?.status, 'running');
  assert.deepEqual(subject.jobRef.get(tagId(MACHINE, 'job-root')), { tabId: TAB, msgId: continuation.id });
});

test('machine-scoped hydration rebuilds only that machine job index', async () => {
  const runningMirror = mirror([{
    id: 'assistant-a', role: 'assistant', content: 'A', status: 'running', at: null,
    jobId: 'job-same', events: [],
  }]);
  const subject = remoteSubject(() => runningMirror);
  const otherMachine = 'm-builder';
  const other = {
    id: tagId(otherMachine, 'tab-remote'),
    chatId: tagId(otherMachine, 'chat-remote'),
    machineId: otherMachine,
    sessionId: tagId(otherMachine, 'session-remote'),
    title: 'Builder', titleLocked: true, group: null, cwd: '/tmp/builder',
    currentModelId: null, currentModeId: null, providerId: 'codex', pending: false,
    messages: [{
      id: tagId(otherMachine, 'assistant-a'), role: 'assistant', content: 'B', status: 'running',
      at: null, jobId: tagId(otherMachine, 'job-same'), events: [],
    }],
    draft: '',
  } as Chat;
  subject.state.chats = [other];
  subject.rebuildJobRefs();

  await subject.hydrateMachine(MACHINE);

  assert.deepEqual(subject.jobRef.get(tagId(MACHINE, 'job-same')), {
    tabId: TAB, msgId: tagId(MACHINE, 'assistant-a'),
  });
  assert.deepEqual(subject.jobRef.get(tagId(otherMachine, 'job-same')), {
    tabId: other.id, msgId: other.messages[0].id,
  });
});

test('local and two remotes with identical raw ids receive only their own events and preserve an unrelated admission anchor', async () => {
  const machineA = MACHINE;
  const machineB = 'm-builder';
  const owned = (machineId: string): Chat => ({
    id: tagId(machineId, 'tab-same'),
    chatId: tagId(machineId, 'chat-same'),
    machineId: machineId || undefined,
    sessionId: tagId(machineId, 'session-same'),
    sessionProviderId: 'codex',
    title: machineId || 'Local', titleLocked: true, group: null, cwd: '/tmp/workass',
    currentModelId: null, currentModeId: null, providerId: 'codex', pending: false,
    messages: [
      {
        id: tagId(machineId, 'user-same'), role: 'user', content: 'prompt', status: 'done',
        at: '2026-08-19T12:00:00Z', events: [],
      },
      {
        id: tagId(machineId, 'assistant-same'), role: 'assistant', content: '', status: 'running',
        at: null, jobId: tagId(machineId, 'job-same'), events: [],
      },
    ],
    draft: '',
  } as Chat);
  const local = owned('');
  const remoteA = owned(machineA);
  const remoteB = owned(machineB);
  const pendingB = {
    ...owned(machineB),
    id: tagId(machineB, 'tab-pending'),
    chatId: tagId(machineB, 'chat-pending'),
    messages: [
      {
        id: tagId(machineB, 'user-pending'), role: 'user', content: 'pending', status: 'done',
        at: '2026-08-19T12:00:00Z', events: [],
      },
      {
        id: tagId(machineB, 'assistant-pending'), role: 'assistant', content: '', status: 'running',
        at: null, events: [],
      },
    ],
  } as Chat;
  const projectionA = mirror([
    {
      id: 'user-same', role: 'user', content: 'prompt', status: 'done',
      at: '2026-08-19T12:00:00Z', events: [],
    },
    {
      id: 'assistant-same', role: 'assistant', content: '', status: 'running',
      at: null, jobId: 'job-same', events: [],
    },
  ], { id: 'tab-same', chatId: 'chat-same' });
  const subject = remoteSubject(() => projectionA);
  subject.state.chats = [local, remoteA, remoteB, pendingB];
  subject.pendingTurnStarts.set(pendingB.id, {
    chatId: pendingB.chatId,
    userId: pendingB.messages[0].id,
    assistantId: pendingB.messages[1].id,
    cancelRequested: false,
  });
  subject.rebuildJobRefs();
  const pendingAnchor = { tabId: pendingB.id, msgId: pendingB.messages[1].id };
  assert.deepEqual(subject.chatJobs.get(pendingB.chatId), pendingAnchor);

  // Replacing A must rebuild A alone. B's renderer-owned pre-admission anchor
  // cannot be reconstructed from a job id because it deliberately has none.
  await subject.hydrateMachine(machineA);
  assert.deepEqual(subject.chatJobs.get(pendingB.chatId), pendingAnchor);

  const data = (machineId: string, chunk: string) => subject.onJobEvent(tagPayload(machineId, {
    type: 'data', id: 'job-same', stream: 'stdout', chunk,
  }));
  data(machineA, 'A');
  assert.deepEqual([
    subject.chat(local.id).messages[1].content,
    subject.chat(remoteA.id).messages[1].content,
    subject.chat(remoteB.id).messages[1].content,
  ], ['', 'A', '']);
  data('', 'L');
  data(machineB, 'B');
  assert.deepEqual([
    subject.chat(local.id).messages[1].content,
    subject.chat(remoteA.id).messages[1].content,
    subject.chat(remoteB.id).messages[1].content,
  ], ['L', 'A', 'B']);

  const endJob = (machineId: string): PublicJob => tagPayload(machineId, {
    id: 'job-same', kind: 'app-chat', key: null, title: 'Same', status: 'done',
    startedAt: '2026-08-19T12:00:00Z', finishedAt: '2026-08-19T12:00:01Z', code: 0,
    permissionMode: 'agent', chatId: 'chat-same', tabId: 'tab-same', sessionId: 'session-same',
    providerId: 'codex', userMessageId: 'user-same', assistantMessageId: 'assistant-same',
    promptText: 'prompt', result: 'done', error: null, stopReason: 'end_turn', crashInterrupted: false,
  }) as PublicJob;
  subject.flushSession = async () => {};
  subject.onJobEvent({ type: 'end', job: endJob(machineA) });
  assert.equal(subject.chat(remoteA.id).messages[1].status, 'done');
  assert.equal(subject.chat(local.id).messages[1].status, 'running');
  assert.equal(subject.chat(remoteB.id).messages[1].status, 'running');
  assert.deepEqual(subject.chatJobs.get(pendingB.chatId), pendingAnchor);
});

test('remote presentation, queue, controls, workspace, history, create, and delete stay on exact actor commands', async () => {
  let currentMirror = mirror();
  const subject = remoteSubject(() => currentMirror);
  subject.schedulePersist = () => {};
  const calls: Array<{ method: string; opts: any }> = [];
  const sessionSaves: Mirror[] = [];

  await withWindowApi({
    saveSession: async (snapshot: Mirror) => { sessionSaves.push(snapshot); return true; },
    chatPresentationSave: async (opts: any) => {
      calls.push({ method: 'presentation', opts });
      return { ok: true, operationId: opts.operationId, presentationRevision: opts.expectedRevision + 1, actorRevision: 2 };
    },
    chatQueueReplace: async (opts: any) => {
      calls.push({ method: 'queue', opts });
      return { ok: true, operationId: opts.operationId, agentQueueRevision: opts.expectedRevision + 1, actorRevision: 3 };
    },
    chatRuntimeControlsSave: async (opts: any) => {
      calls.push({ method: 'controls', opts });
      return {
        ok: true, operationId: opts.operationId, runtimeControlRevision: opts.expectedRevision + 1, actorRevision: 4,
        providerId: opts.providerId, currentModelId: opts.currentModelId, currentModeId: opts.currentModeId,
        modelControls: opts.modelControls,
      };
    },
    appChatNewSession: async (opts: any) => {
      calls.push({ method: 'workspace', opts });
      return {
        operationId: opts.operationId, sessionId: '', workspaceCommitted: true, workspaceRebound: true,
        workspaceRevision: opts.expectedWorkspaceRevision + 1,
      };
    },
    archiveLoad: async (tabId: string) => {
      calls.push({ method: 'history', opts: { tabId } });
      return tagPayload(MACHINE, [{
        id: 'history-user', role: 'user', content: 'history', status: 'done',
        at: '2026-08-19T12:00:00Z', events: [],
      }]);
    },
    chatCreate: async (opts: any) => {
      calls.push({ method: 'create', opts });
      return {
        ok: true, tabId: opts.tabId, chatId: opts.chatId, operationId: opts.operationId,
        actorRevision: 1, presentationRevision: 1, globalRevision: 1,
      };
    },
    chatDelete: async (opts: any) => {
      calls.push({ method: 'delete', opts });
      return { ok: true, operationId: opts.operationId };
    },
  }, async () => {
    await subject.hydrateMachine(MACHINE);
    subject.dirtyChats.clear();
    subject.dirtyChatVersions.clear();
    subject.fullSavePending = false;

    subject.setDraft(TAB, 'remote draft');
    await subject.flushSession();
    subject.enqueue(subject.chat(TAB), 'remote queue');
    await subject.flushSession();

    const remote = subject.chat(TAB) as Chat;
    remote.currentModelId = 'gpt-next';
    await subject.persistRuntimeControls(remote);
    assert.equal(await subject.moveChatToWorkspace(TAB, null, 'C:\\workass-next'), true);
    await subject.ensureFullHistory(TAB);

    const created = subject.newChat(false, 'C:\\workass-next', MACHINE) as Chat;
    await subject.pendingChatCreatePromises.get(created.id);
    await subject.closeChatDurably(created.id);
  });

  assert.ok(calls.some((entry) => entry.method === 'presentation' && entry.opts.tabId === TAB && entry.opts.chatId === CHAT));
  const queue = calls.find((entry) => entry.method === 'queue');
  assert.equal(machineOf(queue?.opts.queue[0].id ?? ''), MACHINE);
  assert.equal(queue?.opts.queue[0].draftImages, undefined);
  assert.ok(calls.some((entry) => entry.method === 'controls' && entry.opts.tabId === TAB && entry.opts.chatId === CHAT));
  assert.ok(calls.some((entry) => entry.method === 'workspace' && entry.opts.tabId === TAB && entry.opts.chatId === CHAT));
  assert.ok(calls.some((entry) => entry.method === 'history' && entry.opts.tabId === TAB));
  assert.ok(calls.some((entry) => entry.method === 'create' && machineOf(entry.opts.tabId) === MACHINE && machineOf(entry.opts.chatId) === MACHINE));
  assert.ok(calls.some((entry) => entry.method === 'delete' && machineOf(entry.opts.tabId) === MACHINE && machineOf(entry.opts.chatId) === MACHINE));
  assert.ok(sessionSaves.length > 0);
  assert.ok(sessionSaves.every((snapshot) => snapshot.chats.every((chat) => machineOf(chat.id) === '')),
    'remote rows never enter this machine\'s whole-session mirror');
});
