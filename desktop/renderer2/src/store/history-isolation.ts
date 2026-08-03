import type { MessageImage, Msg, SteerAnchor, SteerState, TimelineEvent } from './types';
import { restoredAssistantContent, restoredAssistantResult } from '../assistant-output.ts';

type ArchiveRecord = Record<string, unknown>;

export interface ProviderHistoryMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  at: string | null;
}

function text(value: unknown): string { return value == null ? '' : String(value); }

function fingerprint(record: { role?: unknown; content?: unknown; result?: unknown; status?: unknown; at?: unknown }): string {
  return JSON.stringify([
    record.role ?? '',
    restoredAssistantContent(record.role, record.status, record.content),
    record.result ?? '',
    record.status ?? '',
    record.at ?? null,
  ]);
}

function legacyID(record: ArchiveRecord): string {
  const input = fingerprint(record);
  let hash = 2166136261;
  for (let i = 0; i < input.length; i++) {
    hash ^= input.charCodeAt(i);
    hash = Math.imul(hash, 16777619);
  }
  return `legacy-${(hash >>> 0).toString(36)}`;
}

function archiveToMsg(record: ArchiveRecord): Msg {
  return {
    id: text(record.id).trim() || legacyID(record),
    role: record.role === 'user' ? 'user' : 'assistant',
    content: restoredAssistantContent(record.role, record.status, record.content),
    result: restoredAssistantResult(record.content, record.result),
    status: (record.status as Msg['status']) ?? 'done',
    at: record.at == null ? null : text(record.at),
    jobId: record.jobId ? text(record.jobId) : undefined,
    steerState: record.steerState as SteerState | undefined,
    steerAnchor: record.steerAnchor as SteerAnchor | undefined,
    turnRootId: record.turnRootId ? text(record.turnRootId) : undefined,
    turnTerminal: typeof record.turnTerminal === 'boolean' ? record.turnTerminal : undefined,
    events: (record.events as TimelineEvent[]) ?? [],
    images: (record.images as MessageImage[]) ?? undefined,
  };
}

/** One message id belongs to one row. Later/live copies win without moving it. */
export function dedupeMessages(messages: Msg[]): Msg[] {
  const out: Msg[] = [];
  const positions = new Map<string, number>();
  for (const message of messages) {
    const prior = positions.get(message.id);
    if (prior == null) {
      positions.set(message.id, out.length);
      out.push(message);
    } else {
      out[prior] = message;
    }
  }
  return out;
}

/** Canonical transcript order is provider replay order. Steering rows and the
 * assistant segments around them are ordinary chronological messages. */
export function providerHistoryMessages(messages: readonly Msg[]): ProviderHistoryMessage[] {
  const out: ProviderHistoryMessage[] = [];
  const append = (message: Msg) => out.push({
    id: message.id, role: message.role, content: message.content, at: message.at,
  });
  for (const message of messages) {
    if (message.status !== 'done') continue;
    append(message);
  }
  return out;
}

/**
 * The archive and mirror are two copies of ONE chat, never two histories to
 * concatenate by count. Archive order supplies the full history; the live
 * mirror overlays matching rows and contributes only genuinely new rows.
 *
 * Live-only rows are legitimate and can be OLD: a turn whose daemon died
 * mid-flight is finalized locally but never reaches the archive. Appending
 * them after the spine rewrites visible chronology on every reload (and the
 * shuffled order then persists), so they are inserted at their chronological
 * position instead — as whole turn blocks, never row by row, because steer
 * chronology within a turn is positional, not timestamp-ordered.
 */
export function mergeArchivedHistory(current: Msg[], records: ArchiveRecord[]): Msg[] {
  const live = dedupeMessages(current);
  const liveByID = new Map(live.map((message) => [message.id, message]));
  const liveByFingerprint = new Map(live.map((message) => [fingerprint(message), message]));
  const used = new Set<string>();
  const merged: Msg[] = [];

  for (const record of records) {
    const archived = archiveToMsg(record);
    const rawID = text(record.id).trim();
    const replacement = (rawID ? liveByID.get(rawID) : liveByFingerprint.get(fingerprint(record))) ?? archived;
    if (used.has(replacement.id)) continue;
    used.add(replacement.id);
    merged.push(replacement);
  }
  for (const block of liveOnlyTurnBlocks(live, used)) {
    merged.splice(blockInsertIndex(merged, block), 0, ...block.rows);
  }
  return restoreSteerChronology(merged);
}

/**
 * Legacy offset-anchored archives appended a steer row at SUBMISSION time,
 * before the assistant it steered; the migrated mirror orders it after. The
 * archive keeps its legacy shape forever while the mirror lost its anchors on
 * first migration, so each reload would otherwise flip between the two
 * orderings. Re-assert the invariant that survives both eras: a steer row
 * never precedes its own turn root.
 */
function restoreSteerChronology(merged: Msg[]): Msg[] {
  for (let i = merged.length - 1; i >= 0; i--) {
    const row = merged[i];
    if (row.role !== 'user' || !row.turnRootId || row.steerBoundary === 'waiting') continue;
    let rootAt = -1;
    for (let j = i + 1; j < merged.length; j++) {
      if (merged[j].id === row.turnRootId) { rootAt = j; break; }
    }
    if (rootAt < 0) continue;
    merged.splice(i, 1);
    // The root shifted down one slot; inserting at its old index lands the
    // steer immediately after it (and before any later steer of the same
    // root, which this reverse walk already placed).
    merged.splice(rootAt, 0, row);
  }
  return merged;
}

interface LiveOnlyBlock {
  rows: Msg[];
  root?: string;
  job?: string;
  /** Nearest preceding live row that the archive spine already contains. */
  anchorId?: string;
}

function rowTime(message: { at?: string | null }): number | null {
  if (!message.at) return null;
  const t = Date.parse(message.at);
  return Number.isFinite(t) ? t : null;
}

function joinsBlock(block: LiveOnlyBlock, row: Msg): boolean {
  if (row.turnRootId && block.root) return row.turnRootId === block.root;
  // A user row only continues a block through an explicit shared turn root
  // (steer chronology); otherwise it opens a new turn.
  if (row.role === 'user') return false;
  if (row.jobId && block.job) return row.jobId === block.job;
  return true;
}

function liveOnlyTurnBlocks(live: Msg[], used: Set<string>): LiveOnlyBlock[] {
  const blocks: LiveOnlyBlock[] = [];
  let block: LiveOnlyBlock | null = null;
  let anchorId: string | undefined;
  for (const row of live) {
    if (used.has(row.id)) {
      block = null;
      anchorId = row.id;
      continue;
    }
    if (!block || !joinsBlock(block, row)) {
      block = { rows: [], anchorId };
      blocks.push(block);
    }
    block.rows.push(row);
    // A steered turn's root is the ORIGINAL assistant's id; the original row
    // itself often carries no turnRootId (staging only stamps the steer and
    // continuation). Adopt the assistant's own id as the block root so a
    // settled steer rejoins its turn instead of being timestamp-sorted ahead
    // of the assistant it must follow.
    if (!block.root) block.root = row.turnRootId ?? (row.role === 'assistant' ? row.id : undefined);
    if (!block.job && row.jobId) block.job = row.jobId;
  }
  return blocks;
}

function blockInsertIndex(merged: Msg[], block: LiveOnlyBlock): number {
  let ts: number | null = null;
  for (const row of block.rows) {
    ts = rowTime(row);
    if (ts != null) break;
  }
  if (ts == null) {
    // No usable timestamp (e.g. a still-running continuation): stay attached
    // to the same neighbor the live view showed it under.
    if (block.anchorId) {
      const at = merged.findIndex((message) => message.id === block.anchorId);
      if (at >= 0) return at + 1;
    }
    return merged.length;
  }
  // The spine is not timestamp-monotonic (steer rows sit positionally), so
  // compare against the running maximum instead of individual rows.
  let index = 0;
  let effective = Number.NEGATIVE_INFINITY;
  for (let i = 0; i < merged.length; i++) {
    const t = rowTime(merged[i]);
    if (t != null && t > effective) effective = t;
    if (effective <= ts) index = i + 1;
  }
  return index;
}
