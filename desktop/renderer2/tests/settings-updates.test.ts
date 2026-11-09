// Ajustes · Actualizaciones (owner-approved mock page 8, 2026-11-09).
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import { createServer, type ViteDevServer } from 'vite';

const page = readFileSync(new URL('../src/components/settings/ActualizacionesPage.tsx', import.meta.url), 'utf8');

async function vite(t: { after(fn: () => unknown): void }): Promise<ViteDevServer> {
  const server = await createServer({
    root: fileURLToPath(new URL('..', import.meta.url)),
    server: { middlewareMode: true, ws: false }, appType: 'custom', logLevel: 'silent',
  });
  t.after(() => server.close());
  return server;
}

type Helpers = typeof import('../src/components/settings/updates.ts');

const base = {
  supported: true, currentVersion: '0.1.271', targetVersion: null, availableVersion: null, checkedAt: null,
  progress: 0, error: null, blockers: null, receipt: null,
} as const;

test('this machine\'s row says its version and when it last looked, and offers only Actualizar', async (t) => {
  const server = await vite(t);
  const h = await server.ssrLoadModule('/src/components/settings/updates.ts') as Helpers;
  const now = Date.parse('2026-11-09T12:00:00Z');
  const current = { ...base, phase: 'current' as const, checkedAt: '2026-11-09T11:55:00Z' };
  assert.equal(h.selfUpdateDetail(current, now), '0.1.271 · la última · revisado hace 5 min.');
  assert.equal(h.selfUpdateAction(current), null, 'nothing to install, nothing to click: no "Buscar ahora"');
  const ready = { ...base, phase: 'ready' as const, targetVersion: '0.1.272' };
  assert.equal(h.selfUpdateDetail(ready, now), '0.1.271 · 0.1.272 ya está bajada: actualizar solo reinicia Workass.');
  assert.equal(h.selfUpdateAction(ready), 'Actualizar');
  assert.equal(h.selfUpdateAction({ ...base, phase: 'available' as const, targetVersion: '0.1.272' }), 'Actualizar');
  assert.equal(h.selfUpdateAction({ ...base, phase: 'downloading' as const, targetVersion: '0.1.272' }), null,
    'a background download shows progress, as the left panel does');
  assert.equal(h.selfUpdateAction({ ...base, phase: 'check_failed' as const }), null, 'a failed check retries by itself');
  assert.equal(h.selfUpdateAction({ ...base, phase: 'unavailable' as const, supported: false }), null);
  assert.match(h.selfUpdateDetail({ ...base, phase: 'unavailable' as const, supported: false }, now), /no se actualiza desde la app/);
  for (const phase of ['available', 'ready', 'busy', 'failed', 'rollback_healthy'] as const) {
    assert.notEqual(h.selfUpdateAction({ ...base, phase, availableVersion: '0.1.272' }), 'Reiniciar' as never);
  }
});

test('the Instalar solo row says what the shell is doing about a staged release', async (t) => {
  const server = await vite(t);
  const h = await server.ssrLoadModule('/src/components/settings/updates.ts') as Helpers;
  const ready = { ...base, phase: 'ready' as const, targetVersion: '0.1.272' };
  assert.equal(h.autoInstallDetail({ ...ready, autoInstall: { supported: true, enabled: false, waiting: false } }), h.AUTO_INSTALL_DESCRIPTION);
  assert.equal(h.autoInstallDetail({ ...ready, autoInstall: { supported: true, enabled: true, waiting: true } }),
    'Hay trabajo en curso: 0.1.272 se instala apenas termine.');
  assert.equal(h.autoInstallDetail({ ...ready, autoInstall: { supported: true, enabled: true, waiting: false } }),
    '0.1.272 se instala en un momento.');
  assert.equal(h.autoInstallDetail({ ...base, phase: 'current' as const, autoInstall: { supported: true, enabled: true, waiting: false } }),
    h.AUTO_INSTALL_DESCRIPTION);
});

test('another machine\'s row reads its own updater, whatever kind of install answers', async (t) => {
  const server = await vite(t);
  const h = await server.ssrLoadModule('/src/components/settings/updates.ts') as Helpers;
  // A packaged machine with its window open (renderer update.local.status).
  const mac = h.machineUpdateView({
    machineId: 'm-1', local: true,
    diagnostics: { state: { supported: true, phase: 'ready', currentVersion: '0.1.270', targetVersion: '0.1.271', availableVersion: null } },
    runningJobs: { total: 1, wouldDie: [{ workId: 'w', label: 'train' }], protected: [] },
  });
  assert.equal(mac.offered, '0.1.271');
  assert.equal(mac.wouldDie, 1);
  assert.equal(h.machineUpdateDetail(mac), '0.1.270 · 0.1.271 ya está bajada ahí.');
  // A headless Linux daemon.
  const linux = h.machineUpdateView({
    machineId: 'm-2', headless: true,
    diagnostics: { state: { supported: true, phase: 'available', currentVersion: '0.1.270', targetVersion: '0.1.271', availableVersion: '0.1.271' }, runningJobs: { wouldDie: [] } },
  });
  assert.equal(linux.offered, '0.1.271');
  assert.equal(linux.wouldDie, 0);
  assert.equal(h.machineUpdateView({ headless: true, diagnostics: { state: { supported: true, phase: 'downloading', currentVersion: '0.1.270', targetVersion: '0.1.271' } } }).updating, true);
  // A packaged machine whose app is closed: nothing can start an update there.
  const closed = h.machineUpdateView({
    daemonOnly: true, updaterAvailable: false, currentVersion: '0.1.269',
    diagnostics: { state: { phase: 'unavailable', currentVersion: '0.1.269', supported: false } },
  });
  assert.equal(closed.offered, '');
  assert.match(h.machineUpdateDetail(closed), /^0\.1\.269 · Workass está cerrado ahí/);
  // Never offered: not newer, or no version to compare.
  assert.equal(h.machineUpdateView({ diagnostics: { state: { supported: true, phase: 'available', currentVersion: '0.1.271', availableVersion: '0.1.271' } } }).offered, '');
  assert.equal(h.machineUpdateView({ diagnostics: { state: { supported: true, phase: 'available', currentVersion: '', availableVersion: '0.1.271' } } }).offered, '');
  assert.equal(h.machineUpdateView({ diagnostics: { state: { supported: true, phase: 'current', currentVersion: '0.1.271' } } }).wouldDie, null,
    'a host that does not say which jobs run is not read as running none');
  assert.equal(h.machineUpdateDetail(h.machineUpdateView({ diagnostics: { state: { supported: true, phase: 'current', currentVersion: '0.1.271' } } })), '0.1.271 · la última.');
  assert.equal(h.machineUpdateDetail(mac, '0.1.271'), '0.1.270 · actualizando a 0.1.271…');
});

test('updating another machine first says what it would stop there', async (t) => {
  const server = await vite(t);
  const h = await server.ssrLoadModule('/src/components/settings/updates.ts') as Helpers;
  assert.equal(h.machineUpdateWarning('Estudio', { foregroundTurns: 0, backgroundWork: 0, chats: [] }, 0), '');
  assert.equal(h.machineUpdateWarning('Estudio', null, null), '');
  assert.equal(h.machineUpdateWarning('Estudio', { foregroundTurns: 1, backgroundWork: 0, chats: [] }, 0), 'Se detendrá 1 turno activo en Estudio.');
  assert.equal(h.machineUpdateWarning('Estudio', { foregroundTurns: 2, backgroundWork: 1, chats: [] }, 3),
    'Se detendrán 2 turnos activos y 3 tareas en segundo plano en Estudio.');
  assert.equal(h.machineUpdateWarning('Estudio', null, 1), 'Se detendrá 1 tarea en segundo plano en Estudio.');
});

test('an agent row says its installed version and the newer one', async (t) => {
  const server = await vite(t);
  const h = await server.ssrLoadModule('/src/components/settings/updates.ts') as Helpers;
  const update = { providerId: 'agent-a', cli: 'Agent A', installed: '1.9.0', latest: '1.9.2', updateAvailable: true, hint: 'agent-a update' };
  assert.equal(h.agentUpdateDetail(update), '1.9.0 · hay 1.9.2.');
  assert.equal(h.agentUpdateDetail({ ...update, latest: '1.9.0', updateAvailable: false }), '1.9.0 · al día.');
  assert.equal(h.agentUpdateDetail(update, { providerId: 'agent-a', status: 'running', startedAt: '', tail: '' }), '1.9.0 · actualizando a 1.9.2…');
  assert.equal(h.agentUpdateDetail({ ...update, lastError: 'exit 1' }), '1.9.0 · no se pudo actualizar a 1.9.2. A mano: agent-a update');
});

test('Instalar solo is saved in the settings blob with every other key, and handed to the shell', async (t) => {
  const previousWindow = (globalThis as any).window;
  const shellCalls: boolean[] = [];
  const saved: Array<Record<string, unknown>> = [];
  let blob: Record<string, unknown> = { autoInstallUpdates: true, chatTitles: { mode: 'prompt', providerId: '', model: '' }, prApprover: 'someone' };
  (globalThis as any).window = {
    api: {
      getSettings: async () => ({ settings: blob }),
      setSettings: async (next: Record<string, unknown>) => { saved.push(next); blob = next; return { settings: next }; },
    },
    workassUpdater: { setAutoInstall: async (enabled: boolean) => { shellCalls.push(enabled); return null; } },
  };
  t.after(() => {
    if (previousWindow === undefined) delete (globalThis as any).window;
    else (globalThis as any).window = previousWindow;
  });
  const server = await vite(t);
  const module = await server.ssrLoadModule('/src/store/store.ts') as {
    store: { clearToastTimers(): void };
    Store: new () => { state: Record<string, any>; clearToastTimers(): void; setAutoInstallUpdates(enabled: boolean): void };
  };
  const subject = new module.Store();
  t.after(() => { subject.clearToastTimers(); module.store.clearToastTimers(); });

  await (subject as any).hydrateSettings();
  assert.equal(subject.state.autoInstallUpdates, true);
  assert.deepEqual(shellCalls, [true], 'the shell learns the saved choice on load');

  subject.setAutoInstallUpdates(false);
  assert.deepEqual(shellCalls, [true, false], 'and every change at once');
  clearTimeout((subject as any).settingsSaveTimer);
  await (subject as any).flushModelPreferences();
  assert.equal(saved.length, 1);
  assert.equal(saved[0].autoInstallUpdates, false);
  assert.equal(saved[0].prApprover, 'someone', 'settings:set replaces the blob: every other key goes back as it was');
});

// The page reuses this machine's one updater and its one action. It never adds
// a "check now" (the owner rejected it) and never relabels the action.
test('the Actualizaciones page drives the same shell updater as the left panel', () => {
  assert.match(page, /useAppUpdater\(\)/);
  assert.match(page, /updater\.apply\(\)/);
  assert.doesNotMatch(page, /updater\.check\(|\.check\(\)/);
  assert.doesNotMatch(page, /Buscar ahora|Reiniciar/);
  assert.match(page, /appUpdateWorkWarning\(/, 'the same warning about running work before an update');
  assert.match(page, /store\.applyMachineUpdate\(/);
  assert.match(page, /store\.updateProvider\(/);
});
