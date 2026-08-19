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

test('a lost lane-selection reply retries the exact OperationID and clears it only after success', async () => {
  const calls: Array<Record<string, unknown>> = [];
  let attempts = 0;
  const previousWindow = (globalThis as any).window;
  (globalThis as any).window = {
    api: {
      appChatNewSession: async (opts: Record<string, unknown>) => {
        calls.push({ ...opts });
        attempts += 1;
        if (attempts === 1) return undefined;
        return {
          sessionId: 'session-lane-retry',
          cwd: '/tmp/workass-lane-retry',
          providerId: 'codex',
          providerName: 'Codex',
          models: [],
          currentModelId: null,
          modes: [],
          currentModeId: null,
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

    await subject.ensureSession(owner);
    assert.equal(owner.sessionId, null);
    const firstOperationId = owner._sessionOperationId;
    assert.match(firstOperationId, /^lane-select-/);

    await subject.ensureSession(owner);
    assert.equal(owner.sessionId, 'session-lane-retry');
    assert.equal(owner._sessionOperationId, undefined);
    assert.equal(calls.length, 2);
    assert.equal(calls[0].operationId, firstOperationId);
    assert.equal(calls[1].operationId, firstOperationId);
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
  subject.attachJobSession(owner, {
    sessionId: 'session-image-late',
    providerId: 'codex',
  });
  assert.equal(owner.sessionProviderId, 'codex');
  assert.equal(owner.imageSupport, undefined,
    'a new session attachment must not inherit a stale negative capability');
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
      },
    }],
  });
  assert.equal((restored.chats[0] as Chat).imageSupport, false);
});
