// Streaming re-renders land on the indicators' 30 fps beat (energy lane,
// 2026-11-13). A running reply gets a chunk every ~30 ms and each one used to
// commit on whatever display frame came next, so a streaming window drew
// about 50 main-thread frames a second next to the Working line's 30
// compositor frames. Each commit now waits for the next point of the shared
// 1/30 s grid (frame-grid.ts), where the stepped indicators change anyway, so
// text and indicators share one frame: at most 30 a second. The first update
// after a pause still renders at once, so a reply's first words, a tool that
// starts or a permission card show on the very next frame. While the window
// is hidden nothing is painted; updates gather for up to a second and flush
// the moment the window shows.
// The same 1/30 s grid as frame-grid.ts FRAME_GRID_MS (a test pins they agree).
export const BEAT_MS = 1000 / 30;

export interface BeatEnv {
  now(): number;
  hidden(): boolean;
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(id: unknown): void;
}

export const HIDDEN_BEAT_MS = 1000;
// Lands just after the grid point, so the commit's frame is the one in which
// the stepped animations change.
const AFTER_GRID_MS = 1;

export interface Beat {
  request(topic: string): void;
  shown(): void;
  pending(): number;
}

export function createBeat(flush: (topics: string[]) => void, env: BeatEnv): Beat {
  const topics = new Set<string>();
  let timer: unknown = null;
  let lastFlush = -Infinity;
  const run = () => {
    timer = null;
    lastFlush = env.now();
    const ready = [...topics];
    topics.clear();
    if (ready.length) flush(ready);
  };
  return {
    request(topic) {
      topics.add(topic);
      if (timer !== null) return;
      const now = env.now();
      if (env.hidden()) { timer = env.setTimeout(run, HIDDEN_BEAT_MS); return; }
      if (now - lastFlush >= 2 * BEAT_MS) { run(); return; }
      const next = Math.floor(now / BEAT_MS) * BEAT_MS + BEAT_MS + AFTER_GRID_MS;
      timer = env.setTimeout(run, Math.max(0, next - now));
    },
    shown() {
      if (timer === null) return;
      env.clearTimeout(timer);
      run();
    },
    pending: () => topics.size,
  };
}
