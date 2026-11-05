import assert from 'node:assert/strict';
import test from 'node:test';
import type { SpawnedWorkItem } from '../src/wire/types.ts';
import {
  backgroundWork, finishedToday, fmtSpan, fmtWait, joinNames, loadSegments, machineLoads, nextInQueue, queueGroups,
  workMachine, workRunMs, workTotals, workWaitMs,
} from '../src/rail-work.ts';

const NOW = Date.parse('2026-11-05T15:00:00Z');
const ago = (ms: number) => new Date(NOW - ms).toISOString();
const job = (id: string, extra: Partial<SpawnedWorkItem> = {}): SpawnedWorkItem => ({
  id, taskId: id, tabId: 'tab', chatId: 'chat', providerId: 'claude', kind: 'external', label: id,
  status: 'running', startedAt: ago(60_000), updatedAt: ago(0), ...extra,
});
const waiting = (id: string, waitedMs: number, extra: Partial<SpawnedWorkItem> = {}) =>
  job(id, { queuedAt: ago(waitedMs), startedAt: ago(waitedMs), ...extra });
const context = { localMachineId: 'm-self', names: { 'm-far': 'buildbox' }, localName: 'Este Mac' };

test('background work splits into running rows and the queue; agents and their own work stay out', () => {
  const work = [
    job('runs', { queuedAt: ago(90_000), execStartedAt: ago(30_000) }),
    waiting('waits', 50_000),
    job('agent', { kind: 'agent' }),
    job('child', { parentTaskId: 'agent' }),
    job('done', { status: 'exited', finishedAt: ago(1000) }),
    job('plain'),
  ];
  const { running, queued } = backgroundWork(work);
  // longest running first: 'plain' started a minute ago, 'runs' left its queue 30 s ago
  assert.deepEqual(running.map((item) => item.id), ['plain', 'runs']);
  assert.deepEqual(queued.map((item) => item.id), ['waits']);
});

test('a row runs where the chat lives unless it names another host', () => {
  assert.deepEqual(workMachine(job('a'), context), { key: '', name: 'Este Mac', local: true });
  assert.deepEqual(workMachine(job('b', { hostMachine: 'm-self' }), context), { key: '', name: 'Este Mac', local: true });
  assert.deepEqual(workMachine(job('c', { hostMachine: 'm-far' }), context), { key: 'm-far', name: 'buildbox', local: false });
  assert.deepEqual(workMachine(job('d', { hostMachine: 'm-0123456789', hostMachineName: 'workass daemon gpu-host (m-0123456789)' }), context),
    { key: 'm-0123456789', name: 'gpu-host', local: false });
  // A chat on another machine: its plain rows run there, not here.
  assert.deepEqual(workMachine(job('e'), { ...context, chatMachineId: 'm-far' }), { key: 'm-far', name: 'buildbox', local: false });
});

test('machine rows count running + waiting, this machine first', () => {
  const loads = machineLoads(
    [job('r1'), job('r2', { hostMachine: 'm-far' }), job('r3')],
    [waiting('w1', 10), waiting('w2', 10, { hostMachine: 'm-far' }), waiting('w3', 10, { hostMachine: 'm-far' })],
    context,
  );
  assert.deepEqual(loads.map((load) => [load.machine.name, load.running, load.waiting]), [['Este Mac', 2, 1], ['buildbox', 1, 2]]);
});

test('the queue groups by machine, first asked first, and «sigue» names the head of each', () => {
  const groups = queueGroups([
    waiting('late-here', 1000),
    waiting('far-second', 5000, { hostMachine: 'm-far' }),
    waiting('early-here', 9000),
    waiting('far-first', 8000, { hostMachine: 'm-far' }),
  ], context);
  assert.deepEqual(groups.map((group) => [group.machine.name, group.items.map((item) => item.id)]),
    [['Este Mac', ['early-here', 'late-here']], ['buildbox', ['far-first', 'far-second']]]);
  assert.deepEqual(nextInQueue(groups), ['early-here', 'far-first']);
  assert.equal(joinNames(['a']), 'a');
  assert.equal(joinNames(['a', 'b']), 'a y b');
  assert.equal(joinNames(['a', 'b', 'c']), 'a, b y c');
});

test('totals leave out zero parts', () => {
  assert.deepEqual(workTotals(2, 3, 14).map((part) => `${part.count} ${part.word}`), ['2 subagentes', '3 corriendo', '14 en espera']);
  assert.deepEqual(workTotals(0, 3, 0).map((part) => `${part.count} ${part.word}`), ['3 corriendo']);
  assert.deepEqual(workTotals(1, 0, 0).map((part) => part.word), ['subagente']);
  assert.deepEqual(workTotals(0, 0, 0), []);
});

test('a job runs from its exec, waits from its ask, and reads in plain units', () => {
  const item = job('j', { queuedAt: ago(3 * 3600_000), execStartedAt: ago(2 * 3600_000 + 2 * 60_000) });
  assert.equal(fmtSpan(workRunMs(item, NOW)), '2 h 2 min');
  assert.equal(fmtSpan(workWaitMs(item, NOW)), '3 h');
  assert.equal(fmtSpan(40_000), '40 s');
  assert.equal(fmtSpan(41 * 60_000), '41 min');
  assert.equal(fmtSpan(26 * 3600_000), '1 d 2 h');
  assert.equal(fmtSpan(null), '');
  // a wait reads coarser than a run
  assert.equal(fmtWait(2 * 3600_000 - 60_000), '2 h');
  assert.equal(fmtWait(44 * 60_000), '44 min');
  assert.equal(fmtWait(3 * 86400_000), '3 d');
});

test('finished today is this calendar day, and a long bar keeps its proportions', () => {
  assert.equal(finishedToday({ finishedAt: new Date(NOW - 1000).toISOString(), updatedAt: '' }, NOW), true);
  assert.equal(finishedToday({ finishedAt: new Date(NOW - 3 * 86400_000).toISOString(), updatedAt: '' }, NOW), false);
  assert.deepEqual(loadSegments(2, 9), { running: 2, waiting: 9 });
  const long = loadSegments(10, 90);
  assert.equal(long.running + long.waiting, 24);
  assert.ok(long.running >= 1 && long.waiting >= 1);
});
