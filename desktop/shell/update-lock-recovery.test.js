'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  createUpdateBootstrapWindow,
  shouldStartUpdateLockRecovery,
  startWindowsUpdateLockRecovery,
} = require('./update-lock-recovery');

test('only the first Windows update process that lost the profile lock becomes a recovery bootstrap', () => {
  assert.equal(shouldStartUpdateLockRecovery({
    platform: 'win32', ownsProfileInstance: true, recoverController: true, retryChild: false,
  }), true);
  assert.equal(shouldStartUpdateLockRecovery({
    platform: 'win32', ownsProfileInstance: false, recoverController: true, retryChild: false,
  }), false);
  assert.equal(shouldStartUpdateLockRecovery({
    platform: 'win32', ownsProfileInstance: false, recoverController: true, retryChild: true,
  }), false);
  assert.equal(shouldStartUpdateLockRecovery({
    platform: 'darwin', ownsProfileInstance: false, recoverController: true, retryChild: false,
  }), false);
});

test('the Windows update bootstrap is a visible responsive native window', () => {
  let instance = null;
  class FakeWindow {
    constructor(options) { this.options = options; this.urls = []; instance = this; }
    loadURL(url) { this.urls.push(url); return Promise.resolve(); }
    show() { this.shown = true; }
    focus() { this.focused = true; }
  }
  const win = createUpdateBootstrapWindow({ BrowserWindow: FakeWindow, windowIcon: 'C:\\Workass.ico' });
  assert.equal(win, instance);
  assert.equal(win.options.show, true);
  assert.equal(win.options.frame, true);
  assert.equal(win.options.icon, 'C:\\Workass.ico');
  assert.equal(win.shown, true);
  assert.equal(win.focused, true);
  assert.match(decodeURIComponent(win.urls[0]), /Finishing the Workass update/);
});

test('a stale profile lock keeps one bootstrap and retries the real executable until the target window is healthy', async () => {
  const dataRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-lock-recovery-'));
  const executablePath = path.join(dataRoot, 'Workass.exe');
  const installationId = `install-${'1'.repeat(32)}`;
  fs.writeFileSync(executablePath, 'fixture');
  const calls = [];
  const app = {
    setPath(name, value) { calls.push(['setPath', name, value]); },
    whenReady: async () => {},
    quit() { calls.push(['quit']); },
  };
  class FakeWindow {
    constructor(options) { this.options = options; this.handlers = new Map(); }
    loadURL(url) { this.url = url; return Promise.resolve(); }
    show() {}
    focus() {}
    on(name, fn) { this.handlers.set(name, fn); }
    close() { this.handlers.get('closed')?.(); }
  }
  const retry = new EventEmitter();
  retry.unref = () => { retry.unrefCalled = true; };
  let healthCase = 'down';
  const scheduled = [];
  const recovery = await startWindowsUpdateLockRecovery({
    app,
    BrowserWindow: FakeWindow,
    executablePath,
    dataRoot,
    appVersion: '1.2.3',
    installationId,
    installTarget: dataRoot,
    daemonHealthURL: 'https://127.0.0.1:80/workass/health',
    readStatus: async () => healthCase !== 'down'
      ? {
        appVersion: '1.2.3', controller: true, windowVisible: true,
        installationId: healthCase === 'foreign' ? `install-${'2'.repeat(32)}` : installationId,
        installTarget: healthCase === 'wrong-target' ? path.join(dataRoot, 'OtherWorkass') : dataRoot,
        catalog: { readyModelCount: healthCase === 'empty-catalog' ? 0 : 2 },
      }
      : null,
    readDaemon: async () => healthCase !== 'down'
      ? { app: 'workass', version: '1.2.3' }
      : null,
    spawnProcess(command, args, options) {
      calls.push(['spawn', command, args, options]);
      return retry;
    },
    schedule(fn) { scheduled.push(fn); return { unref() {} }; },
    cancelSchedule() {},
  });

  assert.equal(recovery.retryRunning, true);
  const launch = calls.find((entry) => entry[0] === 'spawn');
  assert.equal(launch[1], executablePath);
  assert.equal(launch[3].windowsHide, false);
  assert.equal(launch[3].detached, true);
  assert.equal(launch[3].env.WORKASS_UPDATE_RELAUNCH, '1');
  assert.equal(launch[3].env.WORKASS_LOCK_RECOVERY_CHILD, '1');
  assert.equal(retry.unrefCalled, true);
  assert.equal(scheduled.length, 1);

  healthCase = 'foreign';
  assert.equal(await recovery.poll(), false);
  healthCase = 'wrong-target';
  assert.equal(await recovery.poll(), false);
  healthCase = 'empty-catalog';
  assert.equal(await recovery.poll(), false);
  assert.equal(calls.filter((entry) => entry[0] === 'quit').length, 0);
  healthCase = 'healthy';
  assert.equal(await recovery.poll(), true);
  assert.equal(calls.filter((entry) => entry[0] === 'spawn').length, 1);
  assert.equal(calls.filter((entry) => entry[0] === 'quit').length, 1);
});
