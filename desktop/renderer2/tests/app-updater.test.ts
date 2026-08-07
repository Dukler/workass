import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { appUpdaterBlockerText, appUpdaterPhaseText, appUpdaterReceiptIsRecent, type AppUpdaterState } from '../src/app-updater.ts';

const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
const settings = readFileSync(new URL('../src/components/Settings.tsx', import.meta.url), 'utf8');
const settingsTypes = readFileSync(new URL('../src/store/types.ts', import.meta.url), 'utf8');

const state = (phase: AppUpdaterState['phase'], overrides: Partial<AppUpdaterState> = {}): AppUpdaterState => ({
  supported: true,
  phase,
  currentVersion: '1.0.0',
  targetVersion: '1.1.0',
  checkedAt: null,
  progress: 0,
  error: null,
  blockers: null,
  receipt: null,
  ...overrides,
});

test('busy update text names every daemon-owned blocker without exposing task content', () => {
  const text = appUpdaterBlockerText({ foregroundTurns: 2, backgroundWork: 1, providerUpdates: 1, admissions: 1 });
  assert.match(text, /2 turnos activos/);
  assert.match(text, /1 tarea en segundo plano/);
  assert.match(text, /1 agente actualizándose/);
  assert.match(text, /1 operación iniciándose/);
});

test('download progress and rollback are explicit user-facing states', () => {
  assert.match(appUpdaterPhaseText(state('downloading', { progress: 0.42 })), /42%/);
  assert.match(appUpdaterPhaseText(state('rollback_healthy')), /restauró la versión anterior/);
});

test('failed state surfaces the bounded updater error', () => {
  assert.equal(appUpdaterPhaseText(state('failed', { error: 'checksum inválido' })), 'checksum inválido');
});

test('only a fresh terminal receipt can replay the success seal after an app restart', () => {
  const now = Date.parse('2026-08-06T20:00:00.000Z');
  assert.equal(appUpdaterReceiptIsRecent({ updatedAt: '2026-08-06T19:59:30.000Z' }, now), true);
  assert.equal(appUpdaterReceiptIsRecent({ updatedAt: '2026-08-06T19:55:00.000Z' }, now), false);
  assert.equal(appUpdaterReceiptIsRecent({ updatedAt: 'invalid' }, now), false);
});

test('Workass updater uses the existing footer update-card lifecycle and never adds a Settings section', () => {
  const selfCard = sidebar.slice(sidebar.indexOf('{showSelfUpdate && ('), sidebar.indexOf('{showProvider && ('));
  assert.match(selfCard, /className=\{`updslot/);
  assert.match(selfCard, /className="updring"/);
  assert.match(selfCard, /upd-run upd-done/);
  assert.match(selfCard, /updcard upd-fail/);
  assert.match(selfCard, /className="uretry"/);
  assert.doesNotMatch(settings, /WorkassPanel|useAppUpdater|appUpdaterPhaseText/);
  assert.doesNotMatch(settingsTypes, /SettingsSection\s*=\s*[^;]*'workass'/);
});
