import assert from 'node:assert/strict';
import test from 'node:test';
import type { ForkChildView, ForkPointView, ForkRunView, ForkSummaryChat, SpawnedWorkItem } from '../src/wire/types.ts';
import { forkChildLabel } from '../src/fork-format.ts';
import {
  FORK_KID_PAGE, forkChatBadge, forkFlatKids, forkGroups, forkKidMeta, forkPage,
  forkSelectionAfterActive, forkSelectionVisible, forkSidebarOpen, forkSubsessionRow, forkSummaryForChat,
  formatForkShortDuration, mergeSubsessionRows, spawnSubsessionRow, subsessionBadge,
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

// ── one neutral subsession family (user, 2026-10-16) ─────────────────────────

function work(partial: Partial<SpawnedWorkItem> & Pick<SpawnedWorkItem, 'id'>): SpawnedWorkItem {
  return {
    id: 'sw1', taskId: 'sw1', tabId: 'tab-1', chatId: 'chat-1', providerId: 'mock',
    kind: 'subagent', label: 'opus-revisor', status: 'running',
    startedAt: '2026-10-15T20:49:55Z', updatedAt: '2026-10-15T20:49:55Z',
    ...partial,
  };
}

test('a fork child and a spawned thread carry the SAME neutral row shape: no fork field in spawn rows', () => {
  const groups = forkGroups({ points: [point()], runs: [run([child({ childId: 'f1', status: 'running', startedAt: t(0) })])] });
  const fork = forkSubsessionRow(
    forkFlatKids(groups)[0], { label: forkChildLabel({ label: 'revisor', index: 1 }), brand: 'claude' }, Date.parse(t(18)),
  );
  assert.deepEqual(
    { kind: fork.kind, label: fork.label, state: fork.state, word: fork.word, brand: fork.brand, wallMs: fork.wallMs, startedMs: fork.startedMs },
    { kind: 'fork', label: 'revisor', state: 'running', word: null, brand: 'claude', wallMs: null, startedMs: Date.parse(t(0)) },
  );
  assert.deepEqual(fork.fork, { runId: 'fr-1', rootId: 'f1' }, 'fork rows open the child view');

  const spawn = spawnSubsessionRow(work({}));
  assert.deepEqual(
    { kind: spawn.kind, label: spawn.label, state: spawn.state, word: spawn.word, key: spawn.key, wallMs: spawn.wallMs, startedMs: spawn.startedMs },
    { kind: 'spawn', label: 'opus-revisor', state: 'running', word: null, key: 'spawn:sw1', wallMs: null, startedMs: Date.parse('2026-10-15T20:49:55Z') },
  );
  assert.equal(spawn.fork, undefined, 'spawn rows reveal the rail; they never pose as fork children');
});

test('merged list: fork rows ride their run stamp, oldest first, stable across runs', () => {
  const groups = forkGroups({ points: [point()], runs: [run([
    child({ childId: 'f1', status: 'running', startedAt: t(10), label: 'rama' }),
    child({ childId: 'f0', startedAt: t(2), wallMs: 5000 }),
  ])] });
  const forkRows = forkFlatKids(groups)
    .map((kid) => forkSubsessionRow(kid, { label: kid.child.childId, brand: null }, Date.parse(t(60))));
  const spawnRows = [
    work({ id: 'sw-early', startedAt: t(1) }),
    work({ id: 'sw-late', status: 'exited', startedAt: t(30), finishedAt: t(40), label: 'explorar' }),
  ].map((item) => spawnSubsessionRow(item));
  const merged = mergeSubsessionRows(forkRows, spawnRows);
  assert.deepEqual(merged.map((row) => row.key), ['fr-1:f1', 'fr-1:f0', 'spawn:sw-early', 'spawn:sw-late'],
    'the run was created before every spawned thread; its children keep run order');
  assert.deepEqual(merged.map((row) => row.kind), ['fork', 'fork', 'spawn', 'spawn']);
  assert.deepEqual(merged.map((row) => row.state), ['running', 'completed', 'running', 'completed']);
  assert.equal(merged[1].wallMs, 5000);
  assert.equal(merged[3].wallMs, 10_000);
  assert.deepEqual(mergeSubsessionRows([], []), []);
});

test('ordering is stable: a queued fork child waits at the run stamp, never leaps to the top', () => {
  // Concurrency 1: a starts (stamped), b and c queue (no stamp), and an agent
  // thread started before the run itself. The queued children must not outrank
  // the agent, and must not jump again once they start.
  const runAt = t(20);
  const list = (kids: ForkChildView[]) => mergeSubsessionRows(
    forkFlatKids(forkGroups({ points: [point()], runs: [run(kids, { concurrency: 1, createdAt: runAt })] }))
      .map((kid) => forkSubsessionRow(kid, { label: kid.child.childId, brand: null }, Date.parse(t(60)))),
    [spawnSubsessionRow(work({ id: 'sw-early', startedAt: t(1) }))],
  );
  const before = list([
    child({ childId: 'a', status: 'running', startedAt: t(21) }),
    child({ childId: 'b', status: 'queued', index: 2 }),
    child({ childId: 'c', status: 'queued', index: 3 }),
  ]);
  assert.deepEqual(before.map((row) => row.key), ['spawn:sw-early', 'fr-1:a', 'fr-1:b', 'fr-1:c']);
  assert.equal(before[2].wallMs, null, 'a queued row has no time claim');

  const after = list([
    child({ childId: 'a', status: 'running', startedAt: t(21) }),
    child({ childId: 'b', status: 'running', startedAt: t(25), index: 2 }),
    child({ childId: 'c', status: 'queued', index: 3 }),
  ]);
  assert.deepEqual(after.map((row) => row.key), before.map((row) => row.key),
    'getting a start stamp never moves a row');
  assert.equal(after[2].startedMs, Date.parse(t(25)), 'the stamp is for the running row\u2019s live clock only');
});

test('spawn rows speak the same outcome words the fork rows use', () => {
  const failed = spawnSubsessionRow(work({ status: 'failed', startedAt: t(0), finishedAt: t(3) }));
  assert.deepEqual(
    { state: failed.state, word: failed.word, wallMs: failed.wallMs },
    { state: 'failed', word: 'falló', wallMs: 3000 },
  );
  assert.equal(spawnSubsessionRow(work({ status: 'stopped', startedAt: t(0), finishedAt: t(1) })).word, 'detenido');
  assert.equal(spawnSubsessionRow(work({ status: 'orphaned', startedAt: t(0), finishedAt: t(1) })).state, 'failed');
  assert.equal(spawnSubsessionRow(work({ status: 'cancelled', startedAt: t(0), finishedAt: t(1) })).word, 'detenido');
  assert.equal(spawnSubsessionRow(work({ status: 'exited', startedAt: t(0), finishedAt: t(1) })).state, 'completed');
  assert.equal(spawnSubsessionRow(work({ label: '   ' })).label, 'Subsesión', 'daemon label wins, blank label falls back');
});

test('parent chip: the whole family counts together — fork facts plus spawned threads, shells excluded', () => {
  const fork = forkChatBadge(
    { chatId: 'chat-1', tabId: 'tab-1', forkPoints: 1, children: 2, counts: { running: 1 } },
    undefined,
  );
  const badge = subsessionBadge(fork, [
    work({ id: 'sw-a', status: 'running', startedAt: t(1) }),
    work({ id: 'sw-b', status: 'exited', startedAt: t(2) }),
    work({ id: 'sw-c', status: 'running', startedAt: t(0) }),
    work({ id: 'sw-d', kind: 'bash', status: 'running', startedAt: t(3) }),
  ]);
  assert.deepEqual(badge, { points: 1, children: 5, running: 3, since: Date.parse(t(0)) },
    'agents join the count and date the earliest running start; a running shell is work, not a subsession');
  assert.deepEqual(
    subsessionBadge(null, [work({ id: 'sw-a', status: 'running', startedAt: t(1) })]),
    { points: 0, children: 1, running: 1, since: Date.parse(t(1)) },
    'a chat whose agents spawned only plain threads gets the chip too',
  );
  assert.equal(subsessionBadge(null, []), null, 'no family, no chip');
  assert.equal(subsessionBadge(null, [work({ id: 'sw-x', kind: 'bash', status: 'running' })]), null);
});
