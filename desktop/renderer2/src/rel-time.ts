// Relative-time label for a turn stamp ("hace unos segundos" → min → h → d).
// Pure formatting; the periodic re-render that keeps it fresh lives in the store
// clock (useClock) — see AssistantMessage.RelStamp. `now` is injectable for tests.
export function relTime(at: string | null, now: number = Date.now()): string {
  if (!at) return '';
  const then = new Date(at).getTime();
  if (Number.isNaN(then)) return '';
  const secs = Math.max(0, Math.floor((now - then) / 1000));
  if (secs < 1) return 'ahora';
  if (secs < 60) return `hace ${secs} s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `hace ${mins} min`;
  const hrs = Math.floor(mins / 60);
  if (hrs < 24) return `hace ${hrs} h`;
  const days = Math.floor(hrs / 24);
  return `hace ${days} d`;
}
