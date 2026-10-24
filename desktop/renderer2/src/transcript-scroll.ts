export type TranscriptScrollMetrics = {
  scrollHeight: number;
  scrollTop: number;
  clientHeight: number;
};

export const TRANSCRIPT_BOTTOM_THRESHOLD = 80;

type TranscriptMessageIdentity = {
  id: string;
  role: string;
  steerState?: unknown;
  notice?: unknown;
};

// A steer is rendered in the composer tray while its turn is active and moves
// into the transcript only when that turn settles. That presentation-only move
// is not a new send and must never re-pin a reader who scrolled away. Ordinary
// user rows remain the explicit "follow this turn" signal.
export function latestOrdinaryUserMessageId(
  messages: readonly TranscriptMessageIdentity[],
): string | null {
  for (let index = messages.length - 1; index >= 0; index--) {
    const message = messages[index];
    // A wake-up notice is not the user asking to follow a turn: a reader who
    // scrolled up stays put (review wa-subagent-1792184734224-6, suggestion 2).
    if (message.role === 'user' && message.steerState === undefined && !message.notice) return message.id;
  }
  return null;
}

export function transcriptIsAtBottom(
  metrics: TranscriptScrollMetrics,
  threshold = TRANSCRIPT_BOTTOM_THRESHOLD,
): boolean {
  return metrics.scrollHeight - metrics.scrollTop - metrics.clientHeight < threshold;
}

// Browser-generated scroll events do not reliably identify their origin:
// assigning scrollTop from code still produces a trusted `scroll` event, and
// several writes may be coalesced into one event. Only an explicit user input
// intent is therefore allowed to change the follow/pinned state.
export function transcriptPinnedAfterScroll(
  pinned: boolean,
  userInitiated: boolean,
  metrics: TranscriptScrollMetrics,
  scrollDelta: number,
): boolean {
  if (!userInitiated || scrollDelta === 0) return pinned;
  // Proximity alone cannot establish intent: a small upward wheel delta is
  // still inside the bottom threshold. Image/layout changes must not turn that
  // deliberate departure into a request to follow again.
  return scrollDelta < 0 ? false : transcriptIsAtBottom(metrics);
}

// A wheel over an embedded page (an HTML render's sandboxed frame) scrolls the
// transcript without ever reaching its wheel listener, so no intent is
// recorded and the next pin pulled the reader straight back down (user
// 2026-10-24). Code never takes a followed reader up and away from the bottom:
// every write records where it left the scroll, the spring only closes on the
// bottom, a shrink clamps to it, and the browser's own anchoring is off. A move
// nobody wrote that ends above the bottom is therefore the reader's.
export function transcriptReaderLeftUnannounced(
  pinned: boolean,
  metrics: TranscriptScrollMetrics,
  unwrittenDelta: number,
): boolean {
  return pinned && unwrittenDelta < 0 && !transcriptIsAtBottom(metrics);
}

export const TRANSCRIPT_BACKGROUND_PIN_DELAY_MS = 100;

type TranscriptPinSchedulerOptions = {
  isPinned: () => boolean;
  isForeground: () => boolean;
  pin: () => void;
  requestFrame: (callback: () => void) => number;
  cancelFrame: (handle: number) => void;
  setTimer: (callback: () => void, delayMs: number) => number;
  clearTimer: (handle: number) => void;
};

export function createTranscriptPinScheduler(options: TranscriptPinSchedulerOptions) {
  let frame = 0;
  let timer = 0;

  const cancelPending = () => {
    if (frame) options.cancelFrame(frame);
    if (timer) options.clearTimer(timer);
    frame = 0;
    timer = 0;
  };

  const runFromFrame = () => {
    frame = 0;
    if (timer) options.clearTimer(timer);
    timer = 0;
    if (options.isPinned()) options.pin();
  };

  const runFromTimer = () => {
    timer = 0;
    if (frame) options.cancelFrame(frame);
    frame = 0;
    if (options.isPinned()) options.pin();
  };

  const schedule = () => {
    if (!options.isPinned()) return;
    if (options.isForeground()) {
      if (!frame) frame = options.requestFrame(runFromFrame);
      return;
    }
    if (!timer) timer = options.setTimer(runFromTimer, TRANSCRIPT_BACKGROUND_PIN_DELAY_MS);
  };

  // Focus/visibility restoration is a lifecycle boundary, not another streamed
  // mutation. Reconcile synchronously so the first visible paint is at bottom.
  const reconcile = () => {
    cancelPending();
    if (options.isPinned()) options.pin();
  };

  return { schedule, reconcile, dispose: cancelPending };
}

type ScrollSpringOptions = {
  el: { scrollTop: number; scrollHeight: number; clientHeight: number };
  isPinned: () => boolean;
  requestFrame: (callback: (now: number) => void) => number;
  cancelFrame: (handle: number) => void;
};

// Follows a streaming reply to its end with a critically damped spring
// (approved demo stream-v3, 2026-10-12). Its speed ramps up and down smoothly
// and a line landing mid-glide extends the motion instead of starting a new
// one — a fresh smooth scroll per finished sentence bobbed. One scrollTop
// write per frame while it moves; it stops by itself, and the moment the
// reader is no longer pinned.
export const TRANSCRIPT_SPRING_STIFFNESS = 7.5; // rad/s: settles in ~0.5 s

export function createScrollSpring({ el, isPinned, requestFrame, cancelFrame }: ScrollSpringOptions) {
  const w = TRANSCRIPT_SPRING_STIFFNESS;
  let frame = 0;
  let last = 0;
  let x = 0;
  let v = 0;
  const stop = () => {
    if (frame) cancelFrame(frame);
    frame = 0;
    last = 0;
    v = 0;
  };
  const step = (now: number) => {
    frame = 0;
    if (!isPinned()) { stop(); return; }
    const dt = last ? Math.min(0.05, (now - last) / 1000) : 1 / 60;
    last = now;
    // Someone else moved it (a jump to the bottom, a send): start from there.
    if (Math.abs(el.scrollTop - x) > 2) { x = el.scrollTop; v = 0; }
    const target = Math.max(0, el.scrollHeight - el.clientHeight);
    v += (w * w * (target - x) - 2 * w * v) * dt;
    x += v * dt;
    if (x >= target) { x = target; v = 0; }
    el.scrollTop = x;
    if (target - x < 0.5 && Math.abs(v) < 4) {
      el.scrollTop = target;
      x = target;
      stop();
      return;
    }
    frame = requestFrame(step);
  };
  return {
    kick() {
      if (frame) return;
      x = el.scrollTop;
      frame = requestFrame(step);
    },
    stop,
  };
}
