// What a paged session shell is (session:get {shell, page}, daemon
// cmd/workass/session_page.go) and how a delta rebuilds one.
//
// A paged shell lists only the chats the sidebar can show without a search:
// everything that is not filed in its archive, plus the chats this window
// names (the one it shows, one it was asked to open) and the matches of its
// search. The archive, which is most chats and shows nothing until a search,
// stays on the daemon; deferredChats says how many there are and in which
// folders. An older daemon ignores the option and sends every chat, with no
// deferredChats, and the window behaves exactly as before.

/** A settled chat files into the hidden, searchable archive this long after it settled. */
export const ARCHIVE_AFTER_SETTLED_MS = 5 * 24 * 60 * 60 * 1000;
/** How often the sidebar re-evaluates what has aged into the archive. */
export const SIDEBAR_LIFECYCLE_TICK_MS = 60 * 60 * 1000;
// The daemon leaves a chat out only if it settled this much earlier than the
// sidebar's archive boundary: the sidebar's clock can lag by one lifecycle
// tick, and a margin keeps a chat on the boundary listed rather than missing.
const PAGE_ARCHIVE_MARGIN_MS = SIDEBAR_LIFECYCLE_TICK_MS + 5 * 60 * 60 * 1000;

export interface DeferredChats {
  count: number;
  /** [cwd, count] for the chats the daemon did not send. */
  cwds: [string, number][];
}

export interface SessionPageRequest {
  settledBefore: number;
  include: string[];
  search?: string;
  labels?: [string, string][];
}

export function pageSettledBefore(now = Date.now()): number {
  return now - ARCHIVE_AFTER_SETTLED_MS - PAGE_ARCHIVE_MARGIN_MS;
}

export function parseDeferredChats(raw: unknown): DeferredChats | null {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null;
  const value = raw as { count?: unknown; cwds?: unknown };
  const count = typeof value.count === 'number' && Number.isInteger(value.count) && value.count >= 0 ? value.count : null;
  if (count === null) return null;
  const cwds: [string, number][] = [];
  if (Array.isArray(value.cwds)) {
    for (const pair of value.cwds) {
      if (Array.isArray(pair) && pair.length === 2 && typeof pair[0] === 'string'
        && typeof pair[1] === 'number' && Number.isInteger(pair[1]) && pair[1] > 0) cwds.push([pair[0], pair[1]]);
    }
  }
  return { count, cwds };
}

interface ShellDelta {
  base?: unknown;
  set?: unknown;
  unset?: unknown;
  lists?: unknown;
  patch?: unknown;
  chats?: unknown;
  ids?: unknown;
}

/** A delta reply: {etag, delta: {base, set, unset, patch, chats, ids}}. */
export function isShellDelta(reply: unknown): reply is { etag: string; delta: ShellDelta } {
  if (!reply || typeof reply !== 'object') return false;
  const value = reply as { etag?: unknown; delta?: unknown };
  return typeof value.etag === 'string' && !!value.etag && !!value.delta && typeof value.delta === 'object';
}

/**
 * The shell a delta names, rebuilt from the shell kept under its base etag;
 * null when the delta does not apply to it (the caller then reads the shell
 * whole). The base object is not modified.
 */
export function applyShellDelta(base: unknown, baseEtag: string, reply: { etag: string; delta: ShellDelta }): Record<string, unknown> | null {
  const delta = reply.delta;
  if (!base || typeof base !== 'object' || Array.isArray(base) || delta.base !== baseEtag) return null;
  const prior = base as Record<string, unknown>;
  if (!Array.isArray(prior.chats)) return null;
  const next: Record<string, unknown> = { ...prior };
  if (delta.set !== undefined) {
    if (!delta.set || typeof delta.set !== 'object' || Array.isArray(delta.set)) return null;
    for (const [key, value] of Object.entries(delta.set as Record<string, unknown>)) {
      if (key === 'chats' || key === 'etag') return null;
      next[key] = value;
    }
  }
  if (delta.unset !== undefined) {
    if (!Array.isArray(delta.unset)) return null;
    for (const key of delta.unset) {
      if (typeof key !== 'string' || key === 'chats' || key === 'etag') return null;
      delete next[key];
    }
  }
  // A root list of ids (the chat order) as an edit of the kept one: runs of
  // it ([start, count]) and the ids it lacks.
  if (delta.lists !== undefined) {
    if (!delta.lists || typeof delta.lists !== 'object' || Array.isArray(delta.lists)) return null;
    for (const [key, ops] of Object.entries(delta.lists as Record<string, unknown>)) {
      const kept = prior[key];
      if (key === 'chats' || key === 'etag' || !Array.isArray(kept) || !Array.isArray(ops)) return null;
      const list: unknown[] = [];
      for (const op of ops) {
        if (typeof op === 'string') { list.push(op); continue; }
        if (!Array.isArray(op) || op.length !== 2) return null;
        const [start, count] = op as unknown[];
        if (!Number.isInteger(start) || !Number.isInteger(count) || (start as number) < 0 || (count as number) < 1
          || (start as number) + (count as number) > kept.length) return null;
        list.push(...kept.slice(start as number, (start as number) + (count as number)));
      }
      next[key] = list;
    }
  }
  const byId = new Map<string, unknown>();
  for (const row of prior.chats as unknown[]) {
    const id = row && typeof row === 'object' ? (row as { id?: unknown }).id : undefined;
    if (typeof id !== 'string' || !id) return null;
    byId.set(id, row);
  }
  // Changed fields of rows this window holds; never a row it does not.
  const patches = Array.isArray(delta.patch) ? delta.patch : delta.patch === undefined ? [] : null;
  if (!patches) return null;
  for (const patch of patches) {
    const value = patch && typeof patch === 'object' ? patch as { id?: unknown; set?: unknown; unset?: unknown } : null;
    const id = value?.id;
    const prior = typeof id === 'string' ? byId.get(id) : undefined;
    if (!value || typeof id !== 'string' || !prior || typeof prior !== 'object') return null;
    const row: Record<string, unknown> = { ...(prior as Record<string, unknown>) };
    if (value.set !== undefined) {
      if (!value.set || typeof value.set !== 'object' || Array.isArray(value.set)) return null;
      Object.assign(row, value.set);
    }
    if (value.unset !== undefined) {
      if (!Array.isArray(value.unset)) return null;
      for (const key of value.unset) {
        if (typeof key !== 'string') return null;
        delete row[key];
      }
    }
    if (row.id !== id) return null;
    byId.set(id, row);
  }
  // Rows new to this window, whole.
  const changed = Array.isArray(delta.chats) ? delta.chats : delta.chats === undefined ? [] : null;
  if (!changed) return null;
  for (const row of changed) {
    const id = row && typeof row === 'object' ? (row as { id?: unknown }).id : undefined;
    if (typeof id !== 'string' || !id) return null;
    byId.set(id, row);
  }
  let ids: string[];
  if (delta.ids === undefined) {
    ids = (prior.chats as Array<{ id: string }>).map((row) => row.id);
  } else {
    if (!Array.isArray(delta.ids) || delta.ids.some((id) => typeof id !== 'string')) return null;
    ids = delta.ids as string[];
  }
  const chats: unknown[] = [];
  for (const id of ids) {
    const row = byId.get(id);
    if (row === undefined) return null;
    chats.push(row);
  }
  next.chats = chats;
  next.etag = reply.etag;
  return next;
}
