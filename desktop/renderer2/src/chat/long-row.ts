// Long rows (history longRows): a reply of hundreds of kilobytes arrives as
// its last part, and its earlier parts are read as the reader scrolls up to
// them, a part at a time, or whole when find or copy needs all of it.
//
// A part arrives above what the reader sees: the transcript keeps the
// reader's line where it is (Transcript holdAnchor). When the element it
// holds was re-made by the re-render, the reply notes how much it grew above
// its last part (noteGrowthAbove) and the transcript moves by exactly that.

import { call } from '../wire/api.ts';
import type { Msg, TimelineEvent } from '../store/types';
import { HISTORY_READ_OPTIONS, rebaseEvents, stampToolDetail } from './history';

const growth = new Map<string, number>();

/** A reply grew this much above the text it had (one commit). */
export function noteGrowthAbove(messageId: string, px: number): void {
  if (Math.abs(px) >= 0.5) growth.set(messageId, (growth.get(messageId) ?? 0) + px);
}

/** The growth noted for the reply this commit, once. */
export function takeGrowthAbove(messageId: string | undefined): number {
  if (!messageId) return 0;
  const px = growth.get(messageId) ?? 0;
  growth.delete(messageId);
  return px;
}

/** Forget growth noted by replies that are not the anchor. */
export function clearGrowth(): void {
  growth.clear();
}

type HeadReply = { messageId: string; from: number; to: number; content: string; events: unknown[] };

function isHeadReply(value: unknown, messageId: string, before: number): value is HeadReply {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as Partial<HeadReply>;
  return reply.messageId === messageId && reply.to === before && typeof reply.content === 'string'
    && Number.isInteger(reply.from) && reply.from! >= 0 && reply.from! < before && Array.isArray(reply.events);
}

const loading = new Map<string, Promise<boolean>>();

/** Read the part before what the reply holds (all: every part), and put it
 *  in front. False when it cannot be read. */
export function loadContentHead(tabId: string, msg: Msg, all = false): Promise<boolean> {
  const before = msg.contentFrom ?? 0;
  if (!before) return Promise.resolve(true);
  const key = `${tabId}\u0000${msg.id}\u0000${before}\u0000${all}`;
  const pending = loading.get(key);
  if (pending) return pending;
  const read = (async () => {
    const reply = await call('archiveLoad', tabId, { tail: 1, contentHead: { messageId: msg.id, before, ...(all ? { all: true } : {}) }, ...HISTORY_READ_OPTIONS });
    if (!isHeadReply(reply, msg.id, before)) return false;
    // Another read may have put it in front meanwhile.
    if ((msg.contentFrom ?? 0) !== before) return true;
    const head = stampToolDetail(rebaseEvents(reply.events as TimelineEvent[], reply.from, true), tabId, msg.id);
    const shift = before - reply.from;
    msg.events = [...head, ...msg.events.map((event) => ({ ...event, at: event.at + shift }) as TimelineEvent)];
    msg.content = reply.content + msg.content;
    if (reply.from > 0) msg.contentFrom = reply.from;
    else { delete msg.contentFrom; delete msg.contentLength; }
    return true;
  })().catch(() => false).finally(() => loading.delete(key));
  loading.set(key, read);
  return read;
}
