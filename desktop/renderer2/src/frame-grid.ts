// The looping status animations (the Working orbit and dots, the sidebar forge,
// spinners, the background-work and fork pulses) and the streaming fade are
// stepped: they change only every 1/30 s from their start (app.css,
// tools/stepped-keyframes.mjs). A CSS
// animation starts on whatever frame its element appears, so two of them, say
// the Working line's orbit and a sidebar row's forge, would change on
// different frames, and the compositor would present each set: up to 60 frames
// a second instead of 30. When one starts, its start time is moved onto the
// document timeline's shared 1/30 s grid, so they all change on the same
// frames. The shift is under 17 ms and nothing else about them changes
// (energy lane, 2026-11-12).

export const FRAME_GRID_MS = 1000 / 30;
const STEPPED = /^(?:workorbit|workspin|conn-spin|workdot-\d|fg-[\w-]+|wdot-ring|fk-ping|cb-pulse|mdfresh)$/;

export function isSteppedAnimation(name: string): boolean {
  return STEPPED.test(name);
}

export function alignToFrameGrid(time: number): number {
  return Math.round(time / FRAME_GRID_MS) * FRAME_GRID_MS;
}

type Started = { animationName: string; target: EventTarget | null; pseudoElement?: string };

// Lines up the animation that `event` reports. Returns how many were moved.
export function alignStartedAnimation(doc: Pick<Document, 'getAnimations'>, event: Started): number {
  if (!isSteppedAnimation(event.animationName)) return 0;
  let moved = 0;
  for (const animation of doc.getAnimations()) {
    if ((animation as CSSAnimation).animationName !== event.animationName) continue;
    const effect = animation.effect as KeyframeEffect | null;
    if (!effect || effect.target !== event.target) continue;
    if ((effect.pseudoElement || '') !== (event.pseudoElement || '')) continue;
    const start = animation.startTime;
    if (typeof start !== 'number') continue;
    const aligned = alignToFrameGrid(start);
    if (Math.abs(aligned - start) > 0.01) { animation.startTime = aligned; moved++; }
  }
  return moved;
}

export function installFrameGrid(doc: Document = document): () => void {
  if (typeof doc.getAnimations !== 'function') return () => {};
  const onStart = (event: Event) => { alignStartedAnimation(doc, event as AnimationEvent); };
  doc.addEventListener('animationstart', onStart, true);
  return () => doc.removeEventListener('animationstart', onStart, true);
}
