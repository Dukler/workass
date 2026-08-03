import test from 'node:test';
import assert from 'node:assert/strict';
import { nextUpdatePhase, brandForProvider, DONE_HOLD_MS, EXIT_MS } from '../src/update-card.ts';

test('provider ids resolve to the same brand marks the daemon uses', () => {
  assert.equal(brandForProvider('codex'), 'gpt');
  assert.equal(brandForProvider('gpt-5.6-sol'), 'gpt');
  assert.equal(brandForProvider('claude'), 'claude');
  assert.equal(brandForProvider('opus'), 'claude');
  assert.equal(brandForProvider('qwen'), '');
  assert.equal(brandForProvider(null), '');
});

test('an in-flight update shows the running card from any prior phase', () => {
  assert.equal(nextUpdatePhase('hidden', { active: true, pending: 1 }), 'running');
  assert.equal(nextUpdatePhase('resting', { active: true, pending: 2 }), 'running');
  assert.equal(nextUpdatePhase('done', { active: true, pending: 0 }), 'running');
  assert.equal(nextUpdatePhase('exiting', { active: true, pending: 0 }), 'running');
});

test('finishing the last update seals the card into done', () => {
  assert.equal(nextUpdatePhase('running', { active: false, pending: 0 }), 'done');
});

test('leaving an update with work still pending falls back to resting, not done', () => {
  // A failed CLI stays advertised as updateable, so the success seal must not fire.
  assert.equal(nextUpdatePhase('running', { active: false, pending: 1 }), 'resting');
});

test('done and exiting are held until the component timers advance them', () => {
  assert.equal(nextUpdatePhase('done', { active: false, pending: 0 }), 'done');
  assert.equal(nextUpdatePhase('exiting', { active: false, pending: 0 }), 'exiting');
});

test('resting appears only while updates remain, else hidden', () => {
  assert.equal(nextUpdatePhase('hidden', { active: false, pending: 3 }), 'resting');
  assert.equal(nextUpdatePhase('resting', { active: false, pending: 1 }), 'resting');
  assert.equal(nextUpdatePhase('hidden', { active: false, pending: 0 }), 'hidden');
  assert.equal(nextUpdatePhase('resting', { active: false, pending: 0 }), 'hidden');
});

test('timing constants keep the seal hold and slide-out distinct and short', () => {
  assert.equal(DONE_HOLD_MS, 500);
  assert.ok(EXIT_MS > 0 && EXIT_MS < DONE_HOLD_MS);
});
