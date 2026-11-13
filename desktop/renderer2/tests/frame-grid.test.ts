import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { FRAME_GRID_MS, alignStartedAnimation, alignToFrameGrid, isSteppedAnimation } from '../src/frame-grid.ts';

// Stepped loops start on the shared 1/30 s grid so they change on the same
// frames (energy lane, 2026-11-12).
test('a stepped animation that starts is moved onto the 1/30 s grid, others are left alone', () => {
  const host = {};
  const make = (animationName: string, startTime: number, target: object, pseudoElement: string | null = null) =>
    ({ animationName, startTime, effect: { target, pseudoElement } });
  const orbit = make('workorbit', 1012.5, host, '::after');
  const other = make('workorbit', 1013.1, {}, '::after');
  const fade = make('toast-in', 1012.5, host);
  const doc = { getAnimations: () => [orbit, other, fade] } as unknown as Document;
  assert.equal(alignStartedAnimation(doc, { animationName: 'workorbit', target: host as EventTarget, pseudoElement: '::after' }), 1);
  assert.ok(Math.abs(orbit.startTime / FRAME_GRID_MS - Math.round(orbit.startTime / FRAME_GRID_MS)) < 1e-9);
  assert.ok(Math.abs(orbit.startTime - 1012.5) < FRAME_GRID_MS / 2);
  assert.equal(other.startTime, 1013.1, 'another element keeps its start');
  assert.equal(alignStartedAnimation(doc, { animationName: 'toast-in', target: host as EventTarget }), 0);
  assert.equal(fade.startTime, 1012.5);
  assert.equal(alignToFrameGrid(0), 0);
});

test('every looping stepped animation in app.css is lined up', () => {
  const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  const looping = [...css.matchAll(/animation(?:-name)?:\s*([\w-]+)[^;]*steps\([^;]*infinite/g)].map((m) => m[1]);
  assert.ok(looping.length >= 8, looping.join(','));
  for (const name of looping.filter((n) => n !== 'sv2-working-text' && !/^\d/.test(n))) assert.ok(isSteppedAnimation(name), name);
  for (const name of ['fg-swing', 'fg-flash', 'fg-fly-a-40', 'workdot-2', 'wdot-ring', 'fk-ping', 'cb-pulse', 'workspin', 'conn-spin']) assert.ok(isSteppedAnimation(name), name);
  assert.ok(isSteppedAnimation('mdfresh'));
  assert.ok(!isSteppedAnimation('toast-in'));
});
