import assert from 'node:assert/strict';
import test from 'node:test';
import {
  accountResetConsentFrom, accountResetRows, accountWindowRows, extraUsageLine, formatMoney, formatResetShort,
  hasUsableReset, manageHost, payPerUseRows, resetResultToast, resetsLine, retryDelayMs, staleAgo, subscriptionProviders,
  weeklyEntry, weeklyPace, weeklyPaceText, withAccountResetConsent,
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

test('what the page shows dates itself once it is no longer current', () => {
  const now = at(9, 14, 0);
  assert.equal(staleAgo(iso(at(9, 13, 58)), now), '', 'two minutes old is current');
  assert.equal(staleAgo(iso(at(9, 13, 48)), now), 'hace 12 min');
  assert.equal(staleAgo(iso(at(9, 11, 0)), now), 'hace 3 h');
  assert.equal(staleAgo(iso(at(7, 14, 0)), now), 'hace 2 d');
  assert.equal(staleAgo(undefined, now), '');
});

test('the resets line: cached, waiting on a pause, and needing the owner', () => {
  const now = at(9, 14, 0);
  const cached = iso(at(9, 13, 48));
  const pause = { reason: 'rate-limited', retryAt: iso(at(9, 14, 5)) };
  // Fresh: nothing to say.
  assert.equal(resetsLine({}, iso(at(9, 13, 59)), 'Claude Code', now), null);
  // Cached, whether or not the provider asked for a pause: only the age, no error.
  assert.deepEqual(resetsLine(undefined, cached, 'Claude Code', now), { text: 'Actualizado hace 12 min', error: false, retry: false });
  assert.deepEqual(resetsLine(pause, cached, 'Claude Code', now), { text: 'Actualizado hace 12 min', error: false, retry: false });
  assert.equal(resetsLine(pause, iso(at(9, 13, 59)), 'Claude Code', now), null, 'a pause over a current cache shows nothing');
  // Nothing cached yet: one quiet line that says when.
  assert.deepEqual(resetsLine(pause, undefined, 'Claude Code', now),
    { text: 'Claude Code pidió una pausa. Se vuelve a consultar a las 14:05.', error: false, retry: false });
  assert.deepEqual(resetsLine({ reason: 'rate-limited', retryAt: iso(at(10, 9, 0)) }, undefined, 'X', now).text,
    'X pidió una pausa. Se vuelve a consultar mañana a las 09:00.');
  assert.equal(resetsLine({ reason: 'unavailable', retryAt: iso(at(9, 14, 1)) }, undefined, 'X', now)?.text,
    'No se pudo consultar. Se vuelve a consultar a las 14:01.');
  // Something the owner must fix: with a cache it rides the quiet line with a
  // retry; without one it is the error block.
  const expired = { reason: 'expired', message: 'Abrí Claude Code para renovar la sesión.', retryAt: iso(at(9, 14, 10)) };
  assert.deepEqual(resetsLine(expired, cached, 'X', now),
    { text: 'Actualizado hace 12 min · Abrí Claude Code para renovar la sesión.', error: false, retry: true });
  assert.deepEqual(resetsLine(expired, undefined, 'X', now), { text: 'Abrí Claude Code para renovar la sesión.', error: true, retry: true });
});

test('the page asks again when a pause ends, never sooner', () => {
  const now = at(9, 14, 0);
  assert.equal(retryDelayMs({ reason: 'rate-limited', retryAt: iso(at(9, 14, 5)) }, now), 5 * 60_000 + 1500);
  assert.equal(retryDelayMs({ reason: 'rate-limited', retryAt: iso(at(9, 13, 0)) }, now), 1500);
  assert.equal(retryDelayMs({ reason: 'rate-limited', retryAt: iso(at(9, 16, 0)) }, now), null, 'over an hour away');
  assert.equal(retryDelayMs({ reason: 'expired', retryAt: iso(at(9, 14, 5)) }, now), null, 'the owner acts, not a timer');
  assert.equal(retryDelayMs(null, now), null);
});

// 2026-11-09 (owner): a ChatGPT Free login read "Te quedaste sin uso semanal
// hasta el reset" under a "Semana" bar, and "No hay resets ahora". Its limit
// is a 30-day window (Codex windowDurationMins 43200) and it has no resets.
test('a 30-day window is named and told as 30 days, not a week', () => {
  const now = at(9, 12);
  const month: PlanUsageEntry = { kind: 'rate-limit', id: '30_day', usedPercent: 100, status: 'rejected', resetsAt: iso(at(17, 21, 22)), windowMinutes: 43200 };
  const rows = accountWindowRows([month], now);
  assert.equal(rows[0].label, '30 días');
  assert.match(rows[0].sub, /^Llegaste al límite · se reinicia /);
  const pace = weeklyPace(weeklyEntry([month]), now);
  assert.equal(pace?.state, 'out');
  const text = weeklyPaceText(pace!, now);
  assert.match(text.line, /^Te quedaste sin uso hasta el reset .* a las 21:22\.$/);
  assert.doesNotMatch(text.line, /semanal/);
  assert.match(text.detail, /% de los 30 días\.$/);
  // Without windowMinutes, the id alone says how long it is.
  assert.equal(accountWindowRows([{ kind: 'rate-limit', id: 'codex:30_day', limitName: 'codex', usedPercent: 10, resetsAt: iso(at(17, 21, 22)) }], now)[0].label, 'codex · 30 días');
});

test('a free login shows no resets block', async () => {
  const { readFileSync } = await import('node:fs');
  const page = readFileSync(new URL('../src/components/AccountsPage.tsx', import.meta.url), 'utf8');
  assert.match(page, /const freeLogin = snapshot\?\.account\?\.billing === 'free';/);
  assert.match(page, /\} else if \(freeLogin\) \{[\s\S]{0,200}resetsBody = null;/);
  assert.match(page, /\} else if \(snapshot && !needsConsent && !freeLogin\) \{/);
});

// Owner 2026-11-10: a reset that clears only Claude's Fable bucket says so; one
// that clears more is not "only Fable" and keeps its own title.
test('a reset that frees only Fable is named a Fable reset, and only then', () => {
  const fable = { id: 'launch-2610-fable', status: 'available', resetType: 'full', remaining: 1, total: 1, usableNow: true, refills: ['seven_day_overage_included'], expiresAt: iso(at(14, 22, 53)) };
  const [row] = accountResetRows({ availableCount: 1, credits: [fable] }, at(9, 12));
  assert.equal(row.title, 'Reset de Fable');
  assert.match(row.detail, /^Solo libera lo que usaste con Fable esta semana · vence/);
  assert.equal(row.question, '¿Liberar ahora lo que usaste con Fable esta semana?');
  const toast = resetResultToast({ outcome: 'reset', refilled: ['seven_day_overage_included'], remaining: 0 }, row, 'Claude');
  assert.equal(toast.title, 'Reset de Fable usado');

  const [wider] = accountResetRows({ availableCount: 1, credits: [{ ...fable, id: 'wider', refills: ['seven_day_overage_included', 'seven_day'] }] }, at(9, 12));
  assert.equal(wider.title, 'Reset del límite semanal', 'a reset that also refills the week is not only Fable');
  const [weekly] = accountResetRows({ availableCount: 1, credits: [{ ...fable, id: 'weekly', refills: ['seven_day'] }] }, at(9, 12));
  assert.equal(weekly.title, 'Reset del límite semanal');
});

// Owner 2026-11-10: a free login is not waiting for a reset; its usage comes
// back when the window starts over, and a far date is named by its date.
test('a free login says its usage comes back, not that it resets', () => {
  const now = at(10, 12);
  const month = 43200;
  const entry = { kind: 'rate-limit', id: '30_day', status: 'rejected', usedPercent: 100, windowMinutes: month, limitName: 'codex', resetsAt: iso(at(17, 21, 22)) } as PlanUsageEntry;
  const pace = weeklyPace(entry, now)!;
  assert.equal(weeklyPaceText(pace, now, true).line, 'Usaste todo tu uso gratis. Vuelve el 17 nov a las 21:22, cuando empiecen otros 30 días.');
  assert.match(weeklyPaceText(pace, now, false).line, /reset/, 'a paid plan keeps its reset wording');
  const [row] = accountWindowRows([entry], now, true);
  assert.equal(row.sub, 'Llegaste al límite · vuelve 17 nov 21:22');
  const far = { ...entry, resetsAt: iso(at(28, 9, 0)) } as PlanUsageEntry;
  assert.match(weeklyPaceText(weeklyPace(far, now)!, now, true).line, /Vuelve el 28 nov a las 09:00/);
});
