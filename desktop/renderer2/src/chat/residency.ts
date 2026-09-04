import type { Chat } from '../store/types';

export const CHAT_MESSAGE_TAIL = 60;
export const CHAT_INITIAL_HISTORY = 10;

// Full transcripts are resident only while explicitly reading older history or
// searching it. Ordinary navigation keeps a small recent slice; inactive chats
// retain at most the bounded actor tail.
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

// A heartbeat may carry only the bounded actor tail. Preserve an already-loaded
// full ledger by replacing its overlapping suffix with the authoritative tail.
// Actor revisions advance while a turn streams, so equality is not a safe
// prerequisite: requiring it made every five-second digest collapse the active
// transcript to 60 rows until archive-load put the missing rows back.
export function preserveUnchangedFullHistories(previous: Chat[], restored: Chat[]): string[] {
  const preserved: string[] = [];
  const previousByID = new Map(previous.map((chat) => [chat.id, chat]));
  for (const chat of restored) {
    const prior = previousByID.get(chat.id);
    if (!prior?.historyComplete || chat.historyComplete || prior.chatId !== chat.chatId) continue;
    if (prior.actorRevision === chat.actorRevision) {
      chat.messages = prior.messages;
      chat.messageCount = prior.messageCount ?? prior.messages.length;
      chat.historyComplete = true;
      preserved.push(chat.id);
      continue;
    }
    const firstTailID = chat.messages[0]?.id;
    const overlap = firstTailID ? prior.messages.findIndex((message) => message.id === firstTailID) : -1;
    if (overlap < 0) continue;
    const merged = [...prior.messages.slice(0, overlap), ...chat.messages];
    const authoritativeCount = chat.messageCount ?? merged.length;
    if (merged.length !== authoritativeCount) continue;
    chat.messages = merged;
    chat.messageCount = authoritativeCount;
    chat.historyComplete = true;
    preserved.push(chat.id);
  }
  return preserved;
}
