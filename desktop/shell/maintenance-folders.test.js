'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { showMaintenanceFolder } = require('./maintenance-folders');

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-maintenance-folders-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const runtime = { stateDir: path.join(root, 'data', 'state'), logRoot: path.join(root, 'logs') };
  const calls = [];
  const shell = {
    showItemInFolder: (dir) => { calls.push(['reveal', dir]); },
    openPath: async (dir) => { calls.push(['open', dir]); return ''; },
  };
  return { root, runtime, shell, calls };
}

test('the data folder is revealed and the log folder opened, each from the profile path', async (t) => {
  const { runtime, shell, calls } = fixture(t);
  fs.mkdirSync(runtime.stateDir, { recursive: true });
  fs.mkdirSync(runtime.logRoot, { recursive: true });
  assert.deepEqual(await showMaintenanceFolder('data', { runtime, shell, fs }), { ok: true });
  assert.deepEqual(await showMaintenanceFolder('logs', { runtime, shell, fs }), { ok: true });
  assert.deepEqual(calls, [['reveal', runtime.stateDir], ['open', runtime.logRoot]]);
});

test('only the two named folders, and only when they are directories', async (t) => {
  const { root, runtime, shell, calls } = fixture(t);
  assert.equal((await showMaintenanceFolder('data', { runtime, shell, fs })).ok, false, 'a missing folder is not shown');
  fs.mkdirSync(path.dirname(runtime.logRoot), { recursive: true });
  fs.writeFileSync(runtime.logRoot, 'a file where the folder should be');
  assert.equal((await showMaintenanceFolder('logs', { runtime, shell, fs })).ok, false, 'a file is never opened');
  for (const kind of ['/etc', path.join(root, 'logs'), 'constructor', '__proto__', undefined, { kind: 'data' }]) {
    assert.deepEqual(await showMaintenanceFolder(kind, { runtime, shell, fs }), { ok: false, error: 'unknown folder' });
  }
  assert.deepEqual(calls, []);
});

test('a failed open reports the reason', async (t) => {
  const { runtime, calls } = fixture(t);
  fs.mkdirSync(runtime.logRoot, { recursive: true });
  const shell = { showItemInFolder() {}, openPath: async (dir) => { calls.push(dir); return 'no handler'; } };
  assert.deepEqual(await showMaintenanceFolder('logs', { runtime, shell, fs }), { ok: false, error: 'no handler' });
});

test('preload and main wire the folders through one owned IPC handler that takes no path', () => {
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(preload, /showDataFolder: \(\) => ipcRenderer\.invoke\('workass-maintenance:show-folder', 'data'\)/);
  assert.match(preload, /openLogsFolder: \(\) => ipcRenderer\.invoke\('workass-maintenance:show-folder', 'logs'\)/);
  assert.match(main, /ipcMain\.handle\('workass-maintenance:show-folder', \(event, kind\) => own\(event\)\s*\? showMaintenanceFolder\(kind, \{ runtime: RUNTIME, shell, fs \}\)/);
  assert.match(main, /ipcMain\.removeHandler\('workass-maintenance:show-folder'\)/);
});
