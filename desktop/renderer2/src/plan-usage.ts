import type { PlanUsageEntry, RateLimitResetCredit, RateLimitResetCreditsSummary } from './wire/types';

const RATE_LIMIT_LABEL: Record<string, string> = {
  five_hour: 'Límite de 5 horas',
  seven_day: 'Límite semanal',
  seven_day_oauth_apps: 'Semanal · Apps OAuth',
  seven_day_opus: 'Semanal · Opus',
  seven_day_sonnet: 'Semanal · Sonnet',
};

export function clampPlanUsagePercent(value: unknown): number | null {
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isFinite(n)) return null;
  return Math.max(0, Math.min(100, n));
}

export function formatPlanUsagePercent(value: unknown): string | null {
  const n = clampPlanUsagePercent(value);
  if (n == null) return null;
  return Number.isInteger(n) ? String(n) : n.toFixed(1).replace(/\.0$/, '');
}

export function rateLimitLabel(id: string | undefined, limitName?: string): string {
  if (!id) return limitName || 'Límite';
  const base = id.includes(':') ? id.slice(id.lastIndexOf(':') + 1) : id;
  if (id.startsWith('seven_day_model:')) return `Semanal · ${limitName || base}`;
  const label = RATE_LIMIT_LABEL[base];
  if (label) return id.includes(':') && limitName ? `${limitName} · ${label}` : label;
  const human = base.replace(/[_-]+/g, ' ').trim();
  return human ? human.charAt(0).toUpperCase() + human.slice(1) : id;
}

export function relativePlanReset(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return '';
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return '';
  const diff = t - now;
  if (diff <= 0) return 'reiniciado';
  const min = Math.floor(diff / 60000);
  if (min < 1) return 'en menos de 1 min';
  if (min < 60) return `en ${min} min`;
  const h = Math.floor(min / 60);
  if (h < 24) { const m = min % 60; return m ? `en ${h} h ${m} min` : `en ${h} h`; }
  const d = Math.floor(h / 24);
  if (d < 365) return `en ${d} d`;
  return `en ${Math.floor(d / 365)} a`;
}

// A reset within the next 24h gets a live, second-by-second countdown instead of
// the coarse "en 3 h" label; anything a day or more away stays static.
export function isLiveReset(iso: string | undefined, now: number = Date.now()): boolean {
  if (!iso) return false;
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return false;
  const diff = t - now;
  return diff > 0 && diff < 24 * 60 * 60 * 1000;
}

// A provider percentage belongs to one exact reset window. Once that boundary
// passes, the old percentage is no longer current account data and must not be
// presented as if it still applied while the metadata refresh is in flight.
export function isExpiredPlanReset(iso: string | undefined, now: number = Date.now()): boolean {
  if (!iso) return false;
  const t = Date.parse(iso);
  return Number.isFinite(t) && t <= now;
}

// Countdown clock for a live reset: "H:MM:SS" past an hour, "M:SS" under one.
// Non-positive / invalid input reads as already reset.
export function formatCountdown(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return 'reiniciado';
  const total = Math.floor(ms / 1000);
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600);
  const p = (n: number) => String(n).padStart(2, '0');
  return h > 0 ? `${h}:${p(m)}:${p(s)}` : `${m}:${p(s)}`;
}

export function isHotRateLimit(entry: PlanUsageEntry): boolean {
  const percent = clampPlanUsagePercent(entry.usedPercent);
  return (!!entry.status && entry.status !== 'allowed') || (percent != null && percent >= 80);
}

export interface AvailableRateLimitReset {
  count: number;
  credit?: RateLimitResetCredit;
}

export interface RateLimitResetAttempt {
  creditKey: string;
  idempotencyKey: string;
}

// `availableCount` is authoritative: Codex may expose a count while omitting or
// capping the detail rows. Prefer a concrete available credit when one exists,
// but keep the action usable without an id so Codex can select the next credit.
export function availableRateLimitReset(summary: RateLimitResetCreditsSummary | undefined): AvailableRateLimitReset | null {
  const count = Math.max(0, Math.floor(Number(summary?.availableCount) || 0));
  if (count === 0) return null;
  const credit = (summary?.credits ?? []).find((item) => !item.status || item.status === 'available');
  return { count, credit };
}

export function rateLimitResetExpiry(iso: string | undefined, now: number = Date.now()): string {
  if (!iso) return '';
  const relative = relativePlanReset(iso, now);
  if (!relative) return '';
  return relative === 'reiniciado' ? 'Venció' : `Vence ${relative}`;
}

export function prepareRateLimitResetAttempt(
  previous: RateLimitResetAttempt | null,
  reset: AvailableRateLimitReset,
  createKey: () => string,
): RateLimitResetAttempt {
  const creditKey = reset.credit?.id || `available:${reset.count}`;
  if (previous?.creditKey === creditKey) return previous;
  return { creditKey, idempotencyKey: createKey() };
}
