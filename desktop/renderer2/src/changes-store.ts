// The «Cambios» watch, owned by the renderer (see internal/changes).
//
// The daemon looks at repos only while a watch is open, so this module opens
// one exactly while the tab is on screen: the tab mounting asks for it, the
// tab unmounting, the window hiding or the connection dropping end it, and a
// renewal every RENEW_MS keeps the daemon's lease alive in between. The
// window regaining focus asks for a fresh look. Snapshots arrive as
// changes:state events; the last one is kept for the tab count.

import { useSyncExternalStore } from 'react';
import { call, has, on } from './wire/api';
import type { ChangesDiff, ChangesState } from './wire/types';

const RENEW_MS = 30_000;
const DIFF_CACHE_MS = 20_000;
const DIFF_CACHE_LIMIT = 60;

let snapshot: ChangesState | null = null;
let version = 0;
const listeners = new Set<() => void>();

let wanted = 0;
let visible = true;
let watchId = '';
let starting: Promise<void> | null = null;
let renewTimer: ReturnType<typeof setInterval> | null = null;
let subscribed = false;
let windowHooks = false;

function bump(): void {
  version += 1;
  for (const listener of listeners) listener();
}

// Events arrive in order on one socket and always win. A watch reply carries
// the state as it was when the daemon answered, so it never replaces a newer
// event that overtook it.
function apply(next: ChangesState | null | undefined, fromReply = false): void {
  if (!next || !Array.isArray(next.repos)) return;
  if (fromReply && snapshot && (next.seq ?? 0) < (snapshot.seq ?? 0)) return;
  snapshot = next;
  bump();
}

function subscribe(): void {
  if (subscribed) return;
  subscribed = true;
  on('onChangesState', (state: ChangesState) => apply(state));
}

function installWindowHooks(): void {
  if (windowHooks || typeof window === 'undefined' || typeof document === 'undefined') return;
  windowHooks = true;
  document.addEventListener('visibilitychange', () => {
    visible = document.visibilityState !== 'hidden';
    sync();
  });
  window.addEventListener('focus', () => changesFocus());
  visible = document.visibilityState !== 'hidden';
}

function shouldWatch(): boolean { return wanted > 0 && visible; }

function sync(): void {
  if (shouldWatch()) {
    if (!watchId && !starting) starting = start().finally(() => { starting = null; });
  } else if (watchId) {
    stop();
  }
}

async function start(): Promise<void> {
  if (!has('changesWatch')) return;
  subscribe();
  const reply = await call('changesWatch', {});
  if (!reply?.id) return;
  if (!shouldWatch()) {
    // Closed while the watch was being opened.
    void call('changesUnwatch', { id: reply.id });
    return;
  }
  watchId = reply.id;
  apply(reply.state, true);
  if (renewTimer) clearInterval(renewTimer);
  renewTimer = setInterval(() => void renew(), RENEW_MS);
}

async function renew(): Promise<void> {
  if (!watchId) return;
  const reply = await call('changesWatch', { id: watchId });
  if (reply?.id && watchId && reply.id !== watchId) watchId = reply.id;
}

function stop(): void {
  const id = watchId;
  watchId = '';
  if (renewTimer) { clearInterval(renewTimer); renewTimer = null; }
  if (id) void call('changesUnwatch', { id });
}

/** The tab is on screen. Returns the release for when it leaves. */
export function openChangesWatch(): () => void {
  installWindowHooks();
  wanted += 1;
  sync();
  let released = false;
  return () => {
    if (released) return;
    released = true;
    wanted = Math.max(0, wanted - 1);
    sync();
  };
}

/** The window regained focus: ask for a fresh look if the tab is watching. */
export function changesFocus(): void {
  if (watchId && has('changesFocus')) void call('changesFocus');
}

/** The connection came back: the daemon dropped the old watch with its
 *  socket, so open a new one if the tab is still on screen. */
export function changesReconnected(): void {
  if (!watchId) return;
  watchId = '';
  if (renewTimer) { clearInterval(renewTimer); renewTimer = null; }
  sync();
}

export function changesSnapshot(): ChangesState | null { return snapshot; }

export function useChangesState(): ChangesState | null {
  useSyncExternalStore(
    (cb) => { listeners.add(cb); return () => listeners.delete(cb); },
    () => version,
    () => version,
  );
  return snapshot;
}

// ── Diffs: fetched on hover or expand only, kept briefly ─────────────────

const diffs = new Map<string, { at: number; diff: Promise<ChangesDiff | null> }>();

/** One file's diff. `stamp` changes when the file's counts change, so a new
 *  edit is never answered from the cache. */
export function changesDiff(root: string, path: string, stamp: string): Promise<ChangesDiff | null> {
  const key = `${root}\u0000${path}\u0000${stamp}`;
  const now = Date.now();
  const cached = diffs.get(key);
  if (cached && now - cached.at < DIFF_CACHE_MS) return cached.diff;
  const diff = has('changesDiff')
    ? call('changesDiff', { root, path }).then((reply) => reply ?? null, () => null)
    : Promise.resolve(null);
  diffs.set(key, { at: now, diff });
  while (diffs.size > DIFF_CACHE_LIMIT) diffs.delete(diffs.keys().next().value as string);
  void diff.then((value) => { if (!value) diffs.delete(key); });
  return diff;
}

/** Tests only. */
export function resetChangesStore(state: ChangesState | null = null): void {
  snapshot = state;
  wanted = 0;
  watchId = '';
  starting = null;
  if (renewTimer) { clearInterval(renewTimer); renewTimer = null; }
  diffs.clear();
  bump();
}

/** Tests only: whether a watch is open. */
export function changesWatching(): boolean { return !!watchId; }
