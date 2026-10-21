// Transcript windowing (after t3code's virtualized timeline, without a list
// library). Every resident message stays in the store, but only the units
// around the viewport are mounted; spacers stand in for the rest. A unit is a
// message or an assistant turn block, identified by a stable key.
//
// Position is owned by an ANCHOR, not by scrollTop: the first unit at the top
// of the viewport and its offset from that edge. The window is cut around the
// anchor, and after every commit the anchor is put back where the reader saw
// it, so prepending older pages, replacing estimates with measured heights, or
// sliding the window never moves what is being read. A pinned reader has no
// anchor: the window is the newest units and the bottom is followed as before.

/** Rendered beyond each edge of the viewport. */
export const TRANSCRIPT_OVERSCAN_PX = 1600;
/** The rendered window is cut again once less than this remains past an edge. */
export const TRANSCRIPT_REWINDOW_SLACK_PX = 600;
/** Older history loads once the reader is this close to the oldest loaded row. */
export const TRANSCRIPT_LOAD_AHEAD_PX = 2400;
/** A unit never measured yet. Corrected on mount, before paint. */
export const TRANSCRIPT_ESTIMATED_UNIT_PX = 240;

export type TranscriptAnchor = {
  key: string;
  /** Unit top minus viewport top, in px. Negative when it began above. */
  offset: number;
};

export type TranscriptWindow = {
  from: number;
  to: number;
  topPad: number;
  bottomPad: number;
};

export type TranscriptWindowInput = {
  keys: readonly string[];
  heights: ReadonlyMap<string, number>;
  viewport: number;
  overscan?: number;
  /** null: pinned to the newest unit. */
  anchor: TranscriptAnchor | null;
  estimate?: number;
};

export function unitHeight(heights: ReadonlyMap<string, number>, key: string, estimate = TRANSCRIPT_ESTIMATED_UNIT_PX): number {
  const known = heights.get(key);
  return known !== undefined && Number.isFinite(known) && known >= 0 ? known : estimate;
}

export function transcriptWindow(input: TranscriptWindowInput): TranscriptWindow {
  const { keys, heights, anchor } = input;
  const viewport = Math.max(0, input.viewport);
  const overscan = Math.max(0, input.overscan ?? TRANSCRIPT_OVERSCAN_PX);
  const estimate = input.estimate ?? TRANSCRIPT_ESTIMATED_UNIT_PX;
  const count = keys.length;
  const height = (index: number) => unitHeight(heights, keys[index], estimate);
  let from: number;
  let to: number;
  const at = anchor ? keys.indexOf(anchor.key) : -1;
  if (at < 0) {
    // Pinned (or the anchor left the resident history): the newest units.
    to = count;
    from = count;
    let covered = 0;
    while (from > 0 && covered < viewport + overscan) covered += height(--from);
  } else {
    const offset = anchor!.offset;
    from = at;
    let above = 0;
    while (from > 0 && above < overscan + Math.max(0, offset)) above += height(--from);
    to = at;
    let below = 0;
    while (to < count && below < viewport - offset + overscan) below += height(to++);
  }
  let topPad = 0;
  for (let index = 0; index < from; index++) topPad += height(index);
  let bottomPad = 0;
  for (let index = to; index < count; index++) bottomPad += height(index);
  return { from, to, topPad, bottomPad };
}

/**
 * Whether the mounted window still covers the reader. It is cut again only
 * when an edge comes within the slack, or when it grew far past what the
 * viewport needs (so a long scroll does not keep everything mounted).
 */
export function transcriptWindowStale(rendered: Pick<TranscriptWindow, 'from' | 'to'>, input: TranscriptWindowInput): boolean {
  const slack = transcriptWindow({ ...input, overscan: TRANSCRIPT_REWINDOW_SLACK_PX });
  if (slack.from < rendered.from || slack.to > rendered.to) return true;
  const wide = transcriptWindow({ ...input, overscan: (input.overscan ?? TRANSCRIPT_OVERSCAN_PX) * 3 });
  return rendered.from < wide.from || rendered.to > wide.to;
}

export type MeasuredUnit = { key: string; top: number; bottom: number };

/**
 * A unit's height is its pitch: top to the next unit's top, so collapsed
 * margins between messages count exactly once. The last mounted unit takes
 * its box plus the median gap seen between the others.
 */
export function recordUnitHeights(heights: Map<string, number>, units: readonly MeasuredUnit[]): void {
  if (!units.length) return;
  const gaps: number[] = [];
  for (let index = 0; index + 1 < units.length; index++) {
    const pitch = units[index + 1].top - units[index].top;
    if (pitch >= 0) heights.set(units[index].key, pitch);
    gaps.push(Math.max(0, units[index + 1].top - units[index].bottom));
  }
  gaps.sort((a, b) => a - b);
  const gap = gaps.length ? gaps[Math.floor(gaps.length / 2)] : 0;
  const last = units[units.length - 1];
  heights.set(last.key, Math.max(0, last.bottom - last.top) + gap);
}

/** The first unit still visible at the viewport's top edge. */
export function transcriptAnchorAt(units: readonly MeasuredUnit[], viewportTop: number): TranscriptAnchor | null {
  for (const unit of units) {
    if (unit.bottom > viewportTop + 1) return { key: unit.key, offset: unit.top - viewportTop };
  }
  const last = units[units.length - 1];
  return last ? { key: last.key, offset: last.top - viewportTop } : null;
}

/**
 * Messages appended after the newest one the reader saw pinned. An identity,
 * not a count: older history loading above the reader is never "new".
 */
export function newMessagesSince(messages: readonly { id: string }[], seenId: string | null): number {
  if (!seenId) return 0;
  for (let index = messages.length - 1; index >= 0; index--) {
    if (messages[index].id === seenId) return messages.length - 1 - index;
  }
  return 0;
}

// ---- per-chat view memory --------------------------------------------------
// Switching back to a chat restores how it was left: pinned chats stay pinned
// to the newest row, a reader in the middle returns to the same row and
// offset. Heights travel with the view, so the restored window is exact.

export type TranscriptView = {
  pinned: boolean;
  anchor: TranscriptAnchor | null;
  heights: Map<string, number>;
  /** Messages from the anchor to the newest one, inclusive, when it was left. */
  depth?: number;
  /** The newest message when it was left. */
  newestId?: string | null;
};

/**
 * A remembered reading position on its way back. Until it is restored nothing
 * may overwrite it: every scroll and layout pass would otherwise record the
 * row a fresh mount happens to show at the top (the first loaded row, at
 * scrollTop 0) in its place.
 */
export type TranscriptRestore = {
  anchor: TranscriptAnchor;
  depth?: number;
  newestId?: string | null;
};

/** Older history loaded past the remembered depth before a missing row counts as gone. */
export const TRANSCRIPT_RESTORE_SLACK = 60;

/**
 * restore: the row is loaded, put it back where it was read.
 * load: it is older than what is loaded; bring in the next older page.
 * wait: a page is already on its way.
 * abandon: it is gone (history complete, a load found nothing, or history
 *   reaches past where it was without it); reopen at the newest row.
 */
export type TranscriptRestoreStep = 'restore' | 'load' | 'wait' | 'abandon';

export function transcriptRestoreStep(input: {
  messages: readonly { id: string }[];
  restore: TranscriptRestore;
  historyComplete: boolean;
  loading: boolean;
  stalled: boolean;
}): TranscriptRestoreStep {
  const { messages, restore } = input;
  if (messages.some((message) => message.id === restore.anchor.key)) return 'restore';
  if (input.historyComplete || input.stalled) return 'abandon';
  if (restore.depth !== undefined) {
    // Rows appended since leaving push the anchor further from the end.
    const appended = newMessagesSince(messages, restore.newestId ?? null);
    if (messages.length >= restore.depth + appended + TRANSCRIPT_RESTORE_SLACK) return 'abandon';
  }
  return input.loading ? 'wait' : 'load';
}

/** The scrollTop that puts a row whose top is at `rowTop` back at `offset` from the viewport top. */
export function transcriptRestoredScrollTop(scrollTop: number, rowTop: number, viewportTop: number, offset: number): number {
  return scrollTop + (rowTop - viewportTop) - offset;
}

/** How deep a reader is: messages from the anchor row to the newest, inclusive. */
export function transcriptAnchorDepth(messages: readonly { id: string }[], key: string): number | undefined {
  for (let index = messages.length - 1; index >= 0; index--) {
    if (messages[index].id === key) return messages.length - index;
  }
  return undefined;
}

export const TRANSCRIPT_VIEW_MEMORY = 24;

const views = new Map<string, TranscriptView>();

export function rememberTranscriptView(chatKey: string, view: TranscriptView): void {
  if (!chatKey) return;
  views.delete(chatKey);
  views.set(chatKey, view);
  while (views.size > TRANSCRIPT_VIEW_MEMORY) views.delete(views.keys().next().value as string);
}

export function recallTranscriptView(chatKey: string): TranscriptView {
  const saved = chatKey ? views.get(chatKey) : undefined;
  return saved ?? { pinned: true, anchor: null, heights: new Map() };
}

export function forgetTranscriptViews(): void {
  views.clear();
}
