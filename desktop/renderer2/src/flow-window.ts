// In-flow windowing inside a transcript row. The transcript windows whole
// messages (transcript-window.ts); a single message can still carry thousands
// of image frames. Each windowed block inside a message registers here, and
// once per animation frame after its scroller moves, every block is placed
// against the viewport and mounts only what is near it.
//
// Placing reads no layout in the common case. A block's position is cached in
// content coordinates (independent of scrolling) and is read again only after
// the scroller's content or size changed (a layout epoch, bumped by a
// ResizeObserver), so scrolling past a thousand blocks costs arithmetic. All
// reads of a frame come before its writes, so a frame forces layout at most
// once.
//
// Blocks stand in for what they do not mount with boxes of exactly its
// height, so mounting and unmounting moves nothing on screen.

/** Mounted beyond each edge of the viewport (the transcript's own overscan). */
export const FLOW_OVERSCAN_PX = 1600;

/** The visible band of a scroller, in viewport coordinates. */
export type FlowView = { top: number; bottom: number };

export interface FlowBlock {
  element(): Element | null;
  /**
   * Decide what the block needs, given its own top (viewport coordinates) and
   * whether layout may have changed since it was last placed (re-read any
   * size it caches). Read only; returns the write to apply, or null.
   */
  place(top: number, view: FlowView, layoutChanged: boolean): (() => void) | null;
}

type Cached = { epoch: number; contentTop: number };

type Tracker = {
  blocks: Map<FlowBlock, Cached>;
  epoch: number;
  frame: number | null;
  /** The scroll position and layout epoch blocks were last placed for. */
  placedAt: number;
  placedEpoch: number;
  /** The scroller's band, read once per layout epoch. */
  view: FlowView | null;
  viewEpoch: number;
  detach: () => void;
};

const trackers = new Map<Element, Tracker>();

export function flowScroller(element: Element | null): Element | null {
  return element?.closest?.('.transcriptviewport > .scroll') ?? null;
}

export function flowView(scroller: Element | null): FlowView {
  if (!scroller) {
    const height = typeof window !== 'undefined' ? window.innerHeight : 0;
    return { top: 0, bottom: height };
  }
  const rect = scroller.getBoundingClientRect();
  return { top: rect.top, bottom: rect.bottom };
}

let applyWrites: (writes: Array<() => void>) => void = (writes) => { for (const write of writes) write(); };

/** How frame-time writes are committed (the renderer passes React's flushSync). */
export function setFlowWriteApplier(next: (writes: Array<() => void>) => void): void {
  applyWrites = next;
}

/**
 * Place every block once: cached positions where layout is unchanged, one
 * read per block otherwise; then apply every write together.
 */
export function placeFlowBlocks(
  blocks: Map<FlowBlock, Cached>, epoch: number, scrollTop: number, view: FlowView,
  apply: (writes: Array<() => void>) => void,
): void {
  const writes: Array<() => void> = [];
  for (const [block, cached] of blocks) {
    const changed = cached.epoch !== epoch;
    if (changed) {
      const element = block.element();
      if (!element) continue;
      cached.contentTop = element.getBoundingClientRect().top - view.top + scrollTop;
      cached.epoch = epoch;
    }
    const write = block.place(cached.contentTop - scrollTop + view.top, view, changed);
    if (write) writes.push(write);
  }
  if (writes.length) apply(writes);
}

function trackerFor(scroller: Element): Tracker {
  let tracker = trackers.get(scroller);
  if (tracker) return tracker;
  const created: Tracker = {
    blocks: new Map(), epoch: 1, frame: null, placedAt: Number.NaN, placedEpoch: 0, view: null, viewEpoch: 0, detach: () => {},
  };
  const run = () => {
    created.frame = null;
    placeTracked(scroller, created, applyWrites);
  };
  const schedule = () => {
    if (created.frame !== null || typeof requestAnimationFrame !== 'function') return;
    created.frame = requestAnimationFrame(run);
  };
  const relayout = () => { created.epoch++; schedule(); };
  scroller.addEventListener('scroll', schedule, { passive: true });
  // The scroller's own size (a window or rail resize) and its content's size
  // (rows above growing or shrinking) are the layout changes that move blocks.
  const resize = typeof ResizeObserver === 'function' ? new ResizeObserver(relayout) : null;
  resize?.observe(scroller);
  if (scroller.firstElementChild) resize?.observe(scroller.firstElementChild);
  created.detach = () => {
    scroller.removeEventListener('scroll', schedule);
    resize?.disconnect();
    if (created.frame !== null && typeof cancelAnimationFrame === 'function') cancelAnimationFrame(created.frame);
    created.frame = null;
  };
  trackers.set(scroller, created);
  return created;
}

function trackedView(scroller: Element, tracker: Tracker): FlowView {
  if (!tracker.view || tracker.viewEpoch !== tracker.epoch) {
    tracker.view = flowView(scroller);
    tracker.viewEpoch = tracker.epoch;
  }
  return tracker.view;
}

function placeTracked(scroller: Element, tracker: Tracker, apply: (writes: Array<() => void>) => void): void {
  const scrollTop = scroller.scrollTop;
  tracker.placedAt = scrollTop;
  tracker.placedEpoch = tracker.epoch;
  placeFlowBlocks(tracker.blocks, tracker.epoch, scrollTop, trackedView(scroller, tracker), apply);
}

/** Follow a scroller for one block; returns the unsubscribe. */
export function trackFlowBlock(scroller: Element | null, block: FlowBlock): () => void {
  if (!scroller) return () => {};
  const tracker = trackerFor(scroller);
  tracker.blocks.set(block, { epoch: 0, contentTop: 0 });
  return () => {
    tracker.blocks.delete(block);
    if (!tracker.blocks.size) {
      tracker.detach();
      trackers.delete(scroller);
    }
  };
}

/**
 * Place a scroller's blocks now, from a React layout effect, so they are right
 * in the frame about to paint. `relayout`: the commit may have moved content
 * (every position is read again). Otherwise it only places them for a scroll
 * position they were not placed for yet (a jump to the newest row), which is
 * arithmetic. The writes are plain state updates React commits before paint.
 */
export function syncFlowBlocks(scroller: Element | null, relayout: boolean): void {
  if (!scroller) return;
  const tracker = trackers.get(scroller);
  if (!tracker || !tracker.blocks.size) return;
  if (relayout) tracker.epoch++;
  else if (tracker.placedEpoch === tracker.epoch && Math.abs(scroller.scrollTop - tracker.placedAt) < 1) return;
  placeTracked(scroller, tracker, (writes) => { for (const write of writes) write(); });
}
