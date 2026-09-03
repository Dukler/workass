import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { fileURLToPath } from 'node:url';
import type { Chat } from '../src/store/types.ts';

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

function chat(): Chat {
  return {
    id: 'tab-lane-retry',
    chatId: 'chat-lane-retry',
    sessionId: null,
    title: 'Lane retry',
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-lane-retry',
    currentModelId: null,
    currentModeId: null,
    providerId: 'codex',
    pending: false,
    messages: [],
    draft: '',
  } as Chat;
}

test('a lost selected-lane start retries the exact OperationID and attaches only that provider', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      startJob: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) return undefined;
        return {
          id: 'job-lane-retry',
          sessionId: 'session-lane-retry',
          providerId: 'codex',
        };
      },
    },
  };
  try {
    const subject = new StoreCtor();
    const owner = chat();
    subject.state.chats = [owner];
    subject.state.activeId = owner.id;
    subject.state.meta = { daemon: true };
    subject.state.connection = 'connected';
    subject.schedulePersist = () => {};

    assert.equal(await subject._send(owner, 'first selected-lane prompt'), false);
    assert.equal(owner.sessionId, null);
    const failed = owner.messages.find((message) => message.role === 'assistant');
    const submitted = owner.messages.find((message) => message.role === 'user');
    assert.equal((failed as unknown as Record<string, unknown> | undefined)?.retryPrompt, undefined);
    assert.ok(submitted);

    assert.equal(await subject._send(owner, 'second selected-lane prompt'), true);
    assert.equal(owner.sessionId, 'session-lane-retry');
    assert.equal(owner.sessionProviderId, 'codex');
    assert.equal(calls.length, 2);
    assert.equal(calls[0].operationId, submitted.id);
    assert.notEqual(calls[1].operationId, submitted.id);
    assert.equal(calls[0].providerId, 'codex');
    assert.equal(calls[1].providerId, 'codex');
    assert.equal(calls[0].sessionId, undefined);
    assert.equal(calls[1].sessionId, undefined);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});

test('missing additive image capability stays unknown through hydration and late job attachment', () => {
  const subject = new StoreCtor();
  const restored = subject.fromMirror({
    v: 1,
    activeId: 'tab-image-unknown',
    seq: 1,
    theme: 'dark',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    mode: 'chats',
    chats: [{
      id: 'tab-image-unknown',
      chatId: 'chat-image-unknown',
      title: 'Image capability',
      titleLocked: true,
      group: 'workass',
      cwd: '/tmp/workass',
      currentModelId: null,
      currentModeId: null,
      providerId: 'codex',
      draft: '',
      messages: [],
    }],
  });
  const owner = restored.chats[0] as Chat;
  assert.equal(owner.imageSupport, undefined);

  owner.imageSupport = false;
  owner.deliveryCapabilities = {
    stableInputIdentity: true,
    liveSteer: true,
    steerConsumptionReceipt: true,
    consumptionReceipt: true,
    turnReadback: true,
  };
  owner.planUsageSupported = true;
  owner.planUsageResetSupported = true;
  subject.attachJobSession(owner, {
    sessionId: 'session-image-late',
    providerId: 'codex',
  });
  assert.equal(owner.sessionProviderId, 'codex');
  assert.equal(owner.imageSupport, undefined,
    'a new session attachment must not inherit a stale negative capability');
  assert.equal(owner.deliveryCapabilities, undefined,
    'a new session attachment must not inherit stale delivery semantics');
  assert.equal(owner.planUsageSupported, undefined);
  assert.equal(owner.planUsageResetSupported, undefined);
});

test('explicit image rejection from a live matching session remains authoritative', () => {
  const subject = new StoreCtor();
  const restored = subject.fromMirror({
    v: 1,
    activeId: 'tab-image-false',
    seq: 1,
    theme: 'dark',
    panes: { side: true, railWide: false, sideW: 288, railW: 312 },
    mode: 'chats',
    chats: [{
      id: 'tab-image-false',
      chatId: 'chat-image-false',
      title: 'Unsupported images',
      titleLocked: true,
      group: 'workass',
      cwd: '/tmp/workass',
      currentModelId: null,
      currentModeId: null,
      providerId: 'mock',
      draft: '',
      messages: [],
      liveSession: {
        sessionId: 'session-image-false',
        cwd: '/tmp/workass',
        providerId: 'mock',
        models: [],
        currentModelId: null,
        modes: [],
        currentModeId: null,
        imageSupport: false,
        planUsageSupported: true,
        planUsageResetSupported: false,
        deliveryCapabilities: {
          stableInputIdentity: true,
          liveSteer: false,
          steerConsumptionReceipt: false,
          consumptionReceipt: true,
          turnReadback: false,
        },
      },
    }],
  });
  assert.equal((restored.chats[0] as Chat).imageSupport, false);
  assert.deepEqual((restored.chats[0] as Chat).deliveryCapabilities, {
    stableInputIdentity: true,
    liveSteer: false,
    steerConsumptionReceipt: false,
    consumptionReceipt: true,
    turnReadback: false,
  });
  assert.equal((restored.chats[0] as Chat).planUsageSupported, true);
  assert.equal((restored.chats[0] as Chat).planUsageResetSupported, false);
});

test('a job session without provider identity cannot become a chat attachment', () => {
  const subject = new StoreCtor();
  const owner = chat();
  subject.state.chats = [owner];

  assert.equal(subject.attachJobSession(owner, {
    id: 'job-identity-missing',
    sessionId: 'identity-missing-session',
  }), false);
  assert.equal(owner.sessionId, null);
  assert.equal(owner.sessionProviderId, undefined);
});
