import test from 'node:test';
import assert from 'node:assert/strict';
import {
  autosizeComposerTextarea,
  observeComposerTextareaWidth,
} from '../src/composer-autosize.ts';

function textarea(overrides: Record<string, unknown> = {}) {
  const classes = new Map<string, boolean>();
  const el = {
    value: 'wrapped draft',
    scrollHeight: 101,
    scrollTop: 0,
    style: { height: '', overflowY: '' },
    parentElement: { classList: { toggle: (name: string, on: boolean) => classes.set(name, on) } },
    getBoundingClientRect: () => ({ width: 786 }),
    ...overrides,
  };
  return { el: el as unknown as HTMLTextAreaElement, classes };
}

test('autosize fits the complete draft until the cap and collapses only when empty', () => {
  const normal = textarea();
  autosizeComposerTextarea(normal.el);
  assert.equal(normal.el.style.height, '101px');
  assert.equal(normal.el.style.overflowY, 'hidden');

  const capped = textarea({ scrollHeight: 240, scrollTop: 12 });
  autosizeComposerTextarea(capped.el);
  assert.equal(capped.el.style.height, '180px');
  assert.equal(capped.el.style.overflowY, 'auto');
  assert.equal(capped.classes.get('scrolled'), true);

  const empty = textarea({ value: '', scrollHeight: 80 });
  autosizeComposerTextarea(empty.el);
  assert.equal(empty.el.style.height, '');
  assert.equal(empty.el.style.overflowY, 'hidden');
});

test('a post-switch width change triggers one fresh draft measurement', () => {
  let callback!: ResizeObserverCallback;
  let observed: Element | null = null;
  let disconnected = false;
  class FakeObserver {
    constructor(cb: ResizeObserverCallback) { callback = cb; }
    observe(target: Element) { observed = target; }
    disconnect() { disconnected = true; }
  }

  const subject = textarea();
  let resized = 0;
  const stop = observeComposerTextareaWidth(
    subject.el,
    () => { resized++; },
    FakeObserver as unknown as new (cb: ResizeObserverCallback) => ResizeObserver,
  );

  assert.equal(observed, subject.el);
  callback([{ target: subject.el, contentRect: { width: 786 } } as ResizeObserverEntry], {} as ResizeObserver);
  assert.equal(resized, 0, 'the observer initial delivery is not a second measurement');

  callback([{ target: subject.el, contentRect: { width: 1012 } } as ResizeObserverEntry], {} as ResizeObserver);
  assert.equal(resized, 1, 'the selected chat layout width is measured after it lands');

  callback([{ target: subject.el, contentRect: { width: 1012 } } as ResizeObserverEntry], {} as ResizeObserver);
  assert.equal(resized, 1, 'height-only observer delivery cannot create a resize loop');

  stop();
  assert.equal(disconnected, true);
});
