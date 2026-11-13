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

function isHeadReply(value: unknown, messageId: string, before: number | undefined): value is HeadReply {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as Partial<HeadReply>;
  return reply.messageId === messageId && typeof before === 'number' && reply.to === before && typeof reply.content === 'string'
    && Number.isInteger(reply.from) && reply.from! >= 0 && reply.from! < before && Array.isArray(reply.events);
}

const loading = new Map<string, Promise<boolean>>();

// Every part before what a reply holds, read in one background read as soon
// as its last part is on screen ({contentHead: {parts: true}}): the parts
// are framed as the daemon kept them, nothing is decoded twice, and the
// reader scrolling up finds them here. One reply's worth per message.
const ahead = new Map<string, Promise<HeadReply[] | null>>();
const AHEAD_KEPT = 4;

function isPartsReply(value: unknown, messageId: string, before: number): value is { messageId: string; parts: HeadReply[] } {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as { messageId?: unknown; parts?: unknown };
  if (reply.messageId !== messageId || !Array.isArray(reply.parts) || !reply.parts.length) return false;
  let from = 0;
  for (const part of reply.parts as HeadReply[]) {
    if (!isHeadReply(part, messageId, part?.to) || part.from !== from) return false;
    from = part.to;
  }
  return from === before;
}

/** Read every earlier part of a long reply in the background. */
export function prefetchLongRow(tabId: string, msg: Msg): void {
  const before = msg.contentFrom ?? 0;
  if (!before) return;
  const key = `${tabId}\u0000${msg.id}\u0000${before}`;
  if (ahead.has(key)) return;
  ahead.set(key, (async () => {
    const reply = await call('archiveLoad', tabId, { tail: 1, contentHead: { messageId: msg.id, before, parts: true }, ...HISTORY_READ_OPTIONS, background: true });
    return isPartsReply(reply, msg.id, before) ? reply.parts : null;
  })().catch(() => null));
  while (ahead.size > AHEAD_KEPT) ahead.delete(ahead.keys().next().value!);
}

function prepend(tabId: string, msg: Msg, from: number, content: string, rawEvents: unknown[]): void {
  const before = msg.contentFrom ?? 0;
  const head = stampToolDetail(rebaseEvents(rawEvents as TimelineEvent[], from, true), tabId, msg.id);
  const shift = before - from;
  msg.events = [...head, ...msg.events.map((event) => ({ ...event, at: event.at + shift }) as TimelineEvent)];
  msg.content = content + msg.content;
  if (from > 0) msg.contentFrom = from;
  else { delete msg.contentFrom; delete msg.contentLength; }
}

/** Read the part before what the reply holds (all: every part), and put it
 *  in front. False when it cannot be read. */
export function loadContentHead(tabId: string, msg: Msg, all = false): Promise<boolean> {
  const before = msg.contentFrom ?? 0;
  if (!before) return Promise.resolve(true);
  const key = `${tabId}\u0000${msg.id}\u0000${before}\u0000${all}`;
  const pending = loading.get(key);
  if (pending) return pending;
  const read = (async () => {
    // The parts read ahead, when they are here or on their way: all of them.
    const pending = ahead.get(`${tabId}\u0000${msg.id}\u0000${before}`);
    const parts = pending ? await pending : null;
    if (parts) {
      if ((msg.contentFrom ?? 0) !== before) return true;
      prepend(tabId, msg, 0, parts.map((part) => part.content).join(''), parts.flatMap((part) => part.events));
      return true;
    }
    const reply = await call('archiveLoad', tabId, { tail: 1, contentHead: { messageId: msg.id, before, ...(all ? { all: true } : {}) }, ...HISTORY_READ_OPTIONS });
    if (!isHeadReply(reply, msg.id, before)) return false;
    // Another read may have put it in front meanwhile.
    if ((msg.contentFrom ?? 0) !== before) return true;
    prepend(tabId, msg, reply.from, reply.content, reply.events);
    return true;
  })().catch(() => false).finally(() => loading.delete(key));
  loading.set(key, read);
  return read;
}
