import assert from 'node:assert/strict';
import test from 'node:test';
import {
  accountResetConsentFrom, accountResetRows, accountWindowRows, extraUsageLine, formatMoney, formatResetShort,
  hasUsableReset, manageHost, payPerUseRows, resetResultToast, subscriptionProviders, weeklyEntry, weeklyPace,
  weeklyPaceText, withAccountResetConsent,
} from '../src/accounts.ts';
import type { PlanUsageEntry, PlanUsageSnapshot, ProviderRecord, RateLimitResetCreditsSummary } from '../src/wire/types.ts';

// Local wall-clock dates, so every assertion holds in any time zone.
// 2026-11-09 is a Monday; the weekly reset is Wednesday 11 at 15:00.
const at = (day: number, hour: number, minute = 0) => new Date(2026, 10, day, hour, minute).getTime();
const iso = (ms: number) => new Date(ms).toISOString();
const RESET = at(11, 15);
const week = (used: number, extra: Partial<PlanUsageEntry> = {}): PlanUsageEntry => ({
  kind: 'rate-limit', id: 'seven_day', usedPercent: used, resetsAt: iso(RESET), ...extra,
});

test('the weekly pace is on track when the week lasts to its reset', () => {
  const now = at(9, 12);
  const pace = weeklyPace(week(57), now);
  assert.equal(pace?.state, 'ok');
  assert.equal(pace?.elapsedPercent, 70);
  const text = weeklyPaceText(pace!, now);
  assert.equal(text.line, 'Vas bien. Llegás al reset del miércoles con margen.');
  assert.equal(text.detail, 'Usaste 57 % y pasó el 70 % de la semana.');
  assert.equal(text.warn, false);
});

test('the weekly pace says when the week runs out before its reset', () => {
  // Window from Wed 4 15:00; by Fri 6 15:00 (48 h) half is used: the rate
  // spends 100 % in 96 h, Sunday 8 at 15:00.
  const friday = at(6, 15);
  const pace = weeklyPace(week(50), friday);
  assert.equal(pace?.state, 'short');
  assert.equal(pace?.runsOutAt, at(8, 15));
  const text = weeklyPaceText(pace!, friday);
  assert.equal(text.line, 'A este ritmo te quedás sin uso semanal el domingo a las 15:00, antes del reset del miércoles.');
  assert.equal(text.detail, 'Usaste 50 % y pasó el 29 % de la semana.');
  assert.equal(text.warn, true);
  // 75 % in 69 h runs out at 92 h, Sunday 11:00: tomorrow, seen from Saturday.
  const saturday = at(7, 12);
  assert.equal(weeklyPaceText(weeklyPace(week(75), saturday)!, saturday).line,
    'A este ritmo te quedás sin uso semanal mañana a las 11:00, antes del reset del miércoles.');
});

test('a used-up week, a too-early week and missing facts', () => {
  const now = at(9, 12);
  const out = weeklyPace(week(100), now);
  assert.equal(out?.state, 'out');
  assert.equal(weeklyPaceText(out!, now).line, 'Te quedaste sin uso semanal hasta el reset del miércoles a las 15:00.');
  // Two hours into the window (1.2 %) no rate is worth telling...
  assert.equal(weeklyPace(week(5), at(4, 17)), null);
  // ...though nothing used is plainly fine.
  assert.equal(weeklyPace(week(0), at(4, 17))?.state, 'ok');
  assert.equal(weeklyPace(week(40, { resetsAt: undefined }), now), null);
  assert.equal(weeklyPace(week(40, { resetsAt: iso(at(9, 11)) }), now), null, 'a passed reset is stale');
  assert.equal(weeklyPace(week(40, { usedPercent: undefined }), now), null);
  // The reset of today and of tomorrow read as such.
  assert.equal(weeklyPaceText(weeklyPace(week(10), at(11, 9))!, at(11, 9)).line, 'Vas bien. Llegás al reset de hoy con margen.');
  assert.equal(weeklyPaceText(weeklyPace(week(10), at(10, 9))!, at(10, 9)).line, 'Vas bien. Llegás al reset de mañana con margen.');
});

test('the pace uses the reported window length, else the one its id names', () => {
  const now = at(9, 12);
  const reported: PlanUsageEntry = { kind: 'rate-limit', id: 'secondary', usedPercent: 57, resetsAt: iso(RESET), windowMinutes: 10080 };
  assert.equal(weeklyEntry([reported])?.id, 'secondary');
  assert.equal(weeklyPace(reported, now)?.state, 'ok');
  const entries: PlanUsageEntry[] = [
    { kind: 'rate-limit', id: 'seven_day_opus', usedPercent: 90, resetsAt: iso(RESET) },
    { kind: 'rate-limit', id: 'five_hour', usedPercent: 10, resetsAt: iso(at(9, 15)) },
    week(57),
  ];
  assert.equal(weeklyEntry(entries)?.id, 'seven_day', 'the all-models week, not a per-model one');
  assert.equal(weeklyEntry([entries[0]]), undefined);
});

test('usage bars: 5 hours, the week, then per-model weeks, with reset times', () => {
  const now = at(9, 21, 10);
  const rows = accountWindowRows([
    { kind: 'rate-limit', id: 'seven_day_opus', usedPercent: 64, resetsAt: iso(RESET) },
    { kind: 'rate-limit', id: 'seven_day_model:fable', limitName: 'Fable', usedPercent: 4, resetsAt: iso(RESET) },
    { kind: 'rate-limit', id: 'five_hour', usedPercent: 100, resetsAt: iso(at(9, 23, 10)) },
    { kind: 'rate-limit', id: 'seven_day', usedPercent: 57, resetsAt: iso(RESET) },
    { kind: 'cost', amount: 1 },
  ], now);
  assert.deepEqual(rows.map((row) => row.label), ['5 horas', 'Semana · todos los modelos', 'Semana · Opus', 'Semana · Fable']);
  assert.equal(rows[0].value, '100 %');
  assert.equal(rows[0].hot, true);
  assert.equal(rows[0].sub, 'Llegaste al límite · se reinicia 23:10');
  assert.equal(rows[1].sub, 'Se reinicia mié 15:00 (en 1 d)');
  assert.equal(rows[1].hot, false);
  const single = accountWindowRows([week(31)], now);
  assert.equal(single[0].label, 'Semana');
  const buckets = accountWindowRows([{ kind: 'rate-limit', id: 'bucket_b:five_hour', limitName: 'Spark', usedPercent: 3 }], now);
  assert.equal(buckets[0].label, 'Spark · 5 horas');
  const stale = accountWindowRows([{ kind: 'rate-limit', id: 'five_hour', usedPercent: 80, resetsAt: iso(at(9, 20)) }], now);
  assert.equal(stale[0].value, '—');
  assert.equal(stale[0].sub, 'Actualizando…');
});

test('reset times read short: today, tomorrow, the weekday, then the date', () => {
  const now = at(9, 12);
  assert.equal(formatResetShort(iso(at(9, 23, 10)), now), '23:10');
  assert.equal(formatResetShort(iso(at(10, 8, 5)), now), 'mañana 08:05');
  assert.equal(formatResetShort(iso(at(11, 15)), now), 'mié 15:00');
  assert.equal(formatResetShort(iso(at(20, 9)), now), '20 nov 09:00');
  assert.equal(formatResetShort(undefined, now), '');
});

const grantSummary: RateLimitResetCreditsSummary = {
  availableCount: 2,
  source: 'account',
  credits: [
    { id: 'five', resetType: 'session', status: 'available', remaining: 1, total: 1, usableNow: false, unavailableReason: 'needs-limit', refills: ['five_hour'] },
    { id: 'full', resetType: 'full', status: 'available', remaining: 1, total: 2, usableNow: true, refills: ['five_hour', 'seven_day'], expiresAt: iso(at(20, 9)) },
    { id: 'spent', resetType: 'weekly', status: 'used', remaining: 0, total: 1, usableNow: false, refills: ['seven_day'] },
  ],
};

test('per-grant resets get a row each, with the reason one cannot be used', () => {
  const rows = accountResetRows(grantSummary, at(9, 12));
  assert.equal(rows.length, 2, 'a spent grant is not listed');
  assert.equal(rows[0].title, 'Reset del límite de 5 horas');
  assert.equal(rows[0].usable, false);
  assert.equal(rows[0].reason, 'Se usa cuando llegás al límite');
  assert.equal(rows[0].question, '¿Rellenar tu límite de 5 horas ahora?');
  assert.equal(rows[1].title, 'Reset completo ×1');
  assert.equal(rows[1].detail, 'Rellena 5 horas y semana · vence 20 nov 09:00');
  assert.equal(rows[1].usable, true);
  assert.equal(rows[1].creditId, 'full');
  assert.equal(rows[1].question, '¿Rellenar tus límites ahora?');
});

test('a count-only summary gets one row for the next credit', () => {
  const rows = accountResetRows({ availableCount: 1, credits: [{ id: 'credit-a', status: 'available', description: 'Reinicia tus límites', expiresAt: iso(at(14, 22, 53)) }] }, at(9, 12));
  assert.equal(rows.length, 1);
  assert.equal(rows[0].title, 'Reset gratis ×1');
  assert.equal(rows[0].detail, 'Reinicia tus límites · vence 14 nov 22:53');
  assert.equal(rows[0].usable, true);
  assert.equal(rows[0].question, '¿Usar el reset ahora?');
  assert.equal(accountResetRows({ availableCount: 3, credits: null })[0].title, 'Reset gratis ×3');
  assert.deepEqual(accountResetRows({ availableCount: 0, credits: [] }), []);
  assert.deepEqual(accountResetRows(undefined), []);
});

test('the foot dot lights only for a reset usable now', () => {
  const snapshot = (summary: RateLimitResetCreditsSummary): Record<string, PlanUsageSnapshot> => ({ a: { providerId: 'a', capturedAt: '', rateLimitResetCredits: summary } });
  assert.equal(hasUsableReset(snapshot(grantSummary)), true);
  assert.equal(hasUsableReset(snapshot({ ...grantSummary, credits: [grantSummary.credits![0]] })), false, 'waiting for a limit is not usable now');
  assert.equal(hasUsableReset(snapshot({ availableCount: 1, credits: null })), true);
  assert.equal(hasUsableReset({}), false);
});

test('the toast after a claim names what was refilled', () => {
  const [five, full] = accountResetRows(grantSummary, at(9, 12));
  assert.deepEqual(resetResultToast({ outcome: 'reset', refilled: ['five_hour'], remaining: 0 }, five, 'Claude'),
    { title: 'Límite de 5 horas reiniciado', body: 'Ya no te quedan resets de este tipo.' });
  assert.deepEqual(resetResultToast({ outcome: 'reset', remaining: 1 }, full, 'Claude'),
    { title: 'Límites de Claude rellenados', body: 'Te queda 1 reset de este tipo.' });
  assert.equal(resetResultToast({ outcome: 'reset' }, full, 'Codex').body, 'Codex actualizó tus límites.');
  assert.equal(resetResultToast({ outcome: 'nothingToReset', message: 'Queda guardado.' }, full, 'X').body, 'Queda guardado.');
  assert.equal(resetResultToast({ outcome: 'noCredit' }, full, 'X').title, 'Reset no disponible');
  assert.equal(resetResultToast({ outcome: 'alreadyRedeemed' }, full, 'X').title, 'Reset ya usado');
  assert.throws(() => resetResultToast({ outcome: 'mystery' }, full, 'X'));
  assert.throws(() => resetResultToast(undefined, full, 'X'));
});

test('spend reads in pesos-style decimals with the monthly cap', () => {
  assert.equal(formatMoney(12.4, 'USD'), '$12,40');
  assert.equal(formatMoney(20, 'usd', true), '$20');
  assert.equal(formatMoney(1234.5, 'EUR'), '1.234,50 EUR');
  assert.deepEqual(extraUsageLine({ enabled: true, monthlyLimit: 20, used: 0, currency: 'USD' }), { label: 'Límite mensual de gasto', value: '$0,00 de $20', tail: '' });
  assert.deepEqual(extraUsageLine({ enabled: true, used: 12.4 }), { label: 'Uso extra este mes', value: '$12,40', tail: 'sin límite mensual' });
  assert.deepEqual(extraUsageLine({ enabled: false }), { label: 'Uso extra', value: 'apagado', tail: '' });
  assert.equal(extraUsageLine(undefined), null);
  assert.equal(manageHost('https://claude.ai/settings/usage'), 'claude.ai');
  assert.equal(manageHost('javascript:alert(1)'), '');
  assert.equal(manageHost(undefined), '');
});

test('accounts that pay per use and local servers go to their own section', () => {
  const providers: ProviderRecord[] = [
    { id: 'sub', name: 'Sub', enabled: true, status: 'ready', accountUsage: true },
    { id: 'keyed', name: 'Keyed', enabled: true, status: 'ready', accountUsage: true },
    { id: 'cold', name: 'Cold', enabled: true, status: 'ready', accountUsage: true },
    { id: 'off', name: 'Off', enabled: true, status: 'ready', accountUsage: true, disabledByUser: true },
    { id: 'plain', name: 'Plain', enabled: true, status: 'ready' },
    { id: 'lms', name: 'LM Studio (local)', enabled: true, status: 'error', localEndpoint: '127.0.0.1:1234' },
    { id: 'oll', name: 'Ollama (local)', enabled: false, status: 'ready', localEndpoint: '127.0.0.1:11434' },
    { id: 'never', name: 'Never (local)', enabled: false, status: 'inactive', localEndpoint: '127.0.0.1:8000' },
  ];
  const snapshots: Record<string, PlanUsageSnapshot> = {
    sub: { providerId: 'sub', capturedAt: '', account: { billing: 'subscription', plan: 'Max' } },
    keyed: { providerId: 'keyed', capturedAt: '', account: { billing: 'api-key', keySource: 'ANTHROPIC_API_KEY' } },
  };
  assert.deepEqual(subscriptionProviders(providers, snapshots).map((p) => p.id), ['sub', 'cold']);
  const rows = payPerUseRows(providers, snapshots);
  assert.deepEqual(rows.map((row) => [row.key, row.detail, row.status, row.dot]), [
    ['keyed', 'clave de API · ANTHROPIC_API_KEY', 'pago por uso', ''],
    ['lms', 'local · 127.0.0.1:1234', 'no responde', 'off'],
    ['oll', 'local · 127.0.0.1:11434', 'responde', 'ok'],
  ]);
});

test('consent round-trips through the settings blob, touching only its key', () => {
  const settings = { modelFavorites: [{ providerId: 'x', modelId: 'y' }], accountResetConsent: ['a', 7, ' B '] };
  assert.deepEqual(accountResetConsentFrom(settings), ['a', 'b']);
  const granted = withAccountResetConsent(settings, 'c', true);
  assert.deepEqual(granted.accountResetConsent, ['a', 'b', 'c']);
  assert.deepEqual(granted.modelFavorites, settings.modelFavorites);
  assert.deepEqual(withAccountResetConsent(granted, 'a', false).accountResetConsent, ['b', 'c']);
  assert.deepEqual(accountResetConsentFrom(null), []);
});
