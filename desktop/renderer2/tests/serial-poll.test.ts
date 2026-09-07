import assert from 'node:assert/strict';
import { test } from 'node:test';
import { startSerialPoll } from '../src/serial-poll.ts';

test('slow output reads never overlap and cleanup during a read cannot restart polling', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let reads = 0;
  let finish!: () => void;
  const stop = startSerialPoll(() => {
    reads++;
    return new Promise<void>((resolve) => { finish = resolve; });
  }, 2000);
  assert.equal(reads, 1);
  t.mock.timers.tick(60_000);
  assert.equal(reads, 1, 'a minute-long read must not enqueue thirty more reads');
  finish();
  await Promise.resolve();
  t.mock.timers.tick(1999);
  assert.equal(reads, 1);
  t.mock.timers.tick(1);
  assert.equal(reads, 2);
  stop();
  finish();
  await Promise.resolve();
  t.mock.timers.tick(60_000);
  assert.equal(reads, 2);
});

test('closing output cancels a scheduled read and failed reads retain a bounded cadence', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  let reads = 0;
  const stop = startSerialPoll(async () => { reads++; throw new Error('offline'); }, 2000);
  await Promise.resolve();
  t.mock.timers.tick(2000);
  assert.equal(reads, 2);
  await Promise.resolve();
  stop();
  t.mock.timers.tick(60_000);
  assert.equal(reads, 2);
});
