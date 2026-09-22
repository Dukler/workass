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

export const PERM_LABEL: Readonly<Record<string, string>> = {
  allow_once: 'Permitir una vez',
  allow_always: 'Permitir siempre',
  allow: 'Permitir',
  reject_once: 'Rechazar',
  reject_always: 'Rechazar siempre',
  reject: 'Rechazar',
  cancel: 'Cancelar',
};

export interface PermissionOption {
  optionId: string;
  name: string;
  kind: string;
}

export interface PresentedPermissionChoice extends PermissionOption {
  label: string;
}

export function normalizedPermissionLabel(value: string): string {
  return value.trim().toLocaleLowerCase().replace(/[\s_-]+/g, ' ');
}

export function permissionChoices(options: readonly PermissionOption[]): PresentedPermissionChoice[] {
  return options.map((option) => {
    const name = option.name.trim();
    const fallback = PERM_LABEL[option.kind] ?? option.kind;
    const normalizedName = normalizedPermissionLabel(name);
    const redundant = !name ||
      normalizedName === normalizedPermissionLabel(option.kind) ||
      normalizedName === normalizedPermissionLabel(fallback);
    return { ...option, label: redundant ? fallback : name };
  });
}
