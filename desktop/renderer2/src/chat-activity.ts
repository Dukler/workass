import type { Chat } from './store/types';
import type { SpawnedWorkItem } from './wire/types';

// A background item is only work while something is still owed: a service (a
// dev server, a watcher) runs without the chat being busy, so it must not read
// as activity. Without this a single `expo start` left its chat "Trabajando"
// with a climbing clock for as long as the server lived.
export function isServiceWork(item: Pick<SpawnedWorkItem, 'role'>): boolean {
  return item.role === 'service';
}

// The sidebar dot represents work that is still alive for the chat, not only
// an in-flight model response. Claude background tasks can outlive the
// foreground turn, so they keep the chat active until their registry record
// reaches a terminal status.
export function chatHasLiveActivity(
  chat: Pick<Chat, 'messages'>,
  spawnedWork: readonly Pick<SpawnedWorkItem, 'status' | 'role'>[],
): boolean {
  return chat.messages.some((message) => message.status === 'running')
    || spawnedWork.some((item) => item.status === 'running' && !isServiceWork(item));
}
