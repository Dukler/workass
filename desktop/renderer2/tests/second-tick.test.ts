import assert from 'node:assert/strict';
import { mock, test } from 'node:test';
import { relTime, stampTurnsOver } from '../src/rel-time.ts';

// Live time labels share one clock on the wall clock's second boundary, so they
// repaint together, and a label that changes rarely sleeps until it does
// (energy lane, 2026-11-12).
test('every counter ticks on the same timer, at the second boundary, and the timer stops with the last one', async () => {
  mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 10_400 });
  try {
    const { subscribeSecond, secondTickInternals } = await import('../src/second-tick.ts');
    const seen: string[] = [];
    const offA = subscribeSecond(() => seen.push(`a@${Date.now()}`));
    mock.timers.tick(300);
    const offB = subscribeSecond(() => seen.push(`b@${Date.now()}`));
    assert.equal(secondTickInternals.listenerCount(), 2);
    mock.timers.tick(302);  // 11_002: just past the boundary
    assert.deepEqual(seen, ['a@11002', 'b@11002']);
    mock.timers.tick(1000);
    assert.deepEqual(seen.slice(2), ['a@12002', 'b@12002']);
    offA();
    mock.timers.tick(1000);
    assert.deepEqual(seen.slice(4), ['b@13002']);
    offB();
    assert.equal(secondTickInternals.scheduled(), false);
    mock.timers.tick(5000);
    assert.equal(seen.length, 5);
  } finally {
    mock.timers.reset();
  }
});

test('a label that changes once a minute sleeps until then, on the same boundaries', async () => {
  mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 1_000_500 });
  try {
    const { subscribeUntil, secondTickInternals } = await import('../src/second-tick.ts');
    const then = 1_000_500 - 125_250;   // 2 min 5.25 s old: "hace 2 min" until 3 min
    const seen: number[] = [];
    const off = subscribeUntil(() => seen.push(Date.now()), (now) => stampTurnsOver(then, now));
    assert.equal(secondTickInternals.timerAt(), 1_056_002);   // then + 180 s = 1_055_250, next boundary
    mock.timers.tick(55_000);
    assert.deepEqual(seen, []);
    mock.timers.tick(502);
    assert.deepEqual(seen, [1_056_002]);
    assert.equal(relTime(new Date(then).toISOString(), 1_056_002), 'hace 3 min');
    off();
  } finally {
    mock.timers.reset();
  }
});

test('stampTurnsOver names exactly the moment relTime changes', () => {
  const then = Date.UTC(2026, 10, 12, 10, 0, 0, 300);
  const at = new Date(then).toISOString();
  for (const age of [0, 999, 1_000, 59_999, 60_000, 61_234, 3_599_999, 3_600_000, 7_300_000, 86_399_999, 86_400_000, 200_000_000]) {
    const now = then + age;
    const next = stampTurnsOver(then, now);
    assert.ok(next > now, `age ${age}`);
    if (age < 60_000) {
      // "hace N s": every second, as the per-second interval did.
      assert.ok(next - now <= 1000, `age ${age}`);
      continue;
    }
    // Older: no change before `next`, a change exactly at it.
    assert.equal(relTime(at, next - 1), relTime(at, now), `age ${age}: changed early`);
    assert.notEqual(relTime(at, next), relTime(at, now), `age ${age}: no change at ${next - then}`);
  }
});

// Energy lane 2026-11-13: nobody sees the labels of a hidden window, so the
// clock stops while hidden and every label catches up the moment it shows.
test('the clock sleeps while the window is hidden and catches up when it shows', async () => {
  mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 20_500 });
  const { subscribeSecond, secondTickInternals } = await import('../src/second-tick.ts');
  let hidden = false;
  secondTickInternals.setHidden(() => hidden);
  try {
    const seen: number[] = [];
    const off = subscribeSecond(() => seen.push(Date.now()));
    mock.timers.tick(502);   // 21_002
    assert.deepEqual(seen, [21_002]);
    hidden = true;
    mock.timers.tick(1000);  // the tick already scheduled still runs once
    assert.deepEqual(seen, [21_002, 22_002]);
    assert.equal(secondTickInternals.scheduled(), false, 'nothing is scheduled while hidden');
    mock.timers.tick(10_000);
    assert.equal(seen.length, 2);
    hidden = false;
    mock.timers.tick(300);   // 32_302
    secondTickInternals.shown();
    assert.deepEqual(seen.slice(2), [32_302], 'shown: at once');
    mock.timers.tick(700);   // 33_002: back on the boundary
    assert.deepEqual(seen.slice(3), [33_002]);
    off();
  } finally {
    secondTickInternals.setHidden(() => false);
    mock.timers.reset();
  }
});
