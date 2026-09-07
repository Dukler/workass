import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { StateDigestChat } from '../src/wire/types.ts';

let vite: ViteDevServer;
let StoreCtor: new () => any;
let digestChatSessionDiverged: (chat: Chat, digest: StateDigestChat) => boolean;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  const loaded = await vite.ssrLoadModule('/src/store/store.ts');
  StoreCtor = loaded.Store;
  digestChatSessionDiverged = loaded.digestChatSessionDiverged;
});

after(async () => { await vite.close(); });

function runningChat(): Chat {
  return {
    id: 'tab-live', chatId: 'chat-live', actorRevision: 10,
    presentationRevision: 3,
    agentQueueRevision: 4, runtimeControlRevision: 5,
    providerId: 'codex', currentModelId: 'gpt-test', currentModeId: 'agent',
    title: 'live', titleLocked: true, messages: [{
      id: 'assistant-live', role: 'assistant', content: 'partial', status: 'running',
      at: null, jobId: 'job-live', events: [],
    }], draft: '',
  } as Chat;
}

function digest(overrides: Partial<StateDigestChat> = {}): StateDigestChat {
  return {
    tabId: 'tab-live', chatId: 'chat-live', actorRevision: 11,
    presentationRevision: 3,
    runningJobId: 'job-live', lastMessageId: 'assistant-live', messageCount: 1,
    queueLen: 0, queueHeadId: null, agentQueueRevision: 4,
    runtimeControlRevision: 5, providerId: 'codex', currentModelId: 'gpt-test',
    currentModeId: 'agent', pendingPermissionIds: [], ...overrides,
  };
}

test('stream-only actor revisions do not request a full session hydration', () => {
  const chat = runningChat();
  assert.equal(digestChatSessionDiverged(chat, digest({ actorRevision: 999 })), false);
  assert.equal(digestChatSessionDiverged(chat, digest({ presentationRevision: 4 })), true);
  assert.equal(digestChatSessionDiverged(chat, digest({ runningJobId: null })), true);
});

test('daemon liveness answers without waiting for a busy chat-state digest', async () => {
  const previousWindow = (globalThis as any).window;
  let releaseDigest: ((value: unknown) => void) | undefined;
  let metaCalls = 0;
  let digestCalls = 0;
  (globalThis as any).window = {
    api: {
      appMeta: async () => { metaCalls += 1; return { version: 'test' }; },
      stateDigest: () => {
        digestCalls += 1;
        return new Promise((resolve) => { releaseDigest = resolve; });
      },
    },
  };
  try {
    const subject = new StoreCtor();
    const result = await subject.pingConnection();
    assert.deepEqual(result, { version: 'test' });
    assert.equal(metaCalls, 1);
    assert.equal(digestCalls, 1);
    releaseDigest?.({});
  } finally {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  }
});


test('metadata-only idle chats converge after hydration instead of reloading on every heartbeat', () => {
  const subject = new StoreCtor();
  const mirror = { version: 1, activeId: 'tab-other', chats: [{
    id: 'tab-live', chatId: 'chat-live', actorRevision: 11, presentationRevision: 3,
    agentQueueRevision: 4, runtimeControlRevision: 5, providerId: 'codex',
    currentModelId: 'gpt-test', currentModeId: 'agent', title: 'idle',
    messages: [], messageCount: 40, historyComplete: false,
  }] };
  const restored = subject.fromMirror(mirror).chats[0];
  const idleDigest = digest({ runningJobId: null, lastMessageId: 'omitted-last-row', messageCount: 40 });
  for (let heartbeat = 0; heartbeat < 10; heartbeat++) {
    assert.equal(digestChatSessionDiverged(restored, idleDigest), false);
  }
  assert.equal(digestChatSessionDiverged(restored, { ...idleDigest, messageCount: 42 }), true);
  assert.equal(digestChatSessionDiverged(restored, { ...idleDigest, runningJobId: 'new-job' }), true);
  assert.equal(digestChatSessionDiverged(restored, { ...idleDigest, presentationRevision: 4 }), true);
});


test('refresh requests merge while an earlier reconciliation is waiting', async () => {
  const subject = new StoreCtor();
  const tasks: (() => Promise<void>)[] = [];
  const batches: Set<string>[] = [];
  subject.queueAgentRefresh = (_reason: string, task: () => Promise<void>) => tasks.push(task);
  subject.runScopedSync = async (scopes: Set<string>) => { batches.push(scopes); };
  subject.scheduleScopedSync(['session']);
  await new Promise((resolve) => setTimeout(resolve, 350));
  subject.scheduleScopedSync(['permissions']);
  await new Promise((resolve) => setTimeout(resolve, 350));
  subject.scheduleScopedSync(['session', 'catalog']);
  await new Promise((resolve) => setTimeout(resolve, 350));
  assert.equal(tasks.length, 1, 'slow hydration must not accumulate redundant full-session reads');
  await tasks.shift()!();
  assert.deepEqual([...batches[0]].sort(), ['catalog', 'permissions', 'session']);
  subject.scheduleScopedSync(['session']);
  await new Promise((resolve) => setTimeout(resolve, 350));
  assert.equal(tasks.length, 1, 'a later change still gets its reconciliation');
  await tasks.shift()!();
});
