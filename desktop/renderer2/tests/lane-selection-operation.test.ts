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
