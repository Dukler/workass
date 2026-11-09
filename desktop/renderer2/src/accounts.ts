// The Cuentas page's data, as pure functions (components/AccountsPage.tsx
// draws them). Everything here reads daemon-authored, provider-neutral data:
// plan-usage snapshots (entries, account, extraUsage, rateLimitResetCredits)
// and provider records (accountUsage, accountResetConsent, localEndpoint).
// Nothing branches on a provider id.

import type { PlanUsageEntry, PlanUsageExtraUsage, PlanUsageSnapshot, ProviderRecord, RateLimitResetCredit, RateLimitResetCreditsSummary } from './wire/types';
import { availableRateLimitReset, clampPlanUsagePercent, formatPlanUsagePercent, isExpiredPlanReset, isHotRateLimit, rateLimitLabel, relativePlanReset } from './plan-usage.ts';

const MINUTE_MS = 60_000;
const WEEK_MINUTES = 7 * 24 * 60;
const WEEKDAYS = ['domingo', 'lunes', 'martes', 'miércoles', 'jueves', 'viernes', 'sábado'];
const WEEKDAYS_SHORT = ['dom', 'lun', 'mar', 'mié', 'jue', 'vie', 'sáb'];
const MONTHS = ['ene', 'feb', 'mar', 'abr', 'may', 'jun', 'jul', 'ago', 'sep', 'oct', 'nov', 'dic'];

// ---- dates -------------------------------------------------------------

const pad = (n: number) => String(n).padStart(2, '0');
const clock = (d: Date) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;

/** Whole local days from `now` to `t` (0 = today, 1 = tomorrow). */
function dayOffset(t: number, now: number): number {
  const a = new Date(now); const b = new Date(t);
  const start = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  return Math.round((start(b) - start(a)) / 86_400_000);
}

/** "23:10", "mañana 13:04", "mié 15:00" within the week, else "20 nov 09:00". */
export function formatResetShort(iso: string | undefined, now: number = Date.now()): string {
  const t = Date.parse(iso ?? '');
  if (!Number.isFinite(t)) return '';
  const d = new Date(t);
  const days = dayOffset(t, now);
  if (days === 0) return clock(d);
  if (days === 1) return `mañana ${clock(d)}`;
  if (days > 1 && days < 7) return `${WEEKDAYS_SHORT[d.getDay()]} ${clock(d)}`;
  return `${d.getDate()} ${MONTHS[d.getMonth()]} ${clock(d)}`;
}

/** "de hoy", "de mañana", "del miércoles": the reset a sentence points at. */
function resetDayPhrase(t: number, now: number): string {
  const days = dayOffset(t, now);
  if (days === 0) return 'de hoy';
  if (days === 1) return 'de mañana';
  return `del ${WEEKDAYS[new Date(t).getDay()]}`;
}

/** "hoy a las 18:00", "mañana a las 18:00", "el lunes a las 18:00". */
function momentPhrase(t: number, now: number): string {
  const days = dayOffset(t, now);
  const at = `a las ${clock(new Date(t))}`;
  if (days === 0) return `hoy ${at}`;
  if (days === 1) return `mañana ${at}`;
  return `el ${WEEKDAYS[new Date(t).getDay()]} ${at}`;
}

// ---- usage windows -----------------------------------------------------

function windowBase(id: string | undefined): string {
  if (!id) return '';
  if (id.startsWith('seven_day_model:')) return 'seven_day_model';
  return id.includes(':') ? id.slice(id.lastIndexOf(':') + 1) : id;
}

/** The window's length: as the provider reported it, else from its id. */
export function windowMinutesOf(entry: PlanUsageEntry): number | null {
  if (typeof entry.windowMinutes === 'number' && entry.windowMinutes > 0) return entry.windowMinutes;
  const base = windowBase(entry.id);
  if (base === 'five_hour') return 300;
  if (base.startsWith('seven_day')) return WEEK_MINUTES;
  return null;
}

function windowRank(entry: PlanUsageEntry): number {
  const base = windowBase(entry.id);
  if (base === 'five_hour') return 0;
  if (base === 'seven_day') return 1;
  if (base === 'seven_day_model') return 3;
  if (base.startsWith('seven_day')) return 2;
  return 4;
}

const WINDOW_LABEL: Record<string, string> = {
  five_hour: '5 horas',
  seven_day_opus: 'Semana · Opus',
  seven_day_sonnet: 'Semana · Sonnet',
  seven_day_oauth_apps: 'Semana · Apps OAuth',
};

export function accountWindowLabel(entry: PlanUsageEntry, hasPerModelWeeks: boolean): string {
  const base = windowBase(entry.id);
  let label: string;
  if (base === 'seven_day') label = hasPerModelWeeks ? 'Semana · todos los modelos' : 'Semana';
  else if (base === 'seven_day_model') label = `Semana · ${entry.limitName || 'modelo'}`;
  else label = WINDOW_LABEL[base] ?? rateLimitLabel(entry.id, entry.limitName);
  // A provider with several metered buckets prefixes each with its name.
  if (entry.id?.includes(':') && base !== 'seven_day_model' && entry.limitName) label = `${entry.limitName} · ${label}`;
  return label;
}

export interface AccountWindowRow {
  key: string;
  label: string;
  percent: number | null;
  value: string;
  hot: boolean;
  sub: string;
}

/** The usage bars, 5 hours first, then the week, then per-model weeks. */
export function accountWindowRows(entries: ReadonlyArray<PlanUsageEntry> | undefined, now: number = Date.now()): AccountWindowRow[] {
  const limits = (entries ?? []).filter((entry) => entry.kind === 'rate-limit');
  const hasPerModel = limits.some((entry) => windowRank(entry) >= 2 && windowRank(entry) <= 3);
  return limits
    .map((entry, index) => ({ entry, index }))
    .sort((a, b) => windowRank(a.entry) - windowRank(b.entry) || a.index - b.index)
    .map(({ entry, index }) => {
      const expired = isExpiredPlanReset(entry.resetsAt, now);
      const percent = expired ? null : clampPlanUsagePercent(entry.usedPercent);
      const atLimit = !expired && ((percent ?? 0) >= 100 || (!!entry.status && entry.status !== 'allowed'));
      const short = expired ? '' : formatResetShort(entry.resetsAt, now);
      const relative = expired ? '' : relativePlanReset(entry.resetsAt, now);
      let sub = '';
      if (expired) sub = 'Actualizando…';
      else if (atLimit) sub = short ? `Llegaste al límite · se reinicia ${short}` : 'Llegaste al límite';
      else if (short) sub = relative ? `Se reinicia ${short} (${relative})` : `Se reinicia ${short}`;
      const pct = expired ? null : formatPlanUsagePercent(entry.usedPercent);
      return {
        key: entry.id || `window-${index}`,
        label: accountWindowLabel(entry, hasPerModel),
        percent,
        value: pct != null ? `${pct} %` : '—',
        hot: !expired && (atLimit || isHotRateLimit(entry)),
        sub,
      };
    });
}

// ---- weekly pace -------------------------------------------------------

export interface WeeklyPace {
  // ok: at this rate the week lasts to its reset; short: it runs out first;
  // out: already used up.
  state: 'ok' | 'short' | 'out';
  usedPercent: number;
  elapsedPercent: number;
  resetsAt: number;
  runsOutAt?: number;
}

/** The weekly window a pace is told for: the all-models week. */
export function weeklyEntry(entries: ReadonlyArray<PlanUsageEntry> | undefined): PlanUsageEntry | undefined {
  const limits = (entries ?? []).filter((entry) => entry.kind === 'rate-limit');
  return limits.find((entry) => windowBase(entry.id) === 'seven_day')
    ?? limits.find((entry) => (windowMinutesOf(entry) ?? 0) >= WEEK_MINUTES && windowRank(entry) === 4);
}

/**
 * Where the week is heading at the rate used so far. The window started
 * `windowMinutes` before its reset; spending `used` % in the elapsed share
 * runs it out at start + elapsed × 100 / used. Before 2 % of the window has
 * passed there is no rate worth telling (null), nor without a percentage,
 * a reset time or a window length.
 */
export function weeklyPace(entry: PlanUsageEntry | undefined, now: number = Date.now()): WeeklyPace | null {
  if (!entry) return null;
  const used = clampPlanUsagePercent(entry.usedPercent);
  const resetsAt = Date.parse(entry.resetsAt ?? '');
  const minutes = windowMinutesOf(entry);
  if (used == null || !Number.isFinite(resetsAt) || resetsAt <= now || !minutes) return null;
  const windowMs = minutes * MINUTE_MS;
  const start = resetsAt - windowMs;
  const elapsed = now - start;
  if (elapsed <= 0) return null;
  const elapsedShare = Math.min(1, elapsed / windowMs);
  const base = { usedPercent: used, elapsedPercent: Math.round(elapsedShare * 100), resetsAt };
  if (used >= 100) return { state: 'out', ...base };
  if (used > 0 && elapsedShare < 0.02) return null;
  if (used <= 0) return { state: 'ok', ...base };
  const runsOutAt = start + elapsed * (100 / used);
  return runsOutAt < resetsAt ? { state: 'short', ...base, runsOutAt } : { state: 'ok', ...base };
}

export interface PaceText { line: string; detail: string; warn: boolean }

export function weeklyPaceText(pace: WeeklyPace, now: number = Date.now()): PaceText {
  const detail = `Usaste ${Math.round(pace.usedPercent)} % y pasó el ${pace.elapsedPercent} % de la semana.`;
  const reset = resetDayPhrase(pace.resetsAt, now);
  if (pace.state === 'out') {
    return { line: `Te quedaste sin uso semanal hasta el reset ${reset} a las ${clock(new Date(pace.resetsAt))}.`, detail, warn: true };
  }
  if (pace.state === 'short' && pace.runsOutAt != null) {
    return { line: `A este ritmo te quedás sin uso semanal ${momentPhrase(pace.runsOutAt, now)}, antes del reset ${reset}.`, detail, warn: true };
  }
  return { line: `Vas bien. Llegás al reset ${reset} con margen.`, detail, warn: false };
}

// ---- resets ------------------------------------------------------------

export interface AccountResetRow {
  key: string;
  creditId?: string;
  title: string;
  detail: string;
  usable: boolean;
  // Why it cannot be used now, when it cannot.
  reason: string;
  question: string;
  refills: string[];
}

const UNAVAILABLE_REASON: Record<string, string> = {
  'needs-limit': 'Se usa cuando llegás al límite',
  cooldown: 'Disponible en un rato',
  'blocked:five_hour': 'Bloqueado por el límite de 5 horas',
  'blocked:seven_day': 'Bloqueado por el límite semanal',
  'blocked:other': 'Bloqueado por otro límite',
  'cannot-clear': 'No rellena el límite que alcanzaste',
  paused: 'En pausa',
};

function refillShape(refills: ReadonlyArray<string>, resetType?: string): 'both' | 'five' | 'week' | '' {
  const five = refills.some((id) => windowBase(id) === 'five_hour');
  const week = refills.some((id) => windowBase(id).startsWith('seven_day'));
  if ((five && week) || (!refills.length && resetType === 'full')) return 'both';
  if (five || (!refills.length && resetType === 'session')) return 'five';
  if (week || (!refills.length && resetType === 'weekly')) return 'week';
  return '';
}

/** An expiry names its date ("vence 14 nov 22:53"), today and tomorrow aside. */
function expiryText(iso: string | undefined, now: number): string {
  const t = Date.parse(iso ?? '');
  if (!Number.isFinite(t)) return '';
  if (t <= now) return 'venció';
  const d = new Date(t);
  const days = dayOffset(t, now);
  if (days === 0) return `vence hoy ${clock(d)}`;
  if (days === 1) return `vence mañana ${clock(d)}`;
  return `vence ${d.getDate()} ${MONTHS[d.getMonth()]} ${clock(d)}`;
}

function grantRow(credit: RateLimitResetCredit, index: number, now: number): AccountResetRow {
  const refills = credit.refills ?? [];
  const shape = refillShape(refills, credit.resetType);
  const title = shape === 'both' ? 'Reset completo'
    : shape === 'five' ? 'Reset del límite de 5 horas'
      : shape === 'week' ? 'Reset del límite semanal'
        : (credit.title || 'Reset');
  const count = (credit.total ?? 0) > 1 && typeof credit.remaining === 'number' ? ` ×${credit.remaining}` : '';
  const usable = credit.usableNow === true && credit.status === 'available';
  return {
    key: credit.id || `grant-${index}`,
    creditId: credit.id,
    title: title + count,
    detail: [shape === 'both' ? 'Rellena 5 horas y semana' : '', expiryText(credit.expiresAt, now)].filter(Boolean).join(' · '),
    usable,
    reason: usable ? '' : (UNAVAILABLE_REASON[credit.unavailableReason ?? ''] ?? 'No disponible ahora'),
    question: shape === 'both' ? '¿Rellenar tus límites ahora?'
      : shape === 'five' ? '¿Rellenar tu límite de 5 horas ahora?'
        : shape === 'week' ? '¿Rellenar tu límite semanal ahora?'
          : '¿Usar el reset ahora?',
    refills,
  };
}

/**
 * The resets to list. Per-grant resets (an account API that says how many
 * uses each grant has left) get a row each; a count-only summary gets one
 * row for the next available credit, its count authoritative.
 */
export function accountResetRows(summary: RateLimitResetCreditsSummary | undefined, now: number = Date.now()): AccountResetRow[] {
  if (!summary) return [];
  const credits = summary.credits ?? [];
  if (credits.some((credit) => typeof credit.remaining === 'number')) {
    return credits
      .filter((credit) => credit.status !== 'used' && (credit.remaining ?? 1) > 0)
      .map((credit, index) => grantRow(credit, index, now));
  }
  const available = availableRateLimitReset(summary);
  if (!available) return [];
  const credit = available.credit;
  return [{
    key: credit?.id || 'available',
    creditId: credit?.id,
    title: `Reset gratis ×${available.count}`,
    detail: [credit?.description || 'Reinicia tus límites', expiryText(credit?.expiresAt, now)].filter(Boolean).join(' · '),
    usable: true,
    reason: '',
    question: '¿Usar el reset ahora?',
    refills: [],
  }];
}

/** The left panel's dot: some provider has a reset usable right now. */
export function hasUsableReset(snapshots: Readonly<Record<string, PlanUsageSnapshot>>, now: number = Date.now()): boolean {
  return Object.values(snapshots).some((snapshot) => accountResetRows(snapshot?.rateLimitResetCredits, now).some((row) => row.usable));
}

export interface ResetResult {
  outcome?: string;
  refilled?: string[];
  remaining?: number;
  message?: string;
}

/** The toast after a claim; throws on an outcome it does not know. */
export function resetResultToast(result: ResetResult | undefined, row: AccountResetRow, name: string): { title: string; body: string } {
  switch (result?.outcome) {
    case 'reset': {
      const shape = refillShape(result.refilled?.length ? result.refilled : row.refills);
      const title = shape === 'five' ? 'Límite de 5 horas reiniciado'
        : shape === 'week' ? 'Límite semanal reiniciado'
          : `Límites de ${name} rellenados`;
      const left = result.remaining;
      const body = typeof left === 'number'
        ? (left === 0 ? 'Ya no te quedan resets de este tipo.' : left === 1 ? 'Te queda 1 reset de este tipo.' : `Te quedan ${left} resets de este tipo.`)
        : `${name} actualizó tus límites.`;
      return { title, body };
    }
    case 'alreadyRedeemed':
      return { title: 'Reset ya usado', body: 'Ese reset ya se había aplicado.' };
    case 'nothingToReset':
      return { title: 'Reset guardado', body: result.message || 'Todavía no hay un límite para rellenar.' };
    case 'noCredit':
      return { title: 'Reset no disponible', body: result.message || 'El proveedor ya no encuentra ese reset.' };
    default:
      throw new Error('El proveedor no devolvió un resultado de reset reconocido.');
  }
}

// ---- spend -------------------------------------------------------------

export function formatMoney(value: number, currency?: string, dropZeroCents = false): string {
  const digits = dropZeroCents && Number.isInteger(value) ? 0 : 2;
  const amount = value.toLocaleString('es-AR', { minimumFractionDigits: digits, maximumFractionDigits: digits });
  const code = (currency || 'USD').toUpperCase();
  return code === 'USD' ? `$${amount}` : `${amount} ${code}`;
}

export interface SpendLine { label: string; value: string; tail: string }

/** "Límite mensual de gasto $12,40 de $20", or the no-cap / off forms. */
export function extraUsageLine(extra: PlanUsageExtraUsage | undefined): SpendLine | null {
  if (!extra) return null;
  if (!extra.enabled) return { label: 'Uso extra', value: 'apagado', tail: '' };
  const used = formatMoney(extra.used ?? 0, extra.currency);
  if (typeof extra.monthlyLimit === 'number') {
    return { label: 'Límite mensual de gasto', value: `${used} de ${formatMoney(extra.monthlyLimit, extra.currency, true)}`, tail: '' };
  }
  return { label: 'Uso extra este mes', value: used, tail: 'sin límite mensual' };
}

export function manageHost(url: string | undefined): string {
  if (!url) return '';
  try {
    const parsed = new URL(url);
    return parsed.protocol === 'https:' ? parsed.hostname : '';
  } catch {
    return '';
  }
}

// ---- which providers go where -------------------------------------------

function billingOf(snapshot: PlanUsageSnapshot | undefined): string {
  return snapshot?.account?.billing ?? '';
}

const paysPerUse = (billing: string) => billing === 'api-key' || billing === 'cloud';

/** Providers with a plan-usage account, unless that account pays per use. */
export function subscriptionProviders(providers: ReadonlyArray<ProviderRecord>, snapshots: Readonly<Record<string, PlanUsageSnapshot>>): ProviderRecord[] {
  return providers.filter((provider) => provider.enabled && !provider.disabledByUser && provider.accountUsage
    && !paysPerUse(billingOf(snapshots[provider.id])));
}

export interface PayPerUseRow {
  key: string;
  name: string;
  brand?: string;
  detail: string;
  // ok / off draw the dot; '' draws none.
  dot: 'ok' | 'off' | '';
  status: string;
}

/** "Claves de API y modelos locales": accounts that pay per use, and the
 *  model servers on this machine that were ever found or turned on. */
export function payPerUseRows(providers: ReadonlyArray<ProviderRecord>, snapshots: Readonly<Record<string, PlanUsageSnapshot>>): PayPerUseRow[] {
  const rows: PayPerUseRow[] = [];
  for (const provider of providers) {
    if (provider.disabledByUser) continue;
    const name = provider.name || provider.id;
    const account = snapshots[provider.id]?.account;
    if (provider.accountUsage && provider.enabled && account && paysPerUse(account.billing ?? '')) {
      const detail = account.billing === 'cloud'
        ? (account.backend || 'Nube')
        : (account.keySource ? `clave de API · ${account.keySource}` : 'clave de API');
      rows.push({ key: provider.id, name, brand: provider.assistantBrand, detail, dot: '', status: 'pago por uso' });
      continue;
    }
    if (provider.localEndpoint && (provider.enabled || provider.detected || provider.status === 'ready')) {
      const ready = provider.status === 'ready';
      const probed = ready || provider.status === 'error' || provider.status === 'not-found';
      rows.push({
        key: provider.id, name, brand: provider.assistantBrand,
        detail: `local · ${provider.localEndpoint}`,
        dot: ready ? 'ok' : 'off',
        status: ready ? 'responde' : probed ? 'no responde' : 'sin revisar',
      });
    }
  }
  return rows;
}

// ---- consent (app settings) ---------------------------------------------

// Providers whose stored login the owner let Workass use for account resets.
// settings:set replaces the whole blob (daemon normalizeSettings keeps this
// key), so writers read, change this key only, and write back.
export const ACCOUNT_RESET_CONSENT_KEY = 'accountResetConsent';

export function accountResetConsentFrom(settings: Record<string, unknown> | null | undefined): string[] {
  const raw = settings?.[ACCOUNT_RESET_CONSENT_KEY];
  if (!Array.isArray(raw)) return [];
  return [...new Set(raw.filter((id): id is string => typeof id === 'string' && !!id.trim()).map((id) => id.trim().toLowerCase()))];
}

export function withAccountResetConsent(settings: Record<string, unknown> | null | undefined, providerId: string, granted: boolean): Record<string, unknown> {
  const current = accountResetConsentFrom(settings).filter((id) => id !== providerId);
  return { ...(settings ?? {}), [ACCOUNT_RESET_CONSENT_KEY]: granted ? [...current, providerId] : current };
}
