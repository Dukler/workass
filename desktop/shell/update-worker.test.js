'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  defaultOperations,
  runTransaction,
  runtimeIsHealthy,
  startInstalledRuntime,
  targetRuntimeEnv,
  validateTransaction,
  verifyWindowsIncoming,
} = require('./update-worker');

function writeFakeWindowsPE(file) {
  const bytes = Buffer.alloc(256);
  bytes.write('MZ', 0, 'ascii');
  bytes.writeUInt32LE(128, 0x3c);
  bytes.write('PE\0\0', 128, 'ascii');
  bytes.writeUInt16LE(0x8664, 132);
  bytes.writeUInt16LE(0x20b, 152);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, bytes);
}

function writeWindowsRelease(root, version = '1.1.0') {
  const write = (relative, value) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, value);
  };
  write(['manifest.json'], `${JSON.stringify({ schemaVersion: 2, platform: 'windows', arch: 'amd64', version, portable: true, electron: true })}\n`);
  write(['resources', 'app', 'package.json'], `${JSON.stringify({ version })}\n`);
  for (const relative of [
    ['resources', 'app', 'update-manager.js'],
    ['resources', 'app', 'update-worker.js'],
    ['resources', 'renderer', 'index.html'],
    ['frontier-hosts', 'windows-amd64', 'claude-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'codex-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'],
  ]) write(relative, 'fixture');
  writeFakeWindowsPE(path.join(root, 'Workass.exe'));
  writeFakeWindowsPE(path.join(root, 'workass-daemon.exe'));
  writeFakeWindowsPE(path.join(root, 'node', 'windows-amd64', 'node.exe'));
}

function transactionFixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-worker-'));
  const parent = path.join(root, 'Applications');
  const dataRoot = path.join(root, 'data');
  const transactionRoot = path.join(dataRoot, 'updates', 'transactions', 'update-fixture-1234');
  fs.mkdirSync(parent, { recursive: true });
  fs.mkdirSync(transactionRoot, { recursive: true });
  return {
    schemaVersion: 2,
    updateId: 'update-fixture-1234',
    platform: 'darwin',
    currentVersion: '1.0.0',
    targetVersion: '1.1.0',
    shellPID: 44,
    transactionRoot,
    installTarget: path.join(parent, 'Workass.app'),
    incomingTarget: path.join(parent, '.Workass.app.incoming-update-fixture-1234'),
    backupTarget: path.join(parent, '.Workass.app.previous-update-fixture-1234'),
    mutableStateTarget: path.join(dataRoot, 'state'),
    mutableStateBackupTarget: path.join(transactionRoot, 'state-before-activation'),
    failedMutableStateTarget: path.join(transactionRoot, 'state-from-failed-activation'),
    receiptPath: path.join(dataRoot, 'updates', 'receipt.json'),
    daemonHealthURL: 'https://127.0.0.1:8788/workass/health',
    shellStatusURL: 'http://127.0.0.1:8798/__workass-shell/status',
    designatedRequirement: 'identifier "com.workass.app" and certificate root = H"abc"',
  };
}

function operations(overrides = {}) {
  const calls = [];
  return {
    calls,
    waitUntil: async (predicate) => predicate(),
    shellExited: async () => true,
    stopDaemonService: async () => { calls.push('stop-service'); },
    daemonDown: async () => true,
    verifyIncoming: async () => { calls.push('verify'); },
    snapshotMutableState: async () => { calls.push('snapshot-state'); },
    activate: async () => { calls.push('activate'); },
    startRuntime: async () => { calls.push('start-runtime'); },
    launchInstalled: async () => { calls.push('launch'); },
    healthy: async (version) => { calls.push(`healthy:${version}`); return true; },
    stopLaunched: async () => { calls.push('stop-launched'); },
    rollback: async () => { calls.push('rollback'); },
    restoreMutableState: async () => { calls.push('restore-state'); },
    cleanup: async () => { calls.push('cleanup'); },
    cleanupFailed: async () => { calls.push('cleanup-failed'); },
    ...overrides,
  };
}

test('transaction validation requires one same-parent atomic swap', () => {
  const tx = transactionFixture();
  assert.equal(validateTransaction(tx), tx);
  assert.throws(() => validateTransaction({ ...tx, backupTarget: path.join(path.dirname(path.dirname(tx.backupTarget)), 'elsewhere', 'old') }), /one parent/);
  assert.throws(() => validateTransaction({ ...tx, mutableStateTarget: path.dirname(tx.mutableStateTarget) }), /exact Workass state directory/);
  assert.throws(() => validateTransaction({ ...tx, mutableStateBackupTarget: path.join(path.dirname(tx.transactionRoot), 'state') }), /distinct children/);
  assert.throws(() => validateTransaction({ ...tx, designatedRequirement: '' }), /invalid macOS/);
  const windowsInstall = path.join(path.dirname(tx.installTarget), 'Workass');
  const windows = {
    ...tx,
    platform: 'win32',
    installTarget: windowsInstall,
    incomingTarget: path.join(path.dirname(windowsInstall), '.Workass.incoming-update-fixture-1234'),
    backupTarget: path.join(path.dirname(windowsInstall), '.Workass.previous-update-fixture-1234'),
    designatedRequirement: '',
  };
  assert.equal(validateTransaction(windows), windows);
});

test('the swapped macOS daemon is healthy before the shell is launched', async () => {
  const tx = transactionFixture();
  let ensured = null;
  const runtime = {
    daemonURL: 'https://127.0.0.1:8788',
    viewPort: 8798,
  };
  const receipt = await startInstalledRuntime(tx, {
    resolveRuntimeProfile: (options) => {
      assert.equal(options.isPackaged, true);
      assert.equal(options.resourcesPath, path.join(tx.installTarget, 'Contents', 'Resources'));
      assert.equal(options.env.WORKASS_DAEMON_BIND, undefined);
      assert.equal(options.env.WORKASS_PROFILE_FILE, undefined);
      return runtime;
    },
    ensurePackagedDaemon: async (options) => {
      ensured = options;
      return { status: 'installed-and-running' };
    },
  });
  assert.equal(receipt.status, 'installed-and-running');
  assert.equal(receipt.runtime, runtime);
  assert.equal(ensured.runtime, runtime);
  assert.equal(ensured.forceInstall, true);
});

test('the target app profile replaces stale runtime settings inherited from the old shell', () => {
  const env = targetRuntimeEnv({
    HOME: '/Users/tester',
    PATH: '/usr/bin',
    WORKASS_PROFILE: 'prod',
    WORKASS_PROFILE_FILE: '/old/profile.env',
    WORKASS_DAEMON_BIND: 'localhost',
    WORKASS_DAEMON_PORT: '8788',
    WORKASS_URL: 'https://127.0.0.1:8788',
    WORKASS_BROWSER_CONTROL_FILE: '/old/browser-control.json',
    VENDOR_SETTING: 'preserved',
  });
  assert.equal(env.HOME, '/Users/tester');
  assert.equal(env.PATH, '/usr/bin');
  assert.equal(env.VENDOR_SETTING, 'preserved');
  assert.equal(env.WORKASS_PROFILE, undefined);
  assert.equal(env.WORKASS_PROFILE_FILE, undefined);
  assert.equal(env.WORKASS_DAEMON_BIND, undefined);
  assert.equal(env.WORKASS_DAEMON_PORT, undefined);
  assert.equal(env.WORKASS_URL, undefined);
  assert.equal(env.WORKASS_BROWSER_CONTROL_FILE, undefined);
});

test('activation health rejects a daemon that retained the previous bind mode', () => {
  const shell = { controller: true, catalog: { readyModelCount: 2 }, appVersion: '1.1.0' };
  assert.equal(runtimeIsHealthy({
    daemon: { app: 'workass', version: '1.1.0', bind: 'lan' },
    shell,
    expectedVersion: '1.1.0',
    expectedBind: 'lan',
  }), true);
  assert.equal(runtimeIsHealthy({
    daemon: { app: 'workass', version: '1.1.0', bind: 'localhost' },
    shell,
    expectedVersion: '1.1.0',
    expectedBind: 'lan',
  }), false);
});

test('runtime prestart rejects a profile outside the prepared update transaction', async () => {
  const tx = transactionFixture();
  await assert.rejects(() => startInstalledRuntime(tx, {
    resolveRuntimeProfile: () => ({ daemonURL: 'https://127.0.0.1:9999', viewPort: 8798 }),
    ensurePackagedDaemon: async () => ({ status: 'installed-and-running' }),
  }), /does not match/);
});

test('the independent worker accepts the checksum-staged unsigned Windows portable tree', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-worker-windows-'));
  const incomingTarget = path.join(root, '.Workass.incoming-fixture');
  writeWindowsRelease(incomingTarget);
  assert.doesNotThrow(() => verifyWindowsIncoming({ incomingTarget, targetVersion: '1.1.0' }));
  const worker = fs.readFileSync(path.join(__dirname, 'update-worker.js'), 'utf8');
  assert.doesNotMatch(worker, /Get-AuthenticodeSignature|powershell\.exe/);
});

test('the update relaunch asks the existing Workass window to reveal itself only when ready', () => {
  const worker = fs.readFileSync(path.join(__dirname, 'update-worker.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(worker, /WORKASS_UPDATE_RELAUNCH:\s*'1'/);
  assert.match(main, /show:\s*!isUpdateRelaunch/);
  assert.match(main, /win\.once\('ready-to-show', reveal\)/);
  assert.match(main, /focusPrimaryWindow\(\(\) => \[win\]/);
});

test('healthy activation deletes the backup only after all runtime gates pass', async () => {
  const tx = transactionFixture();
  const ops = operations();
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.activated, true);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'snapshot-state', 'activate', 'start-runtime', 'launch', 'healthy:1.1.0', 'cleanup',
  ]);
  assert.equal(JSON.parse(fs.readFileSync(tx.receiptPath, 'utf8')).phase, 'healthy');
});

test('post-health cleanup failure cannot roll a healthy release back', async () => {
  const tx = transactionFixture();
  const ops = operations({
    cleanup: async () => { ops.calls.push('cleanup'); throw new Error('backup is busy'); },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.match(receipt.cleanupWarning, /backup is busy/);
  assert.doesNotMatch(ops.calls.join(','), /stop-launched|rollback|restore-state/);
});

test('failed new runtime is rolled back and the old version must pass the same gates', async () => {
  const tx = transactionFixture();
  const ops = operations({
    healthy: async (version) => {
      ops.calls.push(`healthy:${version}`);
      return version === tx.currentVersion;
    },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.equal(receipt.mutableStateRolledBack, true);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'snapshot-state', 'activate', 'start-runtime', 'launch', 'healthy:1.1.0',
    'stop-launched', 'rollback', 'restore-state', 'start-runtime', 'launch', 'healthy:1.0.0', 'cleanup-failed',
  ]);
});

test('post-rollback cleanup failure cannot hide that the previous release recovered', async () => {
  const tx = transactionFixture();
  const ops = operations({
    healthy: async (version) => {
      ops.calls.push(`healthy:${version}`);
      return version === tx.currentVersion;
    },
    cleanupFailed: async () => { ops.calls.push('cleanup-failed'); throw new Error('holding path is busy'); },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.match(receipt.cleanupWarning, /holding path is busy/);
});

test('daemon stop failure relaunches the untouched app instead of leaving Workass closed', async () => {
  const tx = transactionFixture();
  const ops = operations({
    daemonDown: async () => false,
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.activated, false);
  assert.equal(receipt.rolledBack, false);
  assert.deepEqual(ops.calls, ['stop-service', 'launch', 'healthy:1.0.0']);
});

test('incoming verification failure relaunches the untouched previous release without a bogus rollback swap', async () => {
  const tx = transactionFixture();
  const ops = operations({
    verifyIncoming: async () => { ops.calls.push('verify'); throw new Error('bad incoming release'); },
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.activated, false);
  assert.equal(receipt.rolledBack, false);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'stop-launched', 'start-runtime', 'launch', 'healthy:1.0.0', 'cleanup-failed',
  ]);
});

test('forced post-swap health failure restores the previous bytes on disk', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'bad-new-release');
  const ops = operations({
    snapshotMutableState: async () => {},
    activate: async () => {
      fs.renameSync(tx.installTarget, tx.backupTarget);
      fs.renameSync(tx.incomingTarget, tx.installTarget);
    },
    rollback: async () => {
      fs.renameSync(tx.installTarget, tx.incomingTarget);
      fs.renameSync(tx.backupTarget, tx.installTarget);
    },
    restoreMutableState: async () => {},
    cleanupFailed: async () => { fs.rmSync(tx.incomingTarget, { recursive: true, force: true }); },
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
  assert.equal(fs.existsSync(tx.backupTarget), false);
  assert.equal(fs.existsSync(tx.incomingTarget), false);
});

test('failed activation restores the exact pre-upgrade mutable state before starting the old runtime', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.mkdirSync(path.join(tx.mutableStateTarget, 'chat-actors'), { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  fs.writeFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'), '{"v":7}\n');
  fs.writeFileSync(path.join(tx.mutableStateTarget, 'chat-actors', 'chat.json'), '{"v":20}\n');
  const beforeProvider = fs.readFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'));
  const beforeActor = fs.readFileSync(path.join(tx.mutableStateTarget, 'chat-actors', 'chat.json'));
  const disk = defaultOperations(tx);
  let starts = 0;
  const ops = operations({
    snapshotMutableState: disk.snapshotMutableState,
    activate: disk.activate,
    rollback: disk.rollback,
    restoreMutableState: disk.restoreMutableState,
    cleanup: disk.cleanup,
    cleanupFailed: disk.cleanupFailed,
    startRuntime: async () => {
      starts += 1;
      if (starts === 1) {
        fs.writeFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'), '{"v":8}\n');
        fs.writeFileSync(path.join(tx.mutableStateTarget, 'chat-actors', 'chat.json'), '{"v":21}\n');
      } else {
        assert.deepEqual(fs.readFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json')), beforeProvider);
        assert.deepEqual(fs.readFileSync(path.join(tx.mutableStateTarget, 'chat-actors', 'chat.json')), beforeActor);
      }
    },
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.mutableStateRolledBack, true);
  assert.equal(starts, 2);
  assert.deepEqual(fs.readFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json')), beforeProvider);
  assert.deepEqual(fs.readFileSync(path.join(tx.mutableStateTarget, 'chat-actors', 'chat.json')), beforeActor);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
  assert.equal(fs.existsSync(tx.mutableStateBackupTarget), false);
  assert.equal(fs.existsSync(tx.failedMutableStateTarget), false);
});
