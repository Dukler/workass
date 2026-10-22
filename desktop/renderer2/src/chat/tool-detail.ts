// Lean history sends a large tool payload as a head; the whole output is read
// only when its row opens, in bounded chunks of the same archive channel. Read
// outputs are kept in a small LRU so reopening a row, or a refresh that
// replaces the row object with another lean head, shows it at once.

import { call } from '../wire/api.ts';
import type { ToolEvent } from '../store/types';

/** UTF-16 code units of read outputs kept across opens. */
export const TOOL_DETAIL_CACHE_UNITS = 16 * 1024 * 1024;

const cache = new Map<string, string>();
let cachedUnits = 0;
const inflight = new Map<string, Promise<string | null>>();

type DetailReply = { text: string; offset: number; next: number; total: number };

type DetailField = 'output' | 'title';

function detailKey(tool: Pick<ToolEvent, 'key' | 'detail'>, field: DetailField = 'output'): string | null {
  const owner = tool.detail;
  if (!owner?.tabId || !owner.messageId || !tool.key) return null;
  // Outputs keep their original key; a title is cached beside its output.
  return `${owner.tabId}\u0000${owner.messageId}\u0000${tool.key}${field === 'output' ? '' : `\u0000${field}`}`;
}

function remember(key: string, text: string): void {
  const prior = cache.get(key);
  if (prior !== undefined) { cache.delete(key); cachedUnits -= prior.length; }
  if (text.length > TOOL_DETAIL_CACHE_UNITS) return;
  cache.set(key, text);
  cachedUnits += text.length;
  for (const [oldest, value] of cache) {
    if (cachedUnits <= TOOL_DETAIL_CACHE_UNITS) break;
    cache.delete(oldest);
    cachedUnits -= value.length;
  }
}

function isDetailReply(value: unknown, offset: number): value is DetailReply {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const reply = value as Partial<DetailReply>;
  return typeof reply.text === 'string' && reply.offset === offset
    && Number.isInteger(reply.next) && Number.isInteger(reply.total)
    && reply.next! <= reply.total! && (reply.next! > offset || reply.next === reply.total);
}

/** The whole output if it was already read, else null. */
export function cachedToolOutput(tool: Pick<ToolEvent, 'key' | 'detail' | 'outputDeferred'>): string | null {
  if (!tool.outputDeferred) return null;
  const key = detailKey(tool);
  const text = key ? cache.get(key) : undefined;
  if (key && text !== undefined) remember(key, text);
  return text ?? null;
}

/** Reads a deferred output in full; null when it cannot be read. */
export function loadToolOutput(tool: Pick<ToolEvent, 'key' | 'detail' | 'outputDeferred'>): Promise<string | null> {
  return loadToolField(tool, 'output', !!tool.outputDeferred);
}

/** The whole title if it was already read, else null. */
export function cachedToolTitle(tool: Pick<ToolEvent, 'key' | 'detail' | 'titleDeferred'>): string | null {
  if (!tool.titleDeferred) return null;
  const key = detailKey(tool, 'title');
  const text = key ? cache.get(key) : undefined;
  if (key && text !== undefined) remember(key, text);
  return text ?? null;
}

/** Reads a deferred title in full; null when it cannot be read. */
export function loadToolTitle(tool: Pick<ToolEvent, 'key' | 'detail' | 'titleDeferred'>): Promise<string | null> {
  return loadToolField(tool, 'title', !!tool.titleDeferred);
}

function loadToolField(tool: Pick<ToolEvent, 'key' | 'detail'>, field: DetailField, deferred: boolean): Promise<string | null> {
  const key = detailKey(tool, field);
  if (!deferred || !key || !tool.detail) return Promise.resolve(null);
  const cached = cache.get(key);
  if (cached !== undefined) return Promise.resolve(cached);
  const pending = inflight.get(key);
  if (pending) return pending;
  const { tabId, messageId } = tool.detail;
  const load = (async () => {
    const parts: string[] = [];
    let offset = 0;
    for (;;) {
      const reply = await call('archiveLoad', tabId, { tail: 1, toolDetail: { messageId, key: tool.key, field, offset } });
      if (!isDetailReply(reply, offset)) return null;
      parts.push(reply.text);
      if (reply.next >= reply.total) break;
      offset = reply.next;
    }
    const text = parts.join('');
    remember(key, text);
    return text;
  })().finally(() => inflight.delete(key));
  inflight.set(key, load);
  return load;
}

export function forgetToolOutputs(): void {
  cache.clear();
  cachedUnits = 0;
}
