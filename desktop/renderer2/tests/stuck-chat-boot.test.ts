import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';

let vite: ViteDevServer;
let StoreCtor: new () => any;
const fixtureStores: any[] = [];

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

after(async () => {
  for (const store of fixtureStores) store.clearToastTimers();
  await vite.close();
});

test('fresh app boot loads Codex account limits without cached usage, a chat session, or a prompt', async () => {
  const previousWindow = (globalThis as any).window;
  const previousDocument = (globalThis as any).document;
  const metadataCalls: string[] = [];
  let promptCalls = 0;
  let sessionCalls = 0;
  let receiveUsage: (value: unknown) => void = () => {};
  let finishRead: () => void = () => {};
  const snapshot = { providerId: 'codex', capturedAt: '2026-09-18T12:00:00Z',
    entries: [{ kind: 'rate-limit', id: 'seven_day', usedPercent: 42 }] };
  (globalThis as any).window = {
    api: {
      appMeta: async () => ({ rootDir: '/tmp', workspaceDir: '/tmp', version: 'test' }),
      providersList: async () => [{ id: 'codex', enabled: true, accountResetSupported: true }],
      onChatPlanUsage: (cb: typeof receiveUsage) => { receiveUsage = cb; },
      appChatRefreshPlanUsage: async (providerId: string) => {
        metadataCalls.push(providerId);
        await new Promise<void>((resolve) => { finishRead = resolve; });
        receiveUsage(snapshot);
        return { ok: true, providerId };
      },
      appChatNewSession: async () => { sessionCalls++; throw new Error('no chat session allowed'); },
      startJob: async () => { promptCalls++; throw new Error('no model prompt allowed'); },
    },
    addEventListener: () => {},
  };
  (globalThis as any).document = { documentElement: { setAttribute: () => {}, removeAttribute: () => {} } };
  let subject: any;
  try {
    for (let boot = 0; boot < 2; boot++) {
      subject = new StoreCtor();
      fixtureStores.push(subject);
      subject.schedulePersist = () => {};
      assert.deepEqual(subject.state.planUsageByProvider, {});
      assert.equal(subject.state.chats.length, 0);
      await subject.init();
      assert.equal(subject.state.planUsageLoadingByProvider.codex, true);
      assert.equal(metadataCalls.length, boot + 1, 'boot must request limits without opening a menu');
      finishRead();
      await new Promise((resolve) => setTimeout(resolve, 0));
      assert.deepEqual(subject.state.planUsageByProvider.codex, snapshot);
      assert.equal(subject.state.planUsageLoadingByProvider.codex, false);
      assert.equal(sessionCalls, 0);
      assert.equal(promptCalls, 0);
      subject.monitor.stop();
    }
    assert.deepEqual(metadataCalls, ['codex', 'codex']);
  } finally {
    finishRead();
    subject?.monitor?.stop();
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
    if (previousDocument === undefined) delete (globalThis as any).document;
    else (globalThis as any).document = previousDocument;
  }
});

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
    fixtureStores.push(subject);
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
    fixtureStores.push(subject);
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
