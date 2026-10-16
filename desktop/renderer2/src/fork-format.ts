// Pure presentation helpers for forkable subsessions (approved mock
// desktop/docs/mocks/forks/forks.html). No React, no store: everything here is
// exercised by tests/fork-format.test.ts under node --test.
//
// Vocabulary (mock legend): the cache bar shows where a child's INPUT came
// from — cache read (accent), cache write (muted amber), fresh input (ink2).
// Output is never in the bar. "caché" = read ÷ (read + write + input).

import type { ForkChildView, ForkRunView, ForkTranscriptEntry, ForkUsage } from './wire/types.ts';
import type { ToolEvent } from './store/types.ts';

const n = (value: unknown): number => {
  const num = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(num) && num > 0 ? num : 0;
};

/** Every token the child spent, output included (the rows' "112 k tok"). */
export function forkTokenTotal(usage: Partial<ForkUsage> | null | undefined): number {
  return n(usage?.input) + n(usage?.output) + n(usage?.cacheRead) + n(usage?.cacheCreation);
}

/** Tokens that entered the model: fresh input + cache read + cache write. */
export function forkInputTotal(usage: Partial<ForkUsage> | null | undefined): number {
  return n(usage?.input) + n(usage?.cacheRead) + n(usage?.cacheCreation);
}

/** Cache share 0..1. The daemon's rounded share wins when it sent one. */
export function forkCacheShare(usage: Partial<ForkUsage> | null | undefined, provided?: number | null): number {
  if (typeof provided === 'number' && Number.isFinite(provided) && provided >= 0) return Math.min(1, provided);
  const total = forkInputTotal(usage);
  return total > 0 ? n(usage?.cacheRead) / total : 0;
}

/** One decimal with a comma, trailing ",0" dropped: 44.8 → "44,8", 11.0 → "11". */
function decimal1(value: number): string {
  const fixed = (Math.round(value * 10) / 10).toFixed(1);
  return (fixed.endsWith('.0') ? fixed.slice(0, -2) : fixed).replace('.', ',');
}

/**
 * Spanish compact token count: "4", "1,2 k", "44,8 k", "340 k", "1,2 M".
 * One decimal below 100 k, whole thousands from there; never "1000 k".
 */
export function formatForkTokens(value: number): string {
  const v = n(value);
  if (v < 1000) return String(Math.round(v));
  if (v < 99_950) return `${decimal1(v / 1000)} k`;
  if (v < 999_500) return `${Math.round(v / 1000)} k`;
  if (v < 99_950_000) return `${decimal1(v / 1_000_000)} M`;
  return `${Math.round(v / 1_000_000)} M`;
}

/** "90 %" — Spanish spacing, whole percent. */
export function formatForkPercent(share: number): string {
  const s = Number.isFinite(share) ? Math.max(0, Math.min(1, share)) : 0;
  return `${Math.round(s * 100)} %`;
}

/** Wall time: "0,8 s", "10 s", "1 min 5 s", "2 min", "1 h 4 min". */
export function formatForkDuration(ms: number): string {
  const v = n(ms);
  if (v < 950) return `${decimal1(v / 1000)} s`;
  const totalSeconds = Math.round(v / 1000);
  if (totalSeconds < 60) return `${totalSeconds} s`;
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes < 60) return seconds ? `${minutes} min ${seconds} s` : `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest ? `${hours} h ${rest} min` : `${hours} h`;
}

/** Local wall-clock "16:42" for an ISO stamp; '' when absent or invalid. */
export function formatForkClock(iso: string | null | undefined): string {
  if (!iso) return '';
  const at = Date.parse(iso);
  if (!Number.isFinite(at)) return '';
  const date = new Date(at);
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

export interface CacheBarSegments {
  /** Percent widths of the input total; they sum to 100 when there is input. */
  read: number;
  write: number;
  fresh: number;
  /** Fresh input is drawn only when it is a visible share (>0.2 %), as in the mock. */
  showFresh: boolean;
  empty: boolean;
}

/** Bar widths use the same numbers as the "90 % caché" label, so they agree. */
export function forkCacheBar(usage: Partial<ForkUsage> | null | undefined): CacheBarSegments {
  const read = n(usage?.cacheRead);
  const write = n(usage?.cacheCreation);
  const fresh = n(usage?.input);
  const total = read + write + fresh;
  if (total <= 0) return { read: 0, write: 0, fresh: 0, showFresh: false, empty: true };
  const pct = (value: number) => Math.round((value / total) * 10000) / 100;
  return { read: pct(read), write: pct(write), fresh: pct(fresh), showFresh: fresh / total > 0.002, empty: false };
}

// ── Retries ──────────────────────────────────────────────────────────────────

export interface ForkChildSlot {
  /** The attempt the row shows: the newest one in the chain. */
  child: ForkChildView;
  /** Root child id the chain started from. */
  rootId: string;
  /** How many attempts exist (1 = never retried). */
  attempts: number;
}

/**
 * Retries arrive as extra children whose retryOf names the original. A row
 * shows the latest attempt in the original's place, so a retried child never
 * reads as a second, different child.
 */
export function collapseForkRetries(children: readonly ForkChildView[]): ForkChildSlot[] {
  const byId = new Map(children.map((child) => [child.childId, child]));
  const rootOf = (child: ForkChildView): string => {
    let current = child;
    const seen = new Set<string>();
    while (current.retryOf && !seen.has(current.childId)) {
      seen.add(current.childId);
      const parent = byId.get(current.retryOf);
      if (!parent) return current.retryOf;
      current = parent;
    }
    return current.childId;
  };
  const order: string[] = [];
  const slots = new Map<string, { child: ForkChildView; attempts: number; position: number }>();
  children.forEach((child, position) => {
    const root = rootOf(child);
    const existing = slots.get(root);
    if (!existing) {
      order.push(root);
      slots.set(root, { child, attempts: 1, position });
      return;
    }
    existing.attempts += 1;
    const newer = (child.attempt || 0) > (existing.child.attempt || 0)
      || ((child.attempt || 0) === (existing.child.attempt || 0) && position > existing.position);
    if (newer) { existing.child = child; existing.position = position; }
  });
  return order.map((rootId) => {
    const slot = slots.get(rootId)!;
    return { child: slot.child, rootId, attempts: slot.attempts };
  });
}

// ── Status words ─────────────────────────────────────────────────────────────

export type ForkRowState = 'queued' | 'running' | 'completed' | 'failed' | 'cancelled';

export function forkRowState(status: string | null | undefined): ForkRowState {
  switch (status) {
    case 'queued': case 'running': case 'completed': case 'failed': case 'cancelled': return status;
    default: return 'completed';
  }
}

export function forkSettled(status: string | null | undefined): boolean {
  const state = forkRowState(status);
  return state === 'completed' || state === 'failed' || state === 'cancelled';
}

/** The panel header's single status word. */
export function forkStatusWord(status: string | null | undefined): string {
  switch (forkRowState(status)) {
    case 'queued': return 'en cola';
    case 'running': return 'en curso';
    case 'failed': return 'falló';
    case 'cancelled': return 'detenido';
    default: return 'listo';
  }
}

const SUMMARY_ORDER: Array<[ForkRowState, string, string]> = [
  ['completed', 'listo', 'listos'],
  ['running', 'en curso', 'en curso'],
  ['queued', 'en cola', 'en cola'],
  ['failed', 'falló', 'fallaron'],
  ['cancelled', 'detenido', 'detenidos'],
];

/** "1 listo · 1 en curso · 1 en cola" over the rows actually shown. */
export function forkRunSummary(slots: readonly ForkChildSlot[]): string {
  const counts = new Map<ForkRowState, number>();
  for (const slot of slots) {
    const state = forkRowState(slot.child.status);
    counts.set(state, (counts.get(state) ?? 0) + 1);
  }
  return SUMMARY_ORDER
    .filter(([state]) => counts.has(state))
    .map(([state, one, many]) => { const count = counts.get(state)!; return `${count} ${count === 1 ? one : many}`; })
    .join(' · ');
}

export function forkChildCountLabel(count: number): string {
  return `${count} ${count === 1 ? 'hijo' : 'hijos'}`;
}

export function forkTurnsLabel(turns: number): string {
  const t = Math.max(0, Math.round(n(turns)));
  return `${t} ${t === 1 ? 'turno' : 'turnos'}`;
}

/** Row label: the daemon's label, else "hijo N" (1-based index). */
export function forkChildLabel(child: Pick<ForkChildView, 'label' | 'index'>): string {
  const label = String(child.label ?? '').trim();
  return label || `hijo ${Math.max(1, Math.round(n(child.index)) || 1)}`;
}

// ── Time ─────────────────────────────────────────────────────────────────────

/** Running children tick from startedAt; settled ones report their wall time. */
export function forkChildElapsedMs(child: Pick<ForkChildView, 'status' | 'startedAt' | 'wallMs'>, nowMs: number): number | null {
  const state = forkRowState(child.status);
  if (state === 'queued') return null;
  if (state === 'running') {
    const started = child.startedAt ? Date.parse(child.startedAt) : NaN;
    return Number.isFinite(started) ? Math.max(0, nowMs - started) : null;
  }
  return n(child.wallMs) > 0 ? n(child.wallMs) : null;
}

/**
 * A run's wall time: first start to last finish (to now while running). Falls
 * back to the longest child when stamps are missing.
 */
export function forkRunWallMs(run: Pick<ForkRunView, 'children'>, nowMs: number): number {
  let start = Infinity;
  let end = 0;
  let longest = 0;
  for (const child of run.children) {
    longest = Math.max(longest, n(child.wallMs));
    const started = child.startedAt ? Date.parse(child.startedAt) : NaN;
    if (!Number.isFinite(started)) continue;
    start = Math.min(start, started);
    const finished = child.finishedAt ? Date.parse(child.finishedAt) : NaN;
    if (forkRowState(child.status) === 'running') end = Math.max(end, nowMs);
    else if (Number.isFinite(finished)) end = Math.max(end, finished);
    else end = Math.max(end, started + n(child.wallMs));
  }
  return Number.isFinite(start) && end > start ? end - start : longest;
}

// ── Live activity ────────────────────────────────────────────────────────────

/**
 * The daemon writes a running child's activity either as "<ToolName> <target>"
 * ("Write /tmp/fork-test/child_2.json") or as its own prose ("Iniciando",
 * "Respondiendo"). Split the first form into a tool name + evidence; the
 * caller translates the name and shows prose verbatim with no glyph.
 */
export function splitForkActivity(activity: string | null | undefined): { tool: string; target: string } | { prose: string } | null {
  const text = String(activity ?? '').trim();
  if (!text) return null;
  const match = /^([A-Za-z][A-Za-z0-9_]*)(?:\s+(.+))?$/s.exec(text);
  if (match && /^[A-Z]/.test(match[1]) && match[2]) return { tool: match[1], target: match[2].trim() };
  return { prose: text };
}

// ── Child transcript ─────────────────────────────────────────────────────────

export type ForkTranscriptItem =
  | { kind: 'user'; key: string; text: string }
  | { kind: 'tool'; key: string; tool: ToolEvent }
  | { kind: 'text'; key: string; text: string }
  | { kind: 'error'; key: string; text: string };

/** Transcript entries → rows, pairing each tool call with its result. */
export function forkTranscriptItems(entries: readonly ForkTranscriptEntry[], running: boolean): ForkTranscriptItem[] {
  const results = new Map<string, Extract<ForkTranscriptEntry, { kind: 'tool_result' }>>();
  for (const entry of entries) if (entry.kind === 'tool_result' && entry.toolUseId) results.set(entry.toolUseId, entry);
  const items: ForkTranscriptItem[] = [];
  let lastText = '';
  entries.forEach((entry, index) => {
    const key = `${entry.kind}:${index}`;
    const at = Date.parse(entry.at);
    switch (entry.kind) {
      case 'user':
        items.push({ kind: 'user', key, text: entry.text || '' });
        break;
      case 'tool': {
        const result = results.get(entry.id);
        const ended = result ? Date.parse(result.at) : NaN;
        items.push({
          kind: 'tool', key,
          tool: {
            key: `fork-tool:${entry.id || index}`, at: Number.isFinite(at) ? at : 0, kind: 'tool', id: entry.id || null,
            toolKind: null, title: entry.name, status: result ? (result.isError ? 'failed' : 'completed') : running ? 'in_progress' : 'completed',
            command: null, terminalId: null, input: entry.input === undefined ? null : JSON.stringify(entry.input, null, 2),
            output: result?.text || null, location: entry.target || null,
            startedAt: Number.isFinite(at) ? at : undefined, endedAt: Number.isFinite(ended) ? ended : undefined,
          },
        });
        break;
      }
      case 'assistant':
        if (entry.text?.trim()) { items.push({ kind: 'text', key, text: entry.text }); lastText = entry.text.trim(); }
        break;
      case 'result':
        if (entry.isError) items.push({ kind: 'error', key, text: entry.text || 'La subsesión terminó con un error.' });
        else if (entry.text?.trim() && entry.text.trim() !== lastText) { items.push({ kind: 'text', key, text: entry.text }); lastText = entry.text.trim(); }
        break;
      default:
        break;
    }
  });
  return items;
}
