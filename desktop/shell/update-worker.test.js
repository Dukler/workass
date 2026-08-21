'use strict';

const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { EventEmitter } = require('node:events');
const test = require('node:test');
const {
  atomicJSON,
  daemonServiceIsDown,
  defaultOperations,
  launchUntilHealthy,
  mirrorWindowsDirectory,
  replaceVisibleProgress,
  renamePathWithRetry,
  runTransaction,
  runtimeIsHealthy,
  spawnDetached,
  startInstalledRuntime,
  stopLaunchedProcessTree,
  stopWindowsExecutableProcesses,
  targetRuntimeEnv,
  validateTransaction,
  validateWorkerEntrypoint,
  verifyWindowsIncoming,
  waitUntilDeadline,
  workerSelfTestReport,
} = require('./update-worker');

const RELEASED_UPDATER_COMMIT = '4b3c961841eb9ab59a7430d65bf80f9ac2b87910';

function rejectReadOnlyFileFlushes(run) {
  const originalOpenSync = fs.openSync;
  const originalFsyncSync = fs.fsyncSync;
  const accessByDescriptor = new Map();
  fs.openSync = (...args) => {
    const descriptor = originalOpenSync(...args);
    accessByDescriptor.set(descriptor, args[1]);
    return descriptor;
  };
  fs.fsyncSync = (descriptor) => {
    if (accessByDescriptor.get(descriptor) === 'r') {
      const error = new Error('operation not permitted, fsync');
      error.code = 'EPERM';
      throw error;
    }
    return originalFsyncSync(descriptor);
  };
  try {
    return run();
  } finally {
    fs.openSync = originalOpenSync;
    fs.fsyncSync = originalFsyncSync;
  }
}

test('worker journal writes flush through the writable atomic file handle used on Windows', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-worker-flush-'));
  const file = path.join(root, 'journal.json');
  rejectReadOnlyFileFlushes(() => atomicJSON(file, { schemaVersion: 3, phase: 'armed' }));
  assert.deepEqual(JSON.parse(fs.readFileSync(file, 'utf8')), { schemaVersion: 3, phase: 'armed' });
});

test('the worker self-test reports the one exact transaction schema supported by this release', () => {
  assert.deepEqual(workerSelfTestReport(['--self-test', '--transaction-schema', '4']), {
    schemaVersion: 1,
    product: 'Workass',
    component: 'update-worker',
    supportedTransactionSchemas: [4],
    progressReceiptSchemaVersion: 1,
  });
  assert.throws(() => workerSelfTestReport(['--self-test', '--transaction-schema', '3']), /schema is unsupported/);
  assert.throws(() => workerSelfTestReport(['--self-test']), /schema is unsupported/);
});

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

function writeWindowsRelease(root, version = '1.1.0', installationId = `install-${'1'.repeat(32)}`) {
  const write = (relative, value) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, value);
  };
  write(['manifest.json'], `${JSON.stringify({ schemaVersion: 2, platform: 'windows', arch: 'amd64', version, portable: true, electron: true })}\n`);
  write(['resources', 'app', 'package.json'], `${JSON.stringify({ version })}\n`);
  write(['.workass-installation.json'], `${JSON.stringify({
    schemaVersion: 1,
    product: 'Workass',
    installationId,
  })}\n`);
  for (const relative of [
    ['resources', 'app', 'update-manager.js'],
    ['resources', 'app', 'update-worker.js'],
    ['resources', 'app', 'update-progress.js'],
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
    schemaVersion: 4,
    updateId: 'update-fixture-1234',
    platform: 'darwin',
    currentVersion: '1.0.0',
    targetVersion: '1.1.0',
    shellPID: 44,
    workerId: `worker-${'2'.repeat(32)}`,
    progressId: `progress-${'3'.repeat(32)}`,
    installationId: `install-${'1'.repeat(32)}`,
    transactionRoot,
    installTarget: path.join(parent, 'Workass.app'),
    incomingTarget: path.join(parent, '.Workass.app.incoming-update-fixture-1234'),
    backupTarget: path.join(parent, '.Workass.app.previous-update-fixture-1234'),
    mutableStateTarget: path.join(dataRoot, 'state'),
    mutableStateBackupTarget: path.join(transactionRoot, 'state-before-activation'),
    failedMutableStateTarget: path.join(transactionRoot, 'state-from-failed-activation'),
    receiptPath: path.join(dataRoot, 'updates', 'receipt.json'),
    journalPath: path.join(transactionRoot, 'journal.json'),
    leasePath: path.join(transactionRoot, 'worker-lease.json'),
    workerPath: path.join(transactionRoot, 'update-worker.js'),
    workerRuntimePath: path.join(transactionRoot, 'updater-node'),
    progressModulePath: path.join(transactionRoot, 'update-progress.js'),
    progressReceiptPath: path.join(transactionRoot, 'progress-receipt.json'),
    progressExecutable: path.join(parent, '.Workass.app.incoming-update-fixture-1234', 'Contents', 'MacOS', 'Workass'),
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
    workerRuntimePath: path.join(tx.transactionRoot, 'updater-node.exe'),
    progressExecutable: path.join(tx.transactionRoot, 'incoming-release', 'Workass.exe'),
    designatedRequirement: '',
  };
}

function operations(overrides = {}) {
  const calls = [];
  return {
    calls,
    waitUntil: async (predicate) => predicate(),
    pause: async () => {},
    shellExited: async () => true,
    stopDaemonService: async () => { calls.push('stop-service'); },
    clearUpdateFence: async () => { calls.push('clear-fence'); return true; },
    daemonDown: async () => true,
    stopOldShell: async () => 0,
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

function verifyReleaseMarker(transaction, root, version) {
  const expected = version === transaction.currentVersion ? 'old-release' : 'new-release';
  assert.equal(fs.readFileSync(path.join(root, 'release.txt'), 'utf8'), expected);
}

function writeRecoveryJournal(transaction, overrides = {}) {
  const journal = {
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    phase: 'armed',
    shellStopped: true,
    daemonStopped: true,
    incomingVerified: true,
    mutableStateSnapshotted: false,
    activated: false,
    healthVerified: false,
    rollbackStarted: false,
    rolledBack: false,
    mutableStateRestored: false,
    terminal: false,
    createdAt: new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
  fs.writeFileSync(transaction.journalPath, `${JSON.stringify(journal)}\n`);
  return journal;
}

function writeWindowsBackupReceipt(transaction) {
  fs.writeFileSync(path.join(transaction.transactionRoot, 'installed-before-activation.complete.json'), `${JSON.stringify({
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    version: transaction.currentVersion,
    source: transaction.installTarget,
    backup: transaction.backupTarget,
    mirrorCompleted: true,
    completedAt: new Date().toISOString(),
  })}\n`);
}

test('the released schema-3 updater accepts the current portable artifact shape for the bootstrap transition', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-released-updater-'));
  const releasedWorker = path.join(root, 'released-update-worker.js');
  const source = execFileSync('git', [
    'show', `${RELEASED_UPDATER_COMMIT}:desktop/shell/update-worker.js`,
  ], { cwd: path.resolve(__dirname, '..', '..'), encoding: 'utf8' });
  fs.writeFileSync(releasedWorker, source);
  const released = require(releasedWorker);
  const modern = windowsTransactionFixture();
  const transaction = { ...modern, schemaVersion: 3 };
  for (const field of [
    'progressId', 'workerPath', 'workerRuntimePath', 'progressModulePath',
    'progressReceiptPath', 'progressExecutable',
  ]) delete transaction[field];
  writeWindowsRelease(transaction.incomingTarget, transaction.targetVersion, transaction.installationId);
  assert.equal(fs.statSync(path.join(transaction.incomingTarget, 'resources', 'app', 'update-progress.js')).isFile(), true);
  assert.equal(released.validateTransaction(transaction), transaction);
  const ops = operations({
    verifyIncoming: async () => {
      released.verifyWindowsIncoming(transaction);
      ops.calls.push('verify');
    },
  });
  const receipt = await released.runTransaction(transaction, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.installedVersion, transaction.targetVersion);
  assert.ok(ops.calls.indexOf('verify') < ops.calls.indexOf('activate'));
});

test('transaction validation keeps macOS swap paths beside the app and Windows release paths inside its transaction', () => {
  const tx = transactionFixture();
  assert.equal(validateTransaction(tx), tx);
  assert.throws(() => validateTransaction({ ...tx, requireVisibleWindow: false }), /visible shell window/);
  assert.throws(() => validateTransaction({
    ...tx,
    incomingTarget: tx.installTarget,
    backupTarget: tx.installTarget,
    progressExecutable: path.join(tx.installTarget, 'Contents', 'MacOS', 'Workass'),
    recoveryAttempt: 1,
  }), /must be distinct/);
  assert.throws(() => validateTransaction({
    ...tx,
    incomingTarget: path.join(path.dirname(tx.installTarget), `.Workass.app.incoming-wrong-${tx.updateId}`),
    progressExecutable: path.join(path.dirname(tx.installTarget), `.Workass.app.incoming-wrong-${tx.updateId}`, 'Contents', 'MacOS', 'Workass'),
  }), /share one parent/);
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

test('worker entrypoint is bound to the exact copied worker and transaction paths', () => {
  const tx = transactionFixture();
  const transactionPath = path.join(tx.transactionRoot, 'transaction.json');
  const workerPath = path.join(tx.transactionRoot, 'update-worker.js');
  assert.doesNotThrow(() => validateWorkerEntrypoint(transactionPath, tx, workerPath));
  assert.throws(() => validateWorkerEntrypoint(path.join(path.dirname(tx.transactionRoot), 'transaction.json'), tx, workerPath), /transaction path/);
  assert.throws(() => validateWorkerEntrypoint(transactionPath, tx, __filename), /does not belong/);
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

test('Windows shell cleanup targets every process from only the exact portable executable', () => {
  const executable = path.join(path.sep, 'Apps', 'Workass', 'Workass.exe');
  let invocation = null;
  const stopped = stopWindowsExecutableProcesses(executable, {
    run: (command, args, options) => {
      invocation = { command, args, options };
      return { status: 0, stdout: 'WORKASS_STOPPED=4\r\n' };
    },
  });
  assert.equal(stopped, 4);
  assert.equal(invocation.command, 'powershell.exe');
  assert.deepEqual(invocation.args.slice(0, 3), ['-NoProfile', '-NonInteractive', '-Command']);
  assert.match(invocation.args[3], /Get-Process -Name Workass/);
  assert.match(invocation.args[3], /OrdinalIgnoreCase/);
  assert.equal(invocation.options.env.WORKASS_OLD_EXECUTABLE, executable);
  assert.throws(() => stopWindowsExecutableProcesses(executable, {
    run: () => ({ status: 0, stdout: '' }),
  }), /no process receipt/);
});

test('daemon exit requires exact OS process or service evidence, never a silent health probe', () => {
  const windows = windowsTransactionFixture();
  let invocation = null;
  assert.equal(daemonServiceIsDown(windows, {
    run: (...args) => { invocation = args; return { status: 0, stdout: 'WORKASS_DAEMON_DOWN\r\n' }; },
  }), true);
  assert.equal(invocation[2].env.WORKASS_DAEMON_EXECUTABLE, path.join(windows.installTarget, 'workass-daemon.exe'));
  assert.equal(daemonServiceIsDown(windows, {
    run: () => ({ status: 3, stdout: 'WORKASS_DAEMON_RUNNING\r\n' }),
  }), false);
  assert.equal(daemonServiceIsDown(windows, {
    run: () => ({ status: 0, stdout: '', error: new Error('WMI unavailable') }),
  }), false);

  const mac = transactionFixture();
  mac.launchAgentPath = '/Users/test/Library/LaunchAgents/com.workass.daemon.plist';
  mac.launchdDomain = 'gui/501';
  assert.equal(daemonServiceIsDown(mac, {
    run: () => ({ status: 113, stderr: 'Could not find service' }),
  }), true);
  assert.equal(daemonServiceIsDown(mac, {
    run: () => ({ status: 0, stdout: 'pid = 9123' }),
  }), false);
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
  const disk = defaultOperations(tx, {
    mirrorWindowsDirectory: mirror,
    verifyWindowsTree: verifyReleaseMarker,
  });
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
  const disk = defaultOperations(tx, {
    mirrorWindowsDirectory: mirror,
    verifyWindowsTree: verifyReleaseMarker,
  });
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

test('Windows rebuilds an interrupted backup mirror only while the exact old install is intact', async () => {
  const tx = windowsTransactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.installTarget, 'only-in-old.txt'), 'must-survive');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  let backupAttempts = 0;
  const interrupted = defaultOperations(tx, {
    verifyWindowsTree: verifyReleaseMarker,
    mirrorWindowsDirectory: (source, destination) => {
      if (source === tx.installTarget && destination === tx.backupTarget) {
        backupAttempts += 1;
        fs.mkdirSync(destination, { recursive: true });
        fs.writeFileSync(path.join(destination, 'release.txt'), 'old-release');
        throw new Error('simulated crash during backup mirror');
      }
      throw new Error('target mutation must not start without a committed backup');
    },
  });
  await assert.rejects(() => interrupted.activate(), /simulated crash/);
  assert.equal(fs.existsSync(path.join(tx.transactionRoot, 'installed-before-activation.complete.json')), false);
  assert.equal(fs.existsSync(path.join(tx.backupTarget, 'only-in-old.txt')), false);

  const mirrors = [];
  const resumed = defaultOperations(tx, {
    verifyWindowsTree: verifyReleaseMarker,
    mirrorWindowsDirectory: (source, destination) => {
      mirrors.push([source, destination]);
      fs.rmSync(destination, { recursive: true, force: true });
      fs.cpSync(source, destination, { recursive: true });
    },
  });
  await resumed.activate();
  assert.equal(backupAttempts, 1);
  assert.deepEqual(mirrors, [
    [tx.installTarget, tx.backupTarget],
    [tx.incomingTarget, tx.installTarget],
  ]);
  assert.equal(fs.readFileSync(path.join(tx.backupTarget, 'only-in-old.txt'), 'utf8'), 'must-survive');
  assert.equal(JSON.parse(fs.readFileSync(
    path.join(tx.transactionRoot, 'installed-before-activation.complete.json'), 'utf8',
  )).mirrorCompleted, true);
});

test('macOS resumes after activation completed before its checkpoint without replacing the verified backup', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.backupTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'new-release');
  fs.writeFileSync(path.join(tx.backupTarget, 'release.txt'), 'old-release');
  writeRecoveryJournal(tx, {
    phase: 'activating',
    mutableStateSnapshotted: true,
    activationStarted: true,
  });
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
  let activationReconciliations = 0;
  const ops = operations({
    activate: async () => { activationReconciliations += 1; await disk.activate(); },
    cleanup: disk.cleanup,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(activationReconciliations, 1);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'new-release');
  assert.equal(fs.existsSync(tx.backupTarget), false);
  assert.doesNotMatch(ops.calls.join(','), /rollback|restore-state/);
});

test('Windows resumes after activation mirror completed before its checkpoint without overwriting the old backup', async () => {
  const tx = windowsTransactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.mkdirSync(tx.backupTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'new-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  fs.writeFileSync(path.join(tx.backupTarget, 'release.txt'), 'old-release');
  writeWindowsBackupReceipt(tx);
  writeRecoveryJournal(tx, {
    phase: 'activating',
    mutableStateSnapshotted: true,
    activationStarted: true,
  });
  const mirrors = [];
  const mirror = (source, destination) => {
    mirrors.push([source, destination]);
    fs.rmSync(destination, { recursive: true, force: true });
    fs.cpSync(source, destination, { recursive: true });
  };
  const disk = defaultOperations(tx, {
    mirrorWindowsDirectory: mirror,
    verifyWindowsTree: verifyReleaseMarker,
  });
  const ops = operations({ activate: disk.activate, cleanup: disk.cleanup });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.deepEqual(mirrors, [[tx.incomingTarget, tx.installTarget]]);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'new-release');
  assert.equal(fs.existsSync(tx.backupTarget), false);
});

test('macOS marks a verified backup rollback-ready when a post-rename target is corrupt', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.backupTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'partial-release');
  fs.writeFileSync(path.join(tx.backupTarget, 'release.txt'), 'old-release');
  writeRecoveryJournal(tx, {
    phase: 'activating',
    mutableStateSnapshotted: true,
    activationStarted: true,
  });
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
  const ops = operations({
    activate: disk.activate,
    rollback: disk.rollback,
    restoreMutableState: async () => {},
    cleanupFailed: disk.cleanupFailed,
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
});

test('a completed mutable-state snapshot is reconciled when its checkpoint was lost', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.mutableStateTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'), '{"v":1}\n');
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
  await disk.snapshotMutableState();
  writeRecoveryJournal(tx, { phase: 'snapshotting_state', snapshotStarted: true });
  let snapshotReconciliations = 0;
  const ops = operations({
    snapshotMutableState: async () => { snapshotReconciliations += 1; await disk.snapshotMutableState(); },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(snapshotReconciliations, 1);
  assert.equal(fs.readFileSync(path.join(tx.mutableStateBackupTarget, 'state', 'provider-lanes.json'), 'utf8'), '{"v":1}\n');
});

test('a completed macOS rollback is reconciled when its checkpoint was lost', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  writeRecoveryJournal(tx, {
    phase: 'rollback_started',
    mutableStateSnapshotted: false,
    activated: true,
    rollbackStarted: true,
    rollbackReady: true,
    rolledBack: false,
    error: 'target health failed before worker exit',
  });
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
  const ops = operations({
    rollback: disk.rollback,
    cleanupFailed: disk.cleanupFailed,
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
});

test('a completed Windows rollback mirror is reconciled when its checkpoint was lost', async () => {
  const tx = windowsTransactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.mkdirSync(tx.incomingTarget, { recursive: true });
  fs.mkdirSync(tx.backupTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.writeFileSync(path.join(tx.incomingTarget, 'release.txt'), 'new-release');
  fs.writeFileSync(path.join(tx.backupTarget, 'release.txt'), 'old-release');
  writeWindowsBackupReceipt(tx);
  writeRecoveryJournal(tx, {
    phase: 'rollback_started',
    activated: true,
    rollbackStarted: true,
    rollbackReady: true,
    rolledBack: false,
    error: 'target health failed before worker exit',
  });
  const mirrors = [];
  const mirror = (source, destination) => {
    mirrors.push([source, destination]);
    fs.rmSync(destination, { recursive: true, force: true });
    fs.cpSync(source, destination, { recursive: true });
  };
  const disk = defaultOperations(tx, {
    mirrorWindowsDirectory: mirror,
    verifyWindowsTree: verifyReleaseMarker,
  });
  const ops = operations({
    rollback: disk.rollback,
    cleanupFailed: disk.cleanupFailed,
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.rolledBack, true);
  assert.deepEqual(mirrors, [[tx.backupTarget, tx.installTarget]]);
  assert.equal(fs.readFileSync(path.join(tx.installTarget, 'release.txt'), 'utf8'), 'old-release');
});

test('a completed mutable-state restore is reconciled when its checkpoint was lost', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'old-release');
  fs.mkdirSync(tx.mutableStateBackupTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.mutableStateBackupTarget, 'snapshot.json'), '{"schemaVersion":1,"existed":true}\n');
  fs.mkdirSync(tx.mutableStateTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'), '{"v":1}\n');
  fs.mkdirSync(tx.failedMutableStateTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.failedMutableStateTarget, 'provider-lanes.json'), '{"v":2}\n');
  writeRecoveryJournal(tx, {
    phase: 'restoring_state',
    mutableStateSnapshotted: true,
    activated: true,
    rollbackStarted: true,
    rollbackReady: true,
    rolledBack: true,
    stateRestoreStarted: true,
    mutableStateRestored: false,
    error: 'target health failed before worker exit',
  });
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
  let restorations = 0;
  const ops = operations({
    restoreMutableState: async () => { restorations += 1; await disk.restoreMutableState(); },
    cleanupFailed: disk.cleanupFailed,
    healthy: async (version) => version === tx.currentVersion,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'rollback_healthy');
  assert.equal(receipt.mutableStateRolledBack, true);
  assert.equal(restorations, 1);
  assert.equal(fs.readFileSync(path.join(tx.mutableStateTarget, 'provider-lanes.json'), 'utf8'), '{"v":1}\n');
});

test('a health-verified target finishes after cleanup/checkpoint loss without a second rollback decision', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(tx.installTarget, { recursive: true });
  fs.writeFileSync(path.join(tx.installTarget, 'release.txt'), 'new-release');
  writeRecoveryJournal(tx, {
    phase: 'health_verified',
    mutableStateSnapshotted: true,
    activated: true,
    healthVerified: true,
  });
  const ops = operations({
    healthy: async () => { ops.calls.push('unexpected-health'); return false; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.deepEqual(ops.calls, ['cleanup']);
});

test('a terminal journal reconstructs a missing receipt without repeating any update effect', async () => {
  const tx = transactionFixture();
  writeRecoveryJournal(tx, {
    phase: 'healthy',
    mutableStateSnapshotted: true,
    activated: true,
    healthVerified: true,
    installedVersion: tx.targetVersion,
    terminal: true,
  });
  const ops = operations({
    verifyIncoming: async () => { throw new Error('verification repeated after terminal checkpoint'); },
    activate: async () => { throw new Error('activation repeated after terminal checkpoint'); },
    rollback: async () => { throw new Error('rollback repeated after terminal checkpoint'); },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.installedVersion, tx.targetVersion);
  assert.equal(receipt.installationId, tx.installationId);
  assert.deepEqual(ops.calls, []);
});

test('activation health rejects a daemon that retained the previous bind mode', () => {
  const shell = { controller: true, appVersion: '1.1.0', catalog: { readyModelCount: 1 } };
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
    catalog: { readyModelCount: 1 },
  };
  assert.equal(runtimeIsHealthy({ daemon, shell, expectedVersion: '1.1.0' }), true);
  assert.equal(runtimeIsHealthy({ daemon, shell, expectedVersion: '1.1.0', requireVisibleWindow: true }), false);
  assert.equal(runtimeIsHealthy({ daemon, shell: { ...shell, windowVisible: true }, expectedVersion: '1.1.0', requireVisibleWindow: true }), true);
});

test('activation requires controller authority and at least one ready catalog model', () => {
  const daemon = { app: 'workass', version: '1.1.0' };
  const healthyShell = {
    controller: true,
    appVersion: '1.1.0',
    windowVisible: true,
    catalog: { readyModelCount: 1 },
  };
  assert.equal(runtimeIsHealthy({
    daemon, shell: healthyShell, expectedVersion: '1.1.0', requireVisibleWindow: true,
  }), true);
  assert.equal(runtimeIsHealthy({
    daemon, shell: { ...healthyShell, controller: false }, expectedVersion: '1.1.0', requireVisibleWindow: true,
  }), false);
  assert.equal(runtimeIsHealthy({
    daemon, shell: { ...healthyShell, catalog: { readyModelCount: 0 } }, expectedVersion: '1.1.0', requireVisibleWindow: true,
  }), false);
});

test('activation health rejects a same-version shell owned by another portable installation', () => {
  const tx = windowsTransactionFixture();
  const daemon = { app: 'workass', version: tx.targetVersion };
  const shell = {
    controller: true,
    appVersion: tx.targetVersion,
    windowVisible: true,
    catalog: { readyModelCount: 1 },
    installationId: tx.installationId,
    installTarget: tx.installTarget,
  };
  assert.equal(runtimeIsHealthy({
    daemon,
    shell,
    expectedVersion: tx.targetVersion,
    expectedInstallationId: tx.installationId,
    expectedInstallTarget: tx.installTarget,
    requireVisibleWindow: true,
  }), true);
  assert.equal(runtimeIsHealthy({
    daemon,
    shell: { ...shell, installationId: `install-${'f'.repeat(32)}` },
    expectedVersion: tx.targetVersion,
    expectedInstallationId: tx.installationId,
    expectedInstallTarget: tx.installTarget,
    requireVisibleWindow: true,
  }), false);
  assert.equal(runtimeIsHealthy({
    daemon,
    shell: { ...shell, installTarget: path.join(path.dirname(tx.installTarget), 'ForeignWorkass') },
    expectedVersion: tx.targetVersion,
    expectedInstallationId: tx.installationId,
    expectedInstallTarget: tx.installTarget,
    requireVisibleWindow: true,
  }), false);
});

test('Windows worker requests idempotent loopback daemon shutdown before waiting for daemon exit', async () => {
  const tx = windowsTransactionFixture();
  const calls = [];
  const disk = defaultOperations(tx, {
    requestDaemonShutdown: async (url) => { calls.push(url); return true; },
  });
  await disk.stopDaemonService();
  assert.deepEqual(calls, [tx.daemonHealthURL]);
});

test('worker retries the exact update cancel when the first success response is lost', async () => {
  const tx = windowsTransactionFixture();
  const calls = [];
  const statuses = [0, 200];
  const disk = defaultOperations(tx, {
    requestUpdateCancel: async (url, updateId) => {
      calls.push({ url, updateId });
      return statuses.shift();
    },
  });
  assert.equal(await disk.clearUpdateFence(), true);
  assert.deepEqual(calls, [
    { url: tx.daemonHealthURL, updateId: tx.updateId },
    { url: tx.daemonHealthURL, updateId: tx.updateId },
  ]);
});

test('an exited relaunch child can never make rollback kill a reused PID', async () => {
  const tx = transactionFixture();
  const child = new EventEmitter();
  child.pid = 7331;
  child.exitCode = null;
  child.signalCode = null;
  child.unref = () => {};
  const killed = [];
  const disk = defaultOperations(tx, {
    spawnProcess: () => child,
    stopLaunchedProcessTree: async (pid) => { killed.push(pid); },
    processOwnsExecutable: () => true,
    requestDaemonShutdown: async () => true,
    daemonServiceIsDown: () => true,
  });
  const launched = disk.launchInstalled({ updateRelaunch: true });
  child.emit('spawn');
  await launched;
  child.exitCode = 0;
  child.emit('exit', 0, null);
  await disk.stopLaunched();
  assert.deepEqual(killed, []);
});

test('rollback cleanup uses the injected daemon shutdown boundary', async () => {
  const tx = transactionFixture();
  const calls = [];
  const disk = defaultOperations(tx, {
    requestDaemonShutdown: async (url) => { calls.push(url); return true; },
    daemonServiceIsDown: () => true,
  });
  await disk.stopLaunched();
  assert.deepEqual(calls, [tx.daemonHealthURL]);
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
    pause: async () => {},
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

test('health and daemon polling honor one wall-clock deadline even when every probe consumes its timeout', async () => {
  let clock = 0;
  const probeTimeouts = [];
  const healthy = await launchUntilHealthy({
    launchInstalled: async () => {},
    healthy: async (_version, timeoutMs) => { probeTimeouts.push(timeoutMs); clock += timeoutMs; return false; },
    pause: async (ms) => { clock += ms; },
  }, '1.1.0', {
    attempts: 1000,
    timeoutMs: 120000,
    delayMs: 250,
    now: () => clock,
  });
  assert.equal(healthy, false);
  assert.ok(clock <= 120000);
  assert.ok(probeTimeouts.every((timeout) => timeout > 0 && timeout <= 1500));

  clock = 0;
  const daemonTimeouts = [];
  const down = await waitUntilDeadline(async (timeoutMs) => {
    daemonTimeouts.push(timeoutMs);
    clock += timeoutMs;
    return false;
  }, {
    timeoutMs: 30000,
    maxProbeMs: 600,
    delayMs: 250,
    now: () => clock,
    pause: async (ms) => { clock += ms; },
  });
  assert.equal(down, false);
  assert.ok(clock <= 30000);
  assert.ok(daemonTimeouts.every((timeout) => timeout > 0 && timeout <= 600));
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

test('the independent worker accepts the checksum-staged unsigned Windows portable tree without Authenticode', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-worker-windows-'));
  const incomingTarget = path.join(root, '.Workass.incoming-fixture');
  writeWindowsRelease(incomingTarget);
  assert.doesNotThrow(() => verifyWindowsIncoming({
    incomingTarget,
    targetVersion: '1.1.0',
    installationId: `install-${'1'.repeat(32)}`,
  }));
  const worker = fs.readFileSync(path.join(__dirname, 'update-worker.js'), 'utf8');
  assert.doesNotMatch(worker, /Get-AuthenticodeSignature/);
  assert.match(worker, /stopWindowsExecutableProcesses/);
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

test('progress loss before destructive work relaunches once and fails without an ownerless health wait', async () => {
  const tx = transactionFixture();
  const ops = operations({
    progressVisible: async () => false,
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'failed');
  assert.equal(receipt.activated, false);
  assert.equal(receipt.recoveryRelaunched, true);
  assert.match(receipt.error, /progress window stopped/);
  assert.deepEqual(ops.calls, ['start-runtime', 'launch']);
});

test('a crashed progress owner is replaced visibly before the update continues', async () => {
  const tx = transactionFixture();
  let visible = false;
  const ops = operations({
    progressVisible: async () => visible,
    replaceProgress: async () => {
      ops.calls.push('replace-progress');
      visible = true;
      return true;
    },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.deepEqual(ops.calls, [
    'replace-progress', 'stop-service', 'verify', 'snapshot-state', 'activate',
    'start-runtime', 'launch', 'healthy:1.1.0', 'cleanup',
  ]);
});

test('progress replacement fences its exact old tree and durably rotates ownership before spawn', async () => {
  const tx = transactionFixture();
  fs.mkdirSync(path.dirname(tx.progressExecutable), { recursive: true });
  fs.writeFileSync(tx.progressExecutable, 'app');
  fs.writeFileSync(path.join(tx.transactionRoot, 'transaction.json'), `${JSON.stringify(tx)}\n`);
  const prior = {
    schemaVersion: 1,
    updateId: tx.updateId,
    installationId: tx.installationId,
    workerId: tx.workerId,
    progressId: tx.progressId,
    pid: 9559,
    executablePath: tx.progressExecutable,
    transactionPath: path.join(tx.transactionRoot, 'transaction.json'),
    phase: 'watching',
    windowVisible: true,
    updatedAt: new Date().toISOString(),
  };
  fs.writeFileSync(tx.progressReceiptPath, `${JSON.stringify(prior)}\n`);
  const steps = [];
  const replaced = await replaceVisibleProgress(tx, {
    inspectOwnership: () => ({ running: true, exact: true, pid: prior.pid }),
    terminateProgress: async (ownership) => { steps.push(`fence:${ownership.pid}`); return true; },
    createProgressId: () => `progress-${'9'.repeat(32)}`,
    spawnProgress: async (request) => {
      steps.push('spawn');
      assert.equal(request.command, tx.progressExecutable);
      assert.equal(request.transaction.progressId, `progress-${'9'.repeat(32)}`);
      assert.equal(request.options.detached, true);
      assert.equal(request.options.windowsHide, false);
      return { pid: 9669 };
    },
  });
  assert.equal(replaced, true);
  assert.deepEqual(steps, [`fence:${prior.pid}`, 'spawn']);
  const durable = JSON.parse(fs.readFileSync(path.join(tx.transactionRoot, 'transaction.json'), 'utf8'));
  assert.equal(durable.schemaVersion, 4);
  assert.equal(durable.progressId, `progress-${'9'.repeat(32)}`);
  assert.equal(durable.recoveryAttempt, 1);
  assert.equal(fs.existsSync(tx.progressReceiptPath), false);
});

test('progress replacement fails closed when a live receipt process is not the exact owner', async () => {
  const tx = transactionFixture();
  let spawned = false;
  const replaced = await replaceVisibleProgress(tx, {
    readReceipt: () => ({ pid: 9779 }),
    inspectOwnership: () => ({ running: true, exact: false, ambiguous: true, pid: 9779 }),
    spawnProgress: async () => { spawned = true; },
  });
  assert.equal(replaced, false);
  assert.equal(spawned, false);
});

test('progress loss after activation defers rollback until a visible recovery owner exists', async () => {
  const tx = transactionFixture();
  let progressChecks = 0;
  const ops = operations({
    progressVisible: async () => {
      progressChecks += 1;
      return progressChecks < 5;
    },
    healthy: async (version) => { ops.calls.push(`healthy:${version}`); return version === tx.currentVersion; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'activating');
  assert.equal(receipt.activated, true);
  assert.equal(receipt.recoveryOwnerUnavailable, true);
  assert.ok(progressChecks >= 5);
  assert.deepEqual(ops.calls, [
    'stop-service', 'verify', 'snapshot-state', 'activate',
  ]);
  const journal = JSON.parse(fs.readFileSync(tx.journalPath, 'utf8'));
  assert.equal(journal.phase, 'recovery_owner_unavailable');
  assert.equal(journal.terminal, false);
});

test('a proven healthy target wins the same probe in which progress closes', async () => {
  const ops = operations({
    healthy: async () => true,
  });
  let progressChecks = 0;
  const healthy = await launchUntilHealthy(ops, '1.2.0', {
    attempts: 1,
    timeoutMs: 1000,
    progressVisible: async () => { progressChecks += 1; return false; },
  });
  assert.equal(healthy, true);
  assert.equal(progressChecks, 0);
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
    stopOldShell: async () => { ops.calls.push('stop-old-shell'); forced = true; return 4; },
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

test('commit-never-arrived shell-stop failure stays nonterminal until exact daemon admission is cleared', async () => {
  const tx = windowsTransactionFixture();
  const ops = operations({
    shellExited: async () => false,
    stopOldShell: async () => { throw new Error('profile process is locked'); },
    daemonDown: async () => false,
    clearUpdateFence: async () => false,
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'activating');
  assert.match(receipt.error, /profile process is locked/);
  const journal = JSON.parse(fs.readFileSync(tx.journalPath, 'utf8'));
  assert.equal(journal.phase, 'admission_fence_pending');
  assert.equal(journal.terminal, false);
});

test('Windows sweeps orphaned install processes even after the old main PID already exited', async () => {
  const tx = windowsTransactionFixture();
  const ops = operations({
    shellExited: async () => true,
    stopOldShell: async () => { ops.calls.push('stop-old-shell'); return 3; },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'healthy');
  assert.equal(receipt.oldShellForced, true);
  assert.equal(ops.calls[0], 'stop-old-shell');
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
  assert.deepEqual(ops.calls.slice(0, 6), [
    'stop-service', 'verify', 'snapshot-state', 'activate', 'start-runtime', 'launch',
  ]);
  assert.ok(ops.calls.filter((call) => call === 'healthy:1.1.0').length > 0);
  const rollbackStart = ops.calls.indexOf('stop-launched');
  assert.deepEqual(ops.calls.slice(rollbackStart), [
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
  assert.deepEqual(ops.calls, ['stop-service', 'clear-fence', 'launch', 'healthy:1.0.0']);
});

test('daemon stop failure remains nonterminal while its exact update admission fence remains', async () => {
  const tx = transactionFixture();
  const ops = operations({
    daemonDown: async () => false,
    clearUpdateFence: async () => { ops.calls.push('clear-fence'); return false; },
    healthy: async () => { throw new Error('health must not run behind a prepared update drain'); },
  });
  const receipt = await runTransaction(tx, ops);
  assert.equal(receipt.phase, 'activating');
  assert.match(receipt.error, /admission fence did not clear/);
  const journal = JSON.parse(fs.readFileSync(tx.journalPath, 'utf8'));
  assert.equal(journal.phase, 'admission_fence_pending');
  assert.equal(journal.terminal, false);
  assert.deepEqual(ops.calls, ['stop-service', 'clear-fence']);
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
  const disk = defaultOperations(tx, { verifyMacTree: verifyReleaseMarker });
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
