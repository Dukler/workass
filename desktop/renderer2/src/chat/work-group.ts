// Work groups: a history read with {workGroups} sends a settled row's folded
// calls as a summary (Msg.workSummary) and stubs that keep the row's layout.
// The calls themselves are read when something shows them (its Worked row
// opens, or is about to: pointed at, focused, scrolled into view), a page at
// a time over the same archive channel, and kept in a small LRU so a refresh
// that brings the summary back shows them at once.
//
// The first page is the Worked list's first screen ({limit}); it is drawn as
// soon as it arrives, and kept with its etag in the window's wire cache, so a
// reload asks for it with {ifNoneMatch} and the daemon answers a receipt. The
// rest follows as background reads ({background}: a lane of their own on
// the daemon, behind nothing a reader waits for). Pages come {compact}:
// fields every call has are left out when null, a key that is the call id's
// and a title equal to the command; expandWorkEvent restores them exactly.
// An older daemon ignores limit, compact, ifNoneMatch and background.

import { call } from '../wire/api.ts';
import type { TimelineEvent } from '../store/types';
import { WORK_GROUP_READ_OPTIONS, stampToolDetail } from './history';
import { readWireCache, writeWireCache, isUnchangedReply } from '../store/wire-cache';

/** Rows whose loaded calls are kept across refreshes. */
export const WORK_GROUP_CACHE_ROWS = 16;
/** Calls in the first page: the Worked list's first screen and one ahead. */
export const WORK_GROUP_FIRST_PAGE = 80;
// A first page larger than this is not kept in the wire cache.
const FIRST_PAGE_CACHE_CHARS = 512_000;

const cache = new Map<string, TimelineEvent[]>();
const inflight = new Map<string, Promise<TimelineEvent[] | null>>();
const progress = new Map<string, Set<(events: TimelineEvent[], total: number) => void>>();

type GroupReply = { messageId: string; offset: number; next: number; total: number; events: unknown[]; etag?: string };

function isGroupReply(value: unknown, messageId: string, offset: number): value is GroupReply {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as Partial<GroupReply>;
  return reply.messageId === messageId && reply.offset === offset && Array.isArray(reply.events)
    && Number.isInteger(reply.next) && Number.isInteger(reply.total)
    && reply.next! <= reply.total! && (reply.next! > offset || reply.next === reply.total)
    && reply.events.length === reply.next! - offset;
}

const COMPACT_NULLS = ['id', 'toolKind', 'command', 'terminalId', 'input', 'output', 'location'] as const;

/** A call as a compact page sent it, restored to the call itself. */
export function expandWorkEvent(raw: unknown): TimelineEvent {
  const event = raw as Record<string, unknown>;
  if (!event || event.kind !== 'tool') return raw as TimelineEvent;
  for (const field of COMPACT_NULLS) if (!(field in event)) event[field] = null;
  if (!('key' in event)) event.key = `timeline:tool:${event.id}`;
  if (event.titleIs === 'command') { event.title = event.command; delete event.titleIs; }
  return event as unknown as TimelineEvent;
}

function remember(key: string, events: TimelineEvent[]) {
  cache.delete(key);
  cache.set(key, events);
  while (cache.size > WORK_GROUP_CACHE_ROWS) cache.delete(cache.keys().next().value!);
}

function firstPageCacheName(tabId: string, messageId: string): string {
  return `work-group.${tabId}.${messageId}`;
}

/** The row's calls read so far, as each page arrives. */
export function onWorkGroupProgress(tabId: string, messageId: string, listener: (events: TimelineEvent[], total: number) => void): () => void {
  const key = `${tabId}\u0000${messageId}`;
  const set = progress.get(key) ?? new Set();
  set.add(listener);
  progress.set(key, set);
  return () => { set.delete(listener); if (!set.size) progress.delete(key); };
}

// First pages read ahead of an open (prefetchWorkGroup), by row.
const firstPages = new Map<string, GroupReply>();
const FIRST_PAGES_KEPT = 32;
const prefetching = new Map<string, Promise<void>>();

async function readFirstPage(tabId: string, messageId: string, background: boolean): Promise<unknown> {
  const cacheName = firstPageCacheName(tabId, messageId);
  const kept = readWireCache(cacheName);
  // tail:1 makes a daemon that predates workGroup answer one bounded row,
  // which is not a group reply.
  const group = { messageId, offset: 0, limit: WORK_GROUP_FIRST_PAGE, compact: true, ifNoneMatch: kept?.etag ?? '' };
  let reply = await call('archiveLoad', tabId, { tail: 1, workGroup: group, ...WORK_GROUP_READ_OPTIONS, ...(background ? { background: true } : {}) });
  if (kept && isUnchangedReply(reply, kept.etag)) {
    try { reply = JSON.parse(kept.text); } catch { reply = null; }
  } else if (isGroupReply(reply, messageId, 0) && typeof reply.etag === 'string') {
    const text = JSON.stringify(reply);
    writeWireCache(cacheName, reply.etag, text.length <= FIRST_PAGE_CACHE_CHARS ? text : '');
  }
  return reply;
}

/** Read a row's first page ahead of its open (it is on screen, or about to
 *  be), at background priority. */
export function prefetchWorkGroup(tabId: string, messageId: string): Promise<void> {
  const key = `${tabId}\u0000${messageId}`;
  if (cache.has(key) || firstPages.has(key) || inflight.has(key)) return Promise.resolve();
  const running = prefetching.get(key);
  if (running) return running;
  const read = readFirstPage(tabId, messageId, true).then((reply) => {
    if (!isGroupReply(reply, messageId, 0)) return;
    firstPages.set(key, reply);
    while (firstPages.size > FIRST_PAGES_KEPT) firstPages.delete(firstPages.keys().next().value!);
  }).catch(() => undefined).finally(() => prefetching.delete(key));
  prefetching.set(key, read);
  return read;
}

/** The row's whole timeline, as its events; null when it cannot be read. */
export function loadWorkGroupEvents(tabId: string, messageId: string, onPage?: (events: TimelineEvent[], total: number) => void): Promise<TimelineEvent[] | null> {
  const key = `${tabId}\u0000${messageId}`;
  const cached = cache.get(key);
  if (cached) { remember(key, cached); return Promise.resolve(cached); }
  const unsubscribe = onPage ? onWorkGroupProgress(tabId, messageId, onPage) : null;
  const pending = inflight.get(key);
  if (pending) return pending.finally(() => unsubscribe?.());
  const read = async (ahead: GroupReply | undefined): Promise<TimelineEvent[] | null | 'changed'> => {
    const events: TimelineEvent[] = [];
    let offset = 0;
    let total = -1;
    for (;;) {
      const reply = offset === 0
        ? ahead ?? await readFirstPage(tabId, messageId, false)
        // The rest is read behind anything a reader waits for.
        : await call('archiveLoad', tabId, { tail: 1, workGroup: { messageId, offset, compact: true }, ...WORK_GROUP_READ_OPTIONS, background: true });
      if (!isGroupReply(reply, messageId, offset)) return null;
      // The row changed between pages (a first page read ahead of it):
      // read it again from the start.
      if (total >= 0 && reply.total !== total) return 'changed';
      total = reply.total;
      const page = reply.events.map(expandWorkEvent);
      stampToolDetail(page, tabId, messageId);
      for (const event of page) events.push(event);
      for (const listener of progress.get(key) ?? []) listener(events, reply.total);
      if (reply.next >= reply.total) {
        if (events.length !== reply.total) return null;
        break;
      }
      offset = reply.next;
    }
    remember(key, events);
    return events;
  };
  const ahead = firstPages.get(key);
  firstPages.delete(key);
  const load = read(ahead)
    .then((result) => (result === 'changed' ? read(undefined) : result))
    .then((result) => (result === 'changed' ? null : result))
    .catch(() => null).finally(() => { inflight.delete(key); unsubscribe?.(); });
  inflight.set(key, load);
  return load;
}

export function forgetWorkGroups(): void {
  cache.clear();
  firstPages.clear();
}
