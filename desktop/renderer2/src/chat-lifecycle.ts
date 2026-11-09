// When the sidebar files quiet chats away by itself (Ajustes · Mantenimiento).
// The setting lives in the daemon's app-settings blob under "chatLifecycle"
// (cmd/workass/chat_lifecycle_settings.go), so every window applies the same
// rule; both values are whole days and 0 means never.
//
//  - settleAfterDays: a chat with no activity for this long shows under «En
//    reposo» (SidebarV2 resolveSettled). Off by default: since 2026-09-21
//    nothing files by age unless the owner turns it on.
//  - archiveAfterDays: a chat in «En reposo» leaves the list for the
//    searchable archive this long after it got there (resolveArchived). Five
//    days is the rule the sidebar has always applied.
//
// Neither writes to a chat or deletes anything. Both are read from when a chat
// was last active and when it was put to rest: new activity brings a chat back
// by itself, a search finds an archived one, and Reactivar brings any back.

export interface ChatLifecycle {
  settleAfterDays: number;
  archiveAfterDays: number;
}

export const CHAT_LIFECYCLE_SETTINGS_KEY = 'chatLifecycle';
export const DEFAULT_CHAT_LIFECYCLE: Readonly<ChatLifecycle> = Object.freeze({ settleAfterDays: 0, archiveAfterDays: 5 });
const MAX_DAYS = 365;
const DAY_MS = 24 * 60 * 60 * 1000;

/** The choices Settings offers, 0 (never) last. */
export const SETTLE_AFTER_CHOICES: readonly number[] = [3, 7, 30, 0];
export const ARCHIVE_AFTER_CHOICES: readonly number[] = [5, 14, 30, 0];

function wholeDays(value: unknown, fallback: number): number {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 && value <= MAX_DAYS ? value : fallback;
}

export function parseChatLifecycle(raw: unknown): ChatLifecycle {
  const value = raw && typeof raw === 'object' ? raw as Record<string, unknown> : {};
  return {
    settleAfterDays: wholeDays(value.settleAfterDays, DEFAULT_CHAT_LIFECYCLE.settleAfterDays),
    archiveAfterDays: wholeDays(value.archiveAfterDays, DEFAULT_CHAT_LIFECYCLE.archiveAfterDays),
  };
}

/** The policy in a settings:get reply ({settings: {...}}). */
export function chatLifecycleFromSettings(reply: unknown): ChatLifecycle {
  const settings = reply && typeof reply === 'object' ? (reply as Record<string, unknown>).settings : undefined;
  if (!settings || typeof settings !== 'object') return { ...DEFAULT_CHAT_LIFECYCLE };
  return parseChatLifecycle((settings as Record<string, unknown>)[CHAT_LIFECYCLE_SETTINGS_KEY]);
}

/** The settings blob with this policy in it and every other key kept. */
export function withChatLifecycleInSettings(current: Record<string, unknown> | null | undefined, value: ChatLifecycle): Record<string, unknown> {
  return { ...(current ?? {}), [CHAT_LIFECYCLE_SETTINGS_KEY]: parseChatLifecycle(value) };
}

export function sameChatLifecycle(a: ChatLifecycle, b: ChatLifecycle): boolean {
  return a.settleAfterDays === b.settleAfterDays && a.archiveAfterDays === b.archiveAfterDays;
}

/** How long a quiet chat waits before it is filed by age; 0 is never. */
export function settleAfterMs(policy: ChatLifecycle): number {
  return policy.settleAfterDays * DAY_MS;
}

/** How long a chat stays in «En reposo» before the archive; 0 is never. */
export function archiveAfterMs(policy: ChatLifecycle): number {
  return policy.archiveAfterDays * DAY_MS;
}

/** The choices to show: the offered ones, plus the stored value when it is
 *  another (set by an older or newer window), kept in its place. */
export function lifecycleChoices(offered: readonly number[], current: number): number[] {
  if (offered.includes(current)) return [...offered];
  const days = offered.filter((value) => value > 0);
  const never = offered.includes(0) ? [0] : [];
  return [...[...days, current].sort((a, b) => a - b), ...never];
}

export function lifecycleDaysLabel(days: number): string {
  if (days <= 0) return 'Nunca';
  return days === 1 ? '1 día' : `${days} días`;
}
