// Pure logic for subsessions nested in the sidebar (mock
// desktop/docs/mocks/forks/sidebar-nested.html, as amended by the user on
// 2026-10-16). No React, no store; tested by tests/fork-sidebar.test.ts.
//
// Only real child sessions live here. Subagents (workass_spawn_subagent,
// Claude Code's Agent tool) are work of the parent's turn: they belong in the
// right rail, NEVER in the left panel (user 2026-10-16, correcting 8dc1e66d).
// No fork glyph on any parent-side row; ⑂ lives only inside the child's view.
//
// Shape: a parent chat row carries a neutral count + chevron; expanded, one
// flat single-line list holds every child. Fork retries replace their original
// (latest attempt only), so counts never double-count.

import type { ForkChildView, ForkPointView, ForkRunView, ForkSummaryChat } from './wire/types.ts';
import { collapseForkRetries, forkChildElapsedMs, forkRowState, type ForkRowState } from './fork-format.ts';

/** Children shown per parent chat before "Mostrar N más". */
export const FORK_KID_PAGE = 12;

export interface ForkListState { points: readonly ForkPointView[]; runs: readonly ForkRunView[]; }
export interface ForkKidRow { runId: string; rootId: string; child: ForkChildView; attempts: number; runCreatedAt: string; }
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
      .map((slot) => ({ runId: run.runId, rootId: slot.rootId, child: slot.child, attempts: slot.attempts, runCreatedAt: run.createdAt })));
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

/**
 * Subsessions sit directly under their parent session: one flat list, oldest
 * row first, whatever kind of thread it is. The fork point itself is never a
 * sidebar row — the child's own breadcrumb names it (user 2026-10-16). The
 * child carries its point (its provider brand and its child-side breadcrumb).
 */
export function forkFlatKids(groups: readonly ForkGroup[]): Array<ForkKidRow & { point: ForkPointView }> {
  return groups.flatMap((group) => group.kids.map((kid) => ({ ...kid, point: group.point })));
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

// ── one neutral subsession list (user, 2026-10-16) ───────────────────────────
//
// Parent-side rows never say "fork". `fork` carries what opening the row
// selects (the child's main-area view).

export interface SubsessionRow {
  key: string;
  kind: 'fork';
  label: string;
  /** Model icon provider key; null → the generic glyph. */
  brand: string | null;
  state: ForkRowState;
  /** Muted outcome word; the same palette for both kinds, never red. */
  word: string | null;
  /** Start stamp; a running row ticks from it. Never a sort key. */
  startedMs: number;
  /**
   * Sort position, oldest first (stable: children of one run keep run order).
   * Fork rows order by their run's creation stamp — the daemon stamps a child
   * only when it starts, so a queued child would otherwise leap to the top and
   * jump again when it starts (review wa-subagent-1792170055899-6, corr 2).
   */
  orderMs: number;
  /** Settled wall time; a running row ticks from startedMs instead. */
  wallMs: number | null;
  /** Set for fork children: it opens the main-area subsession view. */
  fork?: { runId: string; rootId: string };
}

/** One fork child as a neutral subsession row; label/brand resolve in the component. */
export function forkSubsessionRow(
  kid: ForkKidRow,
  labels: { label: string; brand: string | null },
  nowMs: number,
): SubsessionRow {
  const meta = forkKidMeta(kid.child, nowMs);
  return {
    key: `${kid.runId}:${kid.rootId}`,
    kind: 'fork',
    label: labels.label,
    brand: labels.brand,
    state: meta.state,
    word: meta.word,
    startedMs: stamp(kid.child.startedAt),
    orderMs: stamp(kid.runCreatedAt),
    wallMs: meta.state === 'running' || meta.state === 'queued' ? null : forkChildElapsedMs(kid.child, nowMs),
    fork: { runId: kid.runId, rootId: kid.rootId },
  };
}

/** The nest's one list: oldest row first. */
export function sortSubsessionRows(rows: readonly SubsessionRow[]): SubsessionRow[] {
  return [...rows].sort((a, b) => a.orderMs - b.orderMs);
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
 * Fork-side facts: a full fork:list snapshot (the expanded or active chat, any
 * machine) wins over the cheap fork:summary.
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

/** Collapsed unless the user opened it, or it is the chat you are in and a subsession is running. */
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
