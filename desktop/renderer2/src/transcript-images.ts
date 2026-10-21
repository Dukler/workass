type ImageVisibilityObserver = Pick<IntersectionObserver, 'observe' | 'disconnect'>;
type ImageVisibilityObserverConstructor = new (
  callback: IntersectionObserverCallback,
  options?: IntersectionObserverInit,
) => ImageVisibilityObserver;

// Loading is a one-way transition for a mounted transcript image. Clearing its
// source when it leaves the viewport also collapses its height and changes the
// reader's scroll position, repeatedly fetching the same image on the way back.
export function observeTranscriptImageOnce(
  element: Element,
  onVisible: () => void,
  Observer: ImageVisibilityObserverConstructor | undefined = globalThis.IntersectionObserver,
): () => void {
  let active = true;
  let revealed = false;
  let observer: ImageVisibilityObserver | undefined;
  const reveal = () => {
    if (!active || revealed) return;
    revealed = true;
    observer?.disconnect();
    onVisible();
  };
  if (Observer) {
    observer = new Observer((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) reveal();
    }, { rootMargin: '400px' });
    observer.observe(element);
  } else {
    reveal();
  }
  return () => { active = false; observer?.disconnect(); };
}


// ── The load gate: don't fetch what the reader flies past ──────────────────
// A fast climb through a screenshot chat mounted thousands of images on the
// way, and each one that brushed the viewport started a read (1,308 reads, 816
// MB, for one climb of a 225 MB chat). An image now starts loading only when
// it is near the viewport AND the transcript has been still for a moment;
// one that leaves before its bytes are in is cancelled (an <img> that loses
// its src aborts its fetch; a queued socket read is dropped). The reserved
// frame keeps the image's size the whole time, so cancelling moves nothing.
//
// "Near" is measured against the transcript's own scroller when there is one:
// with the viewport as root, an intermediate scroller clips the margin away.
// The gate also marks a near frame with data-near, so only frames the reader
// can reach run the pending accent line: one message of that chat mounted
// ~6,000 pending frames, each running its own infinite animation, and the
// main thread spent most of a 97 s climb servicing them.
export const IMAGE_SETTLE_MS = 140;
export const IMAGE_NEAR_MARGIN = '400px';
export const IMAGE_NEAR_ATTR = 'data-near';

export interface TranscriptImageHandlers {
  /** Near the viewport and the scroll has settled: begin loading. */
  start(): void;
  /** Left before it finished loading: stop. start() may run again later. */
  cancel(): void;
}

type GateObserver = ImageVisibilityObserver & { unobserve?(target: Element): void };
type GateObserverConstructor = new (
  callback: IntersectionObserverCallback,
  options?: IntersectionObserverInit,
) => GateObserver;

export interface ImageLoadGateEnv {
  now(): number;
  setTimer(fn: () => void, ms: number): unknown;
  clearTimer(handle: unknown): void;
  Observer?: GateObserverConstructor;
  /** The scroller an element is measured against; null for the viewport. */
  rootOf?(element: Element): Element | null;
  /** Calls back on every scroll anywhere in the document; returns an unsubscribe. */
  onScroll?(fn: () => void): () => void;
}

type Watch = { near: boolean; started: boolean; root: Element | null; handlers: TranscriptImageHandlers };

export interface ImageLoadGate {
  watch(element: Element, handlers: TranscriptImageHandlers): { loaded(): void; stop(): void };
  /** Watched images, near ones and started ones (tests, diagnostics). */
  counts(): { watched: number; near: number; started: number };
}

function markNear(element: Element, near: boolean) {
  const target = element as Element & { toggleAttribute?(name: string, force?: boolean): boolean };
  target.toggleAttribute?.(IMAGE_NEAR_ATTR, near);
}

export function createImageLoadGate(env: ImageLoadGateEnv): ImageLoadGate {
  const watches = new Map<Element, Watch>();
  const observers = new Map<Element | null, { observer: GateObserver; count: number }>();
  let lastScroll = Number.NEGATIVE_INFINITY;
  let timer: unknown = null;
  let unsubscribeScroll: (() => void) | null = null;

  const flush = () => {
    timer = null;
    const wait = lastScroll + IMAGE_SETTLE_MS - env.now();
    if (wait > 0) { timer = env.setTimer(flush, wait); return; }
    for (const watch of Array.from(watches.values())) {
      if (!watch.near || watch.started) continue;
      watch.started = true;
      watch.handlers.start();
    }
  };
  const schedule = () => {
    if (timer !== null) return;
    timer = env.setTimer(flush, Math.max(0, lastScroll + IMAGE_SETTLE_MS - env.now()));
  };
  const setNear = (element: Element, near: boolean) => {
    const watch = watches.get(element);
    if (!watch) return;
    markNear(element, near);
    if (watch.near === near) return;
    watch.near = near;
    if (near) { if (!watch.started) schedule(); return; }
    if (watch.started) { watch.started = false; watch.handlers.cancel(); }
  };
  const observerFor = (root: Element | null): GateObserver | null => {
    if (!env.Observer) return null;
    let entry = observers.get(root);
    if (!entry) {
      const observer = new env.Observer((entries) => {
        for (const item of entries) setNear(item.target, item.isIntersecting);
      }, { root, rootMargin: IMAGE_NEAR_MARGIN });
      entry = { observer, count: 0 };
      observers.set(root, entry);
    }
    if (!unsubscribeScroll) unsubscribeScroll = env.onScroll?.(() => { lastScroll = env.now(); }) ?? null;
    entry.count++;
    return entry.observer;
  };
  const release = (element: Element) => {
    const watch = watches.get(element);
    if (!watch) return;
    watches.delete(element);
    const entry = observers.get(watch.root);
    if (entry) {
      entry.observer.unobserve?.(element);
      if (--entry.count <= 0) { entry.observer.disconnect(); observers.delete(watch.root); }
    }
    if (!watches.size) {
      unsubscribeScroll?.();
      unsubscribeScroll = null;
      if (timer !== null) { env.clearTimer(timer); timer = null; }
    }
  };

  return {
    watch(element, handlers) {
      release(element);
      const watch: Watch = { near: false, started: false, root: env.rootOf?.(element) ?? null, handlers };
      watches.set(element, watch);
      const observer = observerFor(watch.root);
      if (observer) observer.observe(element);
      else setNear(element, true); // No IntersectionObserver: every mounted image loads.
      return {
        // Loaded is final: the image stays, wherever the reader goes.
        loaded: () => { if (watches.get(element) === watch) release(element); },
        stop: () => { if (watches.get(element) === watch) release(element); },
      };
    },
    counts() {
      let near = 0; let started = 0;
      for (const watch of watches.values()) { if (watch.near) near++; if (watch.started) started++; }
      return { watched: watches.size, near, started };
    },
  };
}

let defaultGate: ImageLoadGate | null = null;

/** The transcript's shared gate (one observer per scroller for every mounted image). */
export function transcriptImageGate(): ImageLoadGate {
  if (defaultGate) return defaultGate;
  const doc = typeof document !== 'undefined' ? document : undefined;
  defaultGate = createImageLoadGate({
    now: () => (typeof performance !== 'undefined' ? performance.now() : Date.now()),
    setTimer: (fn, ms) => setTimeout(fn, ms),
    clearTimer: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
    Observer: globalThis.IntersectionObserver,
    rootOf: (element) => element.closest?.('.transcriptviewport > .scroll') ?? null,
    // Scroll events do not bubble; a capturing listener on the document sees
    // the transcript's (and every other scroller's) scrolls.
    onScroll: doc ? (fn) => {
      const options = { capture: true, passive: true } as const;
      doc.addEventListener('scroll', fn, options);
      return () => doc.removeEventListener('scroll', fn, options);
    } : undefined,
  });
  return defaultGate;
}
