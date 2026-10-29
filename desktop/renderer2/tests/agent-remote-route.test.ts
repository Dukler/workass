import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test, { after } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { AgentRouteRequest, StartJobOpts } from '../src/wire/types.ts';
import { tagId } from '../src/wire/machineIds.ts';

const machineId = 'm-san';
const tabId = tagId(machineId, 'tab-hello');
const chatId = tagId(machineId, 'chat-hello');
const fixtureStores: any[] = [];

function ownStore<T>(store: T): T {
  fixtureStores.push(store);
  return store;
}

function clearFixtureStoreTimers(store: any) {
  store.clearToastTimers();
  if (store.saveTimer) clearTimeout(store.saveTimer);
  store.saveTimer = null;
  if (store.mirrorTimer) clearTimeout(store.mirrorTimer);
  store.mirrorTimer = null;
}

function remoteChat(overrides: Partial<Chat> = {}): Chat {
  return {
    id: tabId, chatId, machineId, sessionId: 'session-hello',
    title: 'hello', titleLocked: true, group: 'workass', cwd: 'C:\\Users\\dev\\workass',
    currentModelId: 'gpt-5.5[high]', currentModeId: 'agent-full-access',
    providerId: 'devin', providerName: 'Devin', pending: false, draft: '',
    messages: [], messageCount: 0, historyComplete: true,
    ...overrides,
  };
}

function request(method: string, params: Record<string, unknown> = {}): AgentRouteRequest {
  return { requestId: `request-${method}`, method, params, expiresAt: Date.now() + 10_000 };
}

let sharedServer: ViteDevServer | undefined;
let sharedStore: Promise<{
  Store: new () => StoreShape;
  setMachineRouter: (api: unknown) => void;
}> | undefined;
after(async () => { await sharedServer?.close(); });

async function loadStore(t: { after(fn: () => void | Promise<void>): void }) {
  sharedStore ??= (async () => {
    sharedServer = await createServer({
      root: fileURLToPath(new URL('..', import.meta.url)),
      server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent',
    });
    const storeModule = await sharedServer.ssrLoadModule('/src/store/store.ts');
    ownStore(storeModule.store);
    const apiModule = await sharedServer.ssrLoadModule('/src/wire/api.ts');
    return {
      Store: class extends (storeModule.Store as new () => StoreShape) {
        constructor() { super(); ownStore(this); }
      },
      setMachineRouter: apiModule.setMachineRouter as (api: unknown) => void,
    };
  })();
  t.after(() => {
    for (const store of fixtureStores) clearFixtureStoreTimers(store);
    fixtureStores.length = 0;
  });
  return sharedStore;
}

interface StoreShape {
  state: { chats: Chat[]; activeId: string | null; connection: string };
  routeAgentRequest(request: AgentRouteRequest): Promise<unknown>;
  sendTo(chatId: string, prompt: string): Promise<boolean>;
}

test('remote turn diagnostics reads the owning daemon without history hydration or UI changes', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as StoreShape & {
    machines: { linkFor(id: string): { invoke<T>(channel: string, payload: unknown): Promise<T> } | undefined };
    ensureFullHistory(): Promise<void>;
  };
  subject.state.chats = [remoteChat({ historyComplete: false, messageCount: 5000 })];
  subject.ensureFullHistory = async () => { throw new Error('diagnostics must not read history'); };
  const calls: unknown[] = [];
  subject.machines = { linkFor: (id) => id === machineId ? { invoke: async <T>(channel: string, payload: unknown) => {
    calls.push({ channel, payload });
    return { tabId: 'tab-hello', chatId: 'chat-hello', turns: [{ jobId: 'diagnostic-job', firstThinkingMs: 3500 }] } as T;
  } } : undefined };
  const params = { tab_id: tabId, chat_id: chatId, machine_id: machineId, limit: 3 };
  const result = await subject.routeAgentRequest(request('chat.diagnostics', params)) as Record<string, unknown>;
  assert.deepEqual(calls, [{ channel: 'chat:turn-diagnostics', payload: { tab_id: 'tab-hello', chat_id: 'chat-hello', limit: 3 } }]);
  assert.equal(result.chatId, chatId);
  assert.equal((result.turns as Array<Record<string, unknown>>)[0].jobId, tagId(machineId, 'diagnostic-job'));
  assert.equal(subject.state.activeId, null);
  assert.equal(subject.state.chats[0].messages.length, 0);
  await assert.rejects(subject.routeAgentRequest(request('chat.diagnostics', { ...params, chat_id: tagId('wrong', 'chat-hello') })), /exact/);
  assert.equal(calls.length, 1);
});

test('remote MCP Stop delivers the exact job through the normal cancellation router without loading history', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  const jobId = tagId(machineId, 'job-stop');
  const queue = [{ id: 'follow-up', text: 'keep queued' }];
  subject.state.chats = [remoteChat({ historyComplete: false, queue, messages: [
    { id: 'assistant-stop', role: 'assistant', content: '', status: 'running', at: null, events: [], jobId },
  ] })];
  const calls: string[] = [];
  setMachineRouter({ cancelJob: async (id: string) => { calls.push(id); return { cancelled: true }; } });
  t.after(() => setMachineRouter(undefined));
  const params = { tab_id: tabId, chat_id: chatId, machine_id: machineId };
  assert.deepEqual(await subject.routeAgentRequest(request('chat.cancel', params)), { cancelled: true });
  assert.deepEqual(calls, [jobId]);
  assert.deepEqual(subject.state.chats[0].queue, queue);
  assert.equal(subject.state.activeId, null);
  await assert.rejects(subject.routeAgentRequest(request('chat.cancel', { ...params, machine_id: 'wrong' })), /exact/);
  assert.equal(calls.length, 1);
});

test('agent MCP projection lists and reads a mounted remote chat without focusing it', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat({
    messages: [{ id: tagId(machineId, 'a-1'), role: 'assistant', content: 'remote answer', result: 'done', status: 'done', at: '2026-09-03T00:00:00Z', events: [] }],
    messageCount: 1,
  })];
  subject.state.activeId = null;

  const listed = await subject.routeAgentRequest(request('chat.list')) as { chats: Array<Record<string, unknown>> };
  assert.equal(listed.chats.length, 1);
  assert.equal(listed.chats[0].tabId, tabId);
  assert.equal(listed.chats[0].chatId, chatId);
  assert.equal(listed.chats[0].machineId, machineId);
  assert.equal(subject.state.activeId, null, 'an MCP read must not select the remote row');

  const read = await subject.routeAgentRequest(request('chat.read', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, limit: 40,
  })) as Record<string, unknown>;
  assert.equal(read.running, false);
  assert.equal((read.messages as Array<Record<string, unknown>>)[0].content, 'remote answer');
  assert.equal(subject.state.activeId, null);
});

test('remote pre-admission Stop closes its exact row even when the older daemon emits no end event', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  const jobId = tagId(machineId, 'job-stop-before-attach');
  const owner = remoteChat({ messages: [
    { id: 'assistant-stop', role: 'assistant', content: '', status: 'running', at: null, events: [], jobId },
  ] });
  subject.state.chats = [owner];
  const readbacks: string[] = [];
  (subject as any).hydrateMachine = async (id: string) => { readbacks.push(id); };
  setMachineRouter({ cancelJob: async () => ({ cancelled: true, preAdmission: true, reason: 'cancelled' }) });
  t.after(() => setMachineRouter(undefined));
  await subject.routeAgentRequest(request('chat.cancel', { tab_id: tabId, chat_id: chatId, machine_id: machineId }));
  assert.equal(owner.messages[0].status, 'cancelled');
  assert.deepEqual(readbacks, [machineId]);
  const read = await subject.routeAgentRequest(request('chat.read', { tab_id: tabId, chat_id: chatId, machine_id: machineId })) as Record<string, unknown>;
  assert.equal(read.running, false);
});

test('updater MCP routes remote diagnostics over data and activation over the control lane', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as StoreShape & {
    selfMachineId: string;
    machines: {
      linkFor(machineId: string): { invoke<T>(channel: string, payload: unknown): Promise<T> } | undefined;
      controlLinkFor(machineId: string): { invoke<T>(channel: string, payload: unknown): Promise<T> } | undefined;
    };
    state: StoreShape['state'] & { machines: Array<Record<string, unknown>> };
  };
  subject.selfMachineId = 'machine-local';
  subject.state.machines = [{
    machineId, name: 'San laptop', link: 'ready', reachable: true, paired: true,
  }];
  const calls: Array<{ lane: string; channel: string; payload: unknown }> = [];
  subject.machines = {
    linkFor: (selected) => selected === machineId ? {
      invoke: async <T>(channel: string, payload: unknown) => {
        calls.push({ lane: 'data', channel, payload });
        return { machineId, diagnostics: { state: { phase: 'failed' } } } as T;
      },
    } : undefined,
    controlLinkFor: (selected) => selected === machineId ? {
      invoke: async <T>(channel: string, payload: unknown) => {
        calls.push({ lane: 'control', channel, payload });
        return { machineId, state: { phase: 'installing' } } as T;
      },
    } : undefined,
  };

  const status = await subject.routeAgentRequest(request('update.status', { machine_id: machineId })) as Record<string, unknown>;
  assert.equal(status.machineId, machineId);
  const apply = await subject.routeAgentRequest(request('update.apply', {
    machine_id: machineId,
    expected_current_version: '1.1.0', expected_target_version: '1.2.0',
    authorization: `update ${machineId} from 1.1.0 to 1.2.0`, operation_id: 'update-san-1.2.0',
  })) as Record<string, unknown>;
  assert.equal(apply.machineId, machineId);
  assert.deepEqual(calls.map(({ lane, channel }) => ({ lane, channel })), [
    { lane: 'data', channel: 'app:update-status' },
    { lane: 'control', channel: 'app:update-apply' },
  ]);
});

test('updater MCP invokes only the local shell bridge for the exact local machine', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as StoreShape & { selfMachineId: string };
  subject.selfMachineId = 'machine-local';
  const previousWindow = (globalThis as { window?: unknown }).window;
  let applies = 0;
  (globalThis as { window?: unknown }).window = {
    workassUpdater: {
      diagnostics: async () => ({ schemaVersion: 1, state: { supported: true, phase: 'failed', currentVersion: '1.1.0' } }),
      applyAuthorized: async (payload: Record<string, unknown>) => {
        applies += 1;
        return { supported: true, phase: 'installing', currentVersion: '1.1.0', authorizationReceipt: payload };
      },
    },
  };
  t.after(() => { (globalThis as { window?: unknown }).window = previousWindow; });

  const status = await subject.routeAgentRequest(request('update.local.status', { machine_id: 'machine-local' })) as Record<string, unknown>;
  assert.equal(status.local, true);
  await subject.routeAgentRequest(request('update.local.apply', {
    machine_id: 'machine-local', expected_current_version: '1.1.0', expected_target_version: '1.2.0',
    authorization: 'update machine-local from 1.1.0 to 1.2.0', operation_id: 'update-local-1.2.0',
  }));
  assert.equal(applies, 1);
  await assert.rejects(
    subject.routeAgentRequest(request('update.local.status', { machine_id: machineId })),
    /does not address this exact machine/,
  );
});

test('agent remote idle auto-send omits stale runtime controls and keeps one operation owner', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  subject.state.connection = 'connected';
  subject.state.chats = [remoteChat({
    currentModelId: 'swe-1-6-fast', currentModeId: 'ask', runtimeControlRevision: 2,
  })];
  const starts: StartJobOpts[] = [];
  // The owning daemon already committed revision 3; history hydration can
  // legitimately leave this complete mounted mirror at revision 2.
  const committed = { providerId: 'devin', modelId: 'swe-2-medium', modeId: 'bypass' };
  setMachineRouter({ startJob: async (opts: StartJobOpts) => {
    starts.push(opts);
    for (const key of ['providerId', 'modelId', 'modeId']) {
      assert.equal(Object.hasOwn(opts, key), false, `cached ${key} must be omitted`);
    }
    return { id: tagId(machineId, 'job-current-controls'), status: 'running', ...committed };
  } });
  t.after(() => setMachineRouter(undefined));
  const params = {
    tab_id: tabId, chat_id: chatId, machine_id: machineId,
    operation_id: 'send-current-controls', message: 'continue with committed controls', delivery: 'auto',
  };
  const first = await subject.routeAgentRequest(request('chat.send', params)) as Record<string, unknown>;
  const replay = await subject.routeAgentRequest(request('chat.send', params));
  assert.equal(first.queued, false);
  assert.deepEqual(replay, first);
  assert.equal(starts.length, 1);
  assert.equal(starts[0].operationId, tagId(machineId, 'agent-user-send-current-controls'));
  assert.equal(starts[0].tabId, tabId);
  assert.equal(starts[0].chatId, chatId);
  assert.equal(starts[0].busyMode, 'queue-v1');
  assert.equal(subject.state.chats[0].messages.length, 2);
  assert.equal(subject.state.chats[0].queue?.length ?? 0, 0);
  assert.equal(subject.state.activeId, null);
});

test('human remote send still carries the explicit picker controls', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  subject.state.connection = 'connected';
  subject.state.chats = [remoteChat({ currentModelId: 'swe-2-medium', currentModeId: 'bypass' })];
  const starts: StartJobOpts[] = [];
  setMachineRouter({ startJob: async (opts: StartJobOpts) => {
    starts.push(opts);
    return { id: tagId(machineId, 'job-human-picker'), status: 'running' };
  } });
  t.after(() => setMachineRouter(undefined));
  assert.equal(await subject.sendTo(tabId, 'use my selection'), true);
  assert.equal(starts.length, 1);
  assert.equal(starts[0].providerId, 'devin');
  assert.equal(starts[0].modelId, 'swe-2-medium');
  assert.equal(starts[0].modeId, 'bypass');
});

test('agent MCP remote auto-send becomes one stable FIFO owner while the chat runs', async (t) => {
  const { Store, setMachineRouter } = await loadStore(t);
  const subject = new Store();
  subject.state.connection = 'connected';
  subject.state.chats = [remoteChat({
    messages: [{ id: tagId(machineId, 'a-running'), role: 'assistant', content: '', status: 'running', at: null, events: [], jobId: tagId(machineId, 'job-1') }],
  })];
  const queueWrites: unknown[] = [];
  setMachineRouter({
    chatQueueReplace: async (opts) => {
      queueWrites.push(opts);
      return { ok: true, operationId: opts.operationId, agentQueueRevision: 1, actorRevision: 2 };
    },
  });
  try {
    const params = {
      tab_id: tabId, chat_id: chatId, machine_id: machineId,
      operation_id: 'diagnose-update-once', message: 'why did the update fail?', delivery: 'auto',
    };
    const first = await subject.routeAgentRequest(request('chat.send', params)) as Record<string, unknown>;
    const second = await subject.routeAgentRequest(request('chat.send', params)) as Record<string, unknown>;
    assert.equal(first.queued, true);
    assert.equal(second.queueId, first.queueId);
    assert.equal(subject.state.chats[0].queue?.length, 1, 'a retried MCP call must not duplicate the prompt');
    assert.equal(queueWrites.length, 1, 'the stable duplicate is answered without a second durable queue mutation');
  } finally {
    setMachineRouter(undefined);
  }
});

test('agent MCP remote routes fail closed on a mismatched machine tag', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat()];
  await assert.rejects(
    subject.routeAgentRequest(request('chat.read', {
      tab_id: tabId, chat_id: chatId, machine_id: 'm-other',
    })),
    /exact tab_id \+ chat_id \+ machine_id/,
  );
});

test('agent MCP remote reads drop an indivisible oversized message instead of overflowing transport', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store();
  subject.state.chats = [remoteChat({
    messages: [{
      id: tagId(machineId, 'a-huge'), role: 'assistant', content: '🚀'.repeat(400_000),
      status: 'done', at: '2026-09-03T00:00:00Z', events: [],
    }],
    messageCount: 1,
  })];
  const read = await subject.routeAgentRequest(request('chat.read', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, limit: 1,
  })) as { messages: unknown[]; truncated: boolean };
  assert.equal(read.truncated, true);
  assert.deepEqual(read.messages, []);
  assert.ok(new TextEncoder().encode(JSON.stringify(read)).byteLength < 768 * 1024);
});

function readyMachine() {
  return {
    machineId, name: 'duklerver', reportedName: 'duklerver', nickname: '', address: '192.168.0.212:80',
    link: 'ready' as const, reason: '', secure: true, reachable: true, paired: true,
    requested: false, discovered: true,
  };
}

interface ForwardShape {
  state: { chats: Chat[]; machines: ReturnType<typeof readyMachine>[]; activeId: string | null };
  machines: { linkFor(id: string): { invoke<T>(channel: string, payload: unknown): Promise<T> } | undefined };
  routeAgentRequest(request: AgentRouteRequest): Promise<unknown>;
}

function forwardingStore(subject: ForwardShape, reply: unknown, calls: Array<{ channel: string; payload: unknown }>) {
  subject.state.machines = [readyMachine()];
  subject.machines = {
    linkFor: (id) => id === machineId ? {
      invoke: async <T>(channel: string, payload: unknown) => { calls.push({ channel, payload }); return reply as T; },
    } : undefined,
  };
}

test('configuring a chat on another machine runs that daemon own agent control, with its own ids', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as unknown as ForwardShape;
  const calls: Array<{ channel: string; payload: unknown }> = [];
  forwardingStore(subject, {
    ok: true, tabId: 'tab-hello', chatId: 'chat-hello', cwd: '/srv/work',
    providerId: 'codex', modelId: 'gpt-5.5', modeId: 'agent-full-access',
  }, calls);
  const result = await subject.routeAgentRequest(request('chat.configure', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId,
    permission_intent: 'full', operation_id: 'configure-duklerver-1',
  })) as Record<string, unknown>;
  assert.deepEqual(calls, [{ channel: 'agent:control', payload: { method: 'chat.configure', params: {
    tab_id: 'tab-hello', chat_id: 'chat-hello', permission_intent: 'full', operation_id: 'configure-duklerver-1',
  } } }]);
  // Back out tagged, so the next tool call addresses the same machine.
  assert.equal(result.tabId, tabId);
  assert.equal(result.chatId, chatId);
  assert.equal(result.modeId, 'agent-full-access');
  assert.equal(subject.state.activeId, null);
});

test('spawning and listing subagents on another machine reaches it and tags the ids it mints', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as unknown as ForwardShape;
  const calls: Array<{ channel: string; payload: unknown }> = [];
  forwardingStore(subject, { id: 'sub-1', label: 'remote child', status: 'running' }, calls);
  const spawned = await subject.routeAgentRequest(request('agent.spawn', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, prompt: 'build it', operation_id: 'spawn-1',
  })) as Record<string, unknown>;
  assert.equal(spawned.id, tagId(machineId, 'sub-1'));
  // The tagged child id is accepted back untagged by its own daemon.
  await subject.routeAgentRequest(request('agent.wait', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, id: tagId(machineId, 'sub-1'), operation_id: 'wait-1',
  }));
  assert.equal(calls.length, 2);
  assert.deepEqual((calls[1].payload as { params: Record<string, unknown> }).params.id, 'sub-1');
});

test('a remote provider catalog keeps its native mode ids untagged', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as unknown as ForwardShape;
  const calls: Array<{ channel: string; payload: unknown }> = [];
  forwardingStore(subject, {
    schemaVersion: 2,
    groups: [{ providerId: 'codex', modes: [{ id: 'agent-full-access', name: 'Full access' }] }],
  }, calls);
  const catalog = await subject.routeAgentRequest(request('agent.catalog', { machine_id: machineId })) as Record<string, unknown>;
  const groups = catalog.groups as Array<{ modes: Array<{ id: string }> }>;
  assert.equal(groups[0].modes[0].id, 'agent-full-access');
  assert.equal(catalog.machineId, machineId);
});

test('an unavailable machine fails visibly instead of acting on this one', async (t) => {
  const { Store } = await loadStore(t);
  const subject = new Store() as unknown as ForwardShape;
  const calls: Array<{ channel: string; payload: unknown }> = [];
  forwardingStore(subject, { ok: true }, calls);
  subject.state.machines = [{ ...readyMachine(), link: 'closed' as const }];
  await assert.rejects(subject.routeAgentRequest(request('chat.delete', {
    tab_id: tabId, chat_id: chatId, machine_id: machineId, operation_id: 'delete-1',
  })), /unavailable/);
  assert.equal(calls.length, 0);
  await assert.rejects(subject.routeAgentRequest(request('chat.configure', {
    tab_id: tabId, chat_id: chatId, operation_id: 'no-machine',
  })), /did not name a target machine/);
  await assert.rejects(subject.routeAgentRequest(request('html.render', { machine_id: machineId })), /not supported/);
  assert.equal(calls.length, 0);
});
