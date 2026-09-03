import type { MirrorMsg } from '../store/persistence';
import type { Msg, TimelineEvent } from '../store/types';

const STATUSES = new Set<Msg['status']>(['pending', 'running', 'done', 'failed', 'cancelled']);

// Actor projections already carry canonical chronology and stable message ids.
// Hydration is therefore a validation + shape conversion, never a merge,
// fingerprint match, replay, or chronology rewrite.
export function actorMessages(records: readonly MirrorMsg[]): Msg[] {
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
      images: record.images,
      events: (Array.isArray(record.events) ? record.events : []) as TimelineEvent[],
    };
  });
}
