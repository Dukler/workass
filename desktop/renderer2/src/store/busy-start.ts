import type { QueuedJobStart } from '../wire/types';
import type { Chat, MessageImage, QueuedMsg } from './types';

export function isQueuedJobStart(value: unknown): value is QueuedJobStart {
  if (!value || typeof value !== 'object') return false;
  const receipt = value as Partial<QueuedJobStart>;
  return receipt.queued === true
    && typeof receipt.queueId === 'string'
    && receipt.queueId.length > 0
    && Number.isInteger(receipt.position)
    && Number.isInteger(receipt.agentQueueRevision);
}

export interface BusyStartReconcileResult {
  alreadyStarted: boolean;
}

// Reconcile the optimistic pair against the daemon's durable FIFO receipt.
// A session refresh may race either side of the reply, so this is idempotent:
// current/newer daemon state wins, and an already-started promoted turn is
// never removed merely because its original busy receipt arrived late.
export function reconcileQueuedJobStart(
  chat: Chat,
  origin: { userId: string; assistantId: string },
  prompt: string,
  images: MessageImage[] | undefined,
  receipt: QueuedJobStart,
): BusyStartReconcileResult {
  const currentRevision = Number.isInteger(chat.agentQueueRevision) ? chat.agentQueueRevision ?? 0 : 0;
  const assistant = chat.messages.find((message) => message.id === origin.assistantId);
  const alreadyStarted = !!assistant?.jobId;

  if (!alreadyStarted && currentRevision <= receipt.agentQueueRevision) {
    chat.messages = chat.messages.filter((message) => message.id !== origin.userId
      && message.id !== origin.assistantId
      && message.turnRootId !== origin.assistantId);
  }

  if (currentRevision <= receipt.agentQueueRevision) {
    const queue = [...(chat.queue ?? [])];
    if (!queue.some((item) => item.id === receipt.queueId)) {
      const item: QueuedMsg = {
        id: receipt.queueId,
        text: prompt,
        source: 'host',
        delivery: 'queue',
        queuedAt: receipt.queuedAt,
        images,
      };
      const index = Math.max(0, Math.min(queue.length, receipt.position - 1));
      queue.splice(index, 0, item);
    }
    chat.queue = queue.length ? queue : undefined;
    chat.agentQueueRevision = receipt.agentQueueRevision;
  }
  return { alreadyStarted };
}
