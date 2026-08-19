'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const {
  defaultOperations,
  launchUntilHealthy,
  mirrorWindowsDirectory,
  renamePathWithRetry,
  runTransaction,
  runtimeIsHealthy,
  spawnDetached,
  startInstalledRuntime,
  stopLaunchedProcessTree,
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
    ['resources', 'app', 'update-lock-recovery.js'],
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
    requireVisibleWindow: true,
    designatedRequirement: 'identifier "com.workass.app" and certificate root = H"abc"',
  };
}

function windowsTransactionFixture() {
  const tx = transactionFixture();
  return {
    ...tx,
    platform: 'win32',
    installTarget: path.join(path.dirname(tx.installTarget), 'Workass'),
    incomingTarget: path.join(tx.transactionRoot, 'incoming-release'),
    backupTarget: path.join(tx.transactionRoot, 'installed-before-activation'),
    designatedRequirement: '',
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
    stopOldShell: async () => { calls.push('stop-old-shell'); },
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

test('transaction validation keeps macOS swap paths beside the app and Windows release paths inside its transaction', () => {
  const tx = transactionFixture();
  assert.equal(validateTransaction(tx), tx);
  assert.throws(() => validateTransaction({ ...tx, requireVisibleWindow: false }), /visible shell window/);
  assert.throws(() => validateTransaction({ ...tx, backupTarget: path.join(path.dirname(path.dirname(tx.backupTarget)), 'elsewhere', 'old') }), /one parent/);
  assert.throws(() => validateTransaction({ ...tx, mutableStateTarget: path.dirname(tx.mutableStateTarget) }), /exact Workass state directory/);
  assert.throws(() => validateTransaction({ ...tx, mutableStateBackupTarget: path.join(path.dirname(tx.transactionRoot), 'state') }), /distinct children/);
  assert.throws(() => validateTransaction({ ...tx, designatedRequirement: '' }), /invalid macOS/);
  const windows = windowsTransactionFixture();
  assert.equal(validateTransaction(windows), windows);
  assert.throws(() => validateTransaction({
    ...windows,
    backupTarget: path.join(path.dirname(windows.installTarget), '.Workass.previous-update-fixture-1234'),
  }), /exact children/);
  assert.throws(() => validateTransaction({
    ...windows,
    installTarget: path.dirname(windows.transactionRoot),
  }), /must not overlap/);
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

test('the swapped Windows daemon is prestarted before Electron relaunches', async () => {
  const tx = windowsTransactionFixture();
  const runtime = {
    daemonURL: 'https://127.0.0.1:8788',
    viewPort: 8798,
  };
  let ensured = null;
  const receipt = await startInstalledRuntime(tx, {
    resolveRuntimeProfile: (options) => {
      assert.equal(options.resourcesPath, path.join(tx.installTarget, 'resources'));
      assert.equal(options.isPackaged, true);
      return runtime;
    },
    ensurePortableDaemon: async (options) => {
      ensured = options;
      return { status: 'started-and-running' };
    },
  });
  assert.equal(receipt.status, 'started-and-running');
  assert.equal(ensured.runtime, runtime);
  assert.equal(ensured.resourcesPath, path.join(tx.installTarget, 'resources'));
  assert.equal(ensured.executablePath, path.join(tx.installTarget, 'Workass.exe'));
  assert.equal(ensured.platform, 'win32');
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
    WORKASS_CONTROLLER_RECOVERY: '1',
    WORKASS_UPDATE_RELAUNCH: '1',
    WORKASS_LOCK_RECOVERY_CHILD: '1',
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
  assert.equal(env.WORKASS_CONTROLLER_RECOVERY, undefined);
  assert.equal(env.WORKASS_UPDATE_RELAUNCH, undefined);
  assert.equal(env.WORKASS_LOCK_RECOVERY_CHILD, undefined);
});

test('Windows stops the complete failed Electron process tree before rollback', async () => {
  let running = true;
  const calls = [];
  await stopLaunchedProcessTree(7123, {
    platform: 'win32',
    alive: () => running,
    run: (...args) => {
      calls.push(args);
      running = false;
      return { status: 0 };
    },
    wait: async (predicate) => predicate(),
  });
  assert.deepEqual(calls, [[
    'taskkill.exe', ['/PID', '7123', '/T', '/F'], { windowsHide: true, stdio: 'ignore' },
  ]]);
});

test('rollback path renames retry bounded transient file locks', async () => {
  let attempts = 0;
  const pauses = [];
  await renamePathWithRetry('incoming', 'installed', {
    attempts: 4,
    delayMs: 7,
    rename: () => {
      attempts += 1;
      if (attempts < 3) throw Object.assign(new Error('locked'), { code: 'EBUSY' });
    },
    pause: async (milliseconds) => { pauses.push(milliseconds); },
  });
  assert.equal(attempts, 3);
  assert.deepEqual(pauses, [7, 7]);
  await assert.rejects(() => renamePathWithRetry('incoming', 'installed', {
    rename: () => { throw Object.assign(new Error('invalid'), { code: 'EINVAL' }); },
  }), /invalid/);
});

test('Windows directory mirrors accept robocopy difference codes and reject failure codes', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-robocopy-'));
  const source = path.join(root, 'source');
  const destination = path.join(root, 'destination');
  fs.mkdirSync(source);
  const calls = [];
  mirrorWindowsDirectory(source, destination, {
    run: (...args) => { calls.push(args); return { status: 7 }; },
  });
  assert.equal(calls[0][0], 'robocopy.exe');
  assert.deepEqual(calls[0][1].slice(0, 3), [source, destination, '/MIR']);
  assert.ok(calls[0][1].includes('/XJ'));
  assert.ok(calls[0][1].includes('/SL'));
  assert.throws(() => mirrorWindowsDirectory(source, destination, {
    run: () => ({ status: 8 }),
  }), /robocopy exit 8/);
});

test('Windows activation preserves the install directory and restores it by mirroring the external backup', async () => {
  const tx = windowsTransactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.installTarget, 'stale.txt'), 'remove-me');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  const calls = [];
  const mirror = (source, destination) => {
    calls.push([source, destination]);
    fs.rmSync(destination, { recursive: true, force: true });
    fs.cpSync(source, destination, { recursive: true });
  };
  const disk = defaultOperations(tx, { mirrorWindowsDirectory: mirror });
  await disk.activate();
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'new-release');
  assert.equal(fs.existsSync(path.join(tx.installTarget, 'stale.txt')), false);
  assert.deepEqual(calls, [
    [tx.installTarget, tx.backupTarget],
    [tx.incomingTarget, tx.installTarget],
  ]);
  await disk.rollback();
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'stale.txt'), 'utf8'), 'remove-me');
  assert.deepEqual(calls[2], [tx.backupTarget, tx.installTarget]);
});

test('a partial Windows activation is rolled back before the previous runtime relaunches', async () => {
  const tx = windowsTransactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  let mirrors = 0;
  const mirror = (source, destination) => {
    mirrors += 1;
    fs.rmSync(destination, { recursive: true, force: true });
    fs.cpSync(source, destination, { recursive: true });
    if (mirrors === 2) throw new Error('activation mirror failed');
  };
  const disk = defaultOperations(tx, { mirrorWindowsDirectory: mirror });
  const ops = operations({
    activate: disk.activate,
    rollback: disk.rollback,
    cleanupFailed: disk.cleanupFailed,
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
  assert.equal(fs.existsSync(tx.backupTarget), false);
  assert.equal(fs.existsSync(tx.incomingTarget), false);
});

test('activation health rejects a daemon that retained the previous bind mode', () => {
  const shell = { controller: true, appVersion: '1.1.0' };
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

test('activation health can require a visible shell window', () => {
  const daemon = { app: 'workass', version: '1.1.0', bind: 'lan' };
  const shell = {
    controller: true,
    appVersion: '1.1.0',
    windowVisible: false,
  };
  assert.equal(runtimeIsHealthy({ daemon, shell, expectedVersion: '1.1.0' }), true);
  assert.equal(runtimeIsHealthy({ daemon, shell, expectedVersion: '1.1.0', requireVisibleWindow: true }), false);
  assert.equal(runtimeIsHealthy({ daemon, shell: { ...shell, windowVisible: true }, expectedVersion: '1.1.0', requireVisibleWindow: true }), true);
});

test('activation does not roll back a healthy UI while provider catalogs are still hydrating', () => {
  const daemon = { app: 'workass', version: '1.1.0' };
  const shell = {
    controller: true,
    appVersion: '1.1.0',
    windowVisible: true,
    catalog: { readyModelCount: 0 },
  };
  assert.equal(runtimeIsHealthy({
    daemon, shell, expectedVersion: '1.1.0', requireVisibleWindow: true,
  }), true);
});

test('installed process launch waits for Windows spawn acknowledgement and surfaces failure', async () => {
  const successful = new EventEmitter();
  successful.pid = 8123;
  successful.unref = () => { successful.unrefCalled = true; };
  const launched = spawnDetached('Workass.exe', [], {}, { spawnProcess: () => successful });
  successful.emit('spawn');
  assert.equal(await launched, successful);
  assert.equal(successful.unrefCalled, true);

  const failed = new EventEmitter();
  const rejected = spawnDetached('Workass.exe', [], {}, { spawnProcess: () => failed });
  failed.emit('error', Object.assign(new Error('blocked'), { code: 'EACCES' }));
  await assert.rejects(rejected, /blocked/);
});

test('runtime recovery retries launch so a stale Electron singleton cannot strand the app', async () => {
  const launches = [];
  let healthChecks = 0;
  let clock = -5000;
  const healthy = await launchUntilHealthy({
    launchInstalled: async (options) => { launches.push(options); },
    healthy: async () => { healthChecks += 1; return healthChecks >= 3; },
    waitUntil: async (predicate) => {
      for (let attempt = 0; attempt < 5; attempt += 1) if (await predicate(attempt)) return true;
      return false;
    },
  }, '1.1.0', {
    updateRelaunch: true,
    relaunchIntervalMs: 5000,
    now: () => { clock += 5000; return clock; },
  });
  assert.equal(healthy, true);
  assert.equal(healthChecks, 3);
  assert.deepEqual(launches, [
    { updateRelaunch: true },
    { updateRelaunch: true },
    { updateRelaunch: true },
  ]);
});

test('Windows target relaunch advertises recovery while rollback keeps older installed GUIs visible', async () => {
  const tx = windowsTransactionFixture();
  let launch = null;
  const disk = defaultOperations(tx, {
    spawnProcess: (command, args, options) => {
      launch = { command, args, options };
      const child = new EventEmitter();
      child.pid = 9012;
      child.unref = () => {};
      queueMicrotask(() => child.emit('spawn'));
      return child;
    },
  });
  await disk.launchInstalled({ updateRelaunch: true });
  assert.equal(launch.command, path.join(tx.installTarget, 'Workass.exe'));
  assert.deepEqual(launch.args, []);
  assert.equal(launch.options.cwd, tx.installTarget);
  assert.equal(launch.options.detached, true);
  assert.equal(launch.options.windowsHide, false);
  assert.equal(launch.options.env.WORKASS_UPDATE_RELAUNCH, '1');

  await disk.launchInstalled({ updateRelaunch: false });
  assert.equal(launch.options.env.WORKASS_UPDATE_RELAUNCH, undefined);

  const macTx = transactionFixture();
  const macLaunches = [];
  const macDisk = defaultOperations(macTx, {
    spawnProcess: (command, args, options) => {
      macLaunches.push({ command, args, options });
      const child = new EventEmitter();
      child.pid = 9013;
      child.unref = () => {};
      queueMicrotask(() => child.emit('spawn'));
      return child;
    },
  });
  await macDisk.launchInstalled();
  assert.equal(macLaunches[0].options.windowsHide, true);
  assert.equal(macLaunches[0].options.env.WORKASS_UPDATE_RELAUNCH, '1');
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

test('the update relaunch keeps macOS hidden-until-ready and makes Windows visible immediately', () => {
  const worker = fs.readFileSync(path.join(__dirname, 'update-worker.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(worker, /WORKASS_UPDATE_RELAUNCH:\s*'1'/);
  assert.match(main, /show:\s*showOnCreate/);
  assert.match(main, /if \(isUpdateRelaunch\)/);
  assert.match(main, /win\.once\('ready-to-show', reveal\)/);
  assert.match(main, /focusPrimaryWindow\(\(\) => \[win\]/);
  assert.match(main, /createUpdateBootstrapWindow/);
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

test('Windows force-stops the exact old Electron tree after the graceful quit boundary', async () => {
  const tx = windowsTransactionFixture();
  let forced = false;
  const ops = operations({
    shellExited: async () => forced,
    stopOldShell: async () => { ops.calls.push('stop-old-shell'); forced = true; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.oldShellForced, true);
  assert.equal(ops.calls[0], 'stop-old-shell');
  assert.deepEqual(ops.calls.slice(1), [
    'stop-service', 'verify', 'snapshot-state', 'activate', 'start-runtime',
    'launch', 'healthy:1.1.0', 'cleanup',
  ]);
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
