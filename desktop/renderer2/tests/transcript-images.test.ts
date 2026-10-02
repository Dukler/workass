import assert from 'node:assert/strict';
import test from 'node:test';
import { observeTranscriptImageOnce } from '../src/transcript-images.ts';

function fixture() {
  let callback: IntersectionObserverCallback;
  let disconnects = 0;
  let loads = 0;
  const element = {} as Element;
  class Observer {
    constructor(cb: IntersectionObserverCallback, options?: IntersectionObserverInit) {
      callback = cb;
      assert.equal(options?.rootMargin, '400px');
    }
    observe(target: Element) { assert.equal(target, element); }
    disconnect() { disconnects++; }
  }
  const dispose = observeTranscriptImageOnce(element, () => { loads++; }, Observer);
  return {
    dispose,
    loads: () => loads,
    disconnects: () => disconnects,
    visibility: (visible: boolean) => callback!([{ isIntersecting: visible } as IntersectionObserverEntry], {} as IntersectionObserver),
  };
}

test('tool images load once on first visibility and retain that state while scrolling', () => {
  const image = fixture();
  image.visibility(false);
  assert.equal(image.loads(), 0);
  image.visibility(true);
  assert.equal(image.loads(), 1);
  assert.equal(image.disconnects(), 1);
  image.visibility(false);
  image.visibility(true);
  assert.equal(image.loads(), 1, 'late observer entries cannot restart image loading');
  image.dispose();
});

test('unmounting before first visibility discards queued observer callbacks', () => {
  const image = fixture();
  image.dispose();
  image.visibility(true);
  assert.equal(image.loads(), 0);
  assert.equal(image.disconnects(), 1);
});

test('a renderer without IntersectionObserver still loads each mounted image', () => {
  let loads = 0;
  const dispose = observeTranscriptImageOnce({} as Element, () => { loads++; }, undefined);
  assert.equal(loads, 1);
  dispose();
});
