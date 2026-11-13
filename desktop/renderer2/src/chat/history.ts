import type { MirrorMsg } from '../store/persistence';
import type { Msg, TimelineEvent } from '../store/types';

const STATUSES = new Set<Msg['status']>(['pending', 'running', 'done', 'failed', 'cancelled']);

// Every history read asks for references instead of payloads: images load when
// seen, large tool payloads when their row opens, and a settled turn's folded
// calls (workGroups) when its Worked row opens. Older daemons ignore all three.
// longRows: a settled reply of hundreds of kilobytes comes as its last part
// (Msg.contentFrom); the rest is read as the reader scrolls up to it.
export const HISTORY_READ_OPTIONS = { deferImages: true, leanTools: true, leanTitles: true, workGroups: true, longRows: true } as const;

// A folded row's calls are read as its Worked row draws them: no input, the
// head of each output (the rest loads when a call opens), images by reference.
export const WORK_GROUP_READ_OPTIONS = { deferImages: true, leanTools: true, leanTitles: true, toolSummaries: true } as const;

// Actor projections already carry canonical chronology and stable message ids.
// Hydration is therefore a validation + shape conversion, never a merge,
// fingerprint match, replay, or chronology rewrite.
export function actorMessages(records: readonly MirrorMsg[], tabId?: string): Msg[] {
  return records.map((record) => {
    const id = String(record.id ?? '').trim();
    if (!id) throw new Error('actor history row is missing its stable message id');
    if (record.role !== 'user' && record.role !== 'assistant') {
      throw new Error(`actor history row ${id} has an invalid role`);
    }
    if (!STATUSES.has(record.status)) {
      throw new Error(`actor history row ${id} has an invalid status`);
    }
    const interruptedSteer = record.role === 'user'
      && record.status === 'pending'
      && record.steerState === 'sending';
    return {
      id,
      role: record.role,
      content: record.content == null ? '' : String(record.content),
      result: record.result == null ? undefined : String(record.result),
      status: interruptedSteer ? 'done' : record.status,
      steerState: interruptedSteer ? 'uncertain' : record.steerState,
      agentQueueId: record.agentQueueId,
      steerBoundary: record.steerBoundary,
      steerContinuationId: record.steerContinuationId,
      steerContinuationFor: record.steerContinuationFor,
      turnRootId: record.turnRootId,
      turnTerminal: record.turnTerminal,
      at: record.at == null ? null : String(record.at),
      jobId: record.jobId,
      permission: record.permission,
      turnStartedAt: record.turnStartedAt,
      interrupted: record.interrupted,
      inherited: record.inherited === true ? true : undefined,
      notice: record.notice && typeof record.notice.kind === 'string' ? record.notice : undefined,
      images: record.images,
      workSummary: record.workSummary && typeof record.workSummary === 'object' ? record.workSummary : undefined,
      ...longRowFields(record),
      events: stampToolDetail(rebaseEvents((Array.isArray(record.events) ? record.events : []) as TimelineEvent[], longRowFrom(record)), tabId, id),
    };
  });
}

// A long row's last part (longRows): its text starts at contentFrom of the
// whole reply (UTF-16, like every event's at), whose length is contentLength.
function longRowFrom(record: MirrorMsg): number {
  const from = (record as { contentFrom?: unknown }).contentFrom;
  return typeof from === 'number' && Number.isInteger(from) && from > 0 ? from : 0;
}

function longRowFields(record: MirrorMsg): { contentFrom?: number; contentLength?: number } {
  const from = longRowFrom(record);
  if (!from) return {};
  const length = (record as { contentLength?: unknown }).contentLength;
  return { contentFrom: from, contentLength: typeof length === 'number' ? length : undefined };
}

/** Events placed in text that starts at `from` of the whole reply. */
export function rebaseEvents(events: TimelineEvent[], from: number, longRow = from > 0): TimelineEvent[] {
  if (longRow) expandLongRowImages(events);
  if (!from) return events;
  return events.map((event) => ({ ...event, at: event.at - from }) as TimelineEvent);
}

/** See chat/long-row.ts (kept here, where hydration needs it, to avoid an
 *  import cycle). */
function expandLongRowImages(events: TimelineEvent[]): TimelineEvent[] {
  for (const event of events) {
    if (event.kind !== 'tool' || !Array.isArray(event.images)) continue;
    for (const image of event.images as unknown as Array<Record<string, unknown>>) {
      if (typeof image.deferredImageRef !== 'string' || !image.deferredImageRef) continue;
      if (!('data' in image)) image.data = '';
      if (!('mimeType' in image)) image.mimeType = 'image/png';
    }
  }
  return events;
}

// A deferred payload is read back by its exact owner: the tab the row was read
// from (a tagged remote id routes to its machine) and the row's message id.
export function stampToolDetail(events: TimelineEvent[], tabId: string | undefined, messageId: string): TimelineEvent[] {
  if (!tabId) return events;
  for (const event of events) {
    if (event.kind === 'tool' && (event.outputDeferred || event.inputDeferred || event.titleDeferred)) event.detail = { tabId, messageId };
  }
  return events;
}
