export interface ContextUsageSnapshot {
  used: number;
  size: number;
  updatedAt?: string;
}

export type ContextUsageByProvider = Record<string, ContextUsageSnapshot>;

export interface ContextUsageEventIdentity {
  tabId?: string | null;
  chatId?: string | null;
  sessionId?: string | null;
  providerId?: string | null;
}

export interface ContextUsageChatIdentity {
  id: string;
  chatId?: string | null;
  sessionId?: string | null;
}

const MAX_CONTEXT_PROVIDERS = 64;
const MAX_PROVIDER_ID_CHARS = 256;
const MAX_TIMESTAMP_CHARS = 128;

export function normalizeContextUsage(raw: unknown): ContextUsageSnapshot | null {
  if (!raw || typeof raw !== 'object') return null;
  const value = raw as { used?: unknown; size?: unknown; updatedAt?: unknown };
  const used = Number(value.used);
  const size = Number(value.size);
  if (!Number.isFinite(used) || !Number.isFinite(size) || used < 0 || !(size > 0)) return null;
  const snapshot: ContextUsageSnapshot = { used: Math.round(used), size: Math.round(size) };
  if (typeof value.updatedAt === 'string') {
    const updatedAt = value.updatedAt.trim();
    if (updatedAt && updatedAt.length <= MAX_TIMESTAMP_CHARS) snapshot.updatedAt = updatedAt;
  }
  return snapshot;
}

export function normalizeContextUsageByProvider(raw: unknown): ContextUsageByProvider {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return {};
  const out: ContextUsageByProvider = {};
  for (const [rawProviderID, value] of Object.entries(raw as Record<string, unknown>).slice(0, MAX_CONTEXT_PROVIDERS)) {
    const providerID = rawProviderID.trim();
    if (!providerID || providerID.length > MAX_PROVIDER_ID_CHARS) continue;
    const usage = normalizeContextUsage(value);
    if (usage) out[providerID] = usage;
  }
  return out;
}

export function withContextUsage(
  current: ContextUsageByProvider | undefined,
  rawProviderID: string | null | undefined,
  rawUsage: unknown,
): ContextUsageByProvider {
  const providerID = rawProviderID?.trim() ?? '';
  const usage = normalizeContextUsage(rawUsage);
  const normalized = normalizeContextUsageByProvider(current);
  if (!providerID || providerID.length > MAX_PROVIDER_ID_CHARS || !usage) return normalized;
  return { ...normalized, [providerID]: usage };
}

export function contextUsageForProvider(
  byProvider: ContextUsageByProvider | undefined,
  rawProviderID: string | null | undefined,
): ContextUsageSnapshot | null {
  const providerID = rawProviderID?.trim() ?? '';
  return providerID ? normalizeContextUsage(byProvider?.[providerID]) : null;
}

export function contextUsageIdentityMatches(
  event: ContextUsageEventIdentity,
  chat: ContextUsageChatIdentity,
): boolean {
  // Usage is accepted only with the complete actor address and its provider.
  // Session-only or partial identities can collide after a provider handover,
  // remote hydration, or a replacement conversation reuses one raw id.
  return !!event.tabId
    && !!event.chatId
    && !!event.providerId
    && event.tabId === chat.id
    && event.chatId === chat.chatId;
}

export function contextUsagePercent(usage?: ContextUsageSnapshot | null): number | null {
  if (!usage || !(usage.size > 0)) return null;
  return Math.max(0, Math.min(100, Math.round((usage.used / usage.size) * 100)));
}

export function compactContextTokens(value: number): string {
  if (value >= 1e6) return `${(value / 1e6).toFixed(1)}M`;
  if (value >= 1e3) return `${(value / 1e3).toFixed(1)}k`;
  return String(Math.round(value));
}
