import assert from 'node:assert/strict';
import test from 'node:test';
import {
  CONTROLLER_MIGRATION_KEY, filterCommands, fold, forceReconnect, type Command,
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
    markerCleared: true, takeControlAttempted: true, takeControlSettled: true, reloaded: true,
  });
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
