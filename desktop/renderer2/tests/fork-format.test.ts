import assert from 'node:assert/strict';
import test from 'node:test';
import type { ForkChildView } from '../src/wire/types.ts';
import {
  collapseForkRetries, forkCacheBar, forkCacheShare, forkChildElapsedMs, forkChildLabel, forkInputTotal,
  forkRunSummary, forkRunWallMs, forkStatusWord, forkTokenTotal, forkTurnsLabel,
  formatForkClock, formatForkDuration, formatForkPercent, formatForkTokens, splitForkActivity,
} from '../src/fork-format.ts';

function child(partial: Partial<ForkChildView> & Pick<ForkChildView, 'childId'>): ForkChildView {
  return {
    index: 1, status: 'completed', usage: { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 },
    cacheHitShare: 0, turns: 0, wallMs: 0, attempt: 1, transcriptRetained: true, attachments: [],
    ...partial,
  };
}

// Measured acceptance run (mock data block): hijo 1 = in 4 · out 1,15 k · read 99,7 k · write 11,3 k.
const H1 = { input: 4, output: 1150, cacheRead: 99_700, cacheCreation: 11_300 };

test('row tokens include output; "de entrada" and the cache share exclude it', () => {
  assert.equal(forkTokenTotal(H1), 112_154);
  assert.equal(forkInputTotal(H1), 111_004);
  assert.equal(formatForkTokens(forkTokenTotal(H1)), '112 k');
  assert.equal(formatForkTokens(forkInputTotal(H1)), '111 k');
  assert.equal(formatForkPercent(forkCacheShare(H1)), '90 %');
  // The daemon's rounded share wins over a local recomputation.
  assert.equal(forkCacheShare(H1, 0.884), 0.884);
  assert.equal(forkCacheShare({ input: 0, output: 9, cacheRead: 0, cacheCreation: 0 }), 0, 'no input → 0, never NaN');
  assert.equal(forkTokenTotal(undefined), 0);
});

test('token counts read as Spanish compact numbers', () => {
  assert.equal(formatForkTokens(4), '4');
  assert.equal(formatForkTokens(999), '999');
  assert.equal(formatForkTokens(1150), '1,2 k');
  assert.equal(formatForkTokens(11_300), '11,3 k');
  assert.equal(formatForkTokens(11_000), '11 k', 'a trailing ,0 is dropped');
  assert.equal(formatForkTokens(44_800), '44,8 k');
  assert.equal(formatForkTokens(99_700), '99,7 k');
  assert.equal(formatForkTokens(99_960), '100 k', 'rounding never prints "100,0 k"');
  assert.equal(formatForkTokens(339_800), '340 k');
  assert.equal(formatForkTokens(999_600), '1 M', 'never "1000 k"');
  assert.equal(formatForkTokens(1_240_000), '1,2 M');
  assert.equal(formatForkTokens(Number.NaN), '0');
  assert.equal(formatForkTokens(-5), '0');
});

test('percent and duration formatting', () => {
  assert.equal(formatForkPercent(0.8987), '90 %');
  assert.equal(formatForkPercent(1.4), '100 %');
  assert.equal(formatForkPercent(Number.NaN), '0 %');
  assert.equal(formatForkDuration(400), '0,4 s');
  assert.equal(formatForkDuration(800), '0,8 s');
  assert.equal(formatForkDuration(9799), '10 s');
  assert.equal(formatForkDuration(25_000), '25 s');
  assert.equal(formatForkDuration(65_000), '1 min 5 s');
  assert.equal(formatForkDuration(120_000), '2 min');
  assert.equal(formatForkDuration(3_840_000), '1 h 4 min');
  assert.equal(formatForkDuration(3_600_000), '1 h');
});

test('clock stamps are local HH:MM and tolerate garbage', () => {
  const at = new Date(2026, 9, 15, 16, 42, 9).toISOString();
  assert.equal(formatForkClock(at), '16:42');
  assert.equal(formatForkClock(''), '');
  assert.equal(formatForkClock('not a date'), '');
  assert.equal(formatForkClock(undefined), '');
});

test('cache bar segments are shares of input and agree with the label', () => {
  const bar = forkCacheBar(H1);
  assert.equal(bar.empty, false);
  assert.ok(Math.abs(bar.read + bar.write + bar.fresh - 100) < 0.02, 'segments cover the whole input');
  assert.equal(Math.round(bar.read), 90, 'read width matches "90 % caché"');
  assert.equal(bar.showFresh, false, '4 fresh tokens of 111 k is not a visible share');
  const fresh = forkCacheBar({ input: 5000, output: 1, cacheRead: 40_000, cacheCreation: 5000 });
  assert.deepEqual([fresh.read, fresh.write, fresh.fresh, fresh.showFresh], [80, 10, 10, true]);
  const none = forkCacheBar({ input: 0, output: 30, cacheRead: 0, cacheCreation: 0 });
  assert.equal(none.empty, true, 'output alone draws no bar');
});

test('a retried child shows its latest attempt in the original slot', () => {
  const children = [
    child({ childId: 'c1', index: 1 }),
    child({ childId: 'c2', index: 2, status: 'running' }),
    child({ childId: 'c3', index: 3, status: 'cancelled' }),
    child({ childId: 'c3-r2', index: 3, attempt: 2, retryOf: 'c3', status: 'failed' }),
    child({ childId: 'c3-r3', index: 3, attempt: 3, retryOf: 'c3', status: 'running' }),
  ];
  const slots = collapseForkRetries(children);
  assert.deepEqual(slots.map((slot) => [slot.rootId, slot.child.childId, slot.attempts]), [
    ['c1', 'c1', 1], ['c2', 'c2', 1], ['c3', 'c3-r3', 3],
  ]);
  // Order of arrival does not matter; attempt number decides.
  const shuffled = collapseForkRetries([children[4], children[2], children[3]]);
  assert.deepEqual(shuffled.map((slot) => [slot.rootId, slot.child.childId]), [['c3', 'c3-r3']]);
  // A retry whose original is missing still groups under the original id.
  const orphan = collapseForkRetries([child({ childId: 'c9-r2', attempt: 2, retryOf: 'c9' })]);
  assert.deepEqual(orphan.map((slot) => [slot.rootId, slot.child.childId]), [['c9', 'c9-r2']]);
});

test('run summary counts the rows shown, in the mock order and plurals', () => {
  const slots = collapseForkRetries([
    child({ childId: 'a' }), child({ childId: 'b', status: 'running' }), child({ childId: 'c', status: 'queued' }),
  ]);
  assert.equal(forkRunSummary(slots), '1 listo · 1 en curso · 1 en cola');
  const settled = collapseForkRetries([
    child({ childId: 'a' }), child({ childId: 'b' }), child({ childId: 'c' }), child({ childId: 'd', status: 'failed' }),
    child({ childId: 'e', status: 'cancelled' }), child({ childId: 'f', status: 'cancelled' }),
  ]);
  assert.equal(forkRunSummary(settled), '3 listos · 1 falló · 2 detenidos');
  // The superseded cancelled attempt is not counted once its retry finished.
  const retried = collapseForkRetries([
    child({ childId: 'c3', status: 'cancelled' }), child({ childId: 'c3-r2', attempt: 2, retryOf: 'c3' }),
  ]);
  assert.equal(forkRunSummary(retried), '1 listo');
});

test('labels and status words', () => {
  assert.equal(forkChildLabel({ label: 'frame_00 · hijo 1', index: 1 }), 'frame_00 · hijo 1');
  assert.equal(forkChildLabel({ label: '  ', index: 4 }), 'hijo 4');
  assert.equal(forkTurnsLabel(1), '1 turno');
  assert.equal(forkTurnsLabel(2), '2 turnos');
  assert.deepEqual(['queued', 'running', 'completed', 'failed', 'cancelled'].map(forkStatusWord),
    ['en cola', 'en curso', 'listo', 'falló', 'detenido']);
});

test('elapsed ticks while running and freezes at wall time when settled', () => {
  const now = Date.parse('2026-10-15T20:50:10Z');
  assert.equal(forkChildElapsedMs(child({ childId: 'r', status: 'running', startedAt: '2026-10-15T20:50:00Z' }), now), 10_000);
  assert.equal(forkChildElapsedMs(child({ childId: 'd', wallMs: 9799 }), now), 9799);
  assert.equal(forkChildElapsedMs(child({ childId: 'q', status: 'queued' }), now), null);
  const run = { children: [
    child({ childId: 'a', startedAt: '2026-10-15T20:49:50Z', finishedAt: '2026-10-15T20:50:00Z', wallMs: 10_000 }),
    child({ childId: 'b', startedAt: '2026-10-15T20:49:50Z', finishedAt: '2026-10-15T20:50:15Z', wallMs: 25_000 }),
  ] };
  assert.equal(forkRunWallMs(run, now), 25_000, 'first start to last finish');
  assert.equal(forkRunWallMs({ children: [child({ childId: 'x', wallMs: 7000 })] }, now), 7000, 'no stamps → longest child');
});

test('activity splits tool lines from daemon prose', () => {
  assert.deepEqual(splitForkActivity('Write /tmp/fork-test/child_2.json'), { tool: 'Write', target: '/tmp/fork-test/child_2.json' });
  assert.deepEqual(splitForkActivity('Iniciando'), { prose: 'Iniciando' });
  assert.deepEqual(splitForkActivity('Respondiendo'), { prose: 'Respondiendo' });
  assert.equal(splitForkActivity('  '), null);
});

test('child transcript: each tool pairs with its result; a final result repeating the reply is not shown twice', async () => {
  const { forkTranscriptItems } = await import('../src/fork-format.ts');
  const entries = [
    { kind: 'user' as const, at: '2026-10-15T20:49:52.000Z', text: 'Etiquetá las 12 regiones', attachments: [] },
    { kind: 'call' as const, at: '2026-10-15T20:49:55.000Z', call: 1 },
    { kind: 'tool' as const, at: '2026-10-15T20:49:56.000Z', id: 'tu-1', name: 'Write', target: '/tmp/fork-test/child_1.json', input: { file_path: '/tmp/fork-test/child_1.json' } },
    { kind: 'tool_result' as const, at: '2026-10-15T20:49:56.400Z', toolUseId: 'tu-1', isError: false, text: 'File created' },
    { kind: 'assistant' as const, at: '2026-10-15T20:50:01.000Z', text: '/tmp/fork-test/child_1.json' },
    { kind: 'result' as const, at: '2026-10-15T20:50:01.100Z', subtype: 'success', isError: false, turns: 2, durationMs: 9000, text: '/tmp/fork-test/child_1.json' },
  ];
  const items = forkTranscriptItems(entries, false);
  assert.deepEqual(items.map((item) => item.kind), ['user', 'tool', 'text']);
  const tool = items[1].kind === 'tool' ? items[1].tool : null;
  assert.equal(tool?.title, 'Write');
  assert.equal(tool?.location, '/tmp/fork-test/child_1.json');
  assert.equal(tool?.status, 'completed');
  assert.equal(tool?.output, 'File created');
  assert.equal((tool?.endedAt ?? 0) - (tool?.startedAt ?? 0), 400);

  // Still running: a call without its result is in progress; an error result is said, not hidden.
  const live = forkTranscriptItems([entries[0], entries[2]], true);
  assert.equal(live[1].kind === 'tool' && live[1].tool.status, 'in_progress');
  const failed = forkTranscriptItems([
    { kind: 'tool' as const, at: entries[2].at, id: 'tu-2', name: 'Bash', target: 'ls' },
    { kind: 'tool_result' as const, at: entries[3].at, toolUseId: 'tu-2', isError: true, text: 'boom' },
    { kind: 'result' as const, at: entries[5].at, isError: true, text: 'max turns' },
  ], false);
  assert.deepEqual(failed.map((item) => item.kind), ['tool', 'error']);
  assert.equal(failed[0].kind === 'tool' && failed[0].tool.status, 'failed');
});
