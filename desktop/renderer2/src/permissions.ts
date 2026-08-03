import type { Chat } from './store/types';

export function clearPermissionById(chats: Chat[], permissionId: string): string[] {
  if (!permissionId) return [];
  const touched: string[] = [];
  for (const chat of chats) {
    for (const message of chat.messages) {
      if (message.permission?.id !== permissionId) continue;
      message.permission = undefined;
      touched.push(message.id);
    }
  }
  return touched;
}

export function clearPermissionsOutsideSnapshot(chats: Chat[], pendingIds: ReadonlySet<string>): string[] {
  const touched: string[] = [];
  for (const chat of chats) {
    for (const message of chat.messages) {
      if (!message.permission || pendingIds.has(message.permission.id)) continue;
      message.permission = undefined;
      touched.push(message.id);
    }
  }
  return touched;
}
