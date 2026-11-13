// One shared clock for every live time label, ticking on the wall clock's
// second boundaries. Every "12s" counter (the Working line, the sidebar's
// working row, the reply stamp, running tool groups and cards) used to run its
// own setInterval from whenever it mounted, so three counters repainted on
// three different frames each second. On one timer they change together: one
// style, layout and paint pass, and one presented frame, a second. A label that
// changes less often (a stamp that says "hace 3 min") asks for its next change
// only, so an idle window stops waking up every second (energy lane,
// 2026-11-12). The tick lands just after the boundary, so a counter turns over
// when the clock does.

type Listener = () => void;
// When a listener next needs to run, from the current time (ms since epoch).
type NextDue = (now: number) => number;

const listeners = new Map<Listener, { nextDue: NextDue; due: number }>();
let timer: ReturnType<typeof setTimeout> | null = null;
let timerAt = 0;

const SLACK_MS = 2;
const nextSecond = (now: number) => now - (now % 1000) + 1000;
// The first second boundary at or after `t`, plus a little slack.
const onBoundary = (t: number) => Math.ceil(t / 1000) * 1000 + SLACK_MS;

// A hidden window shows none of these labels, so the clock does not run while
// the document is hidden; when it shows again every label catches up at once
// (energy lane, 2026-11-13). Every subscriber only formats a time for display.
let isHidden: () => boolean = () => typeof document !== 'undefined' && document.visibilityState === 'hidden';
let watching = false;

function watchVisibility(): void {
  if (watching || typeof document === 'undefined' || typeof document.addEventListener !== 'function') return;
  watching = true;
  document.addEventListener('visibilitychange', () => { if (!isHidden()) shown(); });
}

// The window shows again: run every listener now, then resume on the boundary.
function shown(): void {
  const now = Date.now();
  for (const entry of listeners.values()) entry.due = Math.min(entry.due, now);
  if (timer !== null) { clearTimeout(timer); timer = null; }
  fire();
}

function schedule(): void {
  if (!listeners.size || isHidden()) {
    if (timer !== null) { clearTimeout(timer); timer = null; }
    return;
  }
  let at = Infinity;
  for (const entry of listeners.values()) at = Math.min(at, entry.due);
  if (timer !== null && timerAt === at) return;
  if (timer !== null) clearTimeout(timer);
  timerAt = at;
  timer = setTimeout(fire, Math.max(0, at - Date.now()));
}

function fire(): void {
  timer = null;
  const now = Date.now();
  const ready: Listener[] = [];
  for (const [listener, entry] of listeners) {
    if (entry.due > now) continue;
    ready.push(listener);
    entry.due = onBoundary(Math.max(entry.nextDue(now), now + 1));
  }
  for (const listener of ready) if (listeners.has(listener)) listener();
  schedule();
}

// Calls `listener` once a second, on the boundary.
export function subscribeSecond(listener: Listener): () => void {
  return subscribeUntil(listener, nextSecond);
}

// Calls `listener` at the first second boundary at or after nextDue(now), then
// asks again.
export function subscribeUntil(listener: Listener, nextDue: NextDue): () => void {
  const now = Date.now();
  listeners.set(listener, { nextDue, due: onBoundary(Math.max(nextDue(now), now + 1)) });
  watchVisibility();
  schedule();
  return () => {
    listeners.delete(listener);
    schedule();
  };
}

export const secondTickInternals = {
  listenerCount: () => listeners.size, scheduled: () => timer !== null, timerAt: () => timerAt,
  setHidden: (hidden: () => boolean) => { isHidden = hidden; }, shown,
};
