import assert from 'node:assert/strict';
import test from 'node:test';
import {
  createTranscriptPinScheduler,
  latestOrdinaryUserMessageId,
  TRANSCRIPT_BACKGROUND_PIN_DELAY_MS,
  transcriptIsAtBottom,
  transcriptPinnedAfterScroll,
  transcriptReaderLeftUnannounced,
} from '../src/transcript-scroll.ts';

test('only a new ordinary user row changes the transcript follow identity', () => {
  const messages = [
    { id: 'user-1', role: 'user' },
    { id: 'assistant-1', role: 'assistant' },
  ];
  assert.equal(latestOrdinaryUserMessageId(messages), 'user-1');

  messages.push({ id: 'user-2', role: 'user' });
  assert.equal(latestOrdinaryUserMessageId(messages), 'user-2');
});

test('steer submission and terminal presentation do not change the follow identity', () => {
  const active = [
    { id: 'user-1', role: 'user' },
    { id: 'assistant-1', role: 'assistant' },
    { id: 'steer-1', role: 'user', steerState: 'applied' },
    { id: 'assistant-2', role: 'assistant' },
  ];
  const terminal = [active[0], active[1], active[3], active[2]];

  assert.equal(latestOrdinaryUserMessageId(active), 'user-1');
  assert.equal(latestOrdinaryUserMessageId(terminal), 'user-1');
});

const farFromBottom = { scrollHeight: 2_400, scrollTop: 900, clientHeight: 700 };
const nearBottom = { scrollHeight: 2_400, scrollTop: 1_630, clientHeight: 700 };

test('coalesced programmatic scroll events cannot unpin a followed transcript', () => {
  assert.equal(transcriptPinnedAfterScroll(true, false, farFromBottom, -800), true);
});

test('an explicit user scroll can leave and re-enter the followed bottom', () => {
  assert.equal(transcriptPinnedAfterScroll(true, true, farFromBottom, -800), false);
  assert.equal(transcriptPinnedAfterScroll(false, true, nearBottom, 730), true);
});

test('a small upward scroll cannot re-enable following before image or stream growth', () => {
  // onWheel has already released following. Its ensuing scroll event is still
  // within the 80px proximity zone, but the reader is moving away from bottom.
  const afterUpwardWheel = false;
  const metrics = { scrollHeight: 2_400, scrollTop: 1_660, clientHeight: 700 };
  assert.equal(transcriptPinnedAfterScroll(afterUpwardWheel, true, metrics, -40), false);
});

test('image anchoring during an upward wheel intent cannot re-pin even at bottom', () => {
  const bottom = { scrollHeight: 2_400, scrollTop: 1_700, clientHeight: 700 };
  assert.equal(transcriptPinnedAfterScroll(false, true, bottom, -40), false);
});

test('a coalesced programmatic scroll inside the input window cannot re-pin', () => {
  assert.equal(transcriptPinnedAfterScroll(false, true, nearBottom, 0), false);
});

test('a scroll nobody wrote that leaves the bottom upward is the reader (wheel over an HTML frame)', () => {
  assert.equal(transcriptReaderLeftUnannounced(true, farFromBottom, -730), true);
});

test('an unwritten move that stays at the bottom or goes down keeps following', () => {
  // A shrink clamps a followed reader to the new bottom; the spring only descends.
  assert.equal(transcriptReaderLeftUnannounced(true, nearBottom, -40), false);
  assert.equal(transcriptReaderLeftUnannounced(true, farFromBottom, 120), false);
  assert.equal(transcriptReaderLeftUnannounced(true, farFromBottom, 0), false);
});

test('a reader who already left is not re-decided by an unwritten move', () => {
  assert.equal(transcriptReaderLeftUnannounced(false, farFromBottom, -730), false);
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
