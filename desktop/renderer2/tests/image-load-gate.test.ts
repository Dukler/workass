import assert from 'node:assert/strict';
import test from 'node:test';
import { createImageLoadGate, IMAGE_NEAR_ATTR, IMAGE_NEAR_MARGIN, IMAGE_SETTLE_MS } from '../src/transcript-images.ts';

// The gate decides when a transcript image may load: near the viewport, once
// the scroll has been still for a moment. A fast climb through a screenshot
// chat flew past thousands of images and fetched each one it brushed.
function fixture() {
  let clock = 0;
  let timers: Array<{ at: number; fn: () => void; id: number }> = [];
  let nextId = 1;
  let scroll: (() => void) | null = null;
  const observers: Array<{ callback: IntersectionObserverCallback; options?: IntersectionObserverInit; observed: Set<Element>; disconnected: boolean }> = [];
  class Observer {
    record: (typeof observers)[number];
    constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
      this.record = { callback, options, observed: new Set(), disconnected: false };
      observers.push(this.record);
    }
    observe(target: Element) { this.record.observed.add(target); }
    unobserve(target: Element) { this.record.observed.delete(target); }
    disconnect() { this.record.disconnected = true; this.record.observed.clear(); }
  }
  const gate = createImageLoadGate({
    now: () => clock,
    setTimer: (fn, ms) => { const id = nextId++; timers.push({ at: clock + ms, fn, id }); return id; },
    clearTimer: (id) => { timers = timers.filter((timer) => timer.id !== id); },
    Observer,
    rootOf: (element) => (element as unknown as { root?: Element }).root ?? null,
    onScroll: (fn) => { scroll = fn; return () => { scroll = null; }; },
  });
  const advance = (ms: number) => {
    clock += ms;
    for (;;) {
      const due = timers.filter((timer) => timer.at <= clock).sort((a, b) => a.at - b.at)[0];
      if (!due) return;
      timers = timers.filter((timer) => timer !== due);
      due.fn();
    }
  };
  const element = (root?: Element) => {
    const attrs = new Set<string>();
    return {
      root,
      attrs,
      toggleAttribute(name: string, force?: boolean) { if (force) attrs.add(name); else attrs.delete(name); return !!force; },
    } as unknown as Element & { attrs: Set<string> };
  };
  const visibility = (target: Element, visible: boolean) => {
    const record = observers.find((item) => item.observed.has(target))!;
    record.callback([{ target, isIntersecting: visible } as IntersectionObserverEntry], {} as IntersectionObserver);
  };
  return { gate, advance, element, visibility, observers, scrolled: () => scroll?.() };
}

function handlers() {
  const log: string[] = [];
  return { log, start: () => log.push('start'), cancel: () => log.push('cancel') };
}

test('a near image starts once the scroll has settled, a flown-past one never starts', () => {
  const f = fixture();
  const flown = f.element();
  const stays = f.element();
  const a = handlers();
  const b = handlers();
  f.gate.watch(flown, a);
  f.gate.watch(stays, b);
  f.scrolled();
  f.visibility(flown, true);
  f.advance(40);
  f.scrolled();
  f.visibility(flown, false); // gone before the scroll settled
  f.visibility(stays, true);
  f.advance(IMAGE_SETTLE_MS - 1);
  assert.deepEqual(b.log, [], 'still settling');
  f.advance(1);
  assert.deepEqual(a.log, [], 'the image the reader flew past never loaded');
  assert.deepEqual(b.log, ['start']);
  assert.equal(f.observers[0].options?.rootMargin, IMAGE_NEAR_MARGIN);
});

test('scrolling keeps postponing; an image visible with no scroll starts at once', () => {
  const f = fixture();
  const image = f.element();
  const h = handlers();
  f.gate.watch(image, h);
  f.visibility(image, true);
  f.advance(0);
  assert.deepEqual(h.log, ['start'], 'nothing is moving: load now');
  const later = f.element();
  const g = handlers();
  f.gate.watch(later, g);
  f.visibility(later, true);
  for (let i = 0; i < 10; i++) { f.scrolled(); f.advance(50); }
  assert.deepEqual(g.log, [], 'a reader still scrolling loads nothing new');
  f.advance(IMAGE_SETTLE_MS);
  assert.deepEqual(g.log, ['start']);
});

test('leaving before the bytes arrive cancels; coming back starts again; loaded is final', () => {
  const f = fixture();
  const image = f.element();
  const h = handlers();
  const watch = f.gate.watch(image, h);
  f.visibility(image, true);
  f.advance(IMAGE_SETTLE_MS);
  f.visibility(image, false);
  assert.deepEqual(h.log, ['start', 'cancel']);
  f.visibility(image, true);
  f.advance(IMAGE_SETTLE_MS);
  assert.deepEqual(h.log, ['start', 'cancel', 'start']);
  watch.loaded();
  assert.equal(f.observers[0].observed.size, 0, 'a loaded image is no longer observed');
  assert.deepEqual(f.gate.counts(), { watched: 0, near: 0, started: 0 });
  assert.equal(f.observers[0].disconnected, true, 'the last watch releases the observer');
});

test('near frames are marked, so only they run the pending accent line', () => {
  const f = fixture();
  const image = f.element();
  f.gate.watch(image, handlers());
  assert.equal((image as unknown as { attrs: Set<string> }).attrs.has(IMAGE_NEAR_ATTR), false);
  f.visibility(image, true);
  assert.equal((image as unknown as { attrs: Set<string> }).attrs.has(IMAGE_NEAR_ATTR), true);
  f.visibility(image, false);
  assert.equal((image as unknown as { attrs: Set<string> }).attrs.has(IMAGE_NEAR_ATTR), false);
});

test('images are measured against their own scroller, one observer per scroller', () => {
  const f = fixture();
  const scroller = {} as Element;
  f.gate.watch(f.element(scroller), handlers());
  f.gate.watch(f.element(scroller), handlers());
  f.gate.watch(f.element(), handlers());
  assert.equal(f.observers.length, 2);
  assert.equal(f.observers[0].options?.root, scroller);
  assert.equal(f.observers[0].observed.size, 2);
  assert.equal(f.observers[1].options?.root, null);
});

test('without IntersectionObserver every mounted image loads', () => {
  const started: string[] = [];
  const gate = createImageLoadGate({ now: () => 0, setTimer: (fn) => { fn(); return 1; }, clearTimer: () => {} });
  gate.watch({} as Element, { start: () => started.push('start'), cancel: () => started.push('cancel') });
  assert.deepEqual(started, ['start']);
});
