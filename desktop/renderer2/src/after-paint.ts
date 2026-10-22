// Settles once what is rendered now has been painted: after the frame that
// paints it and the next one, then the task queue. A hidden window paints no
// frames, so it settles after 100 ms whatever happens; without animation
// frames at all (tests, workers) it settles at once.
export function afterNextPaint(): Promise<void> {
  if (typeof requestAnimationFrame !== 'function') return Promise.resolve();
  return new Promise<void>((resolve) => {
    let settled = false;
    const settle = () => { if (!settled) { settled = true; resolve(); } };
    requestAnimationFrame(() => requestAnimationFrame(() => setTimeout(settle, 0)));
    setTimeout(settle, 100);
  });
}
