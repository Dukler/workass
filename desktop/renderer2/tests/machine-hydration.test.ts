// Boot replaces the whole state tree from this daemon's session mirror. Anything
// the client owns — rather than the daemon — has to be carried across by hand,
// and the machine book is the newest member of that set (E3).
//
// The bug this pins: `mountMachines()` runs BEFORE hydration, so the flags it
// set were wiped a moment later. The Máquinas pane then reported "this daemon
// has no machine book" against a daemon that had one — and since that pane is
// the only surface that can add the FIRST machine, the feature was unreachable
// from the UI. It failed silently: no error, just an empty-state sentence that
// read like a daemon capability check.
//
// Asserted at source level on purpose: reproducing it needs a full boot with a
// window, a socket and a server mirror, and the invariant is the field list.
import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const source = readFileSync(fileURLToPath(new URL('../src/store/store.ts', import.meta.url)), 'utf8');

/** The carry-over block: from the wholesale replacement to the save that ends it. */
function carryOverBlock(): string {
  const start = source.indexOf('this.state = this.fromMirror(authoritative)');
  assert.ok(start > 0, 'hydration no longer replaces state from the authoritative mirror');
  const end = source.indexOf('this.requireFullSave()', start);
  assert.ok(end > start, 'carry-over block no longer ends at requireFullSave()');
  return source.slice(start, end);
}

test('the machine book survives hydration replacing the state tree', () => {
  const block = carryOverBlock();
  for (const field of ['machines', 'hasMachineChannels', 'hasFleetKey']) {
    assert.match(
      block,
      new RegExp(`this\\.state\\.${field}\\s*=\\s*live\\.${field}`),
      `hydration drops ${field}: mounted before boot, wiped by it`,
    );
  }
});

// Boot is not the only wholesale restore. `restoreSessionSnapshot` runs on
// reconnect AND on the periodic session digest, so a mounted machine's chats
// arrived and were wiped about a second later — the machine row said
// "conectada · 1 conversación" while the list showed nothing.
test('every wholesale chat restore carries the other machines chats', () => {
  const sites = ['private restoreSessionSnapshot', 'this.state = this.fromMirror(authoritative)'];
  for (const site of sites) {
    const start = source.indexOf(site);
    assert.ok(start > 0, `restore path moved: ${site}`);
    const window = source.slice(start, start + 1800);
    const assignment = window.indexOf('this.state.chats = ');
    assert.ok(assignment > 0, `no chats assignment near ${site}`);
    const line = window.slice(assignment, window.indexOf('\n', assignment));
    assert.match(line, /carryRemoteChats\(/, `${site} replaces chats without carrying remote ones: ${line.trim()}`);
  }
});

test('every field mountMachines writes is carried across hydration', () => {
  const mount = source.slice(source.indexOf('private async mountMachines'), source.indexOf('private applyMachineBook'));
  assert.ok(mount.length > 0, 'mountMachines/applyMachineBook no longer exist');
  const written = new Set(Array.from(mount.matchAll(/this\.state\.([A-Za-z]+)\s*=/g), (m) => m[1]));
  assert.ok(written.size > 0, 'mountMachines writes no client state — did it move?');
  const block = carryOverBlock();
  for (const field of written) {
    assert.match(block, new RegExp(`this\\.state\\.${field}\\s*=\\s*live\\.${field}`), `hydration drops ${field}`);
  }
});

// Same trap one call deeper. `reloadFleetKeys` is fired from mountMachines, so
// its writes land before hydration too — but they happen in another method, so
// the scan above cannot see them. Dropped, the pane that shows this machine's
// fleet key disappears a second after boot on a daemon that has one, which reads
// as "this build does not have that feature" rather than as a bug.
test('the fleet key pane survives hydration too', () => {
  const reload = source.slice(source.indexOf('async reloadFleetKeys'), source.indexOf('async revealFleetKey'));
  assert.ok(reload.length > 0, 'reloadFleetKeys no longer exists');
  const written = new Set(Array.from(reload.matchAll(/this\.state\.([A-Za-z]+)\s*=/g), (m) => m[1]));
  assert.ok(written.has('fleetKeys'), 'reloadFleetKeys no longer publishes the key list');
  const block = carryOverBlock();
  for (const field of written) {
    assert.match(block, new RegExp(`this\\.state\\.${field}\\s*=\\s*live\\.${field}`), `hydration drops ${field}`);
  }
});

// The secret opens every machine you own. It is returned to the caller and held
// by the pane that asked for it; the moment it lands in app state it rides every
// snapshot, every session save and every rendered payload.
test('no fleet secret is ever written into app state', () => {
  const stateWrites = Array.from(source.matchAll(/this\.state\.[A-Za-z]+\s*=\s*([^\n;]+)/g), (m) => m[0]);
  for (const write of stateWrites) {
    assert.doesNotMatch(write, /\.secret\b/, `a fleet secret reaches app state: ${write.trim()}`);
  }
});
