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

test('a lost workspace-move reply retries one stable operation and clears it only after success', async () => {
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
          operationId: opts.operationId,
          sessionId: '',
          cwd: '/tmp/workass-move-target',
          workspaceCommitted: true,
          workspaceRebound: true,
          workspaceRevision: 1,
        };
      },
    },
  };
  try {
    const subject = new StoreCtor();
    const owner = {
      id: 'tab-workspace-retry', chatId: 'chat-workspace-retry', sessionId: 'session-old',
      sessionProviderId: 'mock', title: 'Workspace retry', titleLocked: true, group: null,
      cwd: '/tmp/workass-move-old', providerId: 'mock', currentModelId: null, currentModeId: null,
      pending: false, messages: [], draft: '', workspaceRevision: 0,
    } as Chat;
    subject.state.chats = [owner];
    subject.state.activeId = owner.id;
    subject.state.meta = { daemon: true, workspaceRebindMode: 'transactional-v1' };

    assert.equal(await subject.moveChat(owner.id, null, '/tmp/workass-move-target'), false);
    const firstOperationId = owner._sessionOperationId;
    assert.match(firstOperationId, /^workspace-move-/);
    assert.equal(owner.cwd, '/tmp/workass-move-old');

    assert.equal(await subject.moveChat(owner.id, null, '/tmp/workass-move-target'), true);
    assert.equal(owner._sessionOperationId, undefined);
    assert.equal(owner.sessionId, null);
    assert.equal(owner.cwd, '/tmp/workass-move-target');
    assert.equal(calls.length, 2);
    assert.equal(calls[0].operationId, firstOperationId);
    assert.equal(calls[1].operationId, firstOperationId);
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});
