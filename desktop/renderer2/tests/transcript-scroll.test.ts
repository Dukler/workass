import assert from 'node:assert/strict';
import test from 'node:test';
import {
  createTranscriptPinScheduler,
  TRANSCRIPT_BACKGROUND_PIN_DELAY_MS,
  transcriptIsAtBottom,
  transcriptPinnedAfterScroll,
} from '../src/transcript-scroll.ts';

const farFromBottom = { scrollHeight: 2_400, scrollTop: 900, clientHeight: 700 };
const nearBottom = { scrollHeight: 2_400, scrollTop: 1_630, clientHeight: 700 };

test('coalesced programmatic scroll events cannot unpin a followed transcript', () => {
  assert.equal(transcriptPinnedAfterScroll(true, false, farFromBottom), true);
});

test('an explicit user scroll can leave and re-enter the followed bottom', () => {
  assert.equal(transcriptPinnedAfterScroll(true, true, farFromBottom), false);
  assert.equal(transcriptPinnedAfterScroll(false, true, nearBottom), true);
});

test('the transcript bottom threshold preserves the existing 80px contract', () => {
  assert.equal(transcriptIsAtBottom(nearBottom), true);
  assert.equal(transcriptIsAtBottom({ ...nearBottom, scrollTop: 1_620 }), false);
});

function schedulerFixture(foreground: boolean) {
  let pinned = true;
  let pins = 0;
  let nextHandle = 1;
  const frames = new Map<number, () => void>();
  const timers = new Map<number, { callback: () => void; delayMs: number }>();
  const scheduler = createTranscriptPinScheduler({
    isPinned: () => pinned,
    isForeground: () => foreground,
    pin: () => { pins += 1; },
    requestFrame: (callback) => { const handle = nextHandle++; frames.set(handle, callback); return handle; },
    cancelFrame: (handle) => { frames.delete(handle); },
    setTimer: (callback, delayMs) => { const handle = nextHandle++; timers.set(handle, { callback, delayMs }); return handle; },
    clearTimer: (handle) => { timers.delete(handle); },
  });
  return {
    scheduler,
    frames,
    timers,
    pins: () => pins,
    setPinned: (value: boolean) => { pinned = value; },
  };
}

test('foreground stream growth coalesces into one animation-frame pin', () => {
  const fixture = schedulerFixture(true);
  fixture.scheduler.schedule();
  fixture.scheduler.schedule();
  assert.equal(fixture.frames.size, 1);
  assert.equal(fixture.timers.size, 0);
  fixture.frames.values().next().value?.();
  assert.equal(fixture.pins(), 1);
});

test('background stream growth uses a timer when animation frames are paused', () => {
  const fixture = schedulerFixture(false);
  fixture.scheduler.schedule();
  fixture.scheduler.schedule();
  assert.equal(fixture.frames.size, 0);
  assert.equal(fixture.timers.size, 1);
  const pending = fixture.timers.values().next().value;
  assert.equal(pending?.delayMs, TRANSCRIPT_BACKGROUND_PIN_DELAY_MS);
  pending?.callback();
  assert.equal(fixture.pins(), 1);
});

test('focus restoration cancels stale work and reconciles before first paint', () => {
  const fixture = schedulerFixture(false);
  fixture.scheduler.schedule();
  fixture.scheduler.reconcile();
  assert.equal(fixture.timers.size, 0);
  assert.equal(fixture.frames.size, 0);
  assert.equal(fixture.pins(), 1);
});

test('background and focus reconciliation preserve an intentional user unpin', () => {
  const fixture = schedulerFixture(false);
  fixture.setPinned(false);
  fixture.scheduler.schedule();
  fixture.scheduler.reconcile();
  assert.equal(fixture.timers.size, 0);
  assert.equal(fixture.pins(), 0);
});
