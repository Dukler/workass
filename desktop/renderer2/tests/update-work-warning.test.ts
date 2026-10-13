import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { appUpdateWorkWarning } from '../src/app-updater.ts';

const sidebar = readFileSync(new URL('../src/components/Sidebar.tsx', import.meta.url), 'utf8');
const bridge = readFileSync(new URL('../../../internal/httpserve/lan_bridge.go', import.meta.url), 'utf8');
const daemon = readFileSync(new URL('../../../cmd/workass/main.go', import.meta.url), 'utf8');
const titles: Record<string, string> = { a: 'DepthVision', b: 'fiumba playercut', c: 'Notas', d: 'Otra' };
const titleOf = (tabId: string) => titles[tabId] ?? '';

test('nothing running → no warning, the first click updates straight away', () => {
  assert.equal(appUpdateWorkWarning(undefined, titleOf), '');
  assert.equal(appUpdateWorkWarning({ foregroundTurns: 0, backgroundWork: 0, chats: [] }, titleOf), '');
});

test('the warning names what stops and where', () => {
  assert.equal(
    appUpdateWorkWarning({ foregroundTurns: 1, backgroundWork: 2, chats: [{ tabId: 'a', foregroundTurns: 1, backgroundWork: 2 }] }, titleOf),
    'Se detendrán 1 turno activo y 2 tareas en segundo plano en DepthVision.',
  );
  assert.equal(
    appUpdateWorkWarning({ foregroundTurns: 0, backgroundWork: 1, chats: [{ tabId: 'zz', backgroundWork: 1 }] }, titleOf),
    'Se detendrá 1 tarea en segundo plano.',
  );
  assert.equal(
    appUpdateWorkWarning({ foregroundTurns: 4, backgroundWork: 0, chats: ['a', 'b', 'c', 'd'].map((tabId) => ({ tabId, foregroundTurns: 1 })) }, titleOf),
    'Se detendrán 4 turnos activos en DepthVision, fiumba playercut y 2 más.',
  );
});

test('the card arms on running work and the second click proceeds — a heads-up, never a gate', () => {
  assert.match(sidebar, /appUpdateWorkWarning\(\s*await call\('appUpdateWork'\)/);
  assert.match(sidebar, /if \(warning\) \{ setSelfArmed\(warning\); return; \}/);
  assert.match(sidebar, /if \(!selfArmed\) \{/);
  assert.match(sidebar, /'Actualizar igual'/);
  assert.match(sidebar, /setTimeout\(\(\) => setSelfArmed\(''\), UPDATE_ARM_MS\)/);
});

test('the count is an additive read-only channel on the local bridge', () => {
  assert.match(bridge, /appUpdateWork: \(\) => invoke\('app-update:work'\)/);
  assert.match(daemon, /hub\.Register\("app-update:work", func\(args \[\]any\) \(any, error\) \{\n\t\treturn manager\.UpdateWorkInProgress\(\), nil/);
});
