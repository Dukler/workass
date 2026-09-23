import { useCallback, useEffect, useRef, useSyncExternalStore } from 'react';
import type { Chat, Msg, PermissionState } from '../store/types';
import { store } from '../store/store';
import { PermCard } from './messages';

// A pending agent question takes the composer's place (Claude Code / Codex
// style). Permissions attach to a recent assistant row, so only the last few
// assistant rows are watched; token streams bump those topics, but the string
// snapshot only changes when a question appears, starts sending, or clears.
const QUESTION_SCAN = 6;

interface PendingQuestion { msg: Msg; perm: PermissionState }

function recentAssistantRows(chat: Chat | null): Msg[] {
  const rows: Msg[] = [];
  if (!chat) return rows;
  for (let index = chat.messages.length - 1; index >= 0 && rows.length < QUESTION_SCAN; index--) {
    const message = chat.messages[index];
    if (message.role === 'assistant') rows.push(message);
  }
  return rows;
}

export function findPendingQuestion(chat: Chat | null): PendingQuestion | null {
  for (const msg of recentAssistantRows(chat)) {
    if (msg.permission?.question && msg.turnTerminal !== false) return { msg, perm: msg.permission };
  }
  return null;
}

export function usePendingQuestion(chat: Chat | null): PendingQuestion | null {
  const chatId = chat?.id ?? '';
  const topicsKey = recentAssistantRows(chat).map((message) => message.id).join('\0');
  const subscribe = useCallback((callback: () => void) => {
    const unsubscribers = topicsKey ? topicsKey.split('\0').map((id) => store.subscribe(`msg:${id}`, callback)) : [];
    return () => { for (const unsubscribe of unsubscribers) unsubscribe(); };
  }, [topicsKey]);
  const snapshot = useCallback(() => {
    const pending = findPendingQuestion(chatId ? store.chat(chatId) ?? null : null);
    return pending ? `${pending.msg.id}\0${pending.perm.id}\0${pending.perm.resolved ?? ''}` : '';
  }, [chatId]);
  useSyncExternalStore(subscribe, snapshot, snapshot);
  return findPendingQuestion(chatId ? store.chat(chatId) ?? null : null);
}

export function QuestionDock({ chatId, pending }: { chatId: string; pending: PendingQuestion }) {
  const ref = useRef<HTMLDivElement>(null);
  // Take focus when a new question lands so the keyboard works immediately,
  // the way the composer it replaces would have held it.
  useEffect(() => {
    ref.current?.querySelector<HTMLElement>('[data-qrow]')?.focus({ preventScroll: true });
  }, [pending.perm.id]);
  return (
    <div className="qdock-slot" ref={ref}>
      <PermCard key={pending.perm.id} perm={pending.perm} tabId={chatId} msgId={pending.msg.id} />
    </div>
  );
}
