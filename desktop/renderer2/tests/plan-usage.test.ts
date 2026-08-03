import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import {
  availableRateLimitReset,
  clampPlanUsagePercent,
  formatCountdown,
  formatPlanUsagePercent,
  isExpiredPlanReset,
  isHotRateLimit,
  isLiveReset,
  prepareRateLimitResetAttempt,
  rateLimitLabel,
  rateLimitResetExpiry,
  rateLimitValue,
  relativePlanReset,
} from '../src/plan-usage.ts';

test('earned Codex resets stay separate from ordinary reset clocks and honor the authoritative count', () => {
  const detailed = availableRateLimitReset({
    availableCount: 2,
    credits: [
      { id: 'redeemed', status: 'redeemed' },
      { id: 'credit-1', status: 'available', title: 'Full reset' },
    ],
  });
  assert.deepEqual(detailed, {
    count: 2,
    credit: { id: 'credit-1', status: 'available', title: 'Full reset' },
  });
  assert.deepEqual(availableRateLimitReset({ availableCount: 3, credits: null }), { count: 3, credit: undefined });
  assert.equal(availableRateLimitReset({ availableCount: 0, credits: [] }), null);
  assert.equal(rateLimitResetExpiry('2026-07-15T20:00:00Z', Date.parse('2026-07-13T20:00:00Z')), 'Vence en 2 d');
});

test('retrying one earned reset reuses its idempotency key while another credit gets a new one', () => {
  let sequence = 0;
  const makeKey = () => `attempt-${++sequence}`;
  const reset = { count: 2, credit: { id: 'credit-1', status: 'available' } };
  const first = prepareRateLimitResetAttempt(null, reset, makeKey);
  const retry = prepareRateLimitResetAttempt(first, reset, makeKey);
  const next = prepareRateLimitResetAttempt(retry, { count: 1, credit: { id: 'credit-2', status: 'available' } }, makeKey);
  assert.equal(first.idempotencyKey, 'attempt-1');
  assert.equal(retry, first);
  assert.equal(next.idempotencyKey, 'attempt-2');
});

test('earned reset UI belongs to the account menu, not the composer context controls', () => {
  const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
  const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
  const styles = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');
  assert.match(sidebar, /className="acctcredit"/);
  assert.match(sidebar, /store\.useRateLimitReset\('codex'/);
  assert.match(sidebar, /className="acctcredit-mark"[^>]*><ModelIcon provider="gpt" \/>/);
  assert.match(sidebar, /className="acctcredit-title">Codex reset<\/span>/);
  assert.doesNotMatch(sidebar, /className="acctcredit-title">\{reset\.credit\?\.title/);
  assert.doesNotMatch(composer, /plancredit|availableRateLimitReset|useRateLimitReset|inlinePlanResets|planreset-inline/);
  assert.doesNotMatch(styles, /planreset-inline|planreset-item|planreset-name|planreset-time/);
  assert.match(styles, /\.acctcredit-use\s*\{[^}]*border:\s*1px solid var\(--acc\)[^}]*background:\s*var\(--acc\)[^}]*cursor:\s*pointer/);
  assert.match(styles, /\.acctcredit-use:disabled\s*\{[^}]*background:\s*transparent[^}]*color:\s*var\(--muted\)[^}]*cursor:\s*default/);
  assert.match(composer, /<ContextRing usage=/);
  assert.match(composer, /<PlanSection providerLabel=/);
});

test('a reset is live only when it lands within the next 24 hours', () => {
  const now = Date.parse('2026-07-13T16:20:00Z');
  assert.equal(isLiveReset('2026-07-13T19:20:00Z', now), true);   // 3h
  assert.equal(isLiveReset('2026-07-14T16:19:00Z', now), true);   // just under 24h
  assert.equal(isLiveReset('2026-07-14T16:20:00Z', now), false);  // exactly 24h
  assert.equal(isLiveReset('2026-07-15T16:20:00Z', now), false);  // 2d
  assert.equal(isLiveReset('2026-07-13T16:19:00Z', now), false);  // already passed
  assert.equal(isLiveReset(undefined, now), false);
});

test('an expired provider window is stale account data, not another reset label', () => {
  const now = Date.parse('2026-07-15T19:20:00Z');
  assert.equal(isExpiredPlanReset('2026-07-15T18:00:00Z', now), true);
  assert.equal(isExpiredPlanReset('2026-07-15T20:00:00Z', now), false);
  assert.equal(isExpiredPlanReset(undefined, now), false);
  assert.equal(rateLimitValue({ kind: 'rate-limit', usedPercent: 100, resetsAt: '2026-07-15T18:00:00Z' }, now), 'Actualizando límites…');
});

test('the live countdown clock drops the hour segment under an hour', () => {
  assert.equal(formatCountdown(2 * 3600000 + 59 * 60000 + 43000), '2:59:43');
  assert.equal(formatCountdown(9 * 60000 + 3000), '9:03');
  assert.equal(formatCountdown(43000), '0:43');
  assert.equal(formatCountdown(0), 'reiniciado');
  assert.equal(formatCountdown(-5000), 'reiniciado');
});

test('plan usage labels the native five-hour and weekly windows', () => {
  assert.equal(rateLimitLabel('five_hour'), 'Límite de 5 horas');
  assert.equal(rateLimitLabel('seven_day'), 'Límite semanal');
  assert.equal(rateLimitLabel('seven_day_opus'), 'Semanal · Opus');
  assert.equal(rateLimitLabel('seven_day_model:fable', 'Fable'), 'Semanal · Fable');
  assert.equal(rateLimitLabel('team:five_hour', 'Team pool'), 'Team pool · Límite de 5 horas');
});

test('plan usage clamps malformed provider percentages without inventing data', () => {
  assert.equal(clampPlanUsagePercent(undefined), null);
  assert.equal(clampPlanUsagePercent(Number.NaN), null);
  assert.equal(clampPlanUsagePercent(-5), 0);
  assert.equal(clampPlanUsagePercent(120), 100);
  assert.equal(formatPlanUsagePercent(37.5), '37.5');
  assert.equal(formatPlanUsagePercent(78), '78');
});

test('plan usage formats percentage and exact reset relative to a fixed clock', () => {
  const now = Date.parse('2026-07-13T16:20:00Z');
  assert.equal(relativePlanReset('2026-07-13T20:00:00Z', now), 'en 3 h 40 min');
  assert.equal(rateLimitValue({ kind: 'rate-limit', usedPercent: 37.5, resetsAt: '2026-07-13T20:00:00Z' }, now), '37.5% usado · reinicia en 3 h 40 min');
  assert.equal(rateLimitValue({ kind: 'rate-limit', resetsAt: '2026-07-13T20:00:00Z' }, now), 'Se reinicia en 3 h 40 min');
});

test('plan usage highlights provider rejection and utilization at eighty percent', () => {
  assert.equal(isHotRateLimit({ kind: 'rate-limit', usedPercent: 79.9 }), false);
  assert.equal(isHotRateLimit({ kind: 'rate-limit', usedPercent: 80 }), true);
  assert.equal(isHotRateLimit({ kind: 'rate-limit', status: 'rejected', usedPercent: 1 }), true);
});

test('provider plan refresh is independent from chat session creation', () => {
  const store = readFileSync(new URL('../src/store/store.ts', import.meta.url), 'utf8');
  const types = readFileSync(new URL('../src/wire/types.ts', import.meta.url), 'utf8');
  const preload = readFileSync(new URL('../../preload.js', import.meta.url), 'utf8');
  const lan = readFileSync(new URL('../../lan-server.js', import.meta.url), 'utf8');
  assert.match(store, /call\('appChatRefreshPlanUsage',\s*providerId\)/,
    'opening the plan popover must query the selected provider, not rebind the chat session');
  assert.match(types, /appChatRefreshPlanUsage\?:/);
  assert.match(preload, /appChatRefreshPlanUsage:\s*\(providerId\)\s*=>\s*ipcRenderer\.invoke\('app-chat:refresh-plan-usage'/);
  assert.match(lan, /appChatRefreshPlanUsage:\s*\(providerId\)\s*=>\s*invoke\('app-chat:refresh-plan-usage'/);
});
