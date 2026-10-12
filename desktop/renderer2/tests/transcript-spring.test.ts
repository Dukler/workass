import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createScrollSpring } from '../src/transcript-scroll.ts';

function harness(scrollHeight: number) {
  const el = { scrollTop: 0, scrollHeight, clientHeight: 400 };
  let pinned = true;
  let queued: ((now: number) => void) | null = null;
  let now = 0;
  const spring = createScrollSpring({
    el,
    isPinned: () => pinned,
    requestFrame: (callback) => { queued = callback; return 1; },
    cancelFrame: () => { queued = null; },
  });
  const run = (frames: number, grow?: (frame: number) => void) => {
    const positions: number[] = [];
    for (let frame = 0; frame < frames && queued; frame++) {
      grow?.(frame);
      const callback = queued; queued = null; now += 1000 / 60; callback(now);
      positions.push(el.scrollTop);
    }
    return positions;
  };
  return { el, spring, run, unpin: () => { pinned = false; }, idle: () => queued === null };
}

test('the spring glides to the end without overshooting and stops by itself', () => {
  const { el, spring, run, idle } = harness(1000);
  spring.kick();
  const positions = run(120);
  assert.equal(el.scrollTop, 600);
  assert.ok(positions.every((top) => top <= 600), 'never past the end');
  assert.ok(idle(), 'no frame left queued once it arrived');
  const steps = positions.slice(1).map((top, index) => top - positions[index]);
  assert.ok(steps.every((step) => step >= 0), 'only ever moves down');
});

test('a line landing mid-glide extends the motion without a jolt', () => {
  const { el, spring, run } = harness(1000);
  spring.kick();
  const positions = run(90, (frame) => { if (frame === 12) el.scrollHeight += 120; });
  assert.equal(el.scrollTop, 720);
  const speeds = positions.slice(1).map((top, index) => top - positions[index]);
  const jolts = speeds.slice(1).map((speed, index) => Math.abs(speed - speeds[index]));
  assert.ok(Math.max(...jolts) < 12, `largest per-frame speed change ${Math.max(...jolts).toFixed(1)}px`);
});

test('the spring lets go the moment the reader scrolls away', () => {
  const { el, spring, run, unpin, idle } = harness(2000);
  spring.kick();
  run(5);
  const at = el.scrollTop;
  unpin();
  run(5);
  assert.equal(el.scrollTop, at);
  assert.ok(idle());
});
