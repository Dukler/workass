// Work groups: a history read with {workGroups} sends a settled row's folded
// calls as a summary (Msg.workSummary) and stubs that keep the row's layout.
// The calls themselves are read when something shows them (its Worked row
// opens, the rail lists a subagent's calls), a page at a time over the same
// archive channel, and kept in a small LRU so a refresh that brings the
// summary back shows them at once.

import { call } from '../wire/api.ts';
import type { TimelineEvent } from '../store/types';
import { WORK_GROUP_READ_OPTIONS, stampToolDetail } from './history';

/** Rows whose loaded calls are kept across refreshes. */
export const WORK_GROUP_CACHE_ROWS = 16;

const cache = new Map<string, TimelineEvent[]>();
const inflight = new Map<string, Promise<TimelineEvent[] | null>>();

type GroupReply = { messageId: string; offset: number; next: number; total: number; events: unknown[] };

function isGroupReply(value: unknown, messageId: string, offset: number): value is GroupReply {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as Partial<GroupReply>;
  return reply.messageId === messageId && reply.offset === offset && Array.isArray(reply.events)
    && Number.isInteger(reply.next) && Number.isInteger(reply.total)
    && reply.next! <= reply.total! && (reply.next! > offset || reply.next === reply.total);
}

function remember(key: string, events: TimelineEvent[]) {
  cache.delete(key);
  cache.set(key, events);
  while (cache.size > WORK_GROUP_CACHE_ROWS) cache.delete(cache.keys().next().value!);
}

/** The row's whole timeline, as its events; null when it cannot be read. */
export function loadWorkGroupEvents(tabId: string, messageId: string): Promise<TimelineEvent[] | null> {
  const key = `${tabId}\u0000${messageId}`;
  const cached = cache.get(key);
  if (cached) { remember(key, cached); return Promise.resolve(cached); }
  const pending = inflight.get(key);
  if (pending) return pending;
  const load = (async () => {
    const events: unknown[] = [];
    let offset = 0;
    for (;;) {
      // tail:1 makes a daemon that predates workGroup answer one bounded row,
      // which is not a group reply.
      const reply = await call('archiveLoad', tabId, { tail: 1, workGroup: { messageId, offset }, ...WORK_GROUP_READ_OPTIONS });
      if (!isGroupReply(reply, messageId, offset)) return null;
      events.push(...reply.events);
      if (reply.next >= reply.total) {
        if (events.length !== reply.total) return null;
        break;
      }
      offset = reply.next;
    }
    const stamped = stampToolDetail(events as TimelineEvent[], tabId, messageId);
    remember(key, stamped);
    return stamped;
  })().catch(() => null).finally(() => inflight.delete(key));
  inflight.set(key, load);
  return load;
}

export function forgetWorkGroups(): void {
  cache.clear();
}
