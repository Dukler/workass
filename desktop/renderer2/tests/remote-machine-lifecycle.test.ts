import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import { projectSteeringPresentation } from '../src/chat/steering-presentation.ts';
import { commitChronologicalSteer, stageChronologicalSteer } from '../src/steering.ts';
import { LEAN_SESSION_SAVE_MODE, type Mirror, type MirrorMsg } from '../src/store/persistence.ts';
import type { Chat, Msg } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';
import { localId, machineOf, tagId, tagPayload } from '../src/wire/machineIds.ts';

const MACHINE = 'm-lagpc';
const TAB = tagId(MACHINE, 'tab-remote');
const CHAT = tagId(MACHINE, 'chat-remote');

let vite: ViteDevServer;
let StoreCtor: new () => any;
let resolveSettled: (chat: unknown, status: string, active: boolean, now: number, touched: number) => boolean;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  resolveSettled = (await vite.ssrLoadModule('/src/components/SidebarV2.tsx')).resolveSettled;
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

function remoteSubject(nextMirror: () => Mirror | Promise<Mirror>, ownsLink: () => boolean = () => true): any {
  const subject = new StoreCtor();
  subject.schedulePersist = () => {};
  subject.state.meta = { daemon: true, sessionSaveMode: LEAN_SESSION_SAVE_MODE, workspaceRebindMode: 'transactional-v1' };
  subject.state.connection = 'connected';
  const link = {
    invoke: async (channel: string) => {
      assert.equal(channel, 'session:get');
      return nextMirror();
    },
  };
  subject.machines = {
    linkFor: (machineId: string) => machineId === MACHINE ? link : null,
    ownsLink: (machineId: string, candidate: unknown) => machineId === MACHINE && candidate === link && ownsLink(),
    setReason: () => {},
  };
  return subject;
}

test('remote settlement survives a session snapshot read before its actor receipt', async () => {
  let currentMirror = mirror([], { presentationRevision: 1 });
  let releaseStaleHydration!: (value: Mirror) => void;
  let staleHydration: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => staleHydration ?? currentMirror);
  await subject.hydrateMachine(MACHINE);
  subject.dirtyChats.clear();
  subject.dirtyChatVersions.clear();
  subject.fullSavePending = false;

  const staleRead = structuredClone(currentMirror);
  staleHydration = new Promise((resolve) => { releaseStaleHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));

  const flush = subject.flushSession.bind(subject);
  subject.flushSession = async () => {};
  await withWindowApi({
    chatPresentationSave: async (opts: Record<string, unknown>) => ({
      ok: true,
      operationId: opts.operationId,
      presentationRevision: 2,
      actorRevision: 2,
    }),
    saveSession: async () => ({ ok: true, globalRevision: 1 }),
  }, async () => {
    subject.settleChat(TAB, true);
    const settledAt = subject.chat(TAB).settledAt;
    await flush(true);
    assert.equal(subject.chat(TAB).settled, 'settled');
    assert.equal(subject.chat(TAB).presentationRevision, 2);

    releaseStaleHydration(staleRead);
    await hydration;
    assert.equal(subject.chat(TAB).settled, 'settled');
    assert.equal(subject.chat(TAB).settledAt, settledAt);
    assert.equal(subject.chat(TAB).presentationRevision, 2);

    staleHydration = null;
    currentMirror = mirror([], {
      actorRevision: 3,
      presentationRevision: 3,
      settled: 'active',
    });
    await subject.hydrateMachine(MACHINE);
    assert.equal(subject.chat(TAB).settled, 'active', 'a genuinely newer actor projection remains authoritative');
    assert.equal(subject.chat(TAB).presentationRevision, 3);
  });
});

test('a delayed remote hydration cannot erase newer streamed bytes, tools, or terminal state', async () => {
  const jobID = 'job-hydration-race';
  const userID = 'user-hydration-race';
  const assistantID = 'assistant-hydration-race';
  const stale = mirror([
    { id: userID, role: 'user', content: 'race', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: assistantID, role: 'assistant', content: '', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  await subject.hydrateMachine(MACHINE);

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'data', id: jobID, stream: 'stdout', chunk: 'NEW', phase: 'commentary',
  }));
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'acp', id: jobID,
    event: { kind: 'tool', toolKind: 'execute', id: 'tool-race', title: 'Probe', status: 'completed', output: 'ok' },
  }));
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'end',
    job: terminalJob({ id: jobID, userMessageId: userID, assistantMessageId: assistantID }),
  }));
  releaseHydration(stale);
  await hydration;

  const restored = subject.chat(TAB) as Chat;
  const assistant = restored.messages.find((message) => message.id === tagId(MACHINE, assistantID));
  assert.equal(assistant?.content, 'NEW');
  assert.equal(assistant?.status, 'done');
  assert.equal(assistant?.events.some((event) => event.kind === 'tool' && event.id === tagId(MACHINE, 'tool-race')), true);
  assert.equal(subject.jobRef.has(tagId(MACHINE, jobID)), false, 'terminal hydration must not recreate a running anchor');
});

test('a delayed remote hydration cannot restore a consumed steer boundary after terminal', async () => {
  const jobID = 'job-steer-consumed-terminal';
  const rootID = 'assistant-steer-consumed-terminal';
  const steerID = 'steer-consumed-terminal';
  const continuationID = 'continuation-steer-consumed-terminal';
  const stale = mirror([
    { id: 'user-steer-consumed-terminal', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: rootID, role: 'assistant', content: 'before', status: 'running', at: null, jobId: jobID, events: [] },
    {
      id: steerID, role: 'user', content: 'redirect', status: 'done', at: '2026-08-20T12:00:01Z', events: [],
      steerState: 'accepted', steerBoundary: 'waiting', steerContinuationId: continuationID, turnRootId: rootID,
    },
    {
      id: continuationID, role: 'assistant', content: '', status: 'pending', at: null, jobId: jobID, events: [],
      steerBoundary: 'waiting', steerContinuationFor: steerID, turnRootId: rootID, turnTerminal: true,
    },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'acp', id: jobID,
    event: { kind: 'steer-consumed', clientUserMessageId: steerID },
  }));
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'end',
    job: { ...terminalJob({ id: jobID, userMessageId: 'user-steer-consumed-terminal', assistantMessageId: rootID }), consumedSteerIds: [steerID] },
  }));
  releaseHydration(stale);
  await hydration;

  const restored = subject.chat(TAB) as Chat;
  const steer = restored.messages.find((message) => message.id === tagId(MACHINE, steerID));
  assert.equal(steer?.steerState, 'applied');
  assert.equal(steer?.steerBoundary, undefined);
  assert.equal(restored.messages.some((message) => message.status === 'running' || message.status === 'pending'), false);
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(subject.jobRef.has(tagId(MACHINE, jobID)), false);
});

test('a delayed remote hydration cannot resurrect an unconsumed steer continuation removed at terminal', async () => {
  const jobID = 'job-steer-unconsumed-terminal';
  const rootID = 'assistant-steer-unconsumed-terminal';
  const steerID = 'steer-unconsumed-terminal';
  const continuationID = 'continuation-steer-unconsumed-terminal';
  const stale = mirror([
    { id: 'user-steer-unconsumed-terminal', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: rootID, role: 'assistant', content: 'finished', status: 'running', at: null, jobId: jobID, events: [] },
    {
      id: steerID, role: 'user', content: 'late redirect', status: 'done', at: '2026-08-20T12:00:01Z', events: [],
      steerState: 'accepted', steerBoundary: 'waiting', steerContinuationId: continuationID, turnRootId: rootID,
    },
    {
      id: continuationID, role: 'assistant', content: '', status: 'pending', at: null, jobId: jobID, events: [],
      steerBoundary: 'waiting', steerContinuationFor: steerID, turnRootId: rootID, turnTerminal: true,
    },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'end',
    job: terminalJob({ id: jobID, userMessageId: 'user-steer-unconsumed-terminal', assistantMessageId: rootID }),
  }));
  releaseHydration(stale);
  await hydration;

  const restored = subject.chat(TAB) as Chat;
  const steer = restored.messages.find((message) => message.id === tagId(MACHINE, steerID));
  assert.equal(steer?.steerState, 'accepted');
  assert.equal(steer?.steerBoundary, undefined);
  assert.equal(restored.messages.some((message) => message.id === tagId(MACHINE, continuationID)), false);
  assert.equal(restored.messages.some((message) => message.status === 'running' || message.status === 'pending'), false);
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(subject.jobRef.has(tagId(MACHINE, jobID)), false);
});

test('a steer acknowledgement survives an older hydration that omitted its now-transcript owner', async () => {
  const jobID = 'job-steer-ack-hydration';
  const stale = mirror([
    { id: 'user-steer-ack-hydration', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-steer-ack-hydration', role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await withWindowApi({
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    assert.equal(await subject.steerRunning(TAB, 'accepted direction'), true);
  });
  assert.equal(subject.pendingSteers.size, 0, 'the control waiter released before the stale read returned');

  releaseHydration(stale);
  await hydration;
  const restored = subject.chat(TAB) as Chat;
  const accepted = restored.messages.find((message) => message.content === 'accepted direction');
  assert.equal(accepted?.steerState, 'accepted');
  assert.equal(projectSteeringPresentation(restored.messages).steeringTrayMessages.length, 0);
  assert.equal(projectSteeringPresentation(restored.messages).transcriptMessages.filter((message) => message.id === accepted?.id).length, 1);
});

test('a pre-job-id steer acknowledgement survives an older hydration that omitted its transcript owner', async () => {
  const stale = mirror([
    { id: 'user-steer-pre-job-hydration', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-steer-pre-job-hydration', role: 'assistant', content: 'admitting', status: 'running', at: null, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await withWindowApi({
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    assert.equal(await subject.steerRunning(TAB, 'accepted before job id'), true);
  });
  assert.equal(subject.pendingSteers.size, 0, 'the control waiter released before the stale read returned');

  releaseHydration(stale);
  await hydration;
  const restored = subject.chat(TAB) as Chat;
  const accepted = restored.messages.filter((message) => message.content === 'accepted before job id');
  assert.equal(accepted.length, 1);
  assert.equal(accepted[0].steerState, 'accepted');
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(projectSteeringPresentation(restored.messages).transcriptMessages.filter((message) => message.id === accepted[0].id).length, 1);
});

test('a newer terminal actor snapshot beats an equal accepted local boundary without a live end event', async () => {
  const jobID = 'job-terminal-actor-steer';
  const rootID = 'assistant-terminal-actor-steer';
  const initial = mirror([
    { id: 'user-terminal-actor-steer', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: rootID, role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));
  await withWindowApi({
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    assert.equal(await subject.steerRunning(TAB, 'accepted at terminal edge'), true);
  });
  assert.equal(subject.pendingSteers.size, 0);

  const live = subject.chat(TAB) as Chat;
  const accepted = live.messages.find((message) => message.content === 'accepted at terminal edge');
  assert.ok(accepted);
  const terminalMessages = structuredClone(live.messages);
  const terminalSteer = terminalMessages.find((message) => message.id === accepted.id);
  assert.ok(terminalSteer);
  delete terminalSteer.steerBoundary;
  delete terminalSteer.steerContinuationId;
  terminalSteer.status = 'done';
  const waitingIndex = terminalMessages.findIndex((message) => message.steerContinuationFor === accepted.id);
  assert.ok(waitingIndex >= 0);
  const [waiting] = terminalMessages.splice(waitingIndex, 1);
  const terminalRoot = terminalMessages.find((message) => message.id === tagId(MACHINE, rootID));
  assert.ok(terminalRoot);
  terminalRoot.status = 'done';
  terminalRoot.at = '2026-08-20T12:00:02Z';
  terminalRoot.jobId = undefined;

  releaseHydration(mirror(rawRemoteMessages(terminalMessages), { actorRevision: 2 }));
  await hydration;

  const restored = subject.chat(TAB) as Chat;
  const rows = restored.messages.filter((message) => message.id === accepted.id);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].steerState, 'accepted');
  assert.equal(rows[0].steerBoundary, undefined);
  assert.equal(restored.messages.some((message) => message.id === waiting.id), false);
  assert.equal(restored.messages.some((message) => message.status === 'running' || message.status === 'pending'), false);
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(subject.jobRef.has(tagId(MACHINE, jobID)), false);
});

test('a stale full-history reply cannot erase a steer that is still awaiting acknowledgement', async () => {
  const jobID = 'job-history-pending-steer';
  const initial = mirror([
    { id: 'user-history-pending-steer', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-history-pending-steer', role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ], { historyComplete: false, messageCount: 20 });
  const subject = remoteSubject(() => initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };
  let releaseHistory!: (value: unknown) => void;
  let releaseSteer!: (value: unknown) => void;
  const historyReply = new Promise((resolve) => { releaseHistory = resolve; });
  const steerReply = new Promise((resolve) => { releaseSteer = resolve; });

  await withWindowApi({
    archiveLoad: async () => historyReply,
    appChatSteer: async () => steerReply,
  }, async () => {
    const history = subject.ensureFullHistory(TAB);
    await new Promise((resolve) => setTimeout(resolve, 0));
    const steering = subject.steerRunning(TAB, 'pending during history');
    await new Promise((resolve) => setTimeout(resolve, 0));

    releaseHistory(tagPayload(MACHINE, initial.chats[0].messages));
    await history;
    const pending = (subject.chat(TAB) as Chat).messages.filter((message) => message.content === 'pending during history');
    assert.equal(pending.length, 1);
    assert.equal(pending[0].steerState, 'sending');

    releaseSteer({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' });
    assert.equal(await steering, true);
  });

  const restored = subject.chat(TAB) as Chat;
  const accepted = restored.messages.filter((message) => message.content === 'pending during history');
  assert.equal(accepted.length, 1);
  assert.equal(accepted[0].steerState, 'accepted');
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
});

test('a stale full-history reply cannot erase an acknowledged steer after its waiter releases', async () => {
  const jobID = 'job-history-accepted-steer';
  const initial = mirror([
    { id: 'user-history-accepted-steer', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-history-accepted-steer', role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ], { historyComplete: false, messageCount: 20 });
  const subject = remoteSubject(() => initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };
  let releaseHistory!: (value: unknown) => void;
  const historyReply = new Promise((resolve) => { releaseHistory = resolve; });

  await withWindowApi({
    archiveLoad: async () => historyReply,
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    const history = subject.ensureFullHistory(TAB);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(await subject.steerRunning(TAB, 'accepted during history'), true);
    assert.equal(subject.pendingSteers.size, 0);
    releaseHistory(tagPayload(MACHINE, initial.chats[0].messages));
    await history;
  });

  const restored = subject.chat(TAB) as Chat;
  const accepted = restored.messages.filter((message) => message.content === 'accepted during history');
  assert.equal(accepted.length, 1);
  assert.equal(accepted[0].steerState, 'accepted');
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(projectSteeringPresentation(restored.messages).transcriptMessages.filter((message) => message.id === accepted[0].id).length, 1);
});

test('a complete terminal history clears an equal accepted boundary without resurrecting its placeholder', async () => {
  const jobID = 'job-history-terminal-steer';
  const rootID = 'assistant-history-terminal-steer';
  const initial = mirror([
    { id: 'user-history-terminal-steer', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: rootID, role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ], { historyComplete: false, messageCount: 20 });
  const subject = remoteSubject(() => initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };
  let releaseHistory!: (value: unknown) => void;
  const historyReply = new Promise((resolve) => { releaseHistory = resolve; });

  await withWindowApi({
    archiveLoad: async () => historyReply,
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    const history = subject.ensureFullHistory(TAB);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(await subject.steerRunning(TAB, 'accepted before history terminal'), true);
    const live = subject.chat(TAB) as Chat;
    const accepted = live.messages.find((message) => message.content === 'accepted before history terminal');
    assert.ok(accepted);
    const terminalMessages = structuredClone(live.messages);
    const terminalSteer = terminalMessages.find((message) => message.id === accepted.id);
    assert.ok(terminalSteer);
    delete terminalSteer.steerBoundary;
    delete terminalSteer.steerContinuationId;
    terminalSteer.status = 'done';
    const continuationIndex = terminalMessages.findIndex((message) => message.steerContinuationFor === accepted.id);
    assert.ok(continuationIndex >= 0);
    terminalMessages.splice(continuationIndex, 1);
    const terminalRoot = terminalMessages.find((message) => message.id === tagId(MACHINE, rootID));
    assert.ok(terminalRoot);
    terminalRoot.status = 'done';
    terminalRoot.at = '2026-08-20T12:00:02Z';
    terminalRoot.jobId = undefined;

    releaseHistory(tagPayload(MACHINE, rawRemoteMessages(terminalMessages)));
    await history;
  });

  const restored = subject.chat(TAB) as Chat;
  const accepted = restored.messages.filter((message) => message.content === 'accepted before history terminal');
  assert.equal(accepted.length, 1);
  assert.equal(accepted[0].steerState, 'accepted');
  assert.equal(accepted[0].steerBoundary, undefined);
  assert.equal(restored.messages.some((message) => message.steerBoundary === 'waiting'), false);
  assert.equal(restored.messages.some((message) => message.status === 'running' || message.status === 'pending'), false);
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(subject.jobRef.has(tagId(MACHINE, jobID)), false);
});

test('an older stronger receipt for steer one cannot erase a newer acknowledged steer two omitted by hydration', async () => {
  const jobID = 'job-rapid-steer-hydration';
  const initial = mirror([
    { id: 'user-rapid-steer-hydration', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-rapid-steer-hydration', role: 'assistant', content: 'working', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };

  await withWindowApi({
    appChatSteer: async () => ({ ok: true, live: true, strategy: 'receipt-live', receipt: true, turnId: 'native-turn' }),
  }, async () => {
    assert.equal(await subject.steerRunning(TAB, 'first accepted direction'), true);
    const liveAfterFirst = subject.chat(TAB) as Chat;
    const first = liveAfterFirst.messages.find((message) => message.content === 'first accepted direction');
    assert.ok(first);

    const actorAfterFirstReceipt = structuredClone(liveAfterFirst.messages);
    assert.ok(commitChronologicalSteer(actorAfterFirstReceipt, first.id));
    const snapshotAfterFirstReceipt = mirror(rawRemoteMessages(actorAfterFirstReceipt), { actorRevision: 2 });

    delayed = new Promise((resolve) => { releaseHydration = resolve; });
    const hydration = subject.hydrateMachine(MACHINE);
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(await subject.steerRunning(TAB, 'second accepted direction'), true);
    assert.equal(subject.pendingSteers.size, 0);

    releaseHydration(snapshotAfterFirstReceipt);
    await hydration;
  });

  const restored = subject.chat(TAB) as Chat;
  const first = restored.messages.filter((message) => message.content === 'first accepted direction');
  const second = restored.messages.filter((message) => message.content === 'second accepted direction');
  assert.equal(first.length, 1);
  assert.equal(first[0].steerState, 'applied');
  assert.equal(second.length, 1);
  assert.equal(second[0].steerState, 'accepted');
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.ok(restored.messages.indexOf(first[0]) < restored.messages.indexOf(second[0]));
});

test('a definite steer rejection tombstones an older hydration pair and restores one composer owner', async () => {
  const initial = mirror([
    { id: 'user-steer-reject-hydration', role: 'user', content: 'start', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-steer-reject-hydration', role: 'assistant', content: 'working', status: 'running', at: null, events: [] },
  ]);
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  let releaseSteer!: (value: unknown) => void;
  const steerReply = new Promise((resolve) => { releaseSteer = resolve; });
  const subject = remoteSubject(() => delayed ?? initial);
  subject.flushSession = async () => {};
  await subject.hydrateMachine(MACHINE);
  const owner = subject.chat(TAB) as Chat;
  owner.deliveryCapabilities = {
    stableInputIdentity: true, liveSteer: true, steerConsumptionReceipt: true,
    consumptionReceipt: true, turnReadback: true,
  };
  owner.draft = 'rejected direction';

  await withWindowApi({ appChatSteer: async () => steerReply }, async () => {
    const steering = subject.steerRunning(TAB, 'rejected direction');
    await new Promise((resolve) => setTimeout(resolve, 0));
    const staged = subject.chat(TAB) as Chat;
    const stalePending = mirror(rawRemoteMessages(staged.messages));
    delayed = new Promise((resolve) => { releaseHydration = resolve; });
    const hydration = subject.hydrateMachine(MACHINE);
    await new Promise((resolve) => setTimeout(resolve, 0));

    releaseSteer({ ok: false, live: false, strategy: 'rejected', error: 'not steerable' });
    assert.equal(await steering, false);
    subject.setDraft(TAB, 'rejected direction');
    releaseHydration(stalePending);
    await hydration;
  });

  const restored = subject.chat(TAB) as Chat;
  assert.equal(restored.messages.some((message) => message.content === 'rejected direction'), false);
  assert.deepEqual(projectSteeringPresentation(restored.messages).steeringTrayMessages, []);
  assert.equal(restored.draft, 'rejected direction');
});

test('a delayed remote hydration preserves the exact attached lane and its live steering capabilities', async () => {
  const jobID = 'job-capability-race';
  const userID = 'user-capability-race';
  const assistantID = 'assistant-capability-race';
  const stale = mirror();
  let releaseHydration!: (value: Mirror) => void;
  let delayed: Promise<Mirror> | null = null;
  const subject = remoteSubject(() => delayed ?? stale);
  await subject.hydrateMachine(MACHINE);

  delayed = new Promise((resolve) => { releaseHydration = resolve; });
  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'start',
    job: {
      ...terminalJob({ id: jobID, userMessageId: userID, assistantMessageId: assistantID }),
      status: 'running',
      finishedAt: null,
      code: null,
      sessionId: 'session-new',
      deliveryCapabilities: {
        stableInputIdentity: true,
        liveSteer: true,
        steerConsumptionReceipt: true,
        consumptionReceipt: true,
        turnReadback: true,
      },
    },
  }));
  releaseHydration(stale);
  await hydration;

  const restored = subject.chat(TAB) as Chat;
  assert.equal(restored.sessionId, tagId(MACHINE, 'session-new'));
  assert.equal(restored.sessionProviderId, 'codex');
  assert.deepEqual(restored.deliveryCapabilities, {
    stableInputIdentity: true,
    liveSteer: true,
    steerConsumptionReceipt: true,
    consumptionReceipt: true,
    turnReadback: true,
  });
  assert.equal(restored.messages.find((message) => message.id === tagId(MACHINE, assistantID))?.status, 'running');
});

test('events crossing first remote mount converge through bounded actor catch-up reads without replaying chunks', async () => {
  const jobID = 'job-first-hydrate';
  const userID = 'user-first-hydrate';
  const assistantID = 'assistant-first-hydrate';
  const first = mirror();
  const second = mirror([
    { id: userID, role: 'user', content: 'first', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: assistantID, role: 'assistant', content: 'A', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  const caughtUp = mirror([
    { id: userID, role: 'user', content: 'first', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: assistantID, role: 'assistant', content: 'AE', status: 'running', at: null, jobId: jobID, events: [] },
  ]);
  let releaseFirst!: (value: Mirror) => void;
  let releaseSecond!: (value: Mirror) => void;
  const firstRead = new Promise<Mirror>((resolve) => { releaseFirst = resolve; });
  const secondRead = new Promise<Mirror>((resolve) => { releaseSecond = resolve; });
  let reads = 0;
  const subject = remoteSubject(() => {
    reads += 1;
    if (reads === 1) return firstRead;
    if (reads === 2) return secondRead;
    return caughtUp;
  });

  const hydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'start',
    job: {
      ...terminalJob({ id: jobID, userMessageId: userID, assistantMessageId: assistantID }),
      status: 'running', finishedAt: null, code: null, result: null,
    },
  }));
  releaseFirst(first);
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(reads, 2, 'the missed start opens one coalesced catch-up read');

  // S2 already captured A, but its reply is still in flight when E arrives.
  // No job owner is mounted yet, so E raises one more marker instead of being
  // replayed blindly onto a snapshot whose prefix is unknown.
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'data', id: jobID, stream: 'stdout', chunk: 'E', phase: 'commentary',
  }));
  releaseSecond(second);
  await hydration;

  assert.equal(reads, 3, 'one marker raised during S2 owns exactly one converging S3 read');
  const assistant = (subject.chat(TAB) as Chat).messages.find((message) => message.id === tagId(MACHINE, assistantID));
  assert.equal(assistant?.content, 'AE', 'the actor snapshot recovers each byte once; raw chunks are never replayed');
  assert.equal(subject.remoteMachinesNeedingCatchup.size, 0);
});

test('an unknown remote-host chat event after initial mount triggers an immediate coalesced actor read', async () => {
  let current = mirror();
  let reads = 0;
  const subject = remoteSubject(() => { reads += 1; return current; });
  await subject.hydrateMachine(MACHINE);

  const remoteCreated = structuredClone(mirror()).chats[0];
  remoteCreated.id = 'tab-host-created';
  remoteCreated.chatId = 'chat-host-created';
  remoteCreated.title = 'Host created';
  remoteCreated.messages = [
    { id: 'user-host-created', role: 'user', content: 'host work', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: 'assistant-host-created', role: 'assistant', content: 'mounted once', status: 'running', at: null, jobId: 'job-host-created', events: [] },
  ];
  remoteCreated.messageCount = remoteCreated.messages.length;
  current = { ...mirror(), chats: [mirror().chats[0], remoteCreated] };

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'start',
    job: {
      ...terminalJob({ id: 'job-host-created', userMessageId: 'user-host-created', assistantMessageId: 'assistant-host-created' }),
      tabId: 'tab-host-created', chatId: 'chat-host-created', sessionId: 'session-host-created',
      status: 'running', finishedAt: null, code: null, result: null,
    },
  }));
  for (let attempt = 0; attempt < 10 && subject.remoteMachineCatchups.size; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }

  assert.equal(reads, 2, 'the unmatched event starts one catch-up without a socket reopen');
  const mounted = subject.chat(tagId(MACHINE, 'tab-host-created')) as Chat;
  assert.equal(mounted.messages.find((message) => message.id === tagId(MACHINE, 'assistant-host-created'))?.content, 'mounted once');
  assert.equal(subject.remoteMachinesNeedingCatchup.size, 0);
});

test('a refused remote Stop keeps the active owner nonterminal and never drains FIFO', async () => {
  const jobID = 'job-stop-refused';
  const assistantID = 'assistant-stop-refused';
  let delayedRead: Promise<Mirror> | null = null;
  const current = mirror([
    { id: 'user-stop-refused', role: 'user', content: 'work', status: 'done', at: '2026-08-20T12:00:00Z', events: [] },
    { id: assistantID, role: 'assistant', content: 'still working', status: 'running', at: null, jobId: jobID, events: [] },
  ], { queue: [{ id: 'queue-next', text: 'next' }] });
  const subject = remoteSubject(() => delayedRead ?? current);
  await subject.hydrateMachine(MACHINE);
  delayedRead = new Promise(() => {});
  let drains = 0;
  subject.flushNextQueued = () => { drains += 1; };

  await withWindowApi({
    cancelJob: async () => ({ cancelled: false, reason: 'not-owned' }),
  }, async () => {
    await subject.cancelChatTurn(TAB);
  });

  const live = subject.chat(TAB) as Chat;
  assert.equal(live.messages.find((message) => message.id === tagId(MACHINE, assistantID))?.status, 'running');
  assert.deepEqual(live.queue?.map((item) => item.id), [tagId(MACHINE, 'queue-next')]);
  assert.equal(drains, 0);
  assert.equal(subject.state.toasts.at(-1)?.title, 'No se pudo detener');
});

test('replayed remote terminal events do not revive an acknowledged settled chat', async () => {
  const userID = 'user-terminal';
  const assistantID = 'assistant-terminal';
  const jobID = 'job-terminal';
  const subject = remoteSubject(() => mirror([
    { id: userID, role: 'user', content: 'hello remote', status: 'done', at: '2026-08-19T12:00:00Z', events: [] },
    {
      id: assistantID,
      role: 'assistant',
      content: 'done',
      status: 'done',
      at: '2026-08-19T12:00:01Z',
      jobId: jobID,
      events: [],
    },
  ], {
    settled: 'settled',
    settledAt: Date.parse('2026-08-19T12:00:02Z'),
    unread: false,
  }));
  await subject.hydrateMachine(MACHINE);
  subject.state.activeId = null;

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'end',
    job: terminalJob({ id: jobID, userMessageId: userID, assistantMessageId: assistantID }),
  }));

  const settled = subject.chat(TAB) as Chat;
  assert.equal(settled.unread, false);
  assert.equal(settled.settled, 'settled');
  assert.equal(resolveSettled(settled, 'ready', false, Date.now(), Date.now()), true);
});

test('machine rejection evicts its chats and a late snapshot cannot mount them again', async () => {
  let authorized = true;
  let delayedMirror: Promise<Mirror> | null = null;
  let resolveDelayed!: (value: Mirror) => void;
  const subject = remoteSubject(() => delayedMirror ?? mirror(), () => authorized);
  await subject.hydrateMachine(MACHINE);

  const local = {
    id: 'tab-local-survivor',
    chatId: 'chat-local-survivor',
    sessionId: null,
    title: 'Local survivor',
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass',
    currentModelId: null,
    currentModeId: null,
    providerId: 'codex',
    pending: true,
    messages: [],
    draft: '',
  } as Chat;
  subject.state.chats.unshift(local);
  subject.state.activeId = local.id;

  delayedMirror = new Promise((resolve) => { resolveDelayed = resolve; });
  const lateHydration = subject.hydrateMachine(MACHINE);
  await new Promise((resolve) => setTimeout(resolve, 0));

  authorized = false;
  subject.evictMachineChats(MACHINE);
  assert.deepEqual(subject.state.chats.map((chat: Chat) => chat.id), [local.id]);

  resolveDelayed(mirror([], { title: 'Must stay evicted' }));
  await lateHydration;
  assert.deepEqual(subject.state.chats.map((chat: Chat) => chat.id), [local.id]);
  assert.equal(subject.state.activeId, local.id);
});

test('rejection invalidates an old catch-up owner without erasing an immediate re-request', async () => {
  let reads = 0;
  let rejectOld!: (reason: Error) => void;
  let resolveNew!: (value: Mirror) => void;
  const oldCatchup = new Promise<Mirror>((_resolve, reject) => { rejectOld = reject; });
  const newCatchup = new Promise<Mirror>((resolve) => { resolveNew = resolve; });
  const replacement = mirror([], { title: 'Authorized again' });
  const subject = remoteSubject(() => {
    reads += 1;
    if (reads === 1) return mirror();
    if (reads === 2) return oldCatchup;
    if (reads === 3) return newCatchup;
    return replacement;
  });
  await subject.hydrateMachine(MACHINE);

  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'start',
    job: {
      ...terminalJob({ id: 'job-old-owner', userMessageId: 'user-old-owner', assistantMessageId: 'assistant-old-owner' }),
      tabId: 'unknown-old-tab', chatId: 'unknown-old-chat', status: 'running', finishedAt: null,
    },
  }));
  for (let attempt = 0; reads < 2 && attempt < 10; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.equal(reads, 2);

  subject.evictMachineChats(MACHINE);
  subject.onJobEvent(tagPayload(MACHINE, {
    type: 'start',
    job: {
      ...terminalJob({ id: 'job-new-owner', userMessageId: 'user-new-owner', assistantMessageId: 'assistant-new-owner' }),
      tabId: 'unknown-new-tab', chatId: 'unknown-new-chat', status: 'running', finishedAt: null,
    },
  }));
  for (let attempt = 0; reads < 3 && attempt < 10; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.equal(reads, 3, 'the re-request owns a new catch-up read immediately');

  rejectOld(new Error('the rejected owner completed late'));
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(subject.remoteMachineCatchups.size, 1, 'the stale owner cannot clear the replacement owner');

  resolveNew(replacement);
  for (let attempt = 0; subject.remoteMachineCatchups.size && attempt < 10; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.equal(subject.chat(TAB)?.title, 'Authorized again');
  assert.equal(subject.remoteMachineCatchups.size, 0);
  assert.equal(subject.remoteMachinesNeedingCatchup.size, 0);
});

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

function rawRemoteMessages(messages: Msg[]): MirrorMsg[] {
  return structuredClone(messages).map((message) => ({
    ...message,
    id: localId(message.id),
    jobId: message.jobId ? localId(message.jobId) : undefined,
    turnRootId: message.turnRootId ? localId(message.turnRootId) : undefined,
    steerContinuationId: message.steerContinuationId ? localId(message.steerContinuationId) : undefined,
    steerContinuationFor: message.steerContinuationFor ? localId(message.steerContinuationFor) : undefined,
  })) as MirrorMsg[];
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
	const remoteQueueOwner = subject.chat(TAB) as Chat;
	remoteQueueOwner.queue = [{ id: tagId(MACHINE, 'remote-queue'), text: 'remote queue' }];
	subject.markQueueMutation(remoteQueueOwner);
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
