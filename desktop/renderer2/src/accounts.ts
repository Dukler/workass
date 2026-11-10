// The Cuentas page's data, as pure functions (components/AccountsPage.tsx
// draws them). Everything here reads daemon-authored, provider-neutral data:
// plan-usage snapshots (entries, account, extraUsage, rateLimitResetCredits)
// and provider records (accountUsage, accountResetConsent, localEndpoint).
// Nothing branches on a provider id.

import type { AccountResetsAnswer, PlanUsageEntry, PlanUsageExtraUsage, PlanUsageSnapshot, ProviderRecord, RateLimitResetCredit, RateLimitResetCreditsSummary } from './wire/types';
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
  // A weekday alone names the right day only within a week.
  if (days > 6) return `del ${new Date(t).getDate()} ${MONTHS[new Date(t).getMonth()]}`;
  return `del ${WEEKDAYS[new Date(t).getDay()]}`;
}

/** "hoy a las 18:00", "mañana a las 18:00", "el lunes a las 18:00". */
function momentPhrase(t: number, now: number): string {
  const days = dayOffset(t, now);
  const at = `a las ${clock(new Date(t))}`;
  if (days === 0) return `hoy ${at}`;
  if (days === 1) return `mañana ${at}`;
  if (days > 6) return `el ${new Date(t).getDate()} ${MONTHS[new Date(t).getMonth()]} ${at}`;
  return `el ${WEEKDAYS[new Date(t).getDay()]} ${at}`;
}

// ---- usage windows -----------------------------------------------------

function windowBase(id: string | undefined): string {
  if (!id) return '';
  if (id.startsWith('seven_day_model:')) return 'seven_day_model';
  return id.includes(':') ? id.slice(id.lastIndexOf(':') + 1) : id;
}

/** The window's length: as the provider reported it, else from its id. */
// A window named by its length in days ("30_day", internal/acp codexWindowID).
function dayWindow(base: string): number | null {
  const match = /^(\d+)_day$/.exec(base);
  return match ? Number(match[1]) : null;
}

export function windowMinutesOf(entry: PlanUsageEntry): number | null {
  if (typeof entry.windowMinutes === 'number' && entry.windowMinutes > 0) return entry.windowMinutes;
  const base = windowBase(entry.id);
  if (base === 'five_hour') return 300;
  if (base.startsWith('seven_day')) return WEEK_MINUTES;
  const days = dayWindow(base);
  return days ? days * 24 * 60 : null;
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
  else if (dayWindow(base)) label = `${dayWindow(base)} días`;
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
// A free login has no reset to wait for: its allowance comes back when the
// window starts over (owner 2026-11-10: "why does it say I'm having a reset
// if I'm not subscribed"). Its bars and pace say "vuelve", not "reset".
export function accountWindowRows(entries: ReadonlyArray<PlanUsageEntry> | undefined, now: number = Date.now(), free = false): AccountWindowRow[] {
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
      else if (atLimit) sub = short ? `Llegaste al límite · ${free ? 'vuelve' : 'se reinicia'} ${short}` : 'Llegaste al límite';
      else if (short) sub = `${free ? 'Se renueva' : 'Se reinicia'} ${short}${relative ? ` (${relative})` : ''}`;
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
  // The window's length: a week for the paid plans, 30 days for ChatGPT Free.
  windowMinutes: number;
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
  const base = { usedPercent: used, elapsedPercent: Math.round(elapsedShare * 100), resetsAt, windowMinutes: minutes };
  if (used >= 100) return { state: 'out', ...base };
  if (used > 0 && elapsedShare < 0.02) return null;
  if (used <= 0) return { state: 'ok', ...base };
  const runsOutAt = start + elapsed * (100 / used);
  return runsOutAt < resetsAt ? { state: 'short', ...base, runsOutAt } : { state: 'ok', ...base };
}

export interface PaceText { line: string; detail: string; warn: boolean }

export function weeklyPaceText(pace: WeeklyPace, now: number = Date.now(), free = false): PaceText {
  // Told in the window's own length: a 30-day window is not a week.
  const week = Math.abs(pace.windowMinutes - WEEK_MINUTES) <= 60;
  const days = Math.round(pace.windowMinutes / (24 * 60));
  const span = week ? 'de la semana' : `de los ${days} días`;
  const use = week ? 'uso semanal' : 'uso';
  const detail = `Usaste ${Math.round(pace.usedPercent)} % y pasó el ${pace.elapsedPercent} % ${span}.`;
  const reset = resetDayPhrase(pace.resetsAt, now);
  if (free) {
    // The window starting over is what gives a free login its usage back.
    const again = week ? 'empiece otra semana' : `empiecen otros ${days} días`;
    if (pace.state === 'out') return { line: `Usaste todo tu uso gratis. Vuelve ${momentPhrase(pace.resetsAt, now)}, cuando ${again}.`, detail, warn: true };
    if (pace.state === 'short' && pace.runsOutAt != null) {
      return { line: `A este ritmo te quedás sin uso gratis ${momentPhrase(pace.runsOutAt, now)}, antes de que ${again}.`, detail, warn: true };
    }
    return { line: `Vas bien. Tu uso gratis te alcanza hasta que ${again}.`, detail, warn: false };
  }
  if (pace.state === 'out') {
    return { line: `Te quedaste sin ${use} hasta el reset ${reset} a las ${clock(new Date(pace.resetsAt))}.`, detail, warn: true };
  }
  if (pace.state === 'short' && pace.runsOutAt != null) {
    return { line: `A este ritmo te quedás sin ${use} ${momentPhrase(pace.runsOutAt, now)}, antes del reset ${reset}.`, detail, warn: true };
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

// Claude's Fable bucket. A reset that clears only it frees what Fable used
// this week and nothing else (Claude Code calls it a "Fable reset"); one that
// also clears another limit is not only a Fable reset (owner 2026-11-10: say
// it only when it is only that).
const FABLE_WINDOW = 'seven_day_overage_included';

function refillShape(refills: ReadonlyArray<string>, resetType?: string): 'both' | 'five' | 'week' | 'fable' | '' {
  if (refills.length > 0 && refills.every((id) => windowBase(id) === FABLE_WINDOW)) return 'fable';
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
  const title = shape === 'fable' ? 'Reset de Fable'
    : shape === 'both' ? 'Reset completo'
    : shape === 'five' ? 'Reset del límite de 5 horas'
      : shape === 'week' ? 'Reset del límite semanal'
        : (credit.title || 'Reset');
  const count = (credit.total ?? 0) > 1 && typeof credit.remaining === 'number' ? ` ×${credit.remaining}` : '';
  const usable = credit.usableNow === true && credit.status === 'available';
  return {
    key: credit.id || `grant-${index}`,
    creditId: credit.id,
    title: title + count,
    detail: [
      shape === 'both' ? 'Rellena 5 horas y semana' : shape === 'fable' ? 'Solo libera lo que usaste con Fable esta semana' : '',
      expiryText(credit.expiresAt, now),
    ].filter(Boolean).join(' · '),
    usable,
    reason: usable ? '' : (UNAVAILABLE_REASON[credit.unavailableReason ?? ''] ?? 'No disponible ahora'),
    question: shape === 'fable' ? '¿Liberar ahora lo que usaste con Fable esta semana?'
      : shape === 'both' ? '¿Rellenar tus límites ahora?'
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
      const title = shape === 'fable' ? 'Reset de Fable usado'
        : shape === 'five' ? 'Límite de 5 horas reiniciado'
        : shape === 'week' ? 'Límite semanal reiniciado'
          : `Límites de ${name} rellenados`;
      const left = result.remaining;
      const body = shape === 'fable' ? 'Lo que usaste con Fable volvió a tu límite semanal.'
        : typeof left === 'number'
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

// ---- what is shown, and how old it is -----------------------------------

// Under this age what the page shows reads as current and carries no date.
export const STALE_AFTER_MS = 3 * 60_000;

/** "hace 12 min", "hace 3 h", "hace 2 d"; '' while still current. */
export function staleAgo(iso: string | undefined, now: number = Date.now()): string {
  const t = Date.parse(iso ?? '');
  if (!Number.isFinite(t) || now - t < STALE_AFTER_MS) return '';
  const min = Math.floor((now - t) / MINUTE_MS);
  if (min < 60) return `hace ${min} min`;
  const h = Math.floor(min / 60);
  if (h < 24) return `hace ${h} h`;
  return `hace ${Math.floor(h / 24)} d`;
}

/** "a las 14:05" today, else "mañana a las 09:00" / "el lunes a las 09:00". */
function retryPhrase(iso: string, now: number): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return '';
  return dayOffset(t, now) === 0 ? `a las ${clock(new Date(t))}` : momentPhrase(t, now);
}

// The daemon retries these on its own; they never need the owner.
const WAITING_REASONS = new Set(['rate-limited', 'unavailable']);

export interface ResetsLine {
  text: string;
  // An orange block (the owner must act and nothing is cached), else a quiet line.
  error: boolean;
  // Offer the owner's retry.
  retry: boolean;
}

/**
 * The one line under an account's resets. With resets cached: their age,
 * plus what the owner must do when that is why they are old; a pause the
 * provider asked for shows nothing more. Without a cache: when the daemon
 * asks again, quietly, or what the owner must do.
 */
export function resetsLine(answer: Partial<AccountResetsAnswer> | null | undefined, cachedReadAt: string | undefined, name: string, now: number = Date.now()): ResetsLine | null {
  const reason = answer?.reason ?? '';
  const actionable = !!reason && !WAITING_REASONS.has(reason);
  if (cachedReadAt) {
    const parts: string[] = [];
    const age = staleAgo(cachedReadAt, now);
    if (age) parts.push(`Actualizado ${age}`);
    if (actionable && answer?.message) parts.push(answer.message);
    return parts.length ? { text: parts.join(' · '), error: false, retry: actionable } : null;
  }
  if (!reason) return null;
  if (!actionable) {
    const at = answer?.retryAt ? retryPhrase(answer.retryAt, now) : '';
    const why = reason === 'rate-limited' ? `${name} pidió una pausa.` : 'No se pudo consultar.';
    return { text: at ? `${why} Se vuelve a consultar ${at}.` : why, error: false, retry: false };
  }
  return { text: answer?.message || 'No se pudieron leer los resets.', error: true, retry: true };
}

/** How long until a waiting read is due again, for the page to ask then;
 *  null when there is nothing to wait for (or it is over an hour away). */
export function retryDelayMs(answer: Partial<AccountResetsAnswer> | null | undefined, now: number = Date.now()): number | null {
  if (!answer?.retryAt || !WAITING_REASONS.has(answer.reason ?? '')) return null;
  const delay = Date.parse(answer.retryAt) - now;
  if (!Number.isFinite(delay) || delay > 60 * MINUTE_MS) return null;
  return Math.max(0, delay) + 1500;
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
