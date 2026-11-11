import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { appUpdaterBlockerText, appUpdaterCardTitle, appUpdaterPhaseText, type AppUpdaterState } from '../src/app-updater.ts';
import { providerUpdateRows, selfUpdateRow } from '../src/update-rows.ts';

const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
const updaterSource = readFileSync(new URL('../src/app-updater.ts', import.meta.url), 'utf8');
const settings = readFileSync(new URL('../src/components/Settings.tsx', import.meta.url), 'utf8');
const settingsTypes = readFileSync(new URL('../src/store/types.ts', import.meta.url), 'utf8');
const updatesPage = readFileSync(new URL('../src/components/settings/ActualizacionesPage.tsx', import.meta.url), 'utf8');
const css = readFileSync(new URL('../src/styles/app.css', import.meta.url), 'utf8');

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
  assert.match(appUpdaterPhaseText(state('checking')), /versión verificada/);
  assert.doesNotMatch(appUpdaterPhaseText(state('staging')), /firma/);
  assert.match(appUpdaterPhaseText(state('staging', { targetVersion: '1.2.0', stagingStep: 'preflight' })), /Probando la 1\.2\.0 antes de instalarla/);
  assert.match(appUpdaterPhaseText(state('rollback_healthy')), /restauró la versión anterior/);
});

test('failed state surfaces the bounded updater error', () => {
  assert.equal(appUpdaterPhaseText(state('failed', { error: 'checksum inválido' })), 'checksum inválido');
});

test('a retained failure receipt visibly offers the newer release found by background polling', () => {
  const failedWithNewOffer = state('failed', {
    targetVersion: '1.2.0',
    availableVersion: '1.2.0',
    error: 'falló 1.1.0',
  });
  assert.equal(appUpdaterCardTitle(failedWithNewOffer), 'Workass 1.2.0');
  assert.match(appUpdaterPhaseText(failedWithNewOffer), /Workass 1\.2\.0 está disponible/);
  assert.match(appUpdaterPhaseText(failedWithNewOffer), /falló 1\.1\.0/);

  const rollbackWithNewOffer = state('rollback_healthy', {
    targetVersion: '1.2.0',
    availableVersion: '1.2.0',
  });
  assert.equal(appUpdaterCardTitle(rollbackWithNewOffer), 'Workass 1.2.0');
  assert.match(appUpdaterPhaseText(rollbackWithNewOffer), /restauró la versión saludable/);
});

test('availability-check failure is distinct from an attempted update failure', () => {
  const unavailable = state('check_failed', { error: 'GitHub no disponible' });
  assert.equal(appUpdaterPhaseText(unavailable), 'GitHub no disponible');
  assert.equal(appUpdaterCardTitle(unavailable), 'No se pudo buscar actualizaciones');
});

// 2026-11-11 (owner): the update popover is rows, variant A of
// desktop/docs/mocks/update-popover/mock-v2.html: only what needs updating,
// no card, no filled button, no spinning ring, one size in every state, and a
// "Buscar actualizaciones" action that checks now.
test('the update popover lists only what needs updating, as fixed rows with a quiet action', () => {
  const pop = sidebar.slice(sidebar.indexOf('className="footpop"'));
  assert.match(pop, /<div className="footpop-head"><span>Actualizaciones<\/span><CheckUpdatesButton \/><\/div>\s*<UpdateRows \/>/);
  assert.doesNotMatch(sidebar, /updcard|updring|updslot|footvers|uretry/);
  assert.doesNotMatch(css, /\.updcard|\.updring|@property --updring|\.footvers/);
  assert.match(css, /\.uprow \{[^}]*height: 44px;/);
  assert.match(css, /\.upact \{[^}]*background: transparent;/);
  // "La actualización sigue esperando" never comes back: running chat work is
  // not an updater gate (the comment that says so stays with the rows).
  assert.match(sidebar, /La actualización sigue esperando/);
  assert.match(sidebar, /const next = await selfUpdater\.apply\(\);/);
  assert.doesNotMatch(sidebar, /selfUpdater\.(download|install)/);
  assert.doesNotMatch(updaterSource, /\b(?:download|install)\(\):\s*Promise<AppUpdaterState>/);
  // The check button checks Workass and every agent, and holds its width.
  const check = sidebar.slice(sidebar.indexOf('function CheckUpdatesButton()'));
  assert.match(check, /updater\.check\(\)/);
  assert.match(check, /store\.checkProviderUpdates\(\)/);
  assert.match(css, /\.upcheck \{[^}]*min-width:/);
  // The Settings page keeps its own rule: no check button there.
  assert.doesNotMatch(settings, /WorkassPanel|useAppUpdater|appUpdaterPhaseText/);
  assert.doesNotMatch(settingsTypes, /SettingsSection\s*=\s*[^;]*'workass'/);
  assert.match(settingsTypes, /SettingsSection\s*=\s*[^;]*'actualizaciones'/);
  assert.match(settings, /actualizaciones: ActualizacionesPage/);
  assert.match(updatesPage, /useAppUpdater\(\)/);
  assert.doesNotMatch(updatesPage, /'Reiniciar'|Buscar ahora/);
});

test('a Workass release is one row through every step, its action never a slab', () => {
  const row = (phase: AppUpdaterState['phase'], extra: Partial<AppUpdaterState> = {}, armed = '') =>
    selfUpdateRow({ state: state(phase, { targetVersion: '0.1.288', currentVersion: '0.1.286', ...extra }), armed, actionBusy: false, preflightSeconds: 72 });
  assert.equal(row('current'), null, 'up to date: no row at all');
  assert.equal(row('healthy'), null);
  assert.equal(row('downloading', { progress: 0.42 })?.status, 'Bajando 0.1.288 · 42%');
  assert.equal(row('downloading', { progress: 0.42 })?.progress, 0.42);
  assert.equal(row('staging', { stagingStep: 'verify' })?.status, 'Verificando 0.1.288');
  assert.equal(row('staging', { stagingStep: 'preflight' })?.status, 'Probando 0.1.288 con tu estado · 1:12');
  assert.equal(row('staging')?.progress, 'ind');
  assert.equal(row('ready')?.action?.label, 'Actualizar');
  assert.equal(row('ready', {}, 'Se detendrá 1 subagente.')?.action?.label, 'Actualizar igual');
  assert.equal(row('ready', {}, 'Se detendrá 1 subagente.')?.tone, 'warn');
  const failed = row('failed', { targetVersion: '0.1.287', error: 'Workass 0.1.287 no pasó su prueba antes de instalarse (x). Sigue instalada 0.1.286.' });
  assert.equal(failed?.status, 'La 0.1.287 no pasó su prueba');
  assert.equal(failed?.tone, 'err');
  assert.equal(failed?.action?.label, 'Reintentar');
  assert.match(failed?.title ?? '', /no pasó su prueba antes de instalarse/, 'the whole reason on hover');
  assert.equal(row('failed', { availableVersion: '0.1.289' })?.action?.label, 'Actualizar');
});

test('an agent has a row only while it needs updating, is updating, or failed', () => {
  const updates = [
    { providerId: 'claude', cli: 'claude', installed: '2.1.322', latest: '2.1.322', updateAvailable: false },
    { providerId: 'codex', cli: 'codex', installed: '0.167.1', latest: '0.168.0', updateAvailable: true, hint: 'npm i -g @openai/codex' },
  ];
  const name = (id: string, cli: string) => ({ claude: 'Claude Code', codex: 'Codex' } as Record<string, string>)[id] ?? cli;
  const idle = providerUpdateRows(updates, {}, null, name);
  assert.deepEqual(idle.map((r) => r.name), ['Codex'], 'the agent that is up to date is not listed');
  assert.equal(idle[0].status, '0.167.1 → 0.168.0');
  assert.equal(idle[0].action?.label, 'Actualizar');
  const running = providerUpdateRows(updates, { codex: { providerId: 'codex', status: 'running', startedAt: '', tail: 'added 1 package\nchanged 3 packages' } }, null, name);
  assert.equal(running[0].status, 'changed 3 packages');
  assert.equal(running[0].progress, 'ind');
  assert.equal(running[0].action, null);
  const failed = providerUpdateRows(updates, { codex: { providerId: 'codex', status: 'failed', startedAt: '', tail: '', error: 'EACCES' } }, 'codex', name);
  assert.equal(failed[0].status, 'EACCES');
  assert.equal(failed[0].action?.label, 'Reintentar');
  assert.match(failed[0].title, /A mano: npm i -g @openai\/codex/);
});

// A shell with no updater (a release under its update preflight) answers null.
// Rendering from that took the whole window down; the hook keeps its
// "unavailable" state instead.
test('a null updater state from the shell is never adopted', () => {
  assert.match(updaterSource, /const adopt = \(next: AppUpdaterState \| null \| undefined\) => \{ if \(mounted && next\) setState\(next\); \};/);
  assert.match(updaterSource, /void bridge\.getState\(\)\.then\(adopt\)/);
  assert.match(updaterSource, /bridge\.onState\(adopt\)/);
  assert.match(updaterSource, /if \(!next\) return state;/);
});

test('native ZIP installation has a completed state without claiming provider health or rollback', () => {
  assert.equal(appUpdaterCardTitle(state('installed')), 'Listo');
  assert.match(appUpdaterPhaseText(state('installed')), /1\.1\.0 quedó actualizado/);
  assert.doesNotMatch(appUpdaterPhaseText(state('installed')), /saludable|restaur/);
});

test('a release being prepared is named by version, never as Workass updating', () => {
  // Owner 2026-11-11: after 0.1.287 failed its preflight, 0.1.288 staged and
  // the card read "Actualizando Workass · Verificando checksum…", the failed
  // update apparently still going.
  assert.equal(appUpdaterCardTitle(state('staging', { targetVersion: '0.1.288' })), 'Preparando Workass 0.1.288');
  assert.equal(appUpdaterCardTitle(state('downloading', { targetVersion: '0.1.288' })), 'Preparando Workass 0.1.288');
  assert.equal(appUpdaterCardTitle(state('installing', { targetVersion: '0.1.288' })), 'Instalando Workass 0.1.288');
  assert.equal(appUpdaterPhaseText(state('staging', { stagingStep: 'verify' })), 'Verificando la descarga…');
});
