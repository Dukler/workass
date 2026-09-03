import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import { createServer, type ViteDevServer } from 'vite';
import type { Chat } from '../src/store/types.ts';
import type { PublicJob } from '../src/wire/types.ts';

// Settling is T3's third sidebar lane: a chat leaves the live list without being
// deleted. After five days there it moves into the searchable archive, so these
// pin the age rules, explicit overrides, and archive ordering together.

const DAY = 24 * 60 * 60 * 1000;

let vite: ViteDevServer;
let StoreCtor: new () => any;
let resolveSettled: (chat: unknown, status: string, active: boolean, now: number, touched: number) => boolean;
let resolveArchived: (chat: unknown, status: string, now: number, touched: number) => boolean;
let lastTouchedAt: (chat: Chat) => number;
let orderSearchRows: (rows: readonly any[]) => any[];
let orderSidebarRows: (rows: readonly any[]) => any[];
let partitionSidebarRows: (rows: readonly any[]) => { cards: any[]; tail: any[]; archived: any[] };
let canDropSidebarRow: (
  draggedId: string | null | undefined,
  draggedSection: 'live' | 'settled' | 'archived' | null | undefined,
  targetId: string,
  targetSection: 'live' | 'settled' | 'archived',
) => boolean;
let canSettle: (status: string) => boolean;
let resolveStatus: (chat: Chat, live: boolean, active: boolean, obligation?: { state: string }) => string;
let isFullSizeSidebarRow: (settled: boolean, archived: boolean) => boolean;

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
  resolveArchived = sidebar.resolveArchived;
  lastTouchedAt = sidebar.lastTouchedAt;
  orderSearchRows = sidebar.orderSearchRows;
  orderSidebarRows = sidebar.orderSidebarRows;
  partitionSidebarRows = sidebar.partitionSidebarRows;
  canDropSidebarRow = sidebar.canDropSidebarRow;
  canSettle = sidebar.canSettle;
  resolveStatus = sidebar.resolveStatus;
  isFullSizeSidebarRow = sidebar.isFullSizeSidebarRow;
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

test('a chat archives after five days on the settled shelf', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const explicit = chat({ settled: 'settled', settledAt: now - 4 * DAY });

  assert.equal(resolveArchived(explicit, 'ready', now, now - 40 * DAY), false);
  explicit.settledAt = now - 5 * DAY;
  assert.equal(resolveArchived(explicit, 'ready', now, now - 40 * DAY), true);

  // Automatic filing starts at day three, then gets the same five days on the
  // shelf. The chat therefore archives at day eight since activity.
  assert.equal(resolveArchived(chat(), 'ready', now, now - 7 * DAY), false);
  assert.equal(resolveArchived(chat(), 'ready', now, now - 8 * DAY), true);
});

test('settled chats without settledAt use their last activity as the archive lower bound', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');

  assert.equal(resolveArchived(chat({ settled: 'settled' }), 'ready', now, now - 4 * DAY), false);
  assert.equal(resolveArchived(chat({ settled: 'settled' }), 'ready', now, now - 5 * DAY), true);
  assert.equal(resolveArchived(chat({ settled: 'settled' }), 'ready', now, 0), true);
});

test('metadata-only old chats stay automatically settled without resident messages', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const subject = chat({
    lastActivityAt: now - 4 * DAY,
    messages: [],
    messageCount: 12,
    historyComplete: false,
  });

  const touched = lastTouchedAt(subject);
  assert.equal(touched, now - 4 * DAY);
  assert.equal(resolveSettled(subject, 'ready', false, now, touched), true);
  assert.equal(resolveArchived(subject, 'ready', now, touched), false);
});

test('search keeps archived matches after ordinary rows and preserves manual order inside each section', () => {
  const row = (id: string, archived: boolean, order: number, card = false) => ({
    chat: { id }, archived, order, card, status: 'ready', settled: archived, touched: 0,
  });
  const ordered = orderSearchRows([
    row('archived-top', true, 0),
    row('shelved-bottom', false, 3),
    row('archived-first', true, 2),
    row('live', false, 1, true),
  ]);

  assert.deepEqual(ordered.map((item) => item.chat.id), ['live', 'shelved-bottom', 'archived-top', 'archived-first']);
});

test('dragging ignores machine ownership but preserves the lifecycle sections', () => {
  assert.equal(canDropSidebarRow('remote-live', 'live', 'local-live', 'live'), true);
  assert.equal(canDropSidebarRow('local-live', 'live', 'remote-live', 'live'), true);
  assert.equal(canDropSidebarRow('remote-settled', 'settled', 'local-live', 'live'), false);
  assert.equal(canDropSidebarRow('local-archived', 'archived', 'remote-working', 'live'), false);
  assert.equal(canDropSidebarRow('same', 'live', 'same', 'live'), false);
  assert.equal(canDropSidebarRow(null, null, 'target', 'live'), false);
});

test('ordinary sidebar rows follow persisted manual order instead of status or recency', () => {
  const ordered = orderSidebarRows([
    { chat: { id: 'done-new' }, order: 2, status: 'done', touched: 900 },
    { chat: { id: 'working' }, order: 1, status: 'working', touched: 800 },
    { chat: { id: 'ready-old' }, order: 0, status: 'ready', touched: 100 },
  ]);

  assert.deepEqual(ordered.map((item) => item.chat.id), ['ready-old', 'working', 'done-new']);
});

test('normal browsing keeps archived rows out of the live list and restores the settled shelf', () => {
  const row = (id: string, settled: boolean, archived: boolean, order: number) => ({
    chat: { id }, settled, archived, order, card: !settled && !archived,
    status: 'ready', touched: 0,
  });
  const partitioned = partitionSidebarRows([
    row('archived', true, true, 0),
    row('remote-live', false, false, 1),
    row('settled', true, false, 2),
    row('local-live', false, false, 3),
  ]);

  assert.deepEqual(partitioned.cards.map((item) => item.chat.id), ['remote-live', 'local-live']);
  assert.deepEqual(partitioned.tail.map((item) => item.chat.id), ['settled']);
  assert.deepEqual(partitioned.archived.map((item) => item.chat.id), ['archived']);
});

test('nothing still alive, awaiting approval, parked, or unread can sit on the shelf', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const old = now - 40 * DAY;
  const filed = chat({ settled: 'settled' });

  for (const status of ['approval', 'working', 'parked']) {
    assert.equal(resolveSettled(filed, status, false, now, old), false, status);
  }
  assert.equal(resolveSettled(chat({ settled: 'settled', unread: true }), 'ready', false, now, old), false);
});

test('selecting a settled thread leaves it compact and in the same shelf', () => {
  const now = Date.parse('2026-07-25T12:00:00Z');
  const old = now - 4 * DAY;
  const automatic = chat();
  const explicit = chat({ settled: 'settled', settledAt: now - DAY });

  for (const subject of [automatic, explicit]) {
    assert.equal(resolveSettled(subject, 'ready', false, now, old), true);
    const afterSelection = resolveSettled(subject, 'ready', true, now, old);
    assert.equal(afterSelection, true, 'selection alone cannot reactivate or promote the thread');
    assert.equal(isFullSizeSidebarRow(afterSelection, false), false);
  }
});

test('every ordinary thread stays full-size until it is settled', () => {
  assert.equal(isFullSizeSidebarRow(false, false), true);
  assert.equal(isFullSizeSidebarRow(true, false), false);
  assert.equal(isFullSizeSidebarRow(false, true), false);
});

test('settle and un-settle store opposite overrides, and new work retires the shelf', () => {
  const store = new StoreCtor();
  const subject = chat();
  store.state.chats = [subject];
  store.state.activeId = 'tab-other';

  const beforeSettle = Date.now();
  store.settleChat('tab-1', true);
  assert.equal(subject.settled, 'settled');
  assert.ok(subject.settledAt >= beforeSettle && subject.settledAt <= Date.now());
  store.settleChat('tab-1', false);
  assert.equal(subject.settled, 'active');
  assert.equal(subject.settledAt, undefined);

  store.settleChat('tab-1', true);
  store.onJobEvent({ type: 'start', job: { id: 'job-1', chatId: 'chat-1', tabId: 'tab-1', status: 'running' } as PublicJob });
  assert.equal(subject.settled, undefined);
  assert.equal(subject.settledAt, undefined);
});

test('new work resets a manual reactivation so the lifecycle can start fresh', () => {
  const store = new StoreCtor();
  const subject = chat({ settled: 'active' });
  store.state.chats = [subject];
  store.state.activeId = 'tab-other';

  store.onJobEvent({ type: 'start', job: { id: 'job-reactivated', chatId: 'chat-1', tabId: 'tab-1', status: 'running' } as PublicJob });

  assert.equal(subject.settled, undefined);
  assert.equal(subject.settledAt, undefined);
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
