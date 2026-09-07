import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

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

test('the first persistence after boot does not rewrite every acknowledged chat', async () => {
  const previousWindow = (globalThis as any).window;
  const previousDocument = (globalThis as any).document;
  const writes: string[] = [];
  (globalThis as any).window = {
    api: {
      appMeta: async () => ({ daemon: true, rootDir: '/tmp', workspaceDir: '/tmp', version: 'test' }),
      getSession: async () => ({ v: 1, activeId: 'boot-0', chats: Array.from({ length: 100 }, (_, i) => ({
        id: `boot-${i}`, chatId: `conversation-${i}`, cwd: '/tmp', group: 'tmp', title: `Chat ${i}`, titleLocked: true,
        providerId: 'mock', currentModelId: 'mock-deterministic', currentModeId: 'default',
        messages: [], presentationRevision: 2, runtimeControlRevision: 3,
      })) }),
      saveSession: async () => true,
      chatPresentationSave: async (opts: any) => {
        writes.push(opts.tabId);
        return { ok: true, operationId: opts.operationId, presentationRevision: 3, actorRevision: 4 };
      },
    },
    addEventListener: () => {},
  };
  (globalThis as any).document = { documentElement: { setAttribute: () => {}, removeAttribute: () => {} } };
  let subject: any;
  try {
    subject = new StoreCtor();
    subject.schedulePersist = () => {};
    await subject.init();
    await subject.flushSession();
    assert.deepEqual(writes, [], 'startup hydration is not one hundred new presentation edits');
    const changed = subject.chat('boot-0');
    changed.title = 'An actual edit';
    subject.markPresentationMutation(changed);
    subject.touchChat(changed.id);
    await subject.flushSession();
    assert.deepEqual(writes, ['boot-0'], 'only the changed chat needs a write');
  } finally {
    subject?.monitor?.stop();
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
    if (previousDocument === undefined) delete (globalThis as any).document;
    else (globalThis as any).document = previousDocument;
  }
});

test('init reaches hydrated state and constructs the monitor when session:get fails', async () => {
  const previousWindow = (globalThis as any).window;
  const previousDocument = (globalThis as any).document;
  (globalThis as any).window = {
    api: {
      appMeta: async () => ({ rootDir: '/tmp', workspaceDir: '/tmp', version: 'test' }),
      getSession: async () => { throw new Error('fixture getSession failure'); },
    },
    addEventListener: () => {},
  };
  (globalThis as any).document = {
    documentElement: {
      setAttribute: () => {},
      removeAttribute: () => {},
    },
  };
  try {
    const subject = new StoreCtor();
    await subject.init();
    assert.equal(subject.state.hydrated, true);
    assert.ok((subject as any).monitor, 'connection monitor was not constructed before hydration');
    (subject as any).monitor.stop();
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
    if (previousDocument === undefined) delete (globalThis as any).document;
    else (globalThis as any).document = previousDocument;
  }
});
