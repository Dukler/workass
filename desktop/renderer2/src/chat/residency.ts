import type { Chat } from '../store/types';

export const CHAT_MESSAGE_TAIL = 60;

// Full transcripts are resident only for the chat being read. Inactive chats
// keep a bounded actor tail and can reload their complete ledger on demand.
export function releaseInactiveHistories(chats: Chat[], activeId: string | null): string[] {
  const released: string[] = [];
  for (const chat of chats) {
    if (chat.id === activeId || chat.messages.length <= CHAT_MESSAGE_TAIL) continue;
    if (chat.messages.some((message) => message.status === 'running' || message.status === 'pending')) continue;
    chat.messageCount = Math.max(chat.messageCount ?? 0, chat.messages.length);
    chat.messages = chat.messages.slice(-CHAT_MESSAGE_TAIL);
    chat.historyComplete = false;
    released.push(chat.id);
  }
  return released;
}

// A heartbeat with the same actor revision may carry only the bounded tail.
// Preserve an already-loaded full ledger instead of replacing it with that tail.
export function preserveUnchangedFullHistories(previous: Chat[], restored: Chat[]): string[] {
  const preserved: string[] = [];
  const previousByID = new Map(previous.map((chat) => [chat.id, chat]));
  for (const chat of restored) {
    const prior = previousByID.get(chat.id);
    if (!prior?.historyComplete || chat.historyComplete || prior.actorRevision !== chat.actorRevision) continue;
    chat.messages = prior.messages;
    chat.messageCount = prior.messageCount ?? prior.messages.length;
    chat.historyComplete = true;
    preserved.push(chat.id);
  }
  return preserved;
}
