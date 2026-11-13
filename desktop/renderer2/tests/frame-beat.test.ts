import assert from 'node:assert/strict';
import { test } from 'node:test';
import { BEAT_MS, createBeat, HIDDEN_BEAT_MS } from '../src/frame-beat.ts';
import { FRAME_GRID_MS } from '../src/frame-grid.ts';

// Streaming re-renders land on the 30 fps beat; the first after a pause is
// immediate; hidden windows gather for a second (energy lane, 2026-11-13).
function rig() {
  let now = 1000;
  let hidden = false;
  const timers: { at: number; fn: () => void; id: number }[] = [];
  let ids = 0;
  const flushed: { at: number; topics: string[] }[] = [];
  const beat = createBeat((topics) => flushed.push({ at: now, topics }), {
    now: () => now,
    hidden: () => hidden,
    setTimeout: (fn, ms) => { const id = ++ids; timers.push({ at: now + ms, fn, id }); return id; },
    clearTimeout: (id) => { const i = timers.findIndex((t) => t.id === id); if (i >= 0) timers.splice(i, 1); },
  });
  const advance = (ms: number) => {
    const end = now + ms;
    for (;;) {
      timers.sort((a, b) => a.at - b.at);
      const next = timers[0];
      if (!next || next.at > end) break;
      timers.shift(); now = next.at; next.fn();
    }
    now = end;
  };
  return { beat, flushed, advance, setHidden: (h: boolean) => { hidden = h; }, timers, get now() { return now; } };
}

test('the first update after a pause renders at once; a steady stream renders on the 1/30 s grid', () => {
  const r = rig();
  r.beat.request('msg:a');
  assert.deepEqual(r.flushed, [{ at: 1000, topics: ['msg:a'] }], 'immediate');
  for (let i = 0; i < 30; i++) { r.advance(10); r.beat.request('msg:a'); r.beat.request('act'); }
  r.advance(40);
  const later = r.flushed.slice(1);
  assert.ok(later.length >= 8 && later.length <= 10, `about 30 a second, got ${later.length} in 0.34 s`);
  for (const f of later) {
    const phase = f.at % FRAME_GRID_MS;
    assert.ok(phase >= 0.9 && phase <= 1.1, `on the grid: ${f.at}`);
    assert.deepEqual(f.topics, ['msg:a', 'act']);
  }
  r.advance(200);
  r.beat.request('msg:b');
  assert.equal(r.flushed.at(-1)!.at, r.now, 'after a pause: at once again');
});

test('hidden: updates gather for a second and flush when the window shows', () => {
  const r = rig();
  r.setHidden(true);
  r.beat.request('msg:a');
  r.advance(100);
  r.beat.request('msg:a');
  assert.equal(r.flushed.length, 0);
  r.advance(HIDDEN_BEAT_MS);
  assert.equal(r.flushed.length, 1);
  r.beat.request('msg:a');
  r.advance(200);
  r.setHidden(false);
  r.beat.shown();
  assert.equal(r.flushed.length, 2, 'shown: at once');
  assert.equal(r.beat.pending(), 0);
});

test('the beat is the frame grid', () => {
  assert.equal(BEAT_MS, FRAME_GRID_MS);
});
