import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';

// Settling is T3's third sidebar lane: a chat leaves the live list without being
// deleted or archived. The shelf used to be pure age with no way in or out, so
// these pin both the age rule and the two explicit overrides.

const DAY = 24 * 60 * 60 * 1000;

let vite: ViteDevServer;
let StoreCtor: new () => any;
let resolveSettled: (chat: unknown, status: string, active: boolean, now: number, touched: number) => boolean;
let canSettle: (status: string) => boolean;
let resolveStatus: (chat: Chat, live: boolean, active: boolean, obligation?: { state: string }) => string;

before(async () => {
  vite = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    logLevel: 'silent',
    server: { middlewareMode: true },
    appType: 'custom',
  });
  StoreCtor = (await vite.ssrLoadModule('/src/store/store.ts')).Store;
  const sidebar = await vite.ssrLoadModule('/src/components/SidebarV2.tsx');
  resolveSettled = sidebar.resolveSettled;
  canSettle = sidebar.canSettle;
  resolveStatus = sidebar.resolveStatus;
});

after(async () => { await vite.close(); });

function chat(over: Partial<Chat> = {}): Chat {
  return {
    id: 'tab-1',
    chatId: 'chat-1',
    sessionId: 'session-1',
    sessionProviderId: 'codex',
    title: 'Settle',
    titleLocked: true,
    group: null,
    cwd: '/tmp/workass-test',
    currentModelId: 'model-1',
    currentModeId: 'agent',
    providerId: 'codex',
    pending: false,
    messages: [],
    draft: '',
    ...over,
  } as Chat;
}

test('age files a quiet chat away on its own, but not before three days', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const quiet = chat();

  assert.equal(resolveSettled(quiet, 'ready', false, now, now - 2 * DAY), false);
  assert.equal(resolveSettled(quiet, 'ready', false, now, now - 4 * DAY), true);
});

test('a chat with no activity yet is never aged onto the shelf', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  // No messages means a zero timestamp, not "last touched in 1970".
  assert.equal(resolveSettled(chat(), 'ready', false, now, 0), false);
  assert.equal(resolveSettled(chat({ settled: 'settled' }), 'ready', false, now, 0), true);
});

test('the explicit overrides beat the age rule in both directions', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');

  // Filed by hand while still fresh.
  assert.equal(resolveSettled(chat({ settled: 'settled' }), 'ready', false, now, now - 1000), true);
  // Pulled back out: without the 'active' pin, age would re-file it instantly.
  assert.equal(resolveSettled(chat({ settled: 'active' }), 'ready', false, now, now - 40 * DAY), false);
});

test('nothing still alive, awaiting approval, parked, unread, or active can sit on the shelf', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const old = now - 40 * DAY;
  const filed = chat({ settled: 'settled' });

  for (const status of ['approval', 'working', 'parked']) {
    assert.equal(resolveSettled(filed, status, false, now, old), false, status);
  }
  assert.equal(resolveSettled(chat({ settled: 'settled', unread: true }), 'ready', false, now, old), false);
  assert.equal(resolveSettled(filed, 'ready', true, now, old), false);   // the chat you are reading
});

test('settle and un-settle store opposite overrides, and new work retires the shelf', () => {
  const store = new StoreCtor();
  const subject = chat();
  store.state.chats = [subject];
  store.state.activeId = 'tab-other';

  store.settleChat('tab-1', true);
  assert.equal(subject.settled, 'settled');
  store.settleChat('tab-1', false);
  assert.equal(subject.settled, 'active');

  store.settleChat('tab-1', true);
  store.onJobEvent({ type: 'start', job: { id: 'job-1', chatId: 'chat-1', tabId: 'tab-1', status: 'running' } as PublicJob });
  assert.equal(subject.settled, undefined);
});

test('a replayed start for an already-terminal job does not resurrect a settled chat', () => {
  const store = new StoreCtor();
  const subject = chat({
    settled: 'settled',
    messages: [{
      id: 'assistant-1',
      role: 'assistant',
      content: 'already finished',
      status: 'done',
      at: '2026-08-12T12:00:00Z',
      events: [],
      jobId: 'job-finished',
    }],
  });
  store.state.chats = [subject];
  store.state.activeId = 'tab-other';

  store.onJobEvent({
    type: 'start',
    job: { id: 'job-finished', chatId: 'chat-1', tabId: 'tab-1', status: 'running' } as PublicJob,
  });

  assert.equal(subject.messages[0].status, 'done');
  assert.equal(subject.settled, 'settled');
});

test('a chat with work in flight is not a settle target', () => {
  // T3's canSettle twin: what the partition refuses to classify as settled has
  // to be refused as a target too, or the row offers a click it will swallow.
  assert.equal(canSettle('working'), false);
  assert.equal(canSettle('approval'), false);
  // A finished or broken chat is filable — that is the usual "seen it, put it
  // away" gesture, and the acknowledgement below is what lets it land.
  for (const status of ['done', 'failed', 'ready']) assert.equal(canSettle(status), true, status);
});

test('attention from a terminal error can be acknowledged and settled', () => {
  const now = Date.parse('2026-08-10T12:00:00Z');
  const acknowledged = chat({ settled: 'settled' });

  assert.equal(canSettle('attention'), true, 'attention is terminal user-facing news, not live work');
  assert.equal(resolveSettled(acknowledged, 'attention', false, now, now - 1000), true);
  assert.equal(
    resolveStatus(acknowledged, false, false, { state: 'needs_input' }),
    'ready',
    'filing the chat acknowledges the attention pill until new work reactivates it',
  );

  for (const status of ['approval', 'working', 'parked']) {
    assert.equal(canSettle(status), false, status);
  }
});

test('filing a finished chat acknowledges it, so the click actually lands', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const store = new StoreCtor();
  const finished = chat({ unread: true });
  store.state.chats = [finished];
  store.state.activeId = 'tab-other';

  store.settleChat('tab-1', true);
  assert.equal(finished.unread, false);
  assert.equal(resolveSettled(finished, 'ready', false, now, now - 1000), true);

  // Un-settling is not an acknowledgement in reverse: it must never re-arm a
  // cue the user already cleared.
  store.settleChat('tab-1', false);
  assert.equal(finished.unread, false);
});
