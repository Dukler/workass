import { createRoot, type Root } from 'react-dom/client';

// React's createRoot listens on the container for every DOM event it knows,
// `animationiteration` included, whether or not any component uses it. Chromium
// cannot leave an animation to the compositor alone while a page listens for
// that event: it must wake the main thread to see where each running CSS
// animation's iteration ends, so every looping indicator (the Working orbit and
// dots, the sidebar forge, the working pill) cost a style recalc, paint and
// commit on the main thread several times a second each, about 19 a second
// with one turn running. No component handles onAnimationIteration
// (tests/react-root.test.ts keeps it so), so the app's roots skip that one
// listener and the indicators stay on the compositor (energy lane, 2026-11-13).
// Only the listener React adds inside createRoot is skipped; any later
// addEventListener('animationiteration') works as usual.
export const SKIPPED_ROOT_EVENTS: ReadonlySet<string> = new Set(['animationiteration']);

type AddEventListener = EventTarget['addEventListener'];

export function withoutRootListeners<T>(proto: { addEventListener: AddEventListener }, create: () => T): T {
  const add = proto.addEventListener;
  proto.addEventListener = function (this: EventTarget, type: string, listener: EventListenerOrEventListenerObject | null, options?: boolean | AddEventListenerOptions) {
    if (SKIPPED_ROOT_EVENTS.has(type)) return;
    return add.call(this, type, listener, options);
  } as AddEventListener;
  try {
    return create();
  } finally {
    proto.addEventListener = add;
  }
}

export function createAppRoot(container: Element | DocumentFragment): Root {
  return withoutRootListeners(EventTarget.prototype, () => createRoot(container));
}
