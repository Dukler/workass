'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { Readable } = require('node:stream');
const test = require('node:test');
const {
  UpdateManager,
  atomicJSON,
  cheapWorkerLeaseOwnership,
  cleanupUpdateTransactions,
  compareVersions,
  copyLocalArtifact,
  createProgressPublisher,
  defaultFeedURL,
  downloadArtifact,
  ensureInstallationIdentity,
  extractArchive,
  fetchReleaseManifest,
  localFeedPath,
  parseVersion,
  prepareUpdateWorkerRuntime,
  receiptAppliesToInstalledVersion,
  resolveArtifactSource,
  resolveUpdateFeed,
  runUpdateStageWorker,
  runUpdateTransactionCleanupWorker,
  snapshotReleaseManifest,
  spawnArmedUpdateWorker,
  stageAndVerifyRelease,
  validateArchiveLinksBeforeExtraction,
  validateExtractedTree,
  validateReleaseManifest,
  verifyWindowsRelease,
  workerProcessOwnership,
} = require('./update-manager');

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

function writeWindowsRelease(root, version = '1.2.0') {
  const json = (relative, value) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, `${JSON.stringify(value)}\n`);
  };
  const regular = (relative) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, 'fixture');
  };
  json(['manifest.json'], { schemaVersion: 2, platform: 'windows', arch: 'amd64', version, portable: true, electron: true });
  json(['resources', 'app', 'package.json'], { version });
  for (const relative of [
    ['resources', 'app', 'update-manager.js'],
    ['resources', 'app', 'update-worker.js'],
    ['resources', 'app', 'update-lock-recovery.js'],
    ['resources', 'renderer', 'index.html'],
    ['frontier-hosts', 'windows-amd64', 'claude-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'codex-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'],
  ]) regular(relative);
  writeFakeWindowsPE(path.join(root, 'Workass.exe'));
  writeFakeWindowsPE(path.join(root, 'workass-daemon.exe'));
  writeFakeWindowsPE(path.join(root, 'node', 'windows-amd64', 'node.exe'));
}

function writeStoredZip(file, entries) {
  const crc32 = (bytes) => {
    let crc = 0xffffffff;
    for (const byte of bytes) {
      crc ^= byte;
      for (let bit = 0; bit < 8; bit += 1) crc = (crc >>> 1) ^ ((crc & 1) ? 0xedb88320 : 0);
    }
    return (crc ^ 0xffffffff) >>> 0;
  };
  const localParts = [];
  const centralParts = [];
  let localOffset = 0;
  for (const entry of entries) {
    const name = Buffer.from(entry.name, 'utf8');
    const data = Buffer.from(entry.data || '', 'utf8');
    const local = Buffer.alloc(30);
    local.writeUInt32LE(0x04034b50, 0);
    local.writeUInt16LE(20, 4);
    local.writeUInt16LE(0x800, 6);
    local.writeUInt16LE(0, 8);
    local.writeUInt32LE(crc32(data), 14);
    local.writeUInt32LE(data.length, 18);
    local.writeUInt32LE(data.length, 22);
    local.writeUInt16LE(name.length, 26);
    const central = Buffer.alloc(46);
    central.writeUInt32LE(0x02014b50, 0);
    central.writeUInt16LE((3 << 8) | 20, 4);
    central.writeUInt16LE(20, 6);
    central.writeUInt16LE(0x800, 8);
    central.writeUInt16LE(0, 10);
    central.writeUInt32LE(crc32(data), 16);
    central.writeUInt32LE(data.length, 20);
    central.writeUInt32LE(data.length, 24);
    central.writeUInt16LE(name.length, 28);
    central.writeUInt32LE(((entry.mode || 0o100644) << 16) >>> 0, 38);
    central.writeUInt32LE(localOffset, 42);
    localParts.push(local, name, data);
    centralParts.push(central, name);
    localOffset += local.length + name.length + data.length;
  }
  const centralDirectory = Buffer.concat(centralParts);
  const end = Buffer.alloc(22);
  end.writeUInt32LE(0x06054b50, 0);
  end.writeUInt16LE(entries.length, 8);
  end.writeUInt16LE(entries.length, 10);
  end.writeUInt32LE(centralDirectory.length, 12);
  end.writeUInt32LE(localOffset, 16);
  fs.writeFileSync(file, Buffer.concat([...localParts, centralDirectory, end]));
}

function fakeSystemNetwork(routes, requests = [], visited = []) {
  return (options) => {
    requests.push(options);
    const request = new EventEmitter();
    let aborted = false;
    let pendingRedirect = null;
    const dispatch = (url) => {
      if (aborted) return;
      visited.push(url);
      const route = routes.get(url);
      if (!route) { request.emit('error', new Error(`unexpected system-network URL: ${url}`)); return; }
      if ([301, 302, 303, 307, 308].includes(route.status)) {
        pendingRedirect = route.location;
        request.emit('redirect', route.status, 'GET', route.location, { location: [route.location] });
        if (pendingRedirect) {
          pendingRedirect = null;
          request.emit('error', new Error('Redirect was cancelled'));
        }
        return;
      }
      const response = Readable.from(route.body == null ? [] : [route.body]);
      response.statusCode = route.status;
      request.emit('response', response);
    };
    request.followRedirect = () => {
      if (!pendingRedirect) throw new Error('no pending redirect');
      const next = pendingRedirect;
      pendingRedirect = null;
      queueMicrotask(() => dispatch(next));
    };
    request.abort = () => {
      if (aborted) return;
      aborted = true;
      request.emit('abort');
      request.emit('close');
    };
    request.end = () => queueMicrotask(() => dispatch(options.url));
    return request;
  };
}

function manifest(overrides = {}) {
  return {
    schemaVersion: 1,
    product: 'Workass',
    bundleId: 'com.workass.app',
    version: '1.2.0',
    build: 12,
    platform: 'darwin',
    arch: 'arm64',
    designatedRequirement: 'identifier "com.workass.app" and certificate root = H"abc"',
    artifacts: {
      update: {
        name: 'Workass-1.2.0-darwin-arm64.zip',
        url: 'https://releases.example.test/Workass-1.2.0-darwin-arm64.zip',
        sha256: 'a'.repeat(64),
        size: 1024,
      },
    },
    ...overrides,
  };
}

function managerFixture({
  replies = [],
  platform = 'darwin',
  networkRequest = () => { throw new Error('fixture network request was not expected'); },
  currentVersion = '1.1.0',
  initialReceipt = null,
  dependencyOverrides = {},
  beforeInit = () => {},
  primeReady = true,
} = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-manager-'));
  const resourcesPath = platform === 'darwin'
    ? path.join(root, 'Applications', 'Workass.app', 'Contents', 'Resources')
    : path.join(root, 'Workass', 'resources');
  const executablePath = platform === 'darwin'
    ? path.join(root, 'Applications', 'Workass.app', 'Contents', 'MacOS', 'Workass')
    : path.join(root, 'Workass', 'Workass.exe');
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.mkdirSync(path.dirname(executablePath), { recursive: true });
  fs.writeFileSync(executablePath, 'app');
  const arch = platform === 'darwin' ? 'arm64' : 'x64';
  const nodePath = platform === 'darwin'
    ? path.join(resourcesPath, 'runtime', 'node', 'darwin-arm64', 'bin', 'node')
    : path.join(path.dirname(executablePath), 'node', 'windows-amd64', 'node.exe');
  fs.mkdirSync(path.dirname(nodePath), { recursive: true });
  if (platform === 'darwin') fs.symlinkSync(process.execPath, nodePath);
  else { fs.copyFileSync(process.execPath, nodePath); fs.chmodSync(nodePath, 0o700); }
  const calls = [];
  let quit = false;
  const manager = new UpdateManager({
    app: { getVersion: () => currentVersion },
    runtime: {
      dataRoot: path.join(root, 'data'), daemonURL: 'https://127.0.0.1:18788',
      stateDir: path.join(root, 'data', 'state'), viewPort: 8799,
      launchdLabel: 'com.workass.daemon.test',
    },
    resourcesPath,
    executablePath,
    platform,
    arch,
    isPackaged: true,
    quit: () => { quit = true; },
    deps: {
      ...(networkRequest ? { networkRequest } : {}),
      postLocalUpdate: async (_url, action) => {
        calls.push(action);
        return replies.shift() || { status: 500, body: {} };
      },
      spawn: (_exe, args) => {
        calls.push(`spawn:${path.basename(args[0])}`);
        const child = new EventEmitter();
        child.pid = 4242;
        child.unref = () => {};
        child.kill = () => {};
        queueMicrotask(() => {
          const transactionIndex = args.indexOf('--transaction');
          if (transactionIndex !== -1) {
            const transaction = JSON.parse(fs.readFileSync(args[transactionIndex + 1], 'utf8'));
            fs.mkdirSync(path.dirname(transaction.receiptPath), { recursive: true });
            fs.writeFileSync(transaction.receiptPath, `${JSON.stringify({
              schemaVersion: 2,
              updateId: transaction.updateId,
              phase: 'armed',
              previousVersion: transaction.currentVersion,
              targetVersion: transaction.targetVersion,
              installationId: transaction.installationId,
              installTarget: transaction.installTarget,
              workerId: transaction.workerId,
              workerPID: 4242,
            })}\n`);
          }
          child.emit('spawn');
        });
        return child;
      },
      schedule: (fn) => { fn(); return { unref() {} }; },
      cleanupUpdateTransactions: async () => ({ removed: 0, pruned: 0, retained: 0 }),
      prepareWorkerRuntime: async ({ transactionRoot, workerSource, nodeSource, platform: targetPlatform }) => {
        const workerPath = path.join(transactionRoot, 'update-worker.js');
        const preparedNodePath = targetPlatform === 'win32' ? path.join(transactionRoot, 'updater-node.exe') : nodeSource;
        await fs.promises.mkdir(transactionRoot, { recursive: true });
        await fs.promises.copyFile(workerSource, workerPath);
        if (targetPlatform === 'win32') await fs.promises.copyFile(nodeSource, preparedNodePath);
        return { workerPath, nodePath: preparedNodePath };
      },
      ...dependencyOverrides,
    },
  });
  const installTarget = platform === 'darwin' ? path.resolve(resourcesPath, '..', '..') : path.dirname(executablePath);
  const fixtureIdentity = ensureInstallationIdentity(installTarget, { platform });
  if (initialReceipt) {
    fs.mkdirSync(path.dirname(manager.receiptPath), { recursive: true });
    fs.writeFileSync(manager.receiptPath, `${JSON.stringify({
      schemaVersion: 2,
      installationId: fixtureIdentity.installationId,
      installTarget,
      ...initialReceipt,
    })}\n`);
  }
  beforeInit({
    manager,
    root,
    resourcesPath,
    executablePath,
    installTarget,
    installationIdentity: fixtureIdentity,
  });
  const initialState = manager.init();
  if (primeReady) {
    const release = snapshotReleaseManifest(manifest());
    manager.manifest = release;
    const updateId = 'upd-fixture-1234';
    const transactionRoot = path.join(root, 'data', 'updates', 'transactions', updateId);
    fs.mkdirSync(transactionRoot, { recursive: true });
    const workerPath = path.join(transactionRoot, 'update-worker.js');
    fs.writeFileSync(workerPath, 'worker');
    const preparedNodePath = platform === 'win32' ? path.join(transactionRoot, 'updater-node.exe') : nodePath;
    if (platform === 'win32') fs.copyFileSync(nodePath, preparedNodePath);
    manager.prepared = {
      updateId,
      transactionRoot,
      installTarget,
      incomingTarget: platform === 'darwin'
        ? path.join(path.dirname(installTarget), `.Workass.app.incoming-${updateId}`)
        : path.join(transactionRoot, 'incoming-release'),
      backupTarget: platform === 'darwin'
        ? path.join(path.dirname(installTarget), `.Workass.app.previous-${updateId}`)
        : path.join(transactionRoot, 'installed-before-activation'),
      designatedRequirement: platform === 'darwin' ? release.designatedRequirement : '',
      installationId: fixtureIdentity.installationId,
      targetVersion: release.version,
      artifact: release.artifacts.update,
      release,
      workerPath,
      nodePath: preparedNodePath,
    };
    manager.publish({ phase: 'ready', targetVersion: '1.2.0' });
  }
  return { manager, calls, didQuit: () => quit, initialState };
}

function seedRecoveryTransaction({ manager, installTarget, installationIdentity }, {
  updateId = 'upd-recovery-1234',
  workerId = `worker-${'7'.repeat(32)}`,
  journal = {},
} = {}) {
  const transactionRoot = path.join(manager.updateRoot, 'transactions', updateId);
  const incomingTarget = manager.platform === 'darwin'
    ? path.join(path.dirname(installTarget), `.Workass.app.incoming-${updateId}`)
    : path.join(transactionRoot, 'incoming-release');
  const backupTarget = manager.platform === 'darwin'
    ? path.join(path.dirname(installTarget), `.Workass.app.previous-${updateId}`)
    : path.join(transactionRoot, 'installed-before-activation');
  const transaction = {
    schemaVersion: 3,
    updateId,
    platform: manager.platform,
    currentVersion: '1.1.0',
    targetVersion: '1.2.0',
    shellPID: 4000,
    workerId,
    installationId: installationIdentity.installationId,
    transactionRoot,
    installTarget,
    incomingTarget,
    backupTarget,
    mutableStateTarget: manager.runtime.stateDir,
    mutableStateBackupTarget: path.join(transactionRoot, 'state-before-activation'),
    failedMutableStateTarget: path.join(transactionRoot, 'state-from-failed-activation'),
    receiptPath: manager.receiptPath,
    journalPath: path.join(transactionRoot, 'journal.json'),
    leasePath: path.join(transactionRoot, 'worker-lease.json'),
    daemonHealthURL: `${manager.runtime.daemonURL}/workass/health`,
    shellStatusURL: `http://127.0.0.1:${manager.runtime.viewPort}/__workass-shell/status`,
    requireVisibleWindow: true,
    designatedRequirement: manager.platform === 'darwin' ? manifest().designatedRequirement : '',
  };
  fs.mkdirSync(transactionRoot, { recursive: true });
  fs.writeFileSync(path.join(transactionRoot, 'update-worker.js'), 'worker');
  if (manager.platform === 'win32') fs.copyFileSync(process.execPath, path.join(transactionRoot, 'updater-node.exe'));
  fs.writeFileSync(transaction.journalPath, `${JSON.stringify({
    schemaVersion: 1,
    updateId,
    installationId: installationIdentity.installationId,
    installTarget,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    phase: 'activation_started',
    activationStarted: true,
    terminal: false,
    ...journal,
  })}\n`);
  fs.writeFileSync(path.join(transactionRoot, 'transaction.json'), `${JSON.stringify(transaction)}\n`);
  return transaction;
}

test('release versions are strict and monotonic without losing integer precision', () => {
  assert.deepEqual(parseVersion('1.2.3'), [1n, 2n, 3n]);
  assert.equal(parseVersion('01.2.3'), null);
  assert.equal(compareVersions('1.2.3', '1.3.0'), -1);
  assert.equal(compareVersions('2.0.0', '1.9.9'), 1);
  assert.equal(compareVersions('1.2.3', '1.2.3'), 0);
  assert.equal(compareVersions('9007199254740992.0.0', '9007199254740993.0.0'), -1);
});

test('an update receipt belongs only to the release version that can honestly report it', () => {
  const installTarget = path.join(os.tmpdir(), 'Workass-receipt-owner');
  const ownership = { installationId: `install-${'1'.repeat(32)}`, installTarget };
  const receipt = {
    schemaVersion: 2,
    ...ownership,
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
  };
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'rollback_healthy' }, '1.1.0', ownership), true);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'rollback_healthy' }, '1.2.0', ownership), false);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'healthy' }, '1.2.0', ownership), true);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'healthy' }, '1.1.0', ownership), false);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'activating' }, '1.1.0', ownership), true);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'activating' }, '1.2.0', ownership), true);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'failed' }, '9.9.9', ownership), false);
  assert.equal(receiptAppliesToInstalledVersion({ ...receipt, phase: 'failed' }, '1.1.0', {
    ...ownership, installationId: `install-${'2'.repeat(32)}`,
  }), false);
  assert.equal(receiptAppliesToInstalledVersion({
    schemaVersion: 2,
    ...ownership,
    phase: 'rollback_healthy',
    currentVersion: '1.1.0',
    targetVersion: '1.2.0',
  }, '1.1.0', ownership), false);
});

test('a freshly downloaded Windows target ignores the replaced copy rollback receipt', () => {
  const initialReceipt = {
    schemaVersion: 2,
    updateId: 'upd-prior-copy-1234',
    phase: 'rollback_healthy',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    updatedAt: new Date().toISOString(),
  };
  const { initialState } = managerFixture({
    platform: 'win32',
    currentVersion: '1.2.0',
    initialReceipt,
  });
  assert.equal(initialState.phase, 'idle');
  assert.equal(initialState.receipt, null);
  assert.equal(initialState.error, null);
});

test('fresh Windows portable extractions receive distinct persistent installation identities', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-identities-'));
  const firstRoot = path.join(root, 'first', 'Workass');
  const secondRoot = path.join(root, 'second', 'Workass');
  fs.mkdirSync(firstRoot, { recursive: true });
  fs.mkdirSync(secondRoot, { recursive: true });
  const first = ensureInstallationIdentity(firstRoot, { platform: 'win32' });
  const firstAgain = ensureInstallationIdentity(firstRoot, { platform: 'win32' });
  const second = ensureInstallationIdentity(secondRoot, { platform: 'win32' });
  assert.equal(firstAgain.installationId, first.installationId);
  assert.notEqual(second.installationId, first.installationId);

  const firstReceipt = {
    schemaVersion: 2,
    updateId: 'upd-first-copy-1234',
    phase: 'rollback_healthy',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    installationId: first.installationId,
    installTarget: firstRoot,
  };
  assert.equal(receiptAppliesToInstalledVersion(firstReceipt, '1.1.0', {
    installationId: second.installationId,
    installTarget: secondRoot,
  }), false);
});

test('Windows installation identity is flushed through its writable atomic file handle', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-identity-flush-'));
  const file = path.join(root, '.workass-installation.json');
  rejectReadOnlyFileFlushes(() => atomicJSON(file, {
    schemaVersion: 1,
    product: 'Workass',
    installationId: `install-${'7'.repeat(32)}`,
  }));
  assert.equal(JSON.parse(fs.readFileSync(file, 'utf8')).installationId, `install-${'7'.repeat(32)}`);
});

test('the actual rolled-back Windows release retains its own failure receipt', () => {
  const initialReceipt = {
    schemaVersion: 2,
    updateId: 'upd-current-copy-1234',
    phase: 'rollback_healthy',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    updatedAt: new Date().toISOString(),
  };
  const { initialState } = managerFixture({
    platform: 'win32',
    currentVersion: '1.1.0',
    initialReceipt,
  });
  assert.equal(initialState.phase, 'rollback_healthy');
  assert.equal(initialState.receipt.updateId, initialReceipt.updateId);
  assert.equal(initialState.receipt.installationId.startsWith('install-'), true);
  assert.equal(path.isAbsolute(initialState.receipt.installTarget), true);
});

test('terminal pruning removes updater payloads off-main, retries state cleanup, bounds logs, and retains only eight journals', async () => {
  const { manager } = managerFixture({ platform: 'win32', primeReady: false });
  await manager.transactionCleanupPromise;
  manager.deps.cleanupUpdateTransactions = async (request) => cleanupUpdateTransactions(request, {
    run: () => ({ status: 0, stdout: '[]' }),
  });
  const transaction = seedRecoveryTransaction({
    manager,
    installTarget: manager.installTarget,
    installationIdentity: manager.installationIdentity,
  }, {
    updateId: 'upd-terminal-prune-1234',
    workerId: `worker-${'e'.repeat(32)}`,
    journal: {
      phase: 'rollback_healthy', terminal: true, installedVersion: '1.1.0',
      activated: false, rollbackStarted: true, rolledBack: true,
      updatedAt: new Date().toISOString(),
    },
  });
  for (const target of [
    transaction.incomingTarget, transaction.backupTarget,
    transaction.mutableStateBackupTarget, transaction.failedMutableStateTarget,
    path.join(transaction.transactionRoot, 'extracted'),
  ]) {
    fs.mkdirSync(target, { recursive: true });
    fs.writeFileSync(path.join(target, 'payload.bin'), 'payload');
  }
  fs.writeFileSync(path.join(transaction.transactionRoot, 'release.zip'), 'archive');
  fs.writeFileSync(path.join(transaction.transactionRoot, 'release.zip.partial'), 'partial');
  fs.writeFileSync(path.join(transaction.transactionRoot, 'worker.log'), Buffer.alloc(1024 * 1024 + 4096, 7));
  const receipt = {
    schemaVersion: 2,
    updateId: transaction.updateId,
    phase: 'rollback_healthy',
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    installedVersion: transaction.currentVersion,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    workerId: transaction.workerId,
  };
  fs.writeFileSync(manager.receiptPath, `${JSON.stringify(receipt)}\n`);

  for (let index = 0; index < 9; index += 1) {
    const updateId = `upd-old-terminal-${String(index).padStart(4, '0')}`;
    const root = path.join(manager.updateRoot, 'transactions', updateId);
    const old = {
      ...transaction,
      updateId,
      workerId: `worker-${index.toString(16).padStart(32, '0')}`,
      transactionRoot: root,
      incomingTarget: path.join(root, 'incoming-release'),
      backupTarget: path.join(root, 'installed-before-activation'),
      mutableStateBackupTarget: path.join(root, 'state-before-activation'),
      failedMutableStateTarget: path.join(root, 'state-from-failed-activation'),
      journalPath: path.join(root, 'journal.json'),
      leasePath: path.join(root, 'worker-lease.json'),
    };
    fs.mkdirSync(root, { recursive: true });
    fs.writeFileSync(path.join(root, 'transaction.json'), `${JSON.stringify(old)}\n`);
    fs.writeFileSync(old.journalPath, `${JSON.stringify({
      schemaVersion: 1, updateId, installationId: old.installationId,
      installTarget: old.installTarget, previousVersion: old.currentVersion,
      targetVersion: old.targetVersion, phase: 'failed', terminal: true,
      installedVersion: old.currentVersion,
      updatedAt: new Date(Date.now() - (index + 1) * 1000).toISOString(),
    })}\n`);
    fs.writeFileSync(path.join(root, 'updater-node.exe'), 'old-node');
  }

  manager.pruneTerminalPayload(receipt);
  await manager.transactionCleanupPromise;
  for (const target of [
    transaction.incomingTarget, transaction.backupTarget,
    transaction.mutableStateBackupTarget, transaction.failedMutableStateTarget,
    path.join(transaction.transactionRoot, 'extracted'),
    path.join(transaction.transactionRoot, 'release.zip'),
    path.join(transaction.transactionRoot, 'release.zip.partial'),
    path.join(transaction.transactionRoot, 'updater-node.exe'),
  ]) assert.equal(fs.existsSync(target), false, target);
  assert.equal(fs.statSync(path.join(transaction.transactionRoot, 'worker.log')).size, 1024 * 1024);
  const retained = fs.readdirSync(path.join(manager.updateRoot, 'transactions'), { withFileTypes: true })
    .filter((entry) => entry.isDirectory());
  assert.equal(retained.length, 8);
  assert.equal(fs.existsSync(transaction.transactionRoot), true);
});

test('transaction cleanup reclaims inactive updater cache without touching live or recoverable work', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-cleanup-'));
  const transactionsRoot = path.join(root, 'updates', 'transactions');
  const seed = (updateId, schemaVersion = 2) => {
    const transactionRoot = path.join(transactionsRoot, updateId);
    fs.mkdirSync(path.join(transactionRoot, 'incoming-release'), { recursive: true });
    fs.writeFileSync(path.join(transactionRoot, 'incoming-release', 'payload.bin'), 'payload');
    fs.writeFileSync(path.join(transactionRoot, 'release.zip'), 'archive');
    fs.writeFileSync(path.join(transactionRoot, 'updater-node.exe'), 'node');
    fs.writeFileSync(path.join(transactionRoot, 'transaction.json'), `${JSON.stringify({
      schemaVersion, updateId, transactionRoot,
    })}\n`);
    fs.writeFileSync(path.join(transactionRoot, 'update-worker.js'), 'worker');
    return transactionRoot;
  };
  const obsoleteRoot = seed('upd-obsolete-cache-1234');
  const receiptRoot = seed('upd-current-receipt-1234');
  const activeRoot = seed('upd-active-worker-1234');
  const recoverableRoot = seed('upd-current-schema-1234', 3);
  const activeCommand = `${process.execPath} ${path.join(activeRoot, 'update-worker.js')} --transaction ${path.join(activeRoot, 'transaction.json')}`;

  const result = cleanupUpdateTransactions({
    transactionsRoot,
    platform: 'darwin',
    receipt: { updateId: 'upd-current-receipt-1234', phase: 'healthy' },
  }, {
    run: () => ({ status: 0, stdout: `${activeCommand}\n` }),
  });

  assert.deepEqual(result, { removed: 1, pruned: 1, retained: 3 });
  assert.equal(fs.existsSync(obsoleteRoot), false);
  assert.equal(fs.existsSync(receiptRoot), true);
  assert.equal(fs.existsSync(path.join(receiptRoot, 'release.zip')), false);
  assert.equal(fs.existsSync(path.join(receiptRoot, 'incoming-release')), false);
  assert.equal(fs.existsSync(path.join(activeRoot, 'release.zip')), true);
  assert.equal(fs.existsSync(path.join(recoverableRoot, 'release.zip')), true);
});

test('transaction cleanup fails closed when worker process ownership cannot be inspected', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-cleanup-fail-'));
  const transactionsRoot = path.join(root, 'updates', 'transactions');
  const transactionRoot = path.join(transactionsRoot, 'upd-uninspected-cache-1234');
  fs.mkdirSync(transactionRoot, { recursive: true });
  fs.writeFileSync(path.join(transactionRoot, 'transaction.json'), '{"schemaVersion":2}\n');

  const result = cleanupUpdateTransactions({ transactionsRoot, platform: 'win32' }, {
    run: () => ({ status: 1, stdout: '' }),
  });

  assert.deepEqual(result, { removed: 0, pruned: 0, retained: 1, inspectionFailed: true });
  assert.equal(fs.existsSync(transactionRoot), true);
});

test('packaged startup schedules transaction cleanup outside the shell thread', async () => {
  let cleanupRequest = null;
  const { manager } = managerFixture({
    primeReady: false,
    dependencyOverrides: {
      cleanupUpdateTransactions: async (request) => {
        cleanupRequest = request;
        return { removed: 0, pruned: 0, retained: 0 };
      },
    },
  });

  await manager.transactionCleanupPromise;
  assert.equal(cleanupRequest.transactionsRoot, path.join(manager.updateRoot, 'transactions'));
  assert.equal(cleanupRequest.platform, 'darwin');
});

test('the transaction cleanup worker reclaims cache without running deletion on Electron main', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-cleanup-thread-'));
  const transactionsRoot = path.join(root, 'updates', 'transactions');
  const transactionRoot = path.join(transactionsRoot, 'upd-thread-cache-1234');
  fs.mkdirSync(transactionRoot, { recursive: true });
  fs.writeFileSync(path.join(transactionRoot, 'transaction.json'), '{"schemaVersion":2}\n');

  const result = await runUpdateTransactionCleanupWorker({ transactionsRoot, platform: 'darwin' });

  assert.deepEqual(result, { removed: 1, pruned: 0, retained: 0 });
  assert.equal(fs.existsSync(transactionRoot), false);
});

test('platform feed names cannot collide in one GitHub release', () => {
  assert.equal(defaultFeedURL('darwin', 'arm64'), 'https://github.com/Dukler/workass/releases/latest/download/workass-darwin-arm64-release.json');
  assert.equal(defaultFeedURL('win32', 'x64'), 'https://github.com/Dukler/workass/releases/latest/download/workass-windows-amd64-release.json');
});

test('the Windows publisher marks the unsigned portable feed as updater-compatible', () => {
  const publisher = fs.readFileSync(path.join(__dirname, '..', '..', 'scripts', 'stage-windows-portable.sh'), 'utf8');
  assert.match(publisher, /platform:\s*'windows',[\s\S]{0,160}portable:\s*true,[\s\S]{0,240}authenticode:\s*false/);
  assert.match(publisher, /make-icon\.mjs["']? --verify/);
  assert.match(publisher, /stamp-windows-icon\.mjs["']? --exe ["']?\$stage\/Workass\.exe["']? --icon ["']?\$repo_root\/desktop\/assets\/icon\.ico/);
  assert.match(publisher, /stamp-windows-icon\.mjs["']? --verify --exe ["']?\$stage\/Workass\.exe["']? --icon ["']?\$repo_root\/desktop\/assets\/icon\.ico/);
  assert.match(publisher, /desktop\/assets\/icon\.ico["']? ["']?\$stage\/resources\/Workass\.ico/);
  assert.match(publisher, /for shell_file in[^\n]*update-lock-recovery\.js/);
  assert.match(publisher, /https:\/\/github\.com\/Dukler\/workass\/releases\/download\/v\$\{version\}\/\$\{artifactName\}/);
  const rendererBuild = publisher.indexOf('npm run build --prefix desktop/renderer2');
  const rendererSync = publisher.indexOf('scripts/sync-renderer2.sh');
  const daemonBuild = publisher.indexOf('CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build');
  assert.ok(rendererBuild >= 0 && rendererBuild < rendererSync && rendererSync < daemonBuild,
    'the current renderer must be embedded before the Windows daemon is cross-compiled');
  assert.doesNotMatch(publisher, /ineligible for automatic install/);
});

test('Windows updater uses Electron Chromium networking without weakening HTTPS verification', () => {
  const source = fs.readFileSync(path.join(__dirname, 'update-manager.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(main, /\bnet\b[\s\S]{0,120}require\('electron'\)/);
  assert.match(main, /deps:\s*\{\s*networkRequest:\s*\(options\)\s*=>\s*net\.request\(options\)\s*\}/);
  assert.doesNotMatch(source, /getCACertificates|NODE_TLS_REJECT_UNAUTHORIZED/);
  assert.doesNotMatch(source, /networkRequest[\s\S]{0,500}rejectUnauthorized:\s*false/);
});

test('packaged Windows keeps polling the immutable GitHub latest feed while the app remains open', async () => {
  const profile = fs.readFileSync(path.join(__dirname, '..', '..', 'config', 'environments', 'windows-prod.env'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(profile, /^WORKASS_UPDATE_CHANNEL=github$/m);
  assert.match(main, /startAutoChecks\(\{[\s\S]{0,240}intervalMs:\s*30_000/);
  assert.doesNotMatch(main, /60\s*\*\s*60\s*\*\s*1000/);

  const { manager } = managerFixture({ platform: 'win32', arch: 'x64' });
  const scheduled = [];
  const repeated = [];
  const releases = [
    manifest({ version: '1.1.0', platform: 'windows', arch: 'amd64', portable: true }),
    manifest({ version: '1.2.0', platform: 'windows', arch: 'amd64', portable: true }),
  ];
  manager.deps.fetchManifest = async () => releases.shift();
  manager.deps.schedule = (fn, delay) => { const handle = { fn, delay, unref() {} }; scheduled.push(handle); return handle; };
  manager.deps.repeat = (fn, delay) => { const handle = { fn, delay, unref() {} }; repeated.push(handle); return handle; };
  manager.publish({ phase: 'current', targetVersion: null, error: null });

  manager.startAutoChecks({ initialDelayMs: 15_000, intervalMs: 30_000 });
  assert.equal(scheduled[0].delay, 15_000);
  assert.equal(repeated[0].delay, 30_000);
  assert.equal((await scheduled[0].fn()).phase, 'current');
  const discovered = await repeated[0].fn();
  assert.equal(discovered.phase, 'available');
  assert.equal(discovered.targetVersion, '1.2.0');
  manager.dispose();
});

test('system-network updater follows only HTTPS redirects and preserves size and SHA-256 verification', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-system-network-'));
  const destination = path.join(root, 'release.zip');
  const archive = Buffer.from('verified portable archive');
  const sha256 = require('node:crypto').createHash('sha256').update(archive).digest('hex');
  const release = manifest({
    platform: 'windows',
    arch: 'amd64',
    designatedRequirement: undefined,
    portable: true,
    authenticode: false,
    artifacts: { update: { name: 'release.zip', url: 'https://downloads.example.test/release.zip', sha256, size: archive.length } },
  });
  const requests = [];
  const visited = [];
  const routes = new Map([
    ['https://updates.example.test/latest.json', { status: 302, location: 'https://downloads.example.test/release.json' }],
    ['https://downloads.example.test/release.json', { status: 200, body: Buffer.from(JSON.stringify(release)) }],
    ['https://downloads.example.test/release.zip', { status: 200, body: archive }],
  ]);
  const networkRequest = fakeSystemNetwork(routes, requests, visited);

  assert.equal((await fetchReleaseManifest('https://updates.example.test/latest.json', { networkRequest })).version, '1.2.0');
  let progressed = 0;
  await downloadArtifact(release.artifacts.update.url, destination, release.artifacts.update, {
    networkRequest,
    onProgress: (received) => { progressed = received; },
  });
  assert.deepEqual(fs.readFileSync(destination), archive);
  assert.equal(progressed, archive.length);
  assert.deepEqual(visited, [
    'https://updates.example.test/latest.json',
    'https://downloads.example.test/release.json',
    'https://downloads.example.test/release.zip',
  ]);
  for (const options of requests) {
    assert.equal(options.redirect, 'manual');
    assert.equal(options.method, 'GET');
    assert.equal(options.cache, 'no-store');
    assert.equal(options.headers['cache-control'], 'no-cache, no-store');
    assert.equal(options.headers.pragma, 'no-cache');
  }
  await assert.rejects(
    () => fetchReleaseManifest('http://updates.example.test/latest.json', { networkRequest }),
    /updates require HTTPS/,
  );
  const downgradeRequest = fakeSystemNetwork(new Map([
    ['https://updates.example.test/downgrade.json', { status: 302, location: 'http://downloads.example.test/release.json' }],
  ]));
  await assert.rejects(
    () => fetchReleaseManifest('https://updates.example.test/downgrade.json', {
      networkRequest: downgradeRequest,
    }),
    /updates require HTTPS/,
  );
  const invalidRedirectRequest = fakeSystemNetwork(new Map([
    ['https://updates.example.test/invalid-redirect.json', { status: 302, location: 'https://%' }],
  ]));
  await assert.rejects(
    () => fetchReleaseManifest('https://updates.example.test/invalid-redirect.json', {
      networkRequest: invalidRedirectRequest,
    }),
    /invalid update redirect/,
  );
  const badDestination = path.join(root, 'bad.zip');
  await assert.rejects(
    () => downloadArtifact(release.artifacts.update.url, badDestination, {
      ...release.artifacts.update,
      sha256: '0'.repeat(64),
    }, { networkRequest }),
    /checksum/,
  );
  assert.equal(fs.existsSync(badDestination), false);
  assert.equal(fs.existsSync(`${badDestination}.partial`), false);
});

test('the renderer has one owned apply IPC for the complete update intent', () => {
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(preload, /apply:\s*\(\)\s*=>\s*ipcRenderer\.invoke\('workass-updater:apply'\)/);
  assert.match(main, /ipcMain\.handle\('workass-updater:apply',[\s\S]{0,160}own\(event\)[\s\S]{0,160}updateManager\?\.startApply\(\)/);
  assert.doesNotMatch(preload, /workass-updater:(?:download|install)/);
  assert.doesNotMatch(main, /workass-updater:(?:download|install)/);
});

test('macOS dogfood resolves one stable local feed while public builds stay on GitHub', () => {
  const dataRoot = '/Users/test/Library/Application Support/Workass';
  assert.equal(localFeedPath(dataRoot, 'darwin', 'arm64'), path.join(dataRoot, 'update-feed', 'workass-darwin-arm64-release.json'));
  assert.equal(resolveUpdateFeed({ channel: 'local', dataRoot, platform: 'darwin', arch: 'arm64' }), localFeedPath(dataRoot, 'darwin', 'arm64'));
  assert.equal(resolveUpdateFeed({ channel: 'github', dataRoot, platform: 'darwin', arch: 'arm64' }), defaultFeedURL('darwin', 'arm64'));
  assert.throws(() => resolveUpdateFeed({ channel: 'local', dataRoot: 'C:\\Workass', platform: 'win32', arch: 'x64' }), /only on macOS/);
});

test('local feed reads and copies are bounded by the same manifest checksum', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-local-feed-'));
  const feed = path.join(root, 'workass-darwin-arm64-release.json');
  const artifactName = 'Workass-1.2.0-darwin-arm64.zip';
  const source = path.join(root, artifactName);
  const bytes = Buffer.from('local-update-fixture');
  const sha256 = require('node:crypto').createHash('sha256').update(bytes).digest('hex');
  const localManifest = manifest({ artifacts: { update: { name: artifactName, url: artifactName, sha256, size: bytes.length } } });
  fs.writeFileSync(feed, `${JSON.stringify(localManifest)}\n`);
  fs.writeFileSync(source, bytes);
  assert.equal((await fetchReleaseManifest(feed)).version, '1.2.0');
  assert.equal(resolveArtifactSource(artifactName, feed), source);
  assert.throws(() => resolveArtifactSource('../outside.zip', feed), /name is invalid/);
  const destination = path.join(root, 'transaction', 'release.zip');
  let progress = 0;
  await copyLocalArtifact(source, destination, localManifest.artifacts.update, { onProgress: (received) => { progress = received; } });
  assert.deepEqual(fs.readFileSync(destination), bytes);
  assert.equal(progress, bytes.length);
  await assert.rejects(() => copyLocalArtifact(source, path.join(root, 'bad', 'release.zip'), { ...localManifest.artifacts.update, sha256: '0'.repeat(64) }), /checksum/);
});

test('artifact progress is coalesced to bounded meaningful updates and always publishes completion', () => {
  const progress = [];
  const report = createProgressPublisher((value) => progress.push(value), { now: () => 0 });
  for (let received = 1; received <= 10000; received += 1) report(received, 10000);
  assert.ok(progress.length <= 102, `published ${progress.length} progress events`);
  assert.equal(progress.at(-1), 1);
  assert.ok(progress.every((value, index) => index === 0 || value > progress[index - 1]));
});

test('update worker runtime copy and self-test finish asynchronously before a release becomes ready', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-worker-prep-'));
  const workerSource = path.join(root, 'source-worker.js');
  const nodeSource = path.join(root, 'source-node.exe');
  const transactionRoot = path.join(root, 'transaction');
  fs.writeFileSync(workerSource, 'worker');
  fs.writeFileSync(nodeSource, 'node');
  const child = new EventEmitter();
  let invocation = null;
  let settled = false;
  const preparing = prepareUpdateWorkerRuntime({
    transactionRoot, workerSource, nodeSource, platform: 'win32',
  }, {
    spawnProcess: (command, args, options) => { invocation = { command, args, options }; return child; },
  });
  void preparing.then(() => { settled = true; });
  while (!invocation) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(settled, false);
  child.emit('exit', 0, null);
  const runtime = await preparing;
  assert.equal(invocation.command, path.join(transactionRoot, 'updater-node.exe'));
  assert.deepEqual(invocation.args, [path.join(transactionRoot, 'update-worker.js'), '--self-test']);
  assert.deepEqual(fs.readFileSync(runtime.workerPath, 'utf8'), 'worker');
  assert.deepEqual(fs.readFileSync(runtime.nodePath, 'utf8'), 'node');
});

test('the manager checks and stages a local Mac release without any network request', async () => {
  const { manager } = managerFixture({ primeReady: false });
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-local-manager-'));
  const feed = path.join(root, 'workass-darwin-arm64-release.json');
  const artifactName = 'Workass-1.2.0-darwin-arm64.zip';
  const bytes = Buffer.from('local-manager-archive');
  const sha256 = require('node:crypto').createHash('sha256').update(bytes).digest('hex');
  const localManifest = manifest({ artifacts: { update: { name: artifactName, url: artifactName, sha256, size: bytes.length } } });
  fs.writeFileSync(feed, `${JSON.stringify(localManifest)}\n`);
  fs.writeFileSync(path.join(root, artifactName), bytes);
  manager.feedURL = feed;
  manager.deps.stageAndVerify = async (request) => {
    fs.mkdirSync(request.incomingTarget, { recursive: true });
    return { designatedRequirement: localManifest.designatedRequirement };
  };
  assert.equal((await manager.check()).phase, 'available');
  const state = await manager.download();
  assert.equal(state.phase, 'ready');
  assert.equal(manager.prepared.designatedRequirement, localManifest.designatedRequirement);
  assert.deepEqual(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'release.zip')), bytes);
});

test('the Windows manager stages both release trees inside the external update transaction', async () => {
  const { manager } = managerFixture({ platform: 'win32' });
  manager.manifest = manifest({
    platform: 'windows',
    arch: 'amd64',
    portable: true,
    authenticode: false,
    designatedRequirement: undefined,
    artifacts: {
      update: {
        name: 'Workass-1.2.0-windows-amd64.zip',
        url: 'https://releases.example.test/Workass-1.2.0-windows-amd64.zip',
        sha256: 'b'.repeat(64),
        size: 1024,
      },
    },
  });
  manager.publish({ phase: 'available', targetVersion: '1.2.0' });
  manager.deps.downloadArtifact = async (_source, destination) => {
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.writeFileSync(destination, 'archive');
  };
  manager.deps.stageAndVerify = async (request) => {
    writeWindowsRelease(request.incomingTarget);
    return { designatedRequirement: '' };
  };

  const state = await manager.download();
  assert.equal(state.phase, 'ready');
  assert.equal(manager.prepared.incomingTarget, path.join(manager.prepared.transactionRoot, 'incoming-release'));
  assert.equal(manager.prepared.backupTarget, path.join(manager.prepared.transactionRoot, 'installed-before-activation'));
  assert.notEqual(path.dirname(manager.prepared.incomingTarget), path.dirname(manager.prepared.installTarget));
});

test('pre-worker staging failure removes every exact temporary payload on Mac and Windows', async () => {
  for (const platform of ['darwin', 'win32']) {
    const { manager } = managerFixture({ platform, primeReady: false });
    const release = platform === 'win32' ? manifest({
      platform: 'windows', arch: 'amd64', portable: true, designatedRequirement: undefined,
    }) : manifest();
    manager.manifest = snapshotReleaseManifest(release);
    manager.publish({ phase: 'available', targetVersion: release.version });
    let staging = null;
    manager.deps.downloadArtifact = async (_source, destination) => {
      fs.mkdirSync(path.dirname(destination), { recursive: true });
      fs.writeFileSync(destination, 'archive');
    };
    manager.deps.stageAndVerify = async (request) => {
      staging = request;
      fs.mkdirSync(request.extracted, { recursive: true });
      fs.writeFileSync(path.join(request.extracted, 'partial'), 'partial');
      fs.mkdirSync(request.incomingTarget, { recursive: true });
      fs.writeFileSync(path.join(request.incomingTarget, 'partial'), 'partial');
      throw new Error('fixture staging failure');
    };
    const state = await manager.download();
    assert.equal(state.phase, 'failed');
    assert.match(state.error, /fixture staging failure/);
    assert.equal(fs.existsSync(staging.transactionRoot), false);
    assert.equal(fs.existsSync(staging.incomingTarget), false);
    assert.equal(fs.existsSync(staging.currentRoot), true);
  }
});

test('archive extraction, staging, and verification form one bounded worker task', () => {
  const transactionRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-stage-task-'));
  const currentRoot = path.join(transactionRoot, 'installed');
  const installationId = `install-${'3'.repeat(32)}`;
  fs.mkdirSync(currentRoot, { recursive: true });
  fs.writeFileSync(path.join(currentRoot, '.workass-installation.json'), `${JSON.stringify({
    schemaVersion: 1, product: 'Workass', installationId,
  })}\n`);
  const request = {
    schemaVersion: 1,
    transactionRoot,
    archive: path.join(transactionRoot, 'release.zip'),
    extracted: path.join(transactionRoot, 'extracted'),
    currentRoot,
    incomingTarget: path.join(transactionRoot, 'incoming-release'),
    targetVersion: '1.2.0',
    platform: 'win32',
    arch: 'x64',
    installationId,
  };
  const calls = [];
  const result = stageAndVerifyRelease(request, {
    extractArchive: (archive, extracted, platform) => { calls.push(['extract', archive, extracted, platform]); },
    validateExtractedTree: (extracted, platform) => { calls.push(['validate-tree', extracted, platform]); },
    findExtractedRoot: (extracted, platform) => { calls.push(['find', extracted, platform]); return path.join(extracted, 'Workass'); },
    stageRelease: (source, incoming, platform) => {
      calls.push(['stage', source, incoming, platform]);
      fs.mkdirSync(incoming, { recursive: true });
    },
    verifyRelease: (current, incoming, version, platform, arch, identity) => {
      calls.push(['verify', current, incoming, version, platform, arch, identity]);
      return '';
    },
  });
  assert.deepEqual(result, { designatedRequirement: '' });
  assert.deepEqual(calls.map((call) => call[0]), ['extract', 'find', 'validate-tree', 'stage', 'verify']);
  assert.throws(() => stageAndVerifyRelease({
    ...request,
    incomingTarget: path.join(transactionRoot, '..', 'escaped'),
  }), /escaped the transaction/);
});

test('Windows archive inspection rejects link entries before extraction', () => {
  assert.throws(() => validateArchiveLinksBeforeExtraction('C:\\updates\\release.zip', 'win32', {
    run: () => ({
      status: 0,
      stdout: [
        'drwxr-xr-x  0 user group 0 Jan 01 00:00 Workass/',
        'lrwxr-xr-x  0 user group 0 Jan 01 00:00 Workass/escape -> C:/outside',
        '-rw-r--r--  0 user group 4 Jan 01 00:00 Workass/escape/child.txt',
      ].join('\n'),
    }),
  }), /cannot contain links/);
});

test('macOS extraction never follows an archive-created escaping link and selected-app validation rejects it', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-link-traversal-'));
  const archive = path.join(root, 'release.zip');
  const extracted = path.join(root, 'extracted');
  const escapedChild = path.join(root, 'escaped', 'child.txt');
  fs.mkdirSync(path.dirname(escapedChild), { recursive: true });
  fs.writeFileSync(escapedChild, 'sentinel');
  writeStoredZip(archive, [
    { name: 'Workass.app/escape', data: '../../escaped', mode: 0o120777 },
    { name: 'Workass.app/escape/child.txt', data: 'malicious', mode: 0o100644 },
  ]);
  assert.throws(() => extractArchive(archive, extracted, 'darwin'), /extraction failed/);
  assert.equal(fs.readFileSync(escapedChild, 'utf8'), 'sentinel');
  const selected = path.join(extracted, 'Workass.app');
  if (fs.existsSync(selected)) {
    const escape = path.join(selected, 'escape');
    if (fs.lstatSync(escape, { throwIfNoEntry: false })?.isSymbolicLink()) {
      assert.throws(() => validateExtractedTree(selected, 'darwin'), /escapes the extracted root|broken link/);
    } else {
      assert.doesNotThrow(() => validateExtractedTree(selected, 'darwin'));
    }
  }
});

test('extracted link validation is scoped to the selected app and rejects external hard links', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-extracted-links-'));
  const app = path.join(root, 'Workass.app');
  const frameworks = path.join(app, 'Contents', 'Frameworks');
  fs.mkdirSync(frameworks, { recursive: true });
  fs.writeFileSync(path.join(frameworks, 'binary'), 'binary');
  fs.symlinkSync('binary', path.join(frameworks, 'Current'));
  assert.doesNotThrow(() => validateExtractedTree(app, 'darwin'));

  const external = path.join(root, 'outside-hard-link');
  fs.linkSync(path.join(frameworks, 'binary'), external);
  assert.throws(() => validateExtractedTree(app, 'darwin'), /hard link escapes/);
  assert.throws(() => validateExtractedTree(app, 'win32'), /cannot contain links/);
});

test('heavy update staging runs in a worker thread instead of Electron main', async () => {
  let invocation = null;
  class FakeWorker extends EventEmitter {
    constructor(file, options) {
      super();
      invocation = { file, options };
      setImmediate(() => this.emit('message', { ok: true, result: { designatedRequirement: 'fixture' } }));
    }
  }
  let settled = false;
  const pending = runUpdateStageWorker({ schemaVersion: 1 }, {
    WorkerClass: FakeWorker,
    workerFile: '/app/update-manager.js',
  }).then((result) => { settled = true; return result; });
  assert.equal(settled, false);
  assert.equal(invocation.file, '/app/update-manager.js');
  assert.equal(invocation.options.workerData.workassTask, 'stage-update');
  assert.deepEqual(await pending, { designatedRequirement: 'fixture' });
});

test('the real staging worker reports a bounded extraction failure across the thread boundary', async () => {
  const transactionRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-stage-worker-'));
  await assert.rejects(() => runUpdateStageWorker({
    schemaVersion: 1,
    transactionRoot,
    archive: path.join(transactionRoot, 'release.zip'),
    extracted: path.join(transactionRoot, 'extracted'),
    currentRoot: path.join(transactionRoot, 'Workass.app'),
    incomingTarget: path.join(transactionRoot, '.Workass.app.incoming-fixture'),
    targetVersion: '1.2.0',
    platform: 'darwin',
    arch: 'arm64',
  }), /archive could not be inspected/);
});

test('manifest validation requires the exact platform, checksum, size, and platform trust law', () => {
  assert.equal(validateReleaseManifest(manifest(), { platform: 'darwin', arch: 'arm64' }).version, '1.2.0');
  assert.throws(() => validateReleaseManifest(manifest({ arch: 'amd64' }), { platform: 'darwin', arch: 'arm64' }), /another platform/);
  assert.throws(() => validateReleaseManifest(manifest({ designatedRequirement: '' }), { platform: 'darwin', arch: 'arm64' }), /signing requirement/);
  const windows = manifest({ platform: 'windows', arch: 'amd64', designatedRequirement: undefined });
  assert.throws(() => validateReleaseManifest(windows, { platform: 'win32', arch: 'x64' }), /portable release/);
  assert.equal(validateReleaseManifest({ ...windows, portable: true, authenticode: false }, { platform: 'win32', arch: 'x64' }).platform, 'windows');
});

test('packaged unsigned Windows builds enable the GitHub updater without Authenticode', () => {
  const { manager } = managerFixture({ platform: 'win32', arch: 'x64' });
  assert.equal(manager.snapshot().supported, true);
  const { manager: missingSystemNetwork } = managerFixture({ platform: 'win32', arch: 'x64', networkRequest: null });
  assert.equal(missingSystemNetwork.snapshot().supported, false);
  assert.match(missingSystemNetwork.snapshot().error, /verified system-network updater/);
  const source = fs.readFileSync(path.join(__dirname, 'update-manager.js'), 'utf8');
  assert.doesNotMatch(source, /Get-AuthenticodeSignature/);
});

test('unsigned Windows staging verifies the complete portable x86-64 runtime', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-release-'));
  const incoming = path.join(root, 'incoming');
  const installationId = `install-${'4'.repeat(32)}`;
  writeWindowsRelease(incoming);
  fs.writeFileSync(path.join(incoming, '.workass-installation.json'), `${JSON.stringify({
    schemaVersion: 1, product: 'Workass', installationId,
  })}\n`);
  assert.doesNotThrow(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64', installationId));

  fs.writeFileSync(path.join(incoming, 'resources', 'app', 'package.json'), '{"version":"9.9.9"}\n');
  assert.throws(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64', installationId), /shell version/);
  fs.writeFileSync(path.join(incoming, 'resources', 'app', 'package.json'), '{"version":"1.2.0"}\n');
  fs.writeFileSync(path.join(incoming, 'workass-daemon.exe'), 'not-a-pe');
  assert.throws(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64', installationId), /Windows executable|PE32/);
});

test('busy daemon leaves the verified release staged and never starts the worker', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [{ status: 409, body: { ready: false, foregroundTurns: 1 } }] });
  const state = await manager.install();
  assert.equal(state.phase, 'busy');
  assert.equal(state.blockers.foregroundTurns, 1);
  assert.deepEqual(calls, ['prepare']);
  assert.equal(didQuit(), false);
});

test('committed handoff requires one durable worker arm before daemon commit and quit', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [
    { status: 200, body: { ready: true } },
    { status: 202, body: { stopping: true } },
  ] });
  const state = await manager.install();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(calls, ['prepare', 'spawn:update-worker.js', 'commit']);
  assert.equal(didQuit(), true);
  const transaction = JSON.parse(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'transaction.json'), 'utf8'));
  assert.equal(transaction.schemaVersion, 3);
  assert.equal(transaction.transactionRoot, manager.prepared.transactionRoot);
  assert.equal(transaction.mutableStateTarget, manager.runtime.stateDir);
  assert.equal(transaction.mutableStateBackupTarget, path.join(manager.prepared.transactionRoot, 'state-before-activation'));
  assert.equal(transaction.failedMutableStateTarget, path.join(manager.prepared.transactionRoot, 'state-from-failed-activation'));
  assert.equal(transaction.requireVisibleWindow, true);
  const receipt = JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8'));
  assert.equal(receipt.phase, 'armed');
  assert.equal(receipt.updateId, manager.prepared.updateId);
});

test('a lost prepare response retries the same durable transaction identity', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [
    { status: 0, body: {} },
    { status: 200, body: { ready: true, prepared: true } },
    { status: 202, body: { stopping: true } },
  ] });
  const updateId = manager.prepared.updateId;
  const state = await manager.install();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(calls, ['prepare', 'prepare', 'spawn:update-worker.js', 'commit']);
  assert.equal(didQuit(), true);
  assert.equal(JSON.parse(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'transaction.json'), 'utf8')).updateId, updateId);
});

test('a lost commit response transfers ownership to the exact armed worker without cancelling it', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [
    { status: 200, body: { ready: true } },
    { status: 0, body: {} },
  ] });
  const state = await manager.install();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(calls, ['prepare', 'spawn:update-worker.js', 'commit']);
  assert.equal(didQuit(), true);
  assert.equal(JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8')).phase, 'armed');
});

test('an authoritative commit rejection fences the exact worker and remains terminal after restart', async () => {
  const fenced = [];
  const { manager, calls, didQuit } = managerFixture({
    replies: [
      { status: 200, body: { ready: true } },
      { status: 409, body: {} },
      { status: 200, body: { cancelled: true } },
    ],
    dependencyOverrides: {
      inspectWorkerOwnership: async () => ({ owned: true, exact: true, stale: false, pid: 4242 }),
      terminateExactWorker: async (ownership) => { fenced.push(ownership.pid); return true; },
    },
  });
  const state = await manager.install();
  assert.equal(state.phase, 'failed');
  assert.deepEqual(fenced, [4242]);
  assert.deepEqual(calls, ['prepare', 'spawn:update-worker.js', 'commit', 'cancel']);
  assert.equal(didQuit(), false);
  assert.equal(JSON.parse(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'journal.json'), 'utf8')).terminal, true);
  assert.equal(JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8')).phase, 'failed');
  manager.dispose();
  const restarted = manager.init();
  assert.equal(restarted.phase, 'failed');
  assert.equal(manager.recoveryPromise, null);
});

test('a worker that cannot durably arm cancels the prepared handoff and keeps Workass open', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [
    { status: 200, body: { ready: true } },
    { status: 200, body: { cancelled: true } },
  ] });
  manager.deps.spawnArmedWorker = async () => {
    const error = new Error('fixture worker did not arm');
    error.workassWorkerFenced = true;
    throw error;
  };
  await assert.rejects(() => manager.install(), /did not arm/);
  assert.deepEqual(calls, ['prepare', 'cancel']);
  assert.equal(didQuit(), false);
});

test('a lost cancel response is reconciled with the same ID only after the arm-failed worker is fenced', async () => {
  const { manager, calls } = managerFixture({ replies: [
    { status: 200, body: { ready: true } },
    { status: 0, body: {} },
    { status: 200, body: { cancelled: true, alreadyCancelled: true } },
  ] });
  manager.deps.spawnArmedWorker = async () => {
    const error = new Error('arm failed after spawn');
    error.workassWorkerFenced = true;
    throw error;
  };
  await assert.rejects(() => manager.install(), /arm failed/);
  assert.deepEqual(calls, ['prepare', 'cancel', 'cancel']);
});

test('unconfirmed arm-failure cancel stays nonterminal and retries the exact prepared ID', async () => {
  const scheduled = [];
  const actions = [];
  let allowCancel = false;
  const { manager } = managerFixture({
    dependencyOverrides: {
      postLocalUpdate: async (_url, action, updateId) => {
        actions.push([action, updateId]);
        if (action === 'prepare') {
          return actions.filter(([name]) => name === 'prepare').length === 1
            ? { status: 200, body: { ready: true } }
            : { status: 409, body: { ready: false, reason: 'retry stops after proving cancellation' } };
        }
        return allowCancel
          ? { status: 200, body: { cancelled: true, alreadyCancelled: true } }
          : { status: 0, body: {} };
      },
      spawnArmedWorker: async () => {
        const error = new Error('arm failed after child fencing');
        error.workassWorkerFenced = true;
        throw error;
      },
      inspectWorkerOwnership: async () => ({ owned: false, exact: false, stale: false }),
      schedule: (fn) => { scheduled.push(fn); return { unref() {} }; },
    },
  });
  manager.startApply();
  await manager.applyPromise;
  const active = JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8'));
  assert.equal(manager.snapshot().phase, 'installing');
  assert.equal(active.phase, 'preparing');
  assert.equal(JSON.parse(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'journal.json'), 'utf8')).terminal, false);
  assert.equal(scheduled.length, 1);

  allowCancel = true;
  scheduled.shift()();
  await manager.recoveryPromise;
  assert.equal(actions.filter(([action]) => action === 'cancel').length, 3);
  assert.equal(manager.snapshot().phase, 'busy');
  assert.notEqual(manager.snapshot().phase, 'failed');
});

test('worker ownership transfers only after the exact durable arm receipt', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-arm-'));
  const receiptPath = path.join(root, 'receipt.json');
  const child = new EventEmitter();
  child.pid = 101;
  let unrefs = 0;
  child.unref = () => { unrefs += 1; };
  child.kill = () => {};
  const installationId = `install-${'5'.repeat(32)}`;
  const installTarget = path.join(root, 'Workass');
  const workerId = `worker-${'6'.repeat(32)}`;
  const pending = spawnArmedUpdateWorker({
    command: process.execPath,
    args: [],
    receiptPath,
    updateId: 'upd-exact-arm-1234',
    currentVersion: '1.1.0',
    targetVersion: '1.2.0',
    installationId,
    installTarget,
    workerId,
    timeoutMs: 500,
    pollIntervalMs: 5,
  }, { spawnProcess: () => child });
  let settled = false;
  void pending.then(() => { settled = true; });
  child.emit('spawn');
  fs.writeFileSync(receiptPath, `${JSON.stringify({
    schemaVersion: 2,
    updateId: 'upd-another-worker',
    phase: 'armed',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    installationId,
    installTarget,
    workerId,
    workerPID: 101,
  })}\n`);
  await new Promise((resolve) => setTimeout(resolve, 15));
  assert.equal(settled, false);
  fs.writeFileSync(receiptPath, `${JSON.stringify({
    schemaVersion: 2,
    updateId: 'upd-exact-arm-1234',
    phase: 'armed',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    installationId,
    installTarget,
    workerId,
    workerPID: 101,
  })}\n`);
  assert.equal(await pending, child);
  assert.equal(unrefs, 1);
});

test('arm failure does not reject or reopen daemon admission before child termination is confirmed', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-arm-fence-'));
  const child = new EventEmitter();
  child.pid = 3030;
  child.exitCode = null;
  child.unref = () => {};
  child.kill = () => {};
  let confirmTermination;
  const pending = spawnArmedUpdateWorker({
    command: process.execPath,
    receiptPath: path.join(root, 'receipt.json'),
    updateId: 'upd-arm-fence-1234',
    currentVersion: '1.1.0',
    targetVersion: '1.2.0',
    installationId: `install-${'d'.repeat(32)}`,
    installTarget: path.join(root, 'Workass'),
    workerId: `worker-${'e'.repeat(32)}`,
  }, {
    spawnProcess: () => child,
    terminateChild: async () => new Promise((resolve) => { confirmTermination = resolve; }),
  });
  let rejected = false;
  void pending.catch(() => { rejected = true; });
  child.emit('error', new Error('spawned worker failed'));
  while (!confirmTermination) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(rejected, false);
  confirmTermination(true);
  await assert.rejects(pending, /spawned worker failed/);
});

test('worker ownership requires an exact process command and a fresh heartbeat', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-worker-lease-'));
  const transaction = {
    updateId: 'upd-worker-lease-1234',
    workerId: `worker-${'8'.repeat(32)}`,
    installationId: `install-${'8'.repeat(32)}`,
    transactionRoot: root,
  };
  const workerPath = path.join(root, 'update-worker.js');
  const transactionPath = path.join(root, 'transaction.json');
  const timestamp = Date.parse('2026-08-19T12:00:00.000Z');
  const lease = {
    schemaVersion: 1,
    state: 'running',
    updateId: transaction.updateId,
    workerId: transaction.workerId,
    installationId: transaction.installationId,
    pid: 9123,
    workerPath,
    transactionPath,
    updatedAt: new Date(timestamp).toISOString(),
  };
  const exactRun = () => ({ status: 0, stdout: `${process.execPath} ${workerPath} --transaction ${transactionPath}\n` });
  assert.deepEqual(workerProcessOwnership(lease, transaction, {
    platform: 'darwin', alive: () => true, run: exactRun, now: () => timestamp + 1000,
  }), { owned: true, exact: true, stale: false, pid: 9123 });
  assert.deepEqual(workerProcessOwnership(lease, transaction, {
    platform: 'darwin', alive: () => true, run: exactRun, now: () => timestamp + 31 * 60 * 1000,
  }), { owned: false, exact: true, stale: true, pid: 9123 });
  assert.deepEqual(workerProcessOwnership(lease, transaction, {
    platform: 'darwin', alive: () => true, run: () => ({ status: 0, stdout: `${process.execPath} unrelated.js\n` }), now: () => timestamp,
  }), { owned: false, exact: false, stale: false });
  let windowsScript = '';
  assert.equal(workerProcessOwnership(lease, transaction, {
    platform: 'win32', alive: () => true, now: () => timestamp,
    run: (_command, args) => { windowsScript = args.at(-1); return { status: 0, stdout: 'OWNED\r\n' }; },
  }).owned, true);
  assert.match(windowsScript, /OrdinalIgnoreCase/);
});

test('a dead owned worker lease resumes the exact durable journal instead of terminalizing it', async () => {
  const oldWorkerId = `worker-${'9'.repeat(32)}`;
  const order = [];
  let seededTransaction;
  const { manager, didQuit, initialState } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-dead-worker-1234',
      phase: 'activating',
      previousVersion: '1.1.0',
      targetVersion: '1.2.0',
      workerId: oldWorkerId,
      workerPID: 999999,
    },
    beforeInit: (context) => {
      seededTransaction = seedRecoveryTransaction(context, {
        updateId: 'upd-dead-worker-1234',
        workerId: oldWorkerId,
        journal: { phase: 'activating', activationStarted: true },
      });
      fs.writeFileSync(seededTransaction.leasePath, `${JSON.stringify({
        schemaVersion: 1,
        state: 'running',
        updateId: seededTransaction.updateId,
        installationId: seededTransaction.installationId,
        workerId: oldWorkerId,
        pid: 999999,
        workerPath: path.join(seededTransaction.transactionRoot, 'update-worker.js'),
        transactionPath: path.join(seededTransaction.transactionRoot, 'transaction.json'),
        updatedAt: new Date().toISOString(),
      })}\n`);
    },
    dependencyOverrides: {
      workerProcessOwnership: (lease, transaction) => workerProcessOwnership(lease, transaction, {
        platform: 'darwin', alive: () => false,
      }),
      postLocalUpdate: async (_url, action) => {
        order.push(action);
        return action === 'prepare' ? { status: 200, body: { ready: true } } : { status: 202, body: { stopping: true } };
      },
      spawnArmedWorker: async (request) => {
        order.push('spawn');
        const journal = JSON.parse(fs.readFileSync(seededTransaction.journalPath, 'utf8'));
        assert.equal(journal.phase, 'activating');
        assert.equal(journal.activationStarted, true);
        assert.equal(journal.terminal, false);
        fs.writeFileSync(request.receiptPath, `${JSON.stringify({
          schemaVersion: 2,
          updateId: request.updateId,
          phase: 'armed',
          previousVersion: request.currentVersion,
          targetVersion: request.targetVersion,
          installationId: request.installationId,
          installTarget: request.installTarget,
          workerId: request.workerId,
          workerPID: 8123,
        })}\n`);
        return { pid: 8123, kill() {} };
      },
    },
  });
  assert.equal(initialState.phase, 'installing');
  await manager.recoveryPromise;
  assert.deepEqual(order, ['prepare', 'spawn', 'commit']);
  assert.equal(manager.snapshot().phase, 'installing');
  assert.equal(didQuit(), true);
  const resumed = JSON.parse(fs.readFileSync(path.join(seededTransaction.transactionRoot, 'transaction.json'), 'utf8'));
  assert.notEqual(resumed.workerId, oldWorkerId);
  assert.equal(resumed.recoveryAttempt, 1);
});

test('a stale but exact live worker is fenced before its journal is resumed', async () => {
  const oldWorkerId = `worker-${'a'.repeat(32)}`;
  const order = [];
  let seededTransaction;
  const { manager } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-hung-worker-1234',
      phase: 'activating',
      previousVersion: '1.1.0',
      targetVersion: '1.2.0',
      workerId: oldWorkerId,
      workerPID: 7331,
    },
    beforeInit: (context) => {
      seededTransaction = seedRecoveryTransaction(context, {
        updateId: 'upd-hung-worker-1234', workerId: oldWorkerId,
      });
      fs.writeFileSync(seededTransaction.leasePath, `${JSON.stringify({
        schemaVersion: 1, state: 'running', updateId: seededTransaction.updateId,
        installationId: seededTransaction.installationId, workerId: oldWorkerId, pid: 7331,
        workerPath: path.join(seededTransaction.transactionRoot, 'update-worker.js'),
        transactionPath: path.join(seededTransaction.transactionRoot, 'transaction.json'),
        updatedAt: '2026-08-19T00:00:00.000Z',
      })}\n`);
    },
    dependencyOverrides: {
      workerProcessOwnership: () => ({ owned: false, exact: true, stale: true, pid: 7331 }),
      terminateExactWorker: async (ownership) => { order.push(`fence:${ownership.pid}`); return true; },
      postLocalUpdate: async (_url, action) => {
        order.push(action);
        return action === 'prepare' ? { status: 200, body: { ready: true } } : { status: 202, body: { stopping: true } };
      },
      spawnArmedWorker: async (request) => {
        order.push('spawn');
        fs.writeFileSync(request.receiptPath, `${JSON.stringify({
          schemaVersion: 2, updateId: request.updateId, phase: 'armed',
          previousVersion: request.currentVersion, targetVersion: request.targetVersion,
          installationId: request.installationId, installTarget: request.installTarget,
          workerId: request.workerId, workerPID: 8124,
        })}\n`);
        return { pid: 8124, kill() {} };
      },
    },
  });
  await manager.recoveryPromise;
  assert.deepEqual(order, ['fence:7331', 'prepare', 'spawn', 'commit']);
  assert.equal(manager.snapshot().phase, 'installing');
});

test('resume rotates worker identity durably before arm and never terminalizes an unconfirmed cancel retry', async () => {
  const oldWorkerId = `worker-${'f'.repeat(32)}`;
  const scheduled = [];
  let allowCancel = false;
  let prepareCount = 0;
  let transaction;
  const { manager } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-rotate-cancel-1234', phase: 'activating',
      previousVersion: '1.1.0', targetVersion: '1.2.0', workerId: oldWorkerId, workerPID: 999993,
    },
    beforeInit: (context) => {
      transaction = seedRecoveryTransaction(context, {
        updateId: 'upd-rotate-cancel-1234', workerId: oldWorkerId,
      });
    },
    dependencyOverrides: {
      inspectWorkerOwnership: async () => ({ owned: false, exact: false, stale: false }),
      postLocalUpdate: async (_url, action) => {
        if (action === 'prepare') {
          prepareCount += 1;
          return prepareCount === 1
            ? { status: 200, body: { ready: true } }
            : { status: 409, body: { ready: false } };
        }
        return allowCancel ? { status: 200, body: { cancelled: true } } : { status: 0, body: {} };
      },
      spawnArmedWorker: async () => {
        const error = new Error('resumed worker arm failed');
        error.workassWorkerFenced = true;
        throw error;
      },
      schedule: (fn) => { scheduled.push(fn); return { unref() {} }; },
    },
  });
  await manager.recoveryPromise;
  const rotated = JSON.parse(fs.readFileSync(path.join(transaction.transactionRoot, 'transaction.json'), 'utf8'));
  const activeReceipt = JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8'));
  assert.notEqual(rotated.workerId, oldWorkerId);
  assert.equal(activeReceipt.workerId, rotated.workerId);
  assert.equal(activeReceipt.phase, 'preparing');
  assert.equal(manager.snapshot().phase, 'installing');
  assert.equal(JSON.parse(fs.readFileSync(rotated.journalPath, 'utf8')).terminal, false);

  allowCancel = true;
  assert.equal(scheduled.length, 1);
  scheduled.shift()();
  await manager.recoveryPromise;
  assert.equal(manager.snapshot().phase, 'busy');
  assert.notEqual(manager.snapshot().phase, 'failed');
});

test('terminal journal recovery reconstructs the receipt before any daemon prepare response can interfere', async () => {
  for (const forbiddenStatus of [409, 500]) {
    const workerId = `worker-${String(forbiddenStatus).padStart(32, '0')}`;
    let daemonCalls = 0;
    const { manager } = managerFixture({
      primeReady: false,
      initialReceipt: {
        updateId: `upd-terminal-${forbiddenStatus}-1234`, phase: 'activating',
        previousVersion: '1.1.0', targetVersion: '1.2.0', workerId, workerPID: 999991,
      },
      beforeInit: (context) => {
        seedRecoveryTransaction(context, {
          updateId: `upd-terminal-${forbiddenStatus}-1234`,
          workerId,
          journal: {
            phase: 'rollback_healthy', terminal: true, installedVersion: '1.1.0',
            activated: false, rollbackStarted: true, rolledBack: true,
            error: 'target failed health',
          },
        });
      },
      dependencyOverrides: {
        inspectWorkerOwnership: async () => ({ owned: false, exact: false, stale: false }),
        postLocalUpdate: async () => { daemonCalls += 1; return { status: forbiddenStatus, body: {} }; },
      },
    });
    await manager.recoveryPromise;
    assert.equal(daemonCalls, 0);
    assert.equal(manager.snapshot().phase, 'rollback_healthy');
    assert.equal(JSON.parse(fs.readFileSync(manager.receiptPath, 'utf8')).installedVersion, '1.1.0');
  }
});

test('an active receipt without its durable journal cancels exact daemon admission before becoming terminal', async () => {
  const oldWorkerId = `worker-${'b'.repeat(32)}`;
  const daemonActions = [];
  const { manager, initialState } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-missing-journal-1234', phase: 'activating',
      previousVersion: '1.1.0', targetVersion: '1.2.0', workerId: oldWorkerId, workerPID: 999998,
    },
    beforeInit: (context) => {
      const transaction = seedRecoveryTransaction(context, {
        updateId: 'upd-missing-journal-1234', workerId: oldWorkerId,
      });
      fs.rmSync(transaction.journalPath);
    },
    dependencyOverrides: {
      postLocalUpdate: async (_url, action, updateId) => {
        daemonActions.push([action, updateId]);
        return { status: 200, body: { cancelled: false, notPrepared: true } };
      },
    },
  });
  assert.equal(initialState.phase, 'installing');
  await manager.recoveryPromise;
  assert.deepEqual(daemonActions, [['cancel', 'upd-missing-journal-1234']]);
  assert.equal(manager.snapshot().phase, 'failed');
  assert.match(manager.snapshot().error, /no complete recovery journal/);
});

test('crash after daemon prepare cannot lose the durable transaction needed to reconcile the same ID', async () => {
  const first = managerFixture();
  const updateId = first.manager.prepared.updateId;
  first.manager.deps.postLocalUpdate = async (_url, action, requestedId) => {
    assert.equal(action, 'prepare');
    assert.equal(requestedId, updateId);
    const transaction = JSON.parse(fs.readFileSync(path.join(first.manager.prepared.transactionRoot, 'transaction.json'), 'utf8'));
    const journal = JSON.parse(fs.readFileSync(transaction.journalPath, 'utf8'));
    const receipt = JSON.parse(fs.readFileSync(transaction.receiptPath, 'utf8'));
    assert.equal(journal.phase, 'preparing');
    assert.equal(receipt.phase, 'preparing');
    throw new Error('simulated shell crash after prepare acceptance');
  };
  await assert.rejects(() => first.manager.install(), /simulated shell crash/);

  const resumeCalls = [];
  const restarted = new UpdateManager({
    app: { getVersion: () => '1.1.0' },
    runtime: first.manager.runtime,
    resourcesPath: first.manager.resourcesPath,
    executablePath: first.manager.executablePath,
    platform: first.manager.platform,
    arch: first.manager.arch,
    isPackaged: true,
    deps: {
      networkRequest: () => { throw new Error('network not expected'); },
      inspectWorkerOwnership: async () => ({ owned: false, exact: false, stale: false }),
      postLocalUpdate: async (_url, action, requestedId) => {
        resumeCalls.push([action, requestedId]);
        return action === 'cancel'
          ? { status: 200, body: { cancelled: true } }
          : { status: 409, body: { ready: false, foregroundTurns: 1 } };
      },
    },
  });
  assert.equal(restarted.init().phase, 'installing');
  await restarted.recoveryPromise;
  assert.deepEqual(resumeCalls, [['cancel', updateId], ['prepare', updateId]]);
  assert.equal(restarted.snapshot().phase, 'busy');
});

test('fresh worker heartbeats require one off-main identity proof and keep later polling on the cheap path', async () => {
  const workerId = `worker-${'c'.repeat(32)}`;
  let inspections = 0;
  let transaction;
  const { manager } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-fresh-heartbeat-1234', phase: 'activating',
      previousVersion: '1.1.0', targetVersion: '1.2.0', workerId, workerPID: process.pid,
    },
    beforeInit: (context) => {
      transaction = seedRecoveryTransaction(context, {
        updateId: 'upd-fresh-heartbeat-1234', workerId,
      });
      fs.writeFileSync(transaction.leasePath, `${JSON.stringify({
        schemaVersion: 1, state: 'running', updateId: transaction.updateId,
        installationId: transaction.installationId, workerId, pid: process.pid,
        workerPath: path.join(transaction.transactionRoot, 'update-worker.js'),
        transactionPath: path.join(transaction.transactionRoot, 'transaction.json'),
        updatedAt: new Date().toISOString(),
      })}\n`);
    },
    dependencyOverrides: {
      inspectWorkerOwnership: async () => { inspections += 1; return { owned: true, exact: true, pid: process.pid }; },
    },
  });
  assert.equal(cheapWorkerLeaseOwnership(JSON.parse(fs.readFileSync(transaction.leasePath, 'utf8')), transaction).owned, true);
  await new Promise((resolve) => setTimeout(resolve, 1100));
  manager.dispose();
  assert.equal(inspections, 1);
});

test('a fresh lease with a reused live PID is rejected by the first off-main identity proof', async () => {
  const workerId = `worker-${'d'.repeat(32)}`;
  let inspections = 0;
  let prepares = 0;
  const { manager } = managerFixture({
    primeReady: false,
    initialReceipt: {
      updateId: 'upd-reused-live-pid-1234', phase: 'activating',
      previousVersion: '1.1.0', targetVersion: '1.2.0', workerId, workerPID: process.pid,
    },
    beforeInit: (context) => {
      const transaction = seedRecoveryTransaction(context, {
        updateId: 'upd-reused-live-pid-1234', workerId,
      });
      fs.writeFileSync(transaction.leasePath, `${JSON.stringify({
        schemaVersion: 1, state: 'running', updateId: transaction.updateId,
        installationId: transaction.installationId, workerId, pid: process.pid,
        workerPath: path.join(transaction.transactionRoot, 'update-worker.js'),
        transactionPath: path.join(transaction.transactionRoot, 'transaction.json'),
        startedAt: new Date().toISOString(), updatedAt: new Date().toISOString(),
      })}\n`);
    },
    dependencyOverrides: {
      inspectWorkerOwnership: async () => {
        inspections += 1;
        return { owned: false, exact: false, stale: false };
      },
      postLocalUpdate: async (_url, action) => {
        if (action === 'prepare') prepares += 1;
        return { status: 409, body: { ready: false, reason: 'same update remains prepared' } };
      },
    },
  });
  await manager.recoveryPromise;
  manager.dispose();
  assert.equal(inspections, 1);
  assert.equal(prepares, 1);
  assert.equal(manager.snapshot().phase, 'busy');
});

test('one apply action stages and commits an available release without a second click', async () => {
  const { manager } = managerFixture();
  const steps = [];
  manager.publish({ phase: 'available', targetVersion: '1.2.0' });
  manager.download = async () => {
    steps.push('download');
    return manager.publish({ phase: 'ready', progress: 1 });
  };
  manager.install = async () => {
    steps.push('install');
    return manager.publish({ phase: 'installing' });
  };

  const state = await manager.apply();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(steps, ['download', 'install']);
});

test('apply IPC returns a running snapshot immediately while one background intent owns the update', async () => {
  const { manager } = managerFixture();
  const steps = [];
  let finishDownload;
  manager.publish({ phase: 'available', targetVersion: '1.2.0' });
  manager.download = async () => {
    steps.push('download');
    manager.publish({ phase: 'downloading', progress: 0 });
    await new Promise((resolve) => { finishDownload = resolve; });
    return manager.publish({ phase: 'ready', progress: 1 });
  };
  manager.install = async () => {
    steps.push('install');
    return manager.publish({ phase: 'installing' });
  };

  const started = manager.startApply();
  const operation = manager.applyPromise;
  assert.equal(started.phase, 'downloading');
  assert.deepEqual(steps, ['download']);
  assert.equal(manager.startApply().phase, 'downloading');
  assert.deepEqual(steps, ['download']);

  finishDownload();
  await operation;
  assert.equal(manager.snapshot().phase, 'installing');
  assert.deepEqual(steps, ['download', 'install']);
});

test('a missing platform asset means current while a feed failure cannot impersonate an attempted update', async () => {
  const missing = managerFixture({ primeReady: false });
  missing.manager.deps.fetchManifest = async () => { throw Object.assign(new Error('HTTP 404'), { statusCode: 404 }); };
  assert.equal((await missing.manager.check()).phase, 'current');
  const broken = managerFixture({ primeReady: false });
  broken.manager.deps.fetchManifest = async () => { throw new Error('TLS failed'); };
  const state = await broken.manager.check();
  assert.equal(state.phase, 'check_failed');
  assert.match(state.error, /TLS failed/);
  assert.equal(state.receipt, null);
});

test('an empty local feed means current while malformed local metadata stays visible', async () => {
  const empty = managerFixture({ primeReady: false });
  empty.manager.feedURL = path.join(os.tmpdir(), `missing-workass-feed-${Date.now()}`, 'workass-darwin-arm64-release.json');
  assert.equal((await empty.manager.check()).phase, 'current');
  const broken = managerFixture({ primeReady: false });
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'broken-workass-feed-'));
  broken.manager.feedURL = path.join(root, 'workass-darwin-arm64-release.json');
  fs.writeFileSync(broken.manager.feedURL, '{');
  const state = await broken.manager.check();
  assert.equal(state.phase, 'check_failed');
  assert.match(state.error, /valid JSON/);
});

test('retrying a failed availability check cannot download or install when the current release is latest', async () => {
  const { manager } = managerFixture();
  const actions = [];
  manager.deps.fetchManifest = async () => manifest({ version: '1.1.0' });
  manager.download = async () => { actions.push('download'); return manager.snapshot(); };
  manager.install = async () => { actions.push('install'); return manager.snapshot(); };
  manager.publish({ phase: 'check_failed', error: 'offline' });

  const state = await manager.apply();
  assert.equal(state.phase, 'current');
  assert.deepEqual(actions, []);
});

test('concurrent discovery callers share one in-flight feed request', async () => {
  const { manager } = managerFixture({ primeReady: false });
  let release;
  manager.deps.fetchManifest = () => new Promise((resolve) => { release = resolve; });
  const first = manager.check();
  const second = manager.check();
  assert.equal(second, first);
  release(manifest());
  assert.equal((await first).phase, 'available');
  assert.equal((await second).phase, 'available');
});

test('apply coalesces with an in-flight background check and applies its exact manifest', async () => {
  const { manager } = managerFixture();
  let publishRelease;
  const steps = [];
  manager.publish({ phase: 'current', targetVersion: null, availableVersion: null, error: null });
  manager.deps.fetchManifest = () => new Promise((resolve) => { publishRelease = resolve; });
  manager.download = async () => {
    steps.push(`download:${manager.manifest.version}`);
    return manager.publish({ phase: 'ready' });
  };
  manager.install = async () => {
    steps.push(`install:${manager.manifest.version}`);
    return manager.publish({ phase: 'installing' });
  };

  const checking = manager.check({ background: true });
  const applying = manager.apply();
  await Promise.resolve();
  assert.deepEqual(steps, []);
  publishRelease(manifest({ version: '1.3.0' }));
  assert.equal((await checking).targetVersion, '1.3.0');
  assert.equal((await applying).phase, 'installing');
  assert.deepEqual(steps, ['download:1.3.0', 'install:1.3.0']);
});

test('N+2 discovery stays visible without mutating the immutable N+1 staged transaction', async () => {
  const { manager } = managerFixture({ primeReady: false });
  const n1 = snapshotReleaseManifest(manifest({ version: '1.2.0' }));
  const n2 = manifest({
    version: '1.3.0',
    artifacts: { update: {
      name: 'Workass-1.3.0-darwin-arm64.zip',
      url: 'https://releases.example.test/Workass-1.3.0-darwin-arm64.zip',
      sha256: 'b'.repeat(64),
      size: 2048,
    } },
  });
  manager.manifest = n1;
  manager.publish({ phase: 'available', targetVersion: n1.version, availableVersion: n1.version });
  let finishArtifact;
  let stagedRequest = null;
  manager.deps.downloadArtifact = async (_source, destination, artifact) => {
    assert.deepEqual(artifact, n1.artifacts.update);
    await new Promise((resolve) => { finishArtifact = resolve; });
    fs.mkdirSync(path.dirname(destination), { recursive: true });
    fs.writeFileSync(destination, 'archive');
  };
  manager.deps.stageAndVerify = async (request) => {
    stagedRequest = request;
    fs.mkdirSync(request.incomingTarget, { recursive: true });
    return { designatedRequirement: n1.designatedRequirement };
  };
  const downloading = manager.download();
  for (let attempt = 0; !finishArtifact && attempt < 100; attempt += 1) {
    await new Promise((resolve) => setImmediate(resolve));
  }
  assert.equal(typeof finishArtifact, 'function', JSON.stringify(manager.snapshot()));
  manager.deps.fetchManifest = async () => n2;
  const discovered = await manager.check({ background: true });
  assert.equal(discovered.phase, 'downloading');
  assert.equal(discovered.targetVersion, '1.2.0');
  assert.equal(discovered.availableVersion, '1.3.0');
  assert.equal(manager.manifest.version, '1.3.0');
  finishArtifact();
  const ready = await downloading;
  assert.equal(ready.phase, 'ready');
  assert.equal(ready.targetVersion, '1.2.0');
  assert.equal(ready.availableVersion, '1.3.0');
  assert.equal(stagedRequest.targetVersion, '1.2.0');
  assert.equal(manager.prepared.release.version, '1.2.0');
  assert.equal(manager.prepared.artifact.url, n1.artifacts.update.url);

  const handoffs = [];
  manager.deps.postLocalUpdate = async (_url, action) => action === 'prepare'
    ? { status: 200, body: { ready: true } }
    : { status: 202, body: { stopping: true } };
  manager.deps.spawnArmedWorker = async (request) => {
    handoffs.push(request.targetVersion);
    const receipt = {
      schemaVersion: 2, updateId: request.updateId, phase: 'armed',
      previousVersion: request.currentVersion, targetVersion: request.targetVersion,
      installationId: request.installationId, installTarget: request.installTarget,
      workerId: request.workerId, workerPID: 4555,
    };
    fs.writeFileSync(request.receiptPath, `${JSON.stringify(receipt)}\n`);
    return { pid: 4555, workassUpdateReceipt: receipt };
  };
  assert.equal((await manager.install()).phase, 'installing');
  assert.deepEqual(handoffs, ['1.2.0']);
});

test('a retained rollback receipt keeps polling and retry applies the newly discovered N+2 manifest', async () => {
  const { manager } = managerFixture();
  const retained = {
    schemaVersion: 2,
    updateId: 'upd-rollback-n-plus-one',
    phase: 'rollback_healthy',
    previousVersion: '1.1.0',
    targetVersion: '1.2.0',
    installedVersion: '1.1.0',
    installationId: manager.installationIdentity.installationId,
    installTarget: manager.installTarget,
    error: '1.2.0 failed health',
  };
  manager.publish({ phase: retained.phase, receipt: retained, error: retained.error, targetVersion: null, availableVersion: null });
  manager.deps.fetchManifest = async () => manifest({ version: '1.3.0' });
  const discovered = await manager.autoCheck();
  assert.equal(discovered.phase, 'rollback_healthy');
  assert.equal(discovered.receipt.updateId, retained.updateId);
  assert.equal(discovered.error, retained.error);
  assert.equal(discovered.targetVersion, '1.3.0');
  assert.equal(discovered.availableVersion, '1.3.0');

  const steps = [];
  manager.download = async () => {
    steps.push(`download:${manager.manifest.version}`);
    return manager.publish({ phase: 'ready' });
  };
  manager.install = async () => {
    steps.push(`install:${manager.manifest.version}`);
    return manager.publish({ phase: 'installing' });
  };
  assert.equal((await manager.apply()).phase, 'installing');
  assert.deepEqual(steps, ['download:1.3.0', 'install:1.3.0']);
});

test('automatic checks discover a newly published local release without restarting Electron', async () => {
  const { manager } = managerFixture();
  const scheduled = [];
  const repeated = [];
  const cancelled = [];
  const releases = [manifest({ version: '1.1.0' }), manifest({ version: '1.2.0' }), manifest({ version: '1.3.0' })];
  let fetches = 0;
  manager.deps.fetchManifest = async () => { fetches += 1; return releases.shift(); };
  manager.deps.schedule = (fn, delay) => { const handle = { fn, delay, unref() {} }; scheduled.push(handle); return handle; };
  manager.deps.repeat = (fn, delay) => { const handle = { fn, delay, unref() {} }; repeated.push(handle); return handle; };
  manager.deps.cancelSchedule = (handle) => cancelled.push(handle);
  manager.deps.cancelRepeat = (handle) => cancelled.push(handle);
  manager.publish({ phase: 'current', targetVersion: null, error: null });

  manager.startAutoChecks({ initialDelayMs: 15, intervalMs: 30 });
  assert.equal(scheduled[0].delay, 15);
  assert.equal(repeated[0].delay, 30);
  assert.equal((await scheduled[0].fn()).phase, 'current');
  assert.equal((await repeated[0].fn()).phase, 'available');
  assert.equal(fetches, 2);
  const advanced = await repeated[0].fn();
  assert.equal(advanced.phase, 'available');
  assert.equal(advanced.targetVersion, '1.3.0');
  assert.equal(fetches, 3, 'a newer un-downloaded offer must replace the previous offer');

  manager.dispose();
  assert.deepEqual(cancelled, [scheduled[0], repeated[0]]);
});

test('an automatic availability-check failure remains retryable without becoming an update transaction', async () => {
  const { manager } = managerFixture();
  const scheduled = [];
  const repeated = [];
  let fetches = 0;
  manager.deps.fetchManifest = async () => {
    fetches += 1;
    if (fetches === 1) throw new Error('offline');
    return manifest({ version: '1.1.0' });
  };
  manager.deps.schedule = (fn) => { const handle = { fn, unref() {} }; scheduled.push(handle); return handle; };
  manager.deps.repeat = (fn) => { const handle = { fn, unref() {} }; repeated.push(handle); return handle; };
  manager.publish({ phase: 'idle', targetVersion: null, error: null, receipt: null });

  manager.startAutoChecks({ initialDelayMs: 15, intervalMs: 30 });
  const failedCheck = await scheduled[0].fn();
  assert.equal(failedCheck.phase, 'check_failed');
  assert.equal(failedCheck.receipt, null);
  assert.equal((await repeated[0].fn()).phase, 'current');
  assert.equal(fetches, 2);
  manager.dispose();
});
