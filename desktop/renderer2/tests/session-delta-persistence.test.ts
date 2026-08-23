import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import { LEAN_SESSION_SAVE_MODE, localMirror, type Mirror } from '../src/store/persistence.ts';
import { stageChronologicalSteer } from '../src/steering.ts';
import type { Chat, Msg, Workspace } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';
import { splitId, tagId } from '../src/wire/machineIds.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;

const rendererRoot = fileURLToPath(new URL('..', import.meta.url));

before(async () => {
  vite = await createServer({
    root: rendererRoot,
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
});

after(async () => { await vite.close(); });

function chat(id: string, overrides: Partial<Chat> = {}): Chat {
  return {
    id,
    chatId: `chat-${id}`,
    sessionId: `session-${id}`,
    sessionProviderId: 'codex',
    title: id,
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-delta-test',
    currentModelId: 'gpt-test',
    currentModeId: 'agent',
    providerId: 'codex',
    pending: false,
    messages: [],
    draft: '',
    ...overrides,
  } as Chat;
}

function subjectWithChats(chats = [chat('tab-a'), chat('tab-b')]): any {
  const subject = new StoreCtor();
  subject.state.chats = chats;
  subject.state.activeId = chats[0]?.id ?? null;
  subject.state.meta = { daemon: true, sessionSaveMode: LEAN_SESSION_SAVE_MODE };
  subject.dirtyChats?.clear();
  subject.dirtyChatVersions?.clear();
  if ('fullSavePending' in subject) subject.fullSavePending = false;
  if ('fullSaveRevision' in subject) subject.fullSaveRevision = 0;
  subject.schedulePersist = () => {};
  return subject;
}

function savedChatIDs(subject: any): string[] {
  const snapshot = serverSave(subject, true).snapshot;
  return snapshot.chats.map((candidate: Chat) => candidate.id);
}

function serverSave(subject: any, lean: boolean): { snapshot: Mirror; full: boolean } {
  return typeof subject.serverSnapshot === 'function'
    ? subject.serverSnapshot(lean)
    : { snapshot: subject.toMirror(lean), full: false };
}

function isDirty(subject: any, id: string): boolean {
  return subject.dirtyChats?.has(id) === true;
}

// Actor-backed chats no longer persist semantic mutations by omission from a
// `saveSession` mirror. These small receipts model the daemon actor commands
// so the persistence tests exercise the renderer's retry/fence logic without
// reintroducing a second chat authority.
function actorApi() {
  return {
    chatQueueReplace: async (opts: any) => ({
      ok: true, operationId: opts.operationId,
      agentQueueRevision: (opts.expectedRevision ?? 0) + 1, actorRevision: 1,
    }),
    chatCreate: async (opts: any) => ({
      ok: true, tabId: opts.tabId, chatId: opts.chatId, operationId: opts.operationId,
      actorRevision: 1, presentationRevision: 1, globalRevision: 1,
    }),
    chatPresentationSave: async (opts: any) => ({
      ok: true, operationId: opts.operationId,
      presentationRevision: (opts.expectedRevision ?? 0) + 1, actorRevision: 1,
    }),
    chatRuntimeControlsSave: async (opts: any) => ({
      ok: true, operationId: opts.operationId,
      runtimeControlRevision: (opts.expectedRevision ?? 0) + 1, actorRevision: 1,
      providerId: opts.providerId, currentModelId: opts.currentModelId,
      currentModeId: opts.currentModeId, modelControls: opts.modelControls,
    }),
    chatDelete: async (opts: any) => ({ ok: true, operationId: opts.operationId }),
  };
}

async function withWindowApi<T>(api: Record<string, unknown>, task: () => Promise<T>): Promise<T> {
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = { api: { ...actorApi(), ...api } };
  try {
    return await task();
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
}

function terminalJob(owner: Chat, overrides: Partial<PublicJob> = {}): PublicJob {
  return {
    id: 'job-terminal',
    kind: 'app-chat',
    key: null,
    title: owner.title,
    status: 'done',
    startedAt: '2026-07-25T12:00:00Z',
    finishedAt: '2026-07-25T12:00:02Z',
    code: 0,
    permissionMode: 'agent',
    chatId: owner.chatId,
    tabId: owner.id,
    sessionId: owner.sessionId,
    providerId: owner.providerId,
    userMessageId: 'user-terminal',
    assistantMessageId: 'assistant-terminal',
    promptText: 'finish',
    result: 'done',
    error: null,
    stopReason: 'end_turn',
    crashInterrupted: false,
    ...overrides,
  } as PublicJob;
}

test('lean save sends only the changed chat with the additive merge marker', async () => {
  const subject = subjectWithChats();
  const saves: Mirror[] = [];
  await withWindowApi({
    saveSession: async (snapshot: Mirror) => { saves.push(snapshot); return true; },
  }, async () => {
    subject.setDraft('tab-a', 'changed');
    await subject.flushSession();
  });

  assert.deepEqual(saves.map((snapshot) => snapshot.chats.map((candidate) => candidate.id)), [['tab-a']]);
  assert.equal(saves[0]._workassSave, LEAN_SESSION_SAVE_MODE);
});

test('every persisted chat-mutation family marks its exact chat dirty', async (t) => {
  const cases: Array<{
    name: string;
    mutate: (subject: any, owner: Chat) => void | Promise<void>;
    prepare?: (subject: any, owner: Chat) => void;
  }> = [
    { name: 'per-chat pane', mutate: (subject) => subject.toggleRail() },
    { name: 'draft text', mutate: (subject, owner) => subject.setDraft(owner.id, 'draft') },
    { name: 'sidebar metadata', mutate: (subject, owner) => {
      subject.state.activeId = 'another-tab';
      subject.markUnread(owner.id);
    } },
    { name: 'FIFO queue', mutate: (subject, owner) => {
      owner.queue = [{ id: 'queued-once', text: 'queued' }];
      subject.markQueueMutation(owner);
    } },
    { name: 'model/mode controls', mutate: async (subject, owner) => {
      subject.persistControlsNow = async () => {};
      await subject.setModeSel(owner.id, 'plan');
    } },
    { name: 'turn submission', mutate: async (subject, owner) => {
      subject.state.connection = 'disconnected';
      await subject._send(owner, 'offline fixture');
    } },
    {
      name: 'local cancellation',
      prepare: (_subject, owner) => {
        owner.messages = [{
          id: 'assistant-running',
          role: 'assistant',
          content: '',
          status: 'running',
          at: null,
          jobId: 'job-running',
          events: [],
        } as Msg];
      },
      mutate: (subject, owner) => subject.finalizeCancelledLocally(owner, owner.messages[0], 'job-running'),
    },
    {
      name: 'usage snapshot',
      mutate: (subject, owner) => subject.onJobEvent({
        type: 'usage',
        id: 'job-usage',
        tabId: owner.id,
        chatId: owner.chatId,
        sessionId: owner.sessionId,
        providerId: owner.providerId,
        used: 10,
        size: 100,
        inputTokens: 0,
        outputTokens: 0,
      }),
    },
    {
      name: 'terminal turn',
      prepare: (subject, owner) => {
        owner.messages = [{
          id: 'assistant-terminal',
          role: 'assistant',
          content: 'answer',
          status: 'running',
          at: null,
          jobId: 'job-terminal',
          events: [],
        } as Msg];
        subject.jobRef.set('job-terminal', { tabId: owner.id, msgId: 'assistant-terminal' });
        subject.chatJobs.set(owner.chatId, { tabId: owner.id, msgId: 'assistant-terminal' });
        subject.flushSession = async () => {};
      },
      mutate: (subject, owner) => subject.onJobEvent({ type: 'end', job: terminalJob(owner) }),
    },
  ];

  for (const family of cases) {
    await t.test(family.name, async () => {
      const owner = chat('tab-owner');
      const unchanged = chat('tab-unchanged');
      const subject = subjectWithChats([owner, unchanged]);
      family.prepare?.(subject, owner);
      await family.mutate(subject, owner);
      assert.deepEqual(savedChatIDs(subject), [owner.id]);
    });
  }
});

test('a transport disconnect preserves a daemon-owned running turn for reconnect reconciliation', () => {
  const owner = chat('tab-owner', {
    messages: [{
      id: 'assistant-running', role: 'assistant', content: 'partial', status: 'running',
      at: null, jobId: 'job-running', events: [],
    } as Msg],
  });
  const subject = subjectWithChats([owner]);
  subject.rebuildJobRefs();
  subject.dirtyChats.clear();

  subject.setConnection('disconnected');

  assert.equal(owner.messages[0].status, 'running');
  assert.deepEqual(subject.jobRef.get('job-running'), { tabId: owner.id, msgId: 'assistant-running' });
  assert.equal(isDirty(subject, owner.id), false);
});

test('false and throwing saves keep a chat dirty until a successful retry', async () => {
  for (const failure of ['false', 'throw'] as const) {
    const subject = subjectWithChats();
    const payloads: Mirror[] = [];
    let calls = 0;
    await withWindowApi({
      saveSession: async (snapshot: Mirror) => {
        payloads.push(snapshot);
        calls += 1;
        if (calls === 1) {
          if (failure === 'throw') throw new Error('fixture save failure');
          return false;
        }
        return true;
      },
    }, async () => {
      const oldWarn = console.warn;
      console.warn = () => {};
      try {
        subject.setDraft('tab-a', `${failure}-retry`);
        await subject.flushSession();
        assert.equal(isDirty(subject, 'tab-a'), true, failure);
        await subject.flushSession();
      } finally {
        console.warn = oldWarn;
      }
    });
    assert.deepEqual(payloads.map((snapshot) => snapshot.chats.map((candidate) => candidate.id)), [
      ['tab-a'],
      ['tab-a'],
    ]);
    assert.equal(isDirty(subject, 'tab-a'), false, failure);
  }
});

test('a mutation arriving during a save acknowledgement remains dirty and is resent', async () => {
  const subject = subjectWithChats();
  const payloads: Mirror[] = [];
  let release!: (saved: boolean) => void;
  const firstAck = new Promise<boolean>((resolve) => { release = resolve; });
  let callCount = 0;

  await withWindowApi({
    saveSession: async (snapshot: Mirror) => {
      payloads.push(snapshot);
      callCount += 1;
      return callCount === 1 ? firstAck : true;
    },
  }, async () => {
    subject.setDraft('tab-a', 'first');
    const firstSave = subject.flushSession();
    await new Promise((resolve) => setTimeout(resolve, 0));
    subject.setDraft('tab-a', 'second');
    release(true);
    await firstSave;

    assert.equal(isDirty(subject, 'tab-a'), true);
    await subject.flushSession();
  });

  assert.deepEqual(payloads.map((snapshot) => snapshot.chats[0]?.draft), ['first', 'second']);
  assert.equal(isDirty(subject, 'tab-a'), false);
});

test('a successful queue save releases the refresh fence', async () => {
  const owner = chat('tab-a');
  owner.messages = [{
    id: 'assistant-running', role: 'assistant', content: 'working', status: 'running',
    at: null, events: [], jobId: 'job-running',
  } as any];
  const subject = subjectWithChats([owner]);
  subject.schedulePersist = () => {};

  await withWindowApi({ saveSession: async () => true }, async () => {
    assert.equal(subject.queueDraftMessage(owner.id, 'persist exactly once', []), true);
    await subject.flushSession();

    const staleServer = subject.toMirror(false);
    staleServer.chats[0].queue = undefined;
    assert.equal(subject.restoreSessionSnapshot(staleServer), true);
  });

  assert.equal(subject.chat(owner.id)?.queue, undefined, 'a later server snapshot may apply after the save fence clears');
});

test('a stale background refresh cannot remove a newly created active chat before its save', () => {
  const running = chat('tab-running', {
    messages: [{
      id: 'assistant-running', role: 'assistant', content: 'working', status: 'running',
      at: null, events: [], jobId: 'job-running',
    } as Msg],
  });
  const subject = subjectWithChats([running]);
  const staleServer = subject.toMirror(false);

  const created = subject.newChat(true, '/tmp/workass-mobile');
  assert.equal(subject.state.activeId, created.id);

  // The running chat can trigger session repair before the create's delayed
  // full save reaches the daemon. The new row must stay mounted so an inline
  // rename keeps both its DOM focus and the user's selected chat.
  assert.equal(subject.restoreSessionSnapshot(staleServer), true);
  assert.strictEqual(subject.chat(created.id), created);
  assert.equal(subject.state.activeId, created.id);
});

test('a new chat owns a closed right pane before its create receipt arrives', () => {
  const subject = subjectWithChats([chat('tab-existing', { pane: 'rail' })]);

  const created = subject.newChat(true, '/tmp/workass-mobile');

  assert.equal(created.pane, null, 'the first render must not inherit the legacy open-rail fallback');
  assert.equal(subject.active()?.pane, null);
});

test('a new chat here preserves the remote machine before its first actor call', () => {
  const remote = chat(tagId('m-lagpc', 'tab-existing'), {
    chatId: tagId('m-lagpc', 'chat-existing'),
    machineId: 'm-lagpc',
    cwd: 'C:\\Users\\a447079',
  });
  const subject = subjectWithChats([remote]);

  const created = subject.newChat(true, remote.cwd);

  assert.equal(created.machineId, 'm-lagpc');
  assert.ok(splitId(created.id).id.startsWith('tab-'));
  assert.equal(splitId(created.chatId!).machineId, 'm-lagpc');
  assert.ok(splitId(created.chatId!).id.startsWith('conv-'));
  assert.equal(created.cwd, remote.cwd);
  assert.equal(subject.toMirror(false).chats.some((candidate: Chat) => candidate.id === created.id), false,
    'the local daemon must never persist a remote chat as its own');
});

test('Nueva aquí can target an inactive remote row explicitly', () => {
  const local = chat('tab-local', { cwd: '/tmp/workass-local' });
  const subject = subjectWithChats([local]);

  const created = subject.newChat(true, 'C:\\Users\\a447079', 'm-lagpc');

  assert.equal(created.machineId, 'm-lagpc');
  assert.equal(splitId(created.id).machineId, 'm-lagpc');
  assert.equal(splitId(created.chatId!).machineId, 'm-lagpc');
});

test('a stale remote hydration cannot erase a chat after its durable create receipt', async () => {
  const local = chat('tab-local', { cwd: '/tmp/workass-local' });
  const subject = subjectWithChats([local]);
  const staleRemote = subject.toMirror(false);
  staleRemote.chats = [];
  staleRemote.activeId = null;
  let createCalls = 0;
  const machineLink = {
    invoke: async (channel: string) => {
      assert.equal(channel, 'session:get');
      return staleRemote;
    },
  };
  subject.machines = {
    linkFor: () => machineLink,
    ownsLink: (_machineId: string, link: unknown) => link === machineLink,
    setReason: () => {},
  };

  await withWindowApi({
    chatCreate: async (opts: any) => {
      createCalls += 1;
      return {
        ok: true, tabId: opts.tabId, chatId: opts.chatId, operationId: opts.operationId,
        actorRevision: 1, presentationRevision: 1, globalRevision: 1,
      };
    },
  }, async () => {
    const created = subject.newChat(false, 'C:\\Users\\a447079', 'm-lagpc');
    await subject.pendingChatCreatePromises.get(created.id);

    assert.equal(subject.pendingChatCreates.has(created.id), false,
      'the durable receipt must stop duplicate chat:create calls');
    assert.equal(subject.remoteChatCreateFences.has(created.id), true);

    await subject.hydrateMachine('m-lagpc');
    assert.strictEqual(subject.chat(created.id), created,
      'an older session:get must preserve the confirmed optimistic row');
    assert.equal(await subject.ensureChatCreated(created), true);
    assert.equal(createCalls, 1);
  });
});

test('the create fence releases after the daemon echoes the new chat', () => {
  const running = chat('tab-running');
  const subject = subjectWithChats([running]);
  const beforeCreate = subject.toMirror(false);
  const created = subject.newChat(true, '/tmp/workass-mobile');
  const afterCreate = subject.toMirror(false);

  assert.equal(subject.restoreSessionSnapshot(afterCreate), true);
  assert.ok(subject.chat(created.id));

  assert.equal(subject.restoreSessionSnapshot(beforeCreate), true);
  assert.equal(subject.chat(created.id), null);
  assert.equal(subject.state.activeId, running.id);
});

test('the authoritative create echo keeps a newly created thread at the top', async () => {
  const first = chat('tab-first');
  const second = chat('tab-second');
  const subject = subjectWithChats([first, second]);

  await withWindowApi({}, async () => {
    const created = subject.newChat(true, '/tmp/workass-delta-test');
    await subject.pendingChatCreatePromises.get(created.id);

    // An actor may become visible to session:get before its tab id has reached
    // the daemon-global chatOrder projection. That authoritative echo appends
    // the otherwise-correct row; hydration must retain the user's creation
    // position until the create fence settles.
    const echoed = subject.toMirror(false) as Mirror;
    const createdRow = echoed.chats.find((candidate) => candidate.id === created.id)!;
    echoed.chats = [
      echoed.chats.find((candidate) => candidate.id === first.id)!,
      echoed.chats.find((candidate) => candidate.id === second.id)!,
      createdRow,
    ];
    echoed.chatOrder = [first.id, second.id, created.id];

    assert.equal(subject.restoreSessionSnapshot(echoed), true);
    assert.deepEqual(subject.state.chats.map((candidate: Chat) => candidate.id), [created.id, first.id, second.id]);
    assert.deepEqual(serverSave(subject, false).snapshot.chatOrder, [created.id, first.id, second.id]);
  });
});

test('delete, structural, first-save, and post-restore boundaries force a complete save', async (t) => {
  await t.test('first save after boot', () => {
    const subject = new StoreCtor();
    subject.state.chats = [chat('tab-a'), chat('tab-b')];
    const save = serverSave(subject, true);
    assert.equal(save.full, true);
    assert.deepEqual(save.snapshot.chats.map((candidate: Chat) => candidate.id), ['tab-a', 'tab-b']);
  });

  await t.test('post restore', () => {
    const subject = subjectWithChats();
    const mirror = subject.toMirror(false);
    assert.equal(subject.restoreSessionSnapshot(mirror), true);
    assert.equal(serverSave(subject, true).full, true);
  });

  const structuralCases: Array<{
    name: string;
    run: (subject: any, owner: Chat) => void | Promise<unknown>;
    prepare?: (subject: any, owner: Chat) => void;
    api?: Record<string, unknown>;
  }> = [
    { name: 'chat create', run: (subject) => { subject.newChat(); } },
    { name: 'chat rename', run: (subject, owner) => subject.renameChat(owner.id, 'Renamed') },
    {
      name: 'workspace move',
      prepare: (subject, owner) => {
        owner.sessionId = null;
        subject.state.meta = { daemon: true, workspaceRebindMode: 'transactional-v1' };
      },
      api: {
        appChatNewSession: async (opts: any) => ({
          operationId: opts.operationId,
          sessionId: '',
          workspaceCommitted: true,
          workspaceRebound: true,
          workspaceRevision: (opts.expectedWorkspaceRevision ?? 0) + 1,
        }),
      },
      run: (subject, owner) => subject.moveChatToWorkspace(owner.id, null, '/tmp/workass-delta-moved'),
    },
    { name: 'folder removal', run: (subject) => subject.removeWorkspace('/tmp/workass-delta-a') },
    { name: 'folder reorder', run: (subject) => subject.reorderWorkspaces('/tmp/workass-delta-a', null) },
    { name: 'folder collapse', run: (subject) => subject.toggleWorkspaceCollapsed('/tmp/workass-delta-a') },
    { name: 'sidebar settle', run: (subject, owner) => subject.settleChat(owner.id, true) },
    {
      name: 'explicit delete',
      run: async (subject, owner) => subject.closeChatDurably(owner.id),
    },
  ];

  for (const family of structuralCases) {
    await t.test(family.name, async () => {
      const owner = chat('tab-owner');
      const other = chat('tab-other');
      const subject = subjectWithChats([owner, other]);
      subject.state.workspaces = [
        { id: 'workspace-a', name: 'A', path: '/tmp/workass-delta-a' },
        { id: 'workspace-b', name: 'B', path: '/tmp/workass-delta-b' },
      ] as Workspace[];
      const saves: Array<{ snapshot: Mirror; full: boolean }> = [];
      subject.saveServerSnapshot = async (snapshot: Mirror, full: boolean) => {
        saves.push({ snapshot, full });
      };
      family.prepare?.(subject, owner);
      await withWindowApi(family.api ?? {}, () => Promise.resolve(family.run(subject, owner)));
      const save = saves.at(-1) ?? serverSave(subject, true);
      assert.equal(save.full, true);
      assert.equal(save.snapshot.chats.length, subject.state.chats.length);
      if (family.name === 'explicit delete') {
        // Deletion is an actor command now; the session mirror no longer
        // carries omission-based tombstones.
        assert.equal(save.snapshot._workassDeletedChatIds, undefined);
        assert.equal(subject.chat(owner.id), null);
      }
    });
  }
});

test('chat reorder saves only global order and never dirties a chat actor', async () => {
  const owner = chat('tab-owner');
  const other = chat('tab-other');
  const subject = subjectWithChats([owner, other]);
  const saves: Array<{ snapshot: Mirror; full: boolean }> = [];
  subject.saveServerSnapshot = async (snapshot: Mirror, full: boolean) => {
    saves.push({ snapshot, full });
  };

  subject.reorderChat(owner.id, null);
  await new Promise((resolve) => setTimeout(resolve, 0));

  assert.deepEqual(subject.state.chats.map((candidate: Chat) => candidate.id), ['tab-other', 'tab-owner']);
  assert.equal(isDirty(subject, owner.id), false);
  assert.equal(isDirty(subject, other.id), false);
  assert.equal(saves.at(-1)?.full, false);
  assert.deepEqual(saves.at(-1)?.snapshot.chatOrder, ['tab-other', 'tab-owner']);
  assert.deepEqual(saves.at(-1)?.snapshot.chats, []);
});

test('server deltas carry chat commands while localStorage carries only view preferences', () => {
  const subject = subjectWithChats();
  subject.touchChat('tab-a');

  const delta = subject.toMirror(true, new Set(['tab-a']));
  assert.deepEqual(delta.chats.map((candidate: Chat) => candidate.id), ['tab-a']);
  assert.equal(delta._workassSave, LEAN_SESSION_SAVE_MODE);
  assert.deepEqual(delta.chats[0].messages, []);

  const local = localMirror(subject.toMirror(true));
  assert.deepEqual(local.chats, []);
  assert.equal(local._workassSave, undefined);
});

test('streaming extends the debounce to 3000ms and a terminal event flushes exactly once', async () => {
  const running: Msg = {
    id: 'assistant-running',
    role: 'assistant',
    content: '',
    status: 'running',
    at: null,
    jobId: 'job-terminal',
    events: [],
  };
  const owner = chat('tab-owner', { messages: [running] });
  const subject = subjectWithChats([owner]);
  const delays: number[] = [];
  const oldSetTimeout = globalThis.setTimeout;
  subject.scheduleLocalMirror = () => {};
  const schedulePersist = Object.getPrototypeOf(subject).schedulePersist;
  globalThis.setTimeout = ((_: TimerHandler, delay?: number) => {
    delays.push(Number(delay));
    return delays.length as unknown as ReturnType<typeof setTimeout>;
  }) as typeof setTimeout;
  try {
    await withWindowApi({}, async () => {
      schedulePersist.call(subject);
      subject.saveTimer = null;
      running.status = 'done';
      schedulePersist.call(subject);
    });
  } finally {
    globalThis.setTimeout = oldSetTimeout;
  }
  assert.deepEqual(delays, [3000, 600]);

  running.status = 'running';
  subject.jobRef.set('job-terminal', { tabId: owner.id, msgId: running.id });
  subject.chatJobs.set(owner.chatId, { tabId: owner.id, msgId: running.id });
  let scheduled = 0;
  let flushed = 0;
  subject.schedulePersist = () => { scheduled += 1; };
  subject.flushSession = async () => { flushed += 1; };
  subject.onJobEvent({ type: 'end', job: terminalJob(owner) });
  assert.equal(scheduled, 0);
  assert.equal(flushed, 1);
});

test('steer-consumed relies on its immediate flush without scheduling a second save', () => {
  const root: Msg = {
    id: 'assistant-root',
    role: 'assistant',
    content: 'before',
    status: 'running',
    at: null,
    jobId: 'job-steer',
    events: [],
  };
  const steer: Msg = {
    id: 'steer-user',
    role: 'user',
    content: 'direction',
    status: 'pending',
    at: null,
    steerState: 'sending',
    events: [],
  };
  const continuation: Msg = {
    id: 'assistant-continuation',
    role: 'assistant',
    content: '',
    status: 'running',
    at: null,
    jobId: 'job-steer',
    events: [],
  };
  const owner = chat('tab-owner', { messages: [root] });
  assert.ok(stageChronologicalSteer(owner.messages, steer, continuation));
  const subject = subjectWithChats([owner]);
  subject.jobRef.set('job-steer', { tabId: owner.id, msgId: root.id });
  let scheduled = 0;
  let flushed = 0;
  subject.schedulePersist = () => { scheduled += 1; };
  subject.flushSession = async () => { flushed += 1; };

  subject.onJobEvent({
    type: 'acp',
    id: 'job-steer',
    event: { kind: 'steer-consumed', clientUserMessageId: steer.id },
  });

  assert.equal(scheduled, 0);
  assert.equal(flushed, 1);
});

test('a chat delta stays smaller than the equivalent complete session', () => {
  const chats = Array.from({ length: 3 }, (_, chatIndex) => chat(`tab-${chatIndex}`, {
    title: `Conversation ${chatIndex}`,
    draft: chatIndex === 0 ? 'active draft' : '',
    messages: Array.from({ length: 3 }, (_, messageIndex): Msg => ({
      id: `message-${chatIndex}-${messageIndex}`,
      role: messageIndex % 2 ? 'assistant' : 'user',
      content: `message ${chatIndex}/${messageIndex} ${'retained text '.repeat(2 + chatIndex)}`,
      status: 'done',
      at: '2026-07-25T12:00:00Z',
      events: [],
    })),
  }));
  const subject = subjectWithChats(chats);
  const full = subject.toMirror(true) as Mirror;
  const fullBytes = Buffer.byteLength(JSON.stringify(full));
  const delta = subject.toMirror(true, new Set(['tab-1'])) as Mirror;
  const deltaBytes = Buffer.byteLength(JSON.stringify(delta));

  assert.deepEqual(delta.chats.map((candidate) => candidate.id), ['tab-1']);
  assert.equal(delta._workassSave, LEAN_SESSION_SAVE_MODE);
  assert.ok(deltaBytes < fullBytes);
});
