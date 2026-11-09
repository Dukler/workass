// The engine fleet as Settings shows it: live engines and their memory.

import type { ProcessSummary } from '../../wire/types';

export function liveEngines(procs: ProcessSummary[]): ProcessSummary[] {
  return procs.filter((p) => p.engine && p.status === 'running');
}
export function fmtRss(kb: number): string {
  if (!kb) return '—';
  const mb = kb / 1024;
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${Math.round(mb)} MB`;
}
export function fleetLabel(procs: ProcessSummary[]): { count: number; rss: string } {
  const eng = liveEngines(procs);
  const kb = eng.reduce((s, p) => s + (p.rssKb ?? 0), 0);
  return { count: eng.length, rss: fmtRss(kb) };
}
