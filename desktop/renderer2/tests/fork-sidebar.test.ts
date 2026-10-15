import assert from 'node:assert/strict';
import test from 'node:test';
import type { ForkChildView, ForkPointView, ForkRunView, ForkSummaryChat } from '../src/wire/types.ts';
import {
  FORK_KID_PAGE, forkChatBadge, forkFlatKids, forkGroups, forkKidMeta, forkMarkerMeta, forkPage,
  forkSelectionAfterActive, forkSelectionVisible, forkSidebarOpen, forkSummaryForChat, formatForkShortDuration,
} from '../src/fork-sidebar.ts';

function child(partial: Partial<ForkChildView> & Pick<ForkChildView, 'childId'>): ForkChildView {
  return {
    index: 1, status: 'completed', usage: { input: 4, output: 1150, cacheRead: 99_700, cacheCreation: 11_300 },
    cacheHitShare: 0.898, turns: 2, wallMs: 9799, attempt: 1, transcriptRetained: true, attachments: [],
    ...partial,
  };
}
function point(partial: Partial<ForkPointView> = {}): ForkPointView {
  return {
    forkPointId: 'fp-1', name: 'brief-v12+ejemplos', chatId: 'chat-1', tabId: 'tab-1', providerId: 'p',
    status: 'ready', cwd: '/w', createdAt: '2026-10-15T20:49:33Z',
    sealUsage: { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 }, sealWallMs: 0, prefixSize: 41_927, sealTurns: 2,
    ...partial,
  };
}
function run(children: ForkChildView[], partial: Partial<ForkRunView> = {}): ForkRunView {
  return {
    runId: 'fr-1', forkPointId: 'fp-1', chatId: 'chat-1', tabId: 'tab-1', status: 'completed', concurrency: 2,
    retention: 'throwaway', retentionDays: 7, createdAt: '2026-10-15T20:49:52Z', counts: {},
    totals: { input: 12, output: 3400, cacheRead: 299_100, cacheCreation: 35_600, cacheHitShare: 0.893 },
    children, ...partial,
  };
}
const t = (s: number) => new Date(Date.parse('2026-10-15T20:50:00Z') + s * 1000).toISOString();

test('groups follow fork points in createdAt order; children span runs and show their latest attempt', () => {
  const groups = forkGroups({
    points: [point({ forkPointId: 'fp-2', name: 'later', createdAt: '2026-10-15T21:00:00Z' }), point()],
    runs: [
      run([child({ childId: 'b1' })], { runId: 'fr-2', createdAt: '2026-10-15T20:55:00Z' }),
      run([
        child({ childId: 'c1' }), child({ childId: 'c3', index: 3, status: 'cancelled' }),
        child({ childId: 'c3-r2', index: 3, attempt: 2, retryOf: 'c3' }),
      ]),
      run([child({ childId: 'z1' })], { runId: 'fr-3', forkPointId: 'fp-2' }),
    ],
  });
  assert.deepEqual(groups.map((group) => group.point.forkPointId), ['fp-1', 'fp-2']);
  assert.deepEqual(groups[0].kids.map((kid) => [kid.runId, kid.rootId, kid.child.childId]), [
    ['fr-1', 'c1', 'c1'], ['fr-1', 'c3', 'c3-r2'], ['fr-2', 'b1', 'b1'],
  ]);
  assert.deepEqual(groups[1].kids.map((kid) => kid.child.childId), ['z1']);
  assert.deepEqual(forkGroups(undefined), []);
});

test('sidebar list: subsessions sit directly under the session, oldest fork point first', () => {
  const groups = forkGroups({ points: [point({ forkPointId: 'p2', createdAt: t(5) }), point({ forkPointId: 'p1', createdAt: t(0) })], runs: [
    run([child({ childId: 'b1' })], { forkPointId: 'p2' }), run([child({ childId: 'a1' }), child({ childId: 'a2' })], { forkPointId: 'p1' }),
  ] });
  const flat = forkFlatKids(groups);
  assert.deepEqual(flat.map((kid) => kid.child.childId), ['a1', 'a2', 'b1']);
  assert.deepEqual(flat.map((kid) => kid.point.forkPointId), ['p1', 'p1', 'p2']);
  assert.deepEqual(forkFlatKids([]), []);
});

test('transcript marker meta: count + cache when settled, what is moving otherwise', () => {
  const done = forkGroups({ points: [point()], runs: [run([child({ childId: 'a' }), child({ childId: 'b' }), child({ childId: 'c' })])] })[0];
  assert.equal(forkMarkerMeta(done), '3 hijos · 89 % caché');
  const live = forkGroups({ points: [point()], runs: [run([child({ childId: 'a' }), child({ childId: 'b', status: 'running' })])] })[0];
  assert.equal(forkMarkerMeta(live), '2 hijos · 1 en curso');
  assert.equal(forkMarkerMeta({ point: point(), runs: [], kids: [] }), 'prefijo 41,9 k tok');
});

test('child row facts: live time while running, wall time once settled, a muted word for failures', () => {
  const now = Date.parse(t(18));
  assert.deepEqual(forkKidMeta(child({ childId: 'r', status: 'running', startedAt: t(0) }), now), { state: 'running', word: null, time: '18s' });
  assert.deepEqual(forkKidMeta(child({ childId: 'd' }), now), { state: 'completed', word: null, time: '10s' });
  assert.deepEqual(forkKidMeta(child({ childId: 'f', status: 'failed', wallMs: 7000 }), now), { state: 'failed', word: 'falló', time: '7s' });
  assert.deepEqual(forkKidMeta(child({ childId: 'x', status: 'cancelled', wallMs: 800 }), now), { state: 'cancelled', word: 'detenido', time: '1s' });
  assert.deepEqual(forkKidMeta(child({ childId: 'q', status: 'queued' }), now), { state: 'queued', word: null, time: null });
});

test('short durations match the sidebar clock', () => {
  assert.equal(formatForkShortDuration(9799), '10s');
  assert.equal(formatForkShortDuration(0), '0s');
  assert.equal(formatForkShortDuration(59_400), '59s');
  assert.equal(formatForkShortDuration(65_000), '1m');
  assert.equal(formatForkShortDuration(3_700_000), '1h');
  assert.equal(formatForkShortDuration(undefined), '0s');
});

test('show-more paging: the first twelve, then everything that is left', () => {
  const kids = Array.from({ length: 120 }, (_, i) => i);
  assert.equal(FORK_KID_PAGE, 12);
  const first = forkPage(kids, false);
  assert.deepEqual([first.visible.length, first.hidden, first.visible[11]], [12, 108, 11]);
  const all = forkPage(kids, true);
  assert.deepEqual([all.visible.length, all.hidden], [120, 0]);
  const few = forkPage(kids.slice(0, 12), false);
  assert.deepEqual([few.visible.length, few.hidden], [12, 0], 'exactly a page needs no button');
});

test('parent badge: a full snapshot wins over the summary; retries count once; no forks → no badge', () => {
  const summary: ForkSummaryChat = { chatId: 'chat-1', tabId: 'tab-1', forkPoints: 1, children: 3, counts: { running: 1, completed: 2 } };
  assert.deepEqual(forkChatBadge(summary, undefined), { points: 1, children: 3, running: 1, since: 0 });
  const list = { points: [point()], runs: [
    run([
      child({ childId: 'a', startedAt: t(2) }), child({ childId: 'b', status: 'running', startedAt: t(1) }),
      child({ childId: 'c', status: 'cancelled', startedAt: t(0) }), child({ childId: 'c-r2', attempt: 2, retryOf: 'c', status: 'running', startedAt: t(5) }),
    ]),
    run([child({ childId: 'old', startedAt: '2026-10-15T19:00:00Z' })], { runId: 'fr-0', createdAt: '2026-10-15T19:00:00Z' }),
  ] };
  assert.deepEqual(forkChatBadge(summary, list), { points: 1, children: 4, running: 2, since: Date.parse(t(1)) },
    'since = earliest start in a run that still runs; the cancelled attempt and settled runs do not date it');
  assert.equal(forkChatBadge(undefined, undefined), null);
  assert.equal(forkChatBadge({ ...summary, forkPoints: 0 }, { points: [], runs: [] }), null);
});

test('summary lookup matches the tab first, then the conversation id', () => {
  const entries: ForkSummaryChat[] = [
    { chatId: 'chat-a', tabId: 'tab-a', forkPoints: 1, children: 1, counts: {} },
    { chatId: 'chat-b', tabId: 'tab-b', forkPoints: 2, children: 5, counts: {} },
  ];
  assert.equal(forkSummaryForChat(entries, { id: 'tab-b' })?.children, 5);
  assert.equal(forkSummaryForChat(entries, { id: 'renamed-tab', chatId: 'chat-a' })?.children, 1);
  assert.equal(forkSummaryForChat(entries, { id: 'nope', chatId: 'nope' }), undefined);
});

test('disclosure: collapsed by default, auto-open for the active chat with a running child, explicit choice wins', () => {
  assert.equal(forkSidebarOpen(undefined, false, true), false);
  assert.equal(forkSidebarOpen(undefined, true, false), false);
  assert.equal(forkSidebarOpen(undefined, true, true), true);
  assert.equal(forkSidebarOpen(false, true, true), false);
  assert.equal(forkSidebarOpen(true, false, false), true);
});

test('selection routing: survives the switch to its parent, ends on any other chat', () => {
  const selected = { tabId: 'parent', chatId: 'chat-p', runId: 'fr-1', rootId: 'c1' };
  assert.equal(forkSelectionAfterActive(selected, 'other', 'other'), selected, 'unrelated repaint keeps it');
  assert.equal(forkSelectionAfterActive(selected, 'other', 'parent'), selected, 'the pending switch to the parent lands');
  assert.equal(forkSelectionAfterActive(selected, 'parent', 'other'), null, 'picking another chat leaves the child view');
  assert.equal(forkSelectionAfterActive(null, 'a', 'b'), null);
  assert.equal(forkSelectionVisible(selected, 'parent'), true);
  assert.equal(forkSelectionVisible(selected, 'other'), false, 'shown only while its parent is the active chat');
  assert.equal(forkSelectionVisible(null, 'parent'), false);
});
