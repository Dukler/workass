import assert from 'node:assert/strict';
import test from 'node:test';
import {
  CONTROLLER_MIGRATION_KEY, filterCommands, fold, forceReconnect, localDaemonRestartAvailable,
  reconnectCommandTitle, RESTART_FAILURE_MESSAGE, RESTART_TIMEOUT_MESSAGE, type Command,
} from '../src/store/commands.ts';

function cmd(id: string, title: string, keywords?: string): Command {
  return { id, title, keywords, run: () => {} };
}

// The whole reason the command exists. A plain reload leaves the shell's
// controller-migration marker in place, and that marker is what forbids this
// device from re-taking a lease stranded on a dead device identity — so a plain
// reload cannot repair "running, connected, and not the controller". Clearing
// the marker is the step that makes it a recovery command instead of F5.
test('the reload clears the controller marker, which is what a plain reload does not', async () => {
  const removed: string[] = [];
  let reloaded = 0;
  const receipt = await forceReconnect({
    storage: { removeItem: (k: string) => { removed.push(k); } },
    takeControl: async () => ({ controller: true }),
    reload: () => { reloaded += 1; },
  });
  assert.deepEqual(removed, [CONTROLLER_MIGRATION_KEY]);
  assert.equal(reloaded, 1);
  assert.deepEqual(receipt, {
    markerCleared: true, takeControlAttempted: true, takeControlSettled: true,
    daemonRestartAttempted: false, daemonRestartSettled: false, reloaded: true,
  });
});

test('the recovery command restarts the local daemon before reloading', async () => {
  const calls: string[] = [];
  const receipt = await forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined,
    restartDaemon: async () => { calls.push('restart'); return { ok: true }; },
    reload: () => { calls.push('reload'); },
  });
  assert.deepEqual(calls, ['restart', 'reload']);
  assert.equal(receipt.daemonRestartAttempted, true);
  assert.equal(receipt.daemonRestartSettled, true);
});

test('the renderer does not reload on a structured daemon restart failure', async () => {
  let reloaded = 0;
  await assert.rejects(forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined,
    restartDaemon: async () => ({ ok: false, error: 'private bootstrap detail' }),
    reload: () => { reloaded += 1; },
  }), { message: RESTART_FAILURE_MESSAGE });
  assert.equal(reloaded, 0);
});

test('the renderer does not reload when the daemon restart promise rejects', async () => {
  let reloaded = 0;
  await assert.rejects(forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined,
    restartDaemon: async () => { throw new Error('private shell detail'); },
    reload: () => { reloaded += 1; },
  }), { message: RESTART_FAILURE_MESSAGE });
  assert.equal(reloaded, 0);
});

test('restart taking longer than the take-control budget finishes before renderer reload', async () => {
  let finishRestart!: (value: unknown) => void;
  let reloaded = 0;
  const operation = forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined,
    restartDaemon: () => new Promise((resolve) => { finishRestart = resolve; }),
    reload: () => { reloaded += 1; }, restartTimeoutMs: 5000,
  });
  await new Promise((resolve) => setTimeout(resolve, 1650));
  assert.equal(reloaded, 0, 'settling the old 1.5 second race must not reload');
  finishRestart({ ok: true });
  const receipt = await operation;
  assert.equal(receipt.daemonRestartSettled, true);
  assert.equal(reloaded, 1);
});

test('restart timeout keeps the renderer open and tells the user to retry', async () => {
  let reloaded = 0;
  await assert.rejects(forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined,
    restartDaemon: () => new Promise(() => {}),
    reload: () => { reloaded += 1; }, restartTimeoutMs: 5,
  }), { message: RESTART_TIMEOUT_MESSAGE });
  assert.equal(reloaded, 0);
});

// The state this is FOR is a wedged socket, so an invoke that never answers must
// not eat the reload. The timeout is the guarantee.
test('a take-control that never answers still reloads', async () => {
  let reloaded = 0;
  const receipt = await forceReconnect({
    storage: { removeItem: () => {} },
    takeControl: () => new Promise(() => {}),   // never settles, like a dead socket
    reload: () => { reloaded += 1; },
    timeoutMs: 5,
  });
  assert.equal(receipt.takeControlSettled, false, 'the call is abandoned, not awaited');
  assert.equal(reloaded, 1, 'the reload happens anyway');
});

test('a rejecting take-control and unusable storage still reload', async () => {
  let reloaded = 0;
  const receipt = await forceReconnect({
    storage: { removeItem: () => { throw new Error('private mode'); } },
    takeControl: async () => { throw new Error('lan:not-controller'); },
    reload: () => { reloaded += 1; },
  });
  assert.equal(receipt.markerCleared, false);
  assert.equal(reloaded, 1);
});

// A daemon old enough to lack lan:take-control (or a browser tier without it)
// must degrade to a plain reconnect, never throw on the way to the reload.
test('a bridge with no take-control reloads without one', async () => {
  let reloaded = 0;
  const receipt = await forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined, reload: () => { reloaded += 1; },
  });
  assert.equal(receipt.takeControlAttempted, false);
  assert.equal(reloaded, 1);
});

test('without the shell restart bridge this action is an honest reconnect', async () => {
  let reloaded = 0;
  const receipt = await forceReconnect({
    storage: { removeItem: () => {} }, takeControl: undefined, restartDaemon: undefined,
    reload: () => { reloaded += 1; },
  });
  assert.equal(localDaemonRestartAvailable(), false);
  assert.equal(reconnectCommandTitle(false), 'Reconectar Workass');
  assert.equal(receipt.daemonRestartAttempted, false);
  assert.equal(reloaded, 1);
});

test('accents and case do not decide whether a command is findable', () => {
  assert.equal(fold('Máquinas'), 'maquinas');
  const list = [cmd('a', 'Máquinas'), cmd('b', 'Ajustes')];
  assert.deepEqual(filterCommands(list, 'maquinas').map((c) => c.id), ['a']);
  assert.deepEqual(filterCommands(list, 'AJU').map((c) => c.id), ['b']);
});

// ⌘, then Enter has to be a stable gesture: with an empty query the registry
// order stands, and the recovery command is registered first.
test('an empty query keeps registry order so the first command stays under Enter', () => {
  const list = [cmd('reload', 'Recargar y reconectar'), cmd('settings', 'Ajustes')];
  assert.deepEqual(filterCommands(list, '   ').map((c) => c.id), ['reload', 'settings']);
});

test('the words a panicking user actually types reach the reload', () => {
  const list = [
    cmd('reload', 'Recargar y reconectar', 'reload reconectar reconnect refrescar arreglar atascado stuck'),
    cmd('settings', 'Ajustes', 'settings preferencias'),
    cmd('devices', 'Dispositivos', 'devices acceso revocar'),
  ];
  for (const q of ['reload', 'reconectar', 'recargar', 'atascado', 'stuck', 'arreglar']) {
    assert.equal(filterCommands(list, q)[0]?.id, 'reload', `"${q}" should reach the reload`);
  }
});

test('an earlier match outranks a later one, and a miss returns nothing', () => {
  const list = [cmd('a', 'Dispositivos'), cmd('b', 'Ajustes', 'dispositivo revocar')];
  assert.equal(filterCommands(list, 'dispositiv')[0]?.id, 'a');
  assert.deepEqual(filterCommands(list, 'zzzz'), []);
});
