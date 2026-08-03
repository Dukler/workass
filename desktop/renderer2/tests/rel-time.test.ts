import assert from 'node:assert/strict';
import test from 'node:test';
import { relTime } from '../src/rel-time.ts';

const NOW = Date.parse('2026-07-17T12:00:00Z');
const ago = (ms: number) => new Date(NOW - ms).toISOString();

test('relTime ages across every threshold instead of sticking at seconds', () => {
  assert.equal(relTime(ago(0), NOW), 'ahora');
  assert.equal(relTime(ago(5_000), NOW), 'hace 5 s');
  assert.equal(relTime(ago(59_000), NOW), 'hace 59 s');
  assert.equal(relTime(ago(60_000), NOW), 'hace 1 min');
  assert.equal(relTime(ago(5 * 60_000), NOW), 'hace 5 min');
  assert.equal(relTime(ago(59 * 60_000), NOW), 'hace 59 min');
  assert.equal(relTime(ago(3 * 3_600_000), NOW), 'hace 3 h');
  assert.equal(relTime(ago(23 * 3_600_000), NOW), 'hace 23 h');
  // Old turns read in days, not an unbounded "hace 120 h".
  assert.equal(relTime(ago(2 * 86_400_000), NOW), 'hace 2 d');
  assert.equal(relTime(ago(5 * 86_400_000), NOW), 'hace 5 d');
});

test('relTime is defensive about missing/garbage timestamps', () => {
  assert.equal(relTime(null, NOW), '');
  assert.equal(relTime('not-a-date', NOW), '');
  // A future timestamp (clock skew) clamps to the floor instead of going negative.
  assert.equal(relTime(new Date(NOW + 10_000).toISOString(), NOW), 'ahora');
});
