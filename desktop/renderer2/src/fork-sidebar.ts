// Pure logic for fork subsessions nested in the sidebar (approved mock
// desktop/docs/mocks/forks/sidebar-nested.html). No React, no store; tested by
// tests/fork-sidebar.test.ts.
//
// Shape: a parent chat row carries "⑂ N"; expanded, each fork point is a small
// group header with its children as single-line rows underneath. Retries
// replace their original (latest attempt only), so N never double-counts.

import type { ForkChildView, ForkPointView, ForkRunView, ForkSummaryChat, ForkUsage } from './wire/types.ts';
import {
  collapseForkRetries, forkCacheShare, forkChildCountLabel, forkChildElapsedMs, forkRowState,
  formatForkPercent, formatForkTokens, type ForkRowState,
} from './fork-format.ts';

/** Children shown per fork point before "Mostrar N más". */
export const FORK_KID_PAGE = 12;

export interface ForkListState { points: readonly ForkPointView[]; runs: readonly ForkRunView[]; }
export interface ForkKidRow { runId: string; rootId: string; child: ForkChildView; attempts: number; }
export interface ForkGroup { point: ForkPointView; runs: ForkRunView[]; kids: ForkKidRow[]; }

const stamp = (iso: string | null | undefined) => { const at = iso ? Date.parse(iso) : NaN; return Number.isFinite(at) ? at : 0; };

/** One group per fork point (oldest first); its children across runs, run order, latest attempts. */
export function forkGroups(state: ForkListState | null | undefined): ForkGroup[] {
  if (!state) return [];
  const points = [...state.points].sort((a, b) => stamp(a.createdAt) - stamp(b.createdAt));
  return points.map((point) => {
    const runs = state.runs
      .filter((run) => run.forkPointId === point.forkPointId)
      .sort((a, b) => stamp(a.createdAt) - stamp(b.createdAt));
    const kids = runs.flatMap((run) => collapseForkRetries(run.children)
      .map((slot) => ({ runId: run.runId, rootId: slot.rootId, child: slot.child, attempts: slot.attempts })));
    return { point, runs, kids };
  });
}

/** Sidebar-compact wall time, the same shape as the row's "Trabajando 18s". */
export function formatForkShortDuration(ms: number | null | undefined): string {
  const v = typeof ms === 'number' && Number.isFinite(ms) && ms > 0 ? ms : 0;
  const s = Math.round(v / 1000);
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  return `${Math.floor(s / 3600)}h`;
}

function countStates(kids: readonly ForkKidRow[]): Map<ForkRowState, number> {
  const counts = new Map<ForkRowState, number>();
  for (const kid of kids) {
    const state = forkRowState(kid.child.status);
    counts.set(state, (counts.get(state) ?? 0) + 1);
  }
  return counts;
}

// Attention first: what needs a look, then what is moving, then what is done.
const ATTENTION_ORDER: Array<[ForkRowState, string, string]> = [
  ['failed', 'falló', 'fallaron'],
  ['running', 'en curso', 'en curso'],
  ['queued', 'en cola', 'en cola'],
  ['completed', 'listo', 'listos'],
  ['cancelled', 'detenido', 'detenidos'],
];

function phrase(counts: Map<ForkRowState, number>, states: readonly ForkRowState[]): string {
  return ATTENTION_ORDER
    .filter(([state]) => states.includes(state) && counts.has(state))
    .map(([state, one, many]) => { const n = counts.get(state)!; return `${n} ${n === 1 ? one : many}`; })
    .join(' · ');
}

/** A group still needs attention while anything failed, runs or waits. */
export function forkGroupNeedsAttention(kids: readonly ForkKidRow[]): boolean {
  return kids.some((kid) => { const s = forkRowState(kid.child.status); return s === 'failed' || s === 'running' || s === 'queued'; });
}

/** Token totals of every run under a fork point (superseded attempts included: that spend was real). */
export function forkGroupUsage(group: Pick<ForkGroup, 'runs'>): ForkUsage {
  const total: ForkUsage = { input: 0, output: 0, cacheRead: 0, cacheCreation: 0 };
  for (const run of group.runs) {
    total.input += run.totals?.input || 0;
    total.output += run.totals?.output || 0;
    total.cacheRead += run.totals?.cacheRead || 0;
    total.cacheCreation += run.totals?.cacheCreation || 0;
  }
  return total;
}

function groupShare(group: Pick<ForkGroup, 'runs'>): number {
  if (group.runs.length === 1) return forkCacheShare(group.runs[0].totals, group.runs[0].totals?.cacheHitShare);
  return forkCacheShare(forkGroupUsage(group));
}

function pointStateLine(point: ForkPointView): string {
  if (point.status === 'sealing') return 'sellando el prefijo…';
  if (point.status === 'failed') return 'no se pudo sellar';
  return 'sin hijos todavía';
}

/**
 * Subsessions sit directly under their parent session: one flat list, oldest
 * fork point first, each child carrying its fork point (for its model glyph).
 * The fork point itself is not a sidebar row; the child's breadcrumb and the
 * parent's marker line name it.
 */
export function forkFlatKids(groups: readonly ForkGroup[]): Array<ForkKidRow & { point: ForkPointView }> {
  return groups.flatMap((group) => group.kids.map((kid) => ({ ...kid, point: group.point })));
}

/** The parent transcript's one marker line: "3 hijos · 89 % caché" (or what is still moving). */
export function forkMarkerMeta(group: ForkGroup): string {
  if (!group.kids.length) {
    return group.point.status === 'ready' ? `prefijo ${formatForkTokens(group.point.prefixSize)} tok` : pointStateLine(group.point);
  }
  const head = forkChildCountLabel(group.kids.length);
  if (forkGroupNeedsAttention(group.kids)) return `${head} · ${phrase(countStates(group.kids), ['failed', 'running', 'queued'])}`;
  return `${head} · ${formatForkPercent(groupShare(group))} caché`;
}

export interface ForkKidMeta {
  state: ForkRowState;
  /** Muted outcome word ("falló", "detenido"); never red. */
  word: string | null;
  /** Live elapsed while running, wall time once settled, nothing while queued. */
  time: string | null;
}

export function forkKidMeta(child: ForkChildView, nowMs: number): ForkKidMeta {
  const state = forkRowState(child.status);
  const elapsed = forkChildElapsedMs(child, nowMs);
  return {
    state,
    word: state === 'failed' ? 'falló' : state === 'cancelled' ? 'detenido' : null,
    time: elapsed === null ? null : formatForkShortDuration(elapsed),
  };
}

/** First page, then everything once "Mostrar N más" is pressed. */
export function forkPage<T>(items: readonly T[], expanded: boolean, page = FORK_KID_PAGE): { visible: T[]; hidden: number } {
  if (expanded || items.length <= page) return { visible: [...items], hidden: 0 };
  return { visible: items.slice(0, page), hidden: items.length - page };
}

// ── parent row badge ─────────────────────────────────────────────────────────

export interface ForkChatBadge {
  points: number;
  children: number;
  running: number;
  /** Earliest start of a run that still has a child running; 0 when unknown. */
  since: number;
}

export function forkSummaryForChat(
  entries: readonly ForkSummaryChat[],
  chat: { id: string; chatId?: string | null },
): ForkSummaryChat | undefined {
  return entries.find((entry) => entry.tabId === chat.id)
    ?? (chat.chatId ? entries.find((entry) => entry.chatId === chat.chatId) : undefined);
}

/**
 * The parent row's "⑂ N" and working facts. A full fork:list snapshot (the
 * expanded or active chat, any machine) wins over the cheap summary.
 */
export function forkChatBadge(summary: ForkSummaryChat | undefined, list: ForkListState | undefined): ForkChatBadge | null {
  if (list && (list.points.length || list.runs.length)) {
    let children = 0;
    let running = 0;
    let since = 0;
    for (const run of list.runs) {
      const kids = collapseForkRetries(run.children);
      children += kids.length;
      const live = kids.filter((slot) => forkRowState(slot.child.status) === 'running');
      running += live.length;
      if (!live.length) continue;
      for (const slot of kids) {
        const at = stamp(slot.child.startedAt);
        if (at && (!since || at < since)) since = at;
      }
    }
    return { points: list.points.length, children, running, since };
  }
  if (summary && summary.forkPoints > 0) {
    return { points: summary.forkPoints, children: summary.children || 0, running: summary.counts?.running || 0, since: 0 };
  }
  return null;
}

/** Collapsed unless the user opened it, or it is the chat you are in and a child is running. */
export function forkSidebarOpen(explicit: boolean | undefined, active: boolean, running: boolean): boolean {
  return explicit ?? (active && running);
}

// ── selection ────────────────────────────────────────────────────────────────

export interface ForkSelection { tabId: string; chatId: string; runId: string; rootId: string; }

/**
 * Selecting a child switches to its parent first, and that switch can land a
 * moment later. The selection therefore survives everything except a move to a
 * DIFFERENT chat: picking the parent row or any other conversation leaves the
 * child view.
 */
export function forkSelectionAfterActive(
  selected: ForkSelection | null,
  previousActiveId: string | null,
  nextActiveId: string | null,
): ForkSelection | null {
  if (!selected) return null;
  if (previousActiveId === nextActiveId) return selected;
  return nextActiveId === selected.tabId ? selected : null;
}

/** The child view shows only for the chat it belongs to. */
export function forkSelectionVisible(selected: ForkSelection | null, activeId: string | null): boolean {
  return !!selected && selected.tabId === activeId;
}
