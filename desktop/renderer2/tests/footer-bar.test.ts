import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { updatesNeedAttention } from '../src/app-updater.ts';

// 2026-11-08: the account menu at the bottom of the left panel (a fixed name,
// Ajustes and a rare free reset) became two icons, Ajustes and
// Actualizaciones; resets get their own Cuentas page.
const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
const sidebarV2 = readFileSync(new URL('../src/components/SidebarV2.tsx', import.meta.url), 'utf8');
const composer = readFileSync(new URL('../src/components/Composer.tsx', import.meta.url), 'utf8');
const accounts = readFileSync(new URL('../src/components/AccountsPage.tsx', import.meta.url), 'utf8');

test('the foot is three icons, Ajustes, Cuentas and Actualizaciones, and no account menu', () => {
  const bar = sidebar.slice(sidebar.indexOf('export function FooterBar()'));
  assert.equal(bar.match(/className=\{?[`"]footbtn/g)?.length, 2);
  assert.match(bar, /aria-label="Ajustes" onClick=\{\(\) => store\.openSettings\(\)\}/);
  assert.match(bar, /<IcDownload \/>/);
  // 2026-11-09: Cuentas sits between them (AccountsPage.tsx).
  const order = ['<IcGear />', '<AccountsFootButton />', '<IcDownload />'].map((token) => bar.indexOf(token));
  assert.ok(order.every((index, i) => index > 0 && (i === 0 || index > order[i - 1])), `foot order ${order}`);
  const button = accounts.slice(accounts.indexOf('export function AccountsFootButton()'));
  assert.match(button, /className=\{`footbtn\$\{open \? ' on' : ''\}`\}/);
  assert.match(button, /<IcAccount \/>/);
  assert.match(button, /\{ready && <span className="footdot" aria-hidden="true" \/>\}/);
  assert.doesNotMatch(sidebar, /AccountMenu|acctpop|Martin/);
  assert.match(sidebarV2, /<div className="foot">\s*<FooterBar \/>\s*<\/div>/);
});

test('the update icon has a dot only when there is something to install, running or to retry', () => {
  const idle = { supported: true, phase: 'current' as const };
  assert.equal(updatesNeedAttention(idle, [], false, null), false);
  assert.equal(updatesNeedAttention({ supported: true, phase: 'checking' }, [], false, null), false, 'a background check is not news');
  assert.equal(updatesNeedAttention({ supported: true, phase: 'available' }, [], false, null), true);
  assert.equal(updatesNeedAttention({ supported: true, phase: 'failed' }, [], false, null), true);
  assert.equal(updatesNeedAttention({ supported: false, phase: 'available' }, [], false, null), false);
  assert.equal(updatesNeedAttention(idle, [{ updateAvailable: true }], false, null), true);
  assert.equal(updatesNeedAttention(idle, [{ updateAvailable: false }], true, null), true, 'an agent CLI updating');
  assert.equal(updatesNeedAttention(idle, [], false, { failedId: 'claude' }), true, 'a failed update waits for a retry');
});

test('the chat box usage popover carries no reset (owner 2026-11-08: resets get a Cuentas page)', () => {
  assert.doesNotMatch(composer, /ResetCreditRows|ctxdot|useAvailableResets/);
});
