import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import test, { after, before } from 'node:test';
import { createServer, type ViteDevServer } from 'vite';
import { createMachineRouter, projectRemoteEvent } from '../src/wire/machineRouter.ts';
import { tagId } from '../src/wire/machineIds.ts';
import type { MachineSocket } from '../src/wire/machineSocket.ts';
import type { ForkChildView, ForkListReply, ForkPointView, ForkRunView } from '../src/wire/types.ts';

// ── fixtures ────────────────────────────────────────────────────────────────

function child(partial: Partial<ForkChildView> & Pick<ForkChildView, 'childId'>): ForkChildView {
  return {
    index: 1, status: 'completed', usage: { input: 4, output: 1150, cacheRead: 99_700, cacheCreation: 11_300 },
    cacheHitShare: 0.898, turns: 2, wallMs: 9799, attempt: 1, transcriptRetained: true, attachments: [],
    ...partial,
  };
}
function forkPoint(partial: Partial<ForkPointView> = {}): ForkPointView {
  return {
    forkPointId: 'fp-1', name: 'brief-v12+ejemplos', chatId: 'chat-1', tabId: 'tab-1', providerId: 'p',
    status: 'ready', cwd: '/w', createdAt: '2026-10-15T20:49:33Z', afterMessageId: 'a-1',
    sealUsage: { input: 4, output: 8, cacheRead: 82_591, cacheCreation: 1197 }, sealWallMs: 5413,
    prefixSize: 41_927, sealTurns: 2, ...partial,
  };
}
function forkRun(children: ForkChildView[], partial: Partial<ForkRunView> = {}): ForkRunView {
  return {
    runId: 'fr-1', forkPointId: 'fp-1', chatId: 'chat-1', tabId: 'tab-1', status: 'running', concurrency: 2,
    retention: 'throwaway', retentionDays: 7, createdAt: '2026-10-15T20:49:52Z', counts: {},
    totals: { input: 0, output: 0, cacheRead: 0, cacheCreation: 0, cacheHitShare: 0 }, children, ...partial,
  };
}
const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

// ── module wiring: one Vite SSR graph so forks.ts and wire/api.ts share state ──

type ForkStoreShape = {
  start(): void;
  ensure(ref: { tabId: string; chatId: string }): void;
  chat(tabId: string): { points: ForkPointView[]; runs: ForkRunView[] } | undefined;
  summary(): ReadonlyArray<{ tabId: string; children: number }>;
  selected: { tabId: string; runId: string; rootId: string } | null;
  select(next: { tabId: string; chatId: string; runId: string; rootId: string }): void;
  points(tabId: string): ForkPointView[];
  slotChild(tabId: string, runId: string, rootId: string): ForkChildView | undefined;
  subscribe(cb: () => void): () => void;
  subscribePoints(cb: () => void): () => void;
  retryChild(ref: { tabId: string; chatId: string }, runId: string, childId: string): Promise<void>;
  cancelChild(ref: { tabId: string; chatId: string }, runId: string, childId: string): Promise<void>;
};
type ForkModule = { forkStore: ForkStoreShape };

let vite: ViteDevServer;
let setMachineRouter: (api: unknown) => void;
let appStore: {
  state: { toasts: Array<{ title: string; body: string }>; activeId: string | null };
  clearToastTimers(): void;
  bump(topic: string): void;
};

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  setMachineRouter = (await vite.ssrLoadModule('/src/wire/api.ts')).setMachineRouter;
  appStore = (await vite.ssrLoadModule('/src/store/store.ts')).store;
});
after(async () => { appStore?.clearToastTimers(); await vite.close(); });

// A fresh ForkStore per test: invalidate the module so its singleton is new.
async function freshForks(): Promise<ForkStoreShape> {
  const mod = vite.moduleGraph.getModuleById(fileURLToPath(new URL('../src/store/forks.ts', import.meta.url)));
  if (mod) vite.moduleGraph.invalidateModule(mod);
  return ((await vite.ssrLoadModule('/src/store/forks.ts')) as ForkModule).forkStore;
}

function fakeBridge(listReply: () => Promise<ForkListReply> | ForkListReply) {
  const calls: Record<string, unknown[][]> = {};
  let changed: ((payload: unknown) => void) | undefined;
  const record = (name: string, value: (...args: unknown[]) => unknown) => (...args: unknown[]) => {
    (calls[name] ??= []).push(args);
    return value(...args);
  };
  const api = {
    forkList: record('forkList', () => Promise.resolve(listReply())),
    forkRetryChild: record('forkRetryChild', () => Promise.resolve({ ok: false, error: 'child is still running' })),
    forkCancelChild: record('forkCancelChild', () => Promise.resolve({ ok: true })),
    forkTranscript: record('forkTranscript', () => Promise.resolve({ ok: true, entries: [] })),
    onForkChanged: (cb: (payload: unknown) => void) => { changed = cb; },
  };
  return { api, calls, emit: (payload: unknown) => changed?.(payload) };
}

function silenceConsole() {
  const seen: unknown[][] = [];
  const original = { warn: console.warn, error: console.error, log: console.log };
  console.warn = (...args: unknown[]) => { seen.push(args); };
  console.error = (...args: unknown[]) => { seen.push(args); };
  console.log = (...args: unknown[]) => { seen.push(args); };
  return { seen, restore: () => Object.assign(console, original) };
}

const ref = { tabId: 'tab-1', chatId: 'chat-1' };

// ── compatibility ───────────────────────────────────────────────────────────

test('a bridge without fork channels means no forks, silently', async () => {
  setMachineRouter({});
  const forks = await freshForks();
  const quiet = silenceConsole();
  try {
    forks.ensure(ref);
    await wait(20);
    assert.deepEqual(forks.chat('tab-1'), { points: [], runs: [] });
  } finally { quiet.restore(); }
  assert.equal(quiet.seen.length, 0, 'no console line for a missing capability');
});

test('an older daemon that rejects fork:list reads as no forks, with no toast, log or retry loop', async () => {
  const bridge = fakeBridge(() => Promise.reject(new Error('unknown channel fork:list')));
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  const toastsBefore = appStore.state.toasts.length;
  const quiet = silenceConsole();
  try {
    forks.ensure(ref);
    await wait(300);
  } finally { quiet.restore(); }
  assert.deepEqual(forks.chat('tab-1'), { points: [], runs: [] });
  assert.equal(bridge.calls.forkList?.length, 1, 'one attempt, no loop');
  assert.equal(quiet.seen.length, 0);
  assert.equal(appStore.state.toasts.length, toastsBefore);
});

// ── fetch + fork:changed ────────────────────────────────────────────────────

test('fork:changed refetches the chat, debounced, and markers re-render only when points move', async () => {
  let reply: ForkListReply = { ok: true, forkPoints: [forkPoint()], runs: [forkRun([child({ childId: 'c1', status: 'running', activity: 'Write /tmp/a' })])] };
  const bridge = fakeBridge(() => reply);
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  let fine = 0;
  let coarse = 0;
  forks.subscribe(() => { fine += 1; });
  forks.subscribePoints(() => { coarse += 1; });

  forks.ensure(ref);
  await wait(20);
  assert.equal(forks.chat('tab-1').runs.length, 1);
  assert.equal(coarse, 1, 'the first snapshot places the markers');
  const pointsRef = forks.points('tab-1');

  // Activity-only churn: three signals inside the debounce window → one fetch.
  reply = { ...reply, runs: [forkRun([child({ childId: 'c1', status: 'running', activity: 'Read /tmp/b' })])] };
  bridge.emit({ tabId: 'tab-1', chatId: 'chat-1' });
  bridge.emit({ tabId: 'tab-1', chatId: 'chat-1' });
  bridge.emit({ tabId: 'tab-1', chatId: 'chat-1' });
  await wait(250);
  assert.equal(bridge.calls.forkList?.length, 2, 'debounced to a single refetch');
  assert.equal(forks.chat('tab-1').runs[0].children[0].activity, 'Read /tmp/b');
  assert.ok(fine >= 2);
  assert.equal(coarse, 1, 'activity never re-renders the transcript markers');
  assert.equal(forks.points('tab-1'), pointsRef, 'points keep their reference');

  // A new fork point does move markers.
  reply = { ...reply, forkPoints: [forkPoint(), forkPoint({ forkPointId: 'fp-2', createdAt: '2026-10-15T21:00:00Z' })] };
  bridge.emit({ tabId: 'tab-1', chatId: 'chat-1' });
  await wait(250);
  assert.equal(coarse, 2);
  assert.equal(forks.points('tab-1').length, 2);
});

test('fork:changed for a chat this window never opened is ignored', async () => {
  const bridge = fakeBridge(() => ({ ok: true, forkPoints: [], runs: [] }));
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  forks.ensure(ref); // installs the subscription
  await wait(20);
  bridge.emit({ tabId: 'tab-other', chatId: 'chat-other' });
  await wait(250);
  assert.equal(bridge.calls.forkList?.length, 1);
});

test('a row resolves to the newest attempt of its chain', async () => {
  const bridge = fakeBridge(() => ({ ok: true, forkPoints: [forkPoint()], runs: [forkRun([
    child({ childId: 'c3', index: 3, status: 'cancelled' }),
    child({ childId: 'c3-r2', index: 3, attempt: 2, retryOf: 'c3' }),
  ])] }));
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  forks.ensure(ref);
  await wait(20);
  assert.equal(forks.slotChild('tab-1', 'fr-1', 'c3')?.childId, 'c3-r2');
});

// ── actions ─────────────────────────────────────────────────────────────────

test('retry and stop carry the chat ref and a fresh operationId; a refusal is toasted', async () => {
  const bridge = fakeBridge(() => ({ ok: true, forkPoints: [], runs: [] }));
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  const before = appStore.state.toasts.length;
  await forks.retryChild(ref, 'fr-1', 'c3-r2');
  await forks.retryChild(ref, 'fr-1', 'c3-r2');
  await forks.cancelChild(ref, 'fr-1', 'c2');
  const [first] = bridge.calls.forkRetryChild[0] as [Record<string, unknown>];
  const [second] = bridge.calls.forkRetryChild[1] as [Record<string, unknown>];
  assert.equal(first.tabId, 'tab-1');
  assert.equal(first.chatId, 'chat-1');
  assert.equal(first.runId, 'fr-1');
  assert.equal(first.childId, 'c3-r2');
  assert.match(String(first.operationId), /^fork-op-/);
  assert.notEqual(first.operationId, second.operationId, 'each click is its own operation');
  const [stop] = bridge.calls.forkCancelChild[0] as [Record<string, unknown>];
  assert.equal(stop.childId, 'c2');
  assert.match(String(stop.operationId), /^fork-op-/);
  const added = appStore.state.toasts.slice(before);
  assert.deepEqual(added.map((toast) => [toast.title, toast.body]), [
    ['No se pudo reintentar', 'child is still running'],
    ['No se pudo reintentar', 'child is still running'],
  ]);
  appStore.clearToastTimers();
});

// ── routing ─────────────────────────────────────────────────────────────────

function fakeLink(replies: Record<string, unknown> = {}) {
  const calls: Array<{ channel: string; args: unknown[] }> = [];
  const link = {
    invoke: async (channel: string, ...args: unknown[]) => { calls.push({ channel, args }); return replies[channel]; },
    on: () => () => {},
  } as unknown as MachineSocket;
  return { link, calls };
}

test('fork calls for a remote chat go to the machine that owns it, untagged', async () => {
  const remote = fakeLink({ 'fork:list': { ok: true, forkPoints: [], runs: [] }, 'fork:transcript': { ok: true, entries: [] } });
  const localCalls: unknown[][] = [];
  const local = { forkList: (...args: unknown[]) => { localCalls.push(args); return Promise.resolve({ ok: true }); } };
  const router = createMachineRouter({
    local: () => local as never,
    links: () => new Map([['m-far', remote.link]]),
    controlLinks: () => new Map([['m-far', remote.link]]),
    urgentLinks: () => new Map([['m-far', remote.link]]),
    subscribeRemote: () => {},
  }) as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  const tab = tagId('m-far', 'tab-9');
  const chat = tagId('m-far', 'chat-9');

  await router.forkList(tab, chat);
  assert.deepEqual(remote.calls[0], { channel: 'fork:list', args: [{ tabId: 'tab-9', chatId: 'chat-9' }] });
  await router.forkTranscript({ tabId: tab, chatId: chat, runId: 'fr-1', childId: 'c1' });
  assert.deepEqual(remote.calls[1], { channel: 'fork:transcript', args: [{ tabId: 'tab-9', chatId: 'chat-9', runId: 'fr-1', childId: 'c1' }] });
  await router.forkImage({ tabId: tab, chatId: chat, runId: 'fr-1', childId: 'c1', index: 3 });
  await router.forkCancelChild({ tabId: tab, chatId: chat, runId: 'fr-1', childId: 'c1', operationId: 'op-1' });
  await router.forkRetryChild({ tabId: tab, chatId: chat, runId: 'fr-1', childId: 'c1', operationId: 'op-2' });
  await router.forkRetain({ tabId: tab, chatId: chat, runId: 'fr-1', keep: true });
  assert.deepEqual(remote.calls.slice(2).map((call) => call.channel), ['fork:image', 'fork:cancel-child', 'fork:retry-child', 'fork:retain']);
  assert.equal(localCalls.length, 0, 'nothing for another machine reaches this one');

  await router.forkList('tab-local', 'chat-local');
  assert.deepEqual(localCalls, [['tab-local', 'chat-local']], 'an untagged chat stays local');
});

test('a remote fork:changed arrives with the owning machine tag, so it finds its chat', () => {
  const payload = projectRemoteEvent('onForkChanged', 'm-far', { tabId: 'tab-9', chatId: 'chat-9' }) as { tabId: string; chatId: string };
  assert.equal(payload.tabId, tagId('m-far', 'tab-9'));
  assert.equal(payload.chatId, tagId('m-far', 'chat-9'));
});

// ── summary + selection ─────────────────────────────────────────────────────

test('the summary is read at start; a chat with a running child gets its full list for the working clock', async () => {
  const bridge = fakeBridge(() => ({ ok: true, forkPoints: [forkPoint()], runs: [] }));
  let summaryCalls = 0;
  const api = {
    ...bridge.api,
    forkSummary: () => {
      summaryCalls += 1;
      return Promise.resolve({ ok: true, chats: [
        { chatId: 'chat-1', tabId: 'tab-1', forkPoints: 1, children: 3, counts: { running: 1, completed: 2 } },
        { chatId: 'chat-2', tabId: 'tab-2', forkPoints: 1, children: 2, counts: { completed: 2 } },
      ] });
    },
  };
  setMachineRouter(api);
  const forks = await freshForks();
  forks.start();
  await wait(30);
  assert.equal(summaryCalls, 1);
  assert.deepEqual(forks.summary().map((entry) => [entry.tabId, entry.children]), [['tab-1', 3], ['tab-2', 2]]);
  assert.deepEqual((bridge.calls.forkList ?? []).map((args) => args[0]), ['tab-1'], 'only the running chat is fetched in full');
  // Any fork:changed refreshes the summary (debounced) even for a chat never opened here.
  bridge.emit({ tabId: 'tab-2', chatId: 'chat-2' });
  bridge.emit({ tabId: 'tab-2', chatId: 'chat-2' });
  await wait(250);
  assert.equal(summaryCalls, 2);
});

test('a selected child survives the switch to its parent and ends when another chat becomes active', async () => {
  const bridge = fakeBridge(() => ({ ok: true, forkPoints: [forkPoint()], runs: [forkRun([child({ childId: 'c1' })])] }));
  setMachineRouter(bridge.api);
  const forks = await freshForks();
  const before = appStore.state.activeId;
  try {
    appStore.state.activeId = 'tab-other';
    forks.start();
    forks.select({ tabId: 'tab-1', chatId: 'chat-1', runId: 'fr-1', rootId: 'c1' });
    assert.equal(forks.selected?.rootId, 'c1');
    appStore.bump('app'); // unrelated repaint while the parent switch is pending
    assert.equal(forks.selected?.rootId, 'c1');
    appStore.state.activeId = 'tab-1';
    appStore.bump('app'); // the parent becomes active
    assert.equal(forks.selected?.rootId, 'c1');
    appStore.state.activeId = 'tab-2';
    appStore.bump('app'); // the user picked another chat
    assert.equal(forks.selected, null);
  } finally {
    appStore.state.activeId = before;
  }
});
