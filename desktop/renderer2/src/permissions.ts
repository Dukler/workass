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
  allow_once: 'Permitir',
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

// Generic English option names say no more than their kind; they take the
// localized kind label so a card never mixes "Reject" with "Permitir".
// Specific names ("Yes, and don't ask again for git") are kept.
const GENERIC_OPTION_NAMES = new Set([
  'allow', 'allow once', 'always allow', 'allow always', 'yes',
  'reject', 'reject once', 'always reject', 'reject always', 'deny', 'decline', 'no', 'cancel',
]);

export function permissionChoices(options: readonly PermissionOption[]): PresentedPermissionChoice[] {
  return options.map((option) => {
    const name = option.name.trim();
    const fallback = PERM_LABEL[option.kind] ?? option.kind;
    const normalizedName = normalizedPermissionLabel(name);
    const redundant = !name ||
      normalizedName === normalizedPermissionLabel(option.kind) ||
      normalizedName === normalizedPermissionLabel(fallback) ||
      (PERM_LABEL[option.kind] !== undefined && GENERIC_OPTION_NAMES.has(normalizedName));
    return { ...option, label: redundant ? fallback : name };
  });
}
