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
    if (message.role === 'user' && message.steerState === undefined) return message.id;
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
): boolean {
  return userInitiated ? transcriptIsAtBottom(metrics) : pinned;
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
