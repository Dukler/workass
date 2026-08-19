'use strict';

// Independent transactional updater. The shell copies this file and a pinned
// standalone Node executable outside the installation before it quits, so the
// worker never replaces code from underneath a running Electron or daemon
// process. No third-party packages are used.

const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// The worker is forked by the release being replaced. Runtime values inherited
// from that old shell must not override the profile inside the newly installed
// app (for example, during a localhost -> LAN bind migration).
const STALE_RUNTIME_ENV_KEYS = new Set([
  'WORKASS_PROFILE', 'WORKASS_PROFILE_FILE', 'WORKASS_APP_NAME',
  'WORKASS_BUNDLE_ID', 'WORKASS_DAEMON_PORT', 'WORKASS_DAEMON_BIND',
  'WORKASS_VIEW_PORT', 'WORKASS_LAUNCHD_LABEL', 'WORKASS_DATA_ROOT',
  'WORKASS_LOG_ROOT', 'WORKASS_UPDATE_CHANNEL', 'WORKASS_URL',
  'WORKASS_BROWSER_CONTROL_FILE', 'WORKASS_REPO_ROOT',
  'WORKASS_TEST_ROOT', 'WORKASS_PROD',
]);

function targetRuntimeEnv(source = process.env) {
  const clean = { ...source };
  for (const key of STALE_RUNTIME_ENV_KEYS) delete clean[key];
  return clean;
}

function runtimeIsHealthy({ daemon, shell, expectedVersion, expectedBind = '', requireVisibleWindow = false }) {
  return daemon?.app === 'workass' && daemon?.version === expectedVersion &&
    (!expectedBind || daemon?.bind === expectedBind) &&
    shell?.controller === true && Number(shell?.catalog?.readyModelCount || 0) > 0 &&
    shell?.appVersion === expectedVersion &&
    (!requireVisibleWindow || shell?.windowVisible === true);
}

function spawnDetached(command, args, options, { spawnProcess = spawn } = {}) {
  return new Promise((resolve, reject) => {
    let child;
    try {
      child = spawnProcess(command, args, options);
    } catch (err) {
      reject(err);
      return;
    }
    let settled = false;
    child.once('error', (err) => {
      if (settled) return;
      settled = true;
      reject(err);
    });
    child.once('spawn', () => {
      if (settled) return;
      settled = true;
      child.unref?.();
      resolve(child);
    });
  });
}

function atomicJSON(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const incoming = `${file}.incoming-${process.pid}`;
  fs.writeFileSync(incoming, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(incoming, file);
}

function updateReceipt(transaction, phase, extra = {}) {
  const receipt = {
    schemaVersion: 1,
    updateId: transaction.updateId,
    phase,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    updatedAt: new Date().toISOString(),
    ...extra,
  };
  atomicJSON(transaction.receiptPath, receipt);
  return receipt;
}

function pidAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 1) return false;
  try { process.kill(pid, 0); return true; } catch { return false; }
}

async function stopLaunchedProcessTree(pid, {
  platform = process.platform,
  alive = pidAlive,
  kill = process.kill,
  run = spawnSync,
  wait = waitUntil,
} = {}) {
  if (!Number.isInteger(pid) || pid <= 1 || !alive(pid)) return true;
  if (platform === 'win32') {
    const result = run('taskkill.exe', ['/PID', String(pid), '/T', '/F'], {
      windowsHide: true,
      stdio: 'ignore',
    });
    if ((result.error || result.status !== 0) && alive(pid)) {
      throw new Error('failed release process tree did not stop');
    }
  } else {
    try { kill(pid, 'SIGTERM'); } catch { /* already gone */ }
  }
  const stopped = await wait(() => !alive(pid), { attempts: 80, delayMs: 100 });
  if (!stopped) throw new Error('failed release shell did not stop');
  return true;
}

async function renamePathWithRetry(source, destination, {
  rename = fs.renameSync,
  pause = delay,
  attempts = 80,
  delayMs = 250,
} = {}) {
  let lastError = null;
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      rename(source, destination);
      return;
    } catch (err) {
      lastError = err;
      if (!['EACCES', 'EBUSY', 'EPERM'].includes(err?.code) || attempt + 1 >= attempts) throw err;
      await pause(delayMs);
    }
  }
  throw lastError || new Error('release path rename failed');
}

function mirrorWindowsDirectory(source, destination, { run = spawnSync } = {}) {
  if (!fs.statSync(source, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Error('Windows release mirror source is not a directory');
  }
  fs.mkdirSync(destination, { recursive: true });
  const result = run('robocopy.exe', [
    source,
    destination,
    '/MIR',
    '/COPY:DAT',
    '/DCOPY:DAT',
    '/XJ',
    '/SL',
    '/R:5',
    '/W:1',
    '/NFL',
    '/NDL',
    '/NJH',
    '/NJS',
    '/NP',
  ], {
    windowsHide: true,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  // Robocopy uses 0-7 for successful copies (including differences copied)
  // and reserves 8+ for failures.
  if (result.error || !Number.isInteger(result.status) || result.status > 7) {
    throw new Error(`Windows release mirror failed${Number.isInteger(result.status) ? ` (robocopy exit ${result.status})` : ''}`);
  }
}

async function waitUntil(predicate, { attempts = 160, delayMs = 250 } = {}) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (await predicate()) return true;
    await delay(delayMs);
  }
  return false;
}

function requestJSON(url, timeoutMs = 1500) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL(url); } catch { resolve(null); return; }
    const transport = parsed.protocol === 'https:' ? https : http;
    const request = transport.get(parsed, {
      timeout: timeoutMs,
      rejectUnauthorized: false,
      headers: { 'cache-control': 'no-store' },
    }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => { if (body.length < 65536) body += chunk; });
      response.on('end', () => {
        if ((response.statusCode || 500) >= 400) { resolve(null); return; }
        try { resolve(JSON.parse(body)); } catch { resolve(null); }
      });
    });
    request.on('timeout', () => request.destroy());
    request.on('error', () => resolve(null));
  });
}

function requestDaemonShutdown(healthURL, timeoutMs = 1500) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL('/workass/recovery/shutdown', healthURL); } catch { resolve(false); return; }
    const transport = parsed.protocol === 'https:' ? https : http;
    const request = transport.request(parsed, {
      method: 'POST',
      timeout: timeoutMs,
      rejectUnauthorized: false,
      headers: { 'content-length': '0' },
    }, (response) => {
      response.resume();
      response.on('end', () => resolve(response.statusCode === 202));
    });
    request.on('timeout', () => { request.destroy(); resolve(false); });
    request.on('error', () => resolve(false));
    request.end();
  });
}

function runChecked(command, args, label) {
  const result = spawnSync(command, args, { encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.error || result.status !== 0) {
    throw new Error(`${label} failed`);
  }
  return String(result.stdout || '').trim();
}

function verifyMacIncoming(transaction) {
  runChecked('/usr/bin/codesign', ['--verify', '--deep', '--strict', transaction.incomingTarget], 'incoming app signature verification');
  runChecked('/usr/bin/codesign', ['--verify', '--strict', `-R=${transaction.designatedRequirement}`, transaction.incomingTarget], 'incoming app identity verification');
  const plist = path.join(transaction.incomingTarget, 'Contents', 'Info.plist');
  const version = runChecked('/usr/bin/plutil', ['-extract', 'CFBundleShortVersionString', 'raw', '-o', '-', plist], 'incoming app version verification');
  if (version !== transaction.targetVersion) throw new Error('incoming app version does not match the release manifest');
  const runtimeRoot = path.join(transaction.incomingTarget, 'Contents', 'Resources', 'runtime');
  const runtimeManifest = JSON.parse(fs.readFileSync(path.join(runtimeRoot, 'manifest.json'), 'utf8'));
  if (runtimeManifest.version !== transaction.targetVersion || runtimeManifest.platform !== 'darwin') {
    throw new Error('incoming daemon runtime does not match the app release');
  }
  if (!fs.existsSync(path.join(runtimeRoot, 'workass'))) throw new Error('incoming release has no bundled daemon');
}

function requiredRegularFile(file, label) {
  const stat = fs.statSync(file, { throwIfNoEntry: false });
  if (!stat?.isFile() || stat.size <= 0) throw new Error(`incoming Windows release has no ${label}`);
  return stat;
}

function verifyWindowsPE(file, label) {
  const stat = requiredRegularFile(file, label);
  const descriptor = fs.openSync(file, 'r');
  try {
    const dos = Buffer.alloc(64);
    if (fs.readSync(descriptor, dos, 0, dos.length, 0) !== dos.length || dos.toString('ascii', 0, 2) !== 'MZ') {
      throw new Error(`incoming ${label} is not a Windows executable`);
    }
    const peOffset = dos.readUInt32LE(0x3c);
    if (peOffset < 64 || peOffset + 26 > stat.size || peOffset > 1024 * 1024) {
      throw new Error(`incoming ${label} has an invalid PE header`);
    }
    const pe = Buffer.alloc(26);
    if (fs.readSync(descriptor, pe, 0, pe.length, peOffset) !== pe.length ||
        pe.toString('ascii', 0, 4) !== 'PE\0\0' || pe.readUInt16LE(4) !== 0x8664 || pe.readUInt16LE(24) !== 0x20b) {
      throw new Error(`incoming ${label} is not PE32+ x86-64`);
    }
  } finally {
    fs.closeSync(descriptor);
  }
}

function readReleaseJSON(file, label) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch { throw new Error(`incoming Windows ${label} is invalid`); }
}

function verifyWindowsIncoming(transaction) {
  const root = transaction.incomingTarget;
  const manifest = readReleaseJSON(path.join(root, 'manifest.json'), 'runtime manifest');
  if (manifest.schemaVersion !== 2 || manifest.version !== transaction.targetVersion || manifest.platform !== 'windows' ||
      manifest.arch !== 'amd64' || manifest.portable !== true || manifest.electron !== true) {
    throw new Error('incoming Windows runtime does not match the release manifest');
  }
  const packageManifest = readReleaseJSON(path.join(root, 'resources', 'app', 'package.json'), 'shell manifest');
  if (packageManifest.version !== transaction.targetVersion) throw new Error('incoming Windows shell version does not match the release manifest');
  verifyWindowsPE(path.join(root, 'Workass.exe'), 'Workass.exe');
  verifyWindowsPE(path.join(root, 'workass-daemon.exe'), 'workass-daemon.exe');
  verifyWindowsPE(path.join(root, 'node', 'windows-amd64', 'node.exe'), 'portable node.exe');
  for (const [relative, label] of [
    [['resources', 'app', 'update-manager.js'], 'update manager'],
    [['resources', 'app', 'update-worker.js'], 'update worker'],
    [['resources', 'renderer', 'index.html'], 'renderer'],
    [['frontier-hosts', 'windows-amd64', 'claude-native-host.mjs'], 'Claude host'],
    [['frontier-hosts', 'windows-amd64', 'codex-native-host.mjs'], 'Codex host'],
    [['frontier-hosts', 'windows-amd64', 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'], 'Claude Agent SDK'],
  ]) requiredRegularFile(path.join(root, ...relative), label);
}

function validateTransaction(transaction) {
  if (!transaction || transaction.schemaVersion !== 2) throw new Error('unsupported update transaction');
  if (!/^[A-Za-z0-9_-]{8,96}$/.test(String(transaction.updateId || ''))) throw new Error('update transaction has an invalid updateId');
  if (transaction.requireVisibleWindow !== true) throw new Error('update transaction must require a visible shell window');
  for (const field of [
    'updateId', 'platform', 'currentVersion', 'targetVersion',
    'transactionRoot', 'installTarget', 'incomingTarget', 'backupTarget',
    'mutableStateTarget', 'mutableStateBackupTarget', 'failedMutableStateTarget',
    'receiptPath', 'daemonHealthURL', 'shellStatusURL',
  ]) {
    if (!String(transaction[field] || '').trim()) throw new Error(`update transaction is missing ${field}`);
  }
  for (const field of [
    'transactionRoot', 'installTarget', 'incomingTarget', 'backupTarget',
    'mutableStateTarget', 'mutableStateBackupTarget', 'failedMutableStateTarget', 'receiptPath',
  ]) {
    if (!path.isAbsolute(transaction[field])) throw new Error(`${field} must be absolute`);
  }
  const updateRoot = path.dirname(path.dirname(transaction.transactionRoot));
  const dataRoot = path.dirname(updateRoot);
  if (transaction.transactionRoot !== path.join(updateRoot, 'transactions', transaction.updateId) ||
      transaction.receiptPath !== path.join(updateRoot, 'receipt.json')) {
    throw new Error('update transaction paths do not belong to the exact update directory');
  }
  if (transaction.mutableStateTarget !== path.join(dataRoot, 'state') || dataRoot === path.parse(dataRoot).root) {
    throw new Error('mutable state target is not the exact Workass state directory');
  }
  if (path.dirname(transaction.mutableStateBackupTarget) !== transaction.transactionRoot ||
      path.dirname(transaction.failedMutableStateTarget) !== transaction.transactionRoot ||
      transaction.mutableStateBackupTarget === transaction.failedMutableStateTarget ||
      transaction.mutableStateBackupTarget === transaction.mutableStateTarget ||
      transaction.failedMutableStateTarget === transaction.mutableStateTarget) {
    throw new Error('mutable state rollback paths must be distinct children of the update transaction');
  }
  if (transaction.platform === 'darwin') {
    const installParent = path.dirname(transaction.installTarget);
    if (path.dirname(transaction.incomingTarget) !== installParent || path.dirname(transaction.backupTarget) !== installParent) {
      throw new Error('macOS update swap paths must share one parent directory');
    }
    if (!transaction.installTarget.endsWith('.app') || !transaction.designatedRequirement) throw new Error('invalid macOS update transaction');
  } else if (transaction.platform === 'win32') {
    if (transaction.incomingTarget !== path.join(transaction.transactionRoot, 'incoming-release') ||
        transaction.backupTarget !== path.join(transaction.transactionRoot, 'installed-before-activation')) {
      throw new Error('Windows release paths must be exact children of the update transaction');
    }
    const transactionFromInstall = path.relative(transaction.installTarget, transaction.transactionRoot);
    const installFromTransaction = path.relative(transaction.transactionRoot, transaction.installTarget);
    const nestedUnder = (relative) => relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
    if (nestedUnder(transactionFromInstall) || nestedUnder(installFromTransaction)) {
      throw new Error('Windows install and update transaction directories must not overlap');
    }
  } else {
    throw new Error(`unsupported update platform: ${transaction.platform}`);
  }
  return transaction;
}

async function startInstalledRuntime(transaction, dependencies = {}) {
  if (transaction.platform !== 'darwin') return { status: 'not-required' };
  const resourcesPath = path.join(transaction.installTarget, 'Contents', 'Resources');
  const appCode = path.join(resourcesPath, 'app');
  const resolveRuntimeProfile = dependencies.resolveRuntimeProfile ||
    require(path.join(appCode, 'runtime-profile.js')).resolveRuntimeProfile;
  const ensurePackagedDaemon = dependencies.ensurePackagedDaemon ||
    require(path.join(appCode, 'runtime-bootstrap.js')).ensurePackagedDaemon;
  const runtime = resolveRuntimeProfile({
    env: targetRuntimeEnv(process.env),
    isPackaged: true,
    resourcesPath,
    repoRoot: '',
  });
  if (`${runtime.daemonURL}/workass/health` !== transaction.daemonHealthURL ||
      `http://127.0.0.1:${runtime.viewPort}/__workass-shell/status` !== transaction.shellStatusURL) {
    throw new Error('installed runtime profile does not match the prepared update transaction');
  }
  const receipt = await ensurePackagedDaemon({ runtime, resourcesPath, forceInstall: true });
  if (!['installed-and-running', 'already-running'].includes(receipt?.status)) {
    throw new Error('installed Workass runtime did not start');
  }
  return { ...receipt, runtime };
}

function defaultOperations(transaction, dependencies = {}) {
  const launchAgentPath = String(transaction.launchAgentPath || '');
  const launchdDomain = String(transaction.launchdDomain || '');
  const mirrorWindows = dependencies.mirrorWindowsDirectory || mirrorWindowsDirectory;
  const spawnProcess = dependencies.spawnProcess || spawn;
  let launchedPID = 0;
  let expectedDaemonBind = '';
  return {
    shellExited: () => !pidAlive(transaction.shellPID),
    stopDaemonService: async () => {
      if (transaction.platform === 'darwin' && launchAgentPath && launchdDomain) {
        spawnSync('/bin/launchctl', ['bootout', launchdDomain, launchAgentPath], { stdio: 'ignore' });
      }
    },
    daemonDown: async () => !(await requestJSON(transaction.daemonHealthURL, 600)),
    verifyIncoming: async () => {
      if (transaction.platform === 'darwin') verifyMacIncoming(transaction);
      else verifyWindowsIncoming(transaction);
    },
    snapshotMutableState: async () => {
      if (fs.existsSync(transaction.mutableStateBackupTarget) || fs.existsSync(transaction.failedMutableStateTarget)) {
        throw new Error('mutable state rollback target already exists');
      }
      fs.mkdirSync(transaction.mutableStateBackupTarget, { recursive: false, mode: 0o700 });
      const existed = fs.existsSync(transaction.mutableStateTarget);
      if (existed) {
        const stat = fs.statSync(transaction.mutableStateTarget);
        if (!stat.isDirectory()) throw new Error('Workass mutable state target is not a directory');
        fs.cpSync(
          transaction.mutableStateTarget,
          path.join(transaction.mutableStateBackupTarget, 'state'),
          {
            recursive: true,
            errorOnExist: true,
            force: false,
            dereference: false,
            preserveTimestamps: true,
            verbatimSymlinks: true,
            mode: fs.constants.COPYFILE_FICLONE,
          },
        );
      }
      atomicJSON(path.join(transaction.mutableStateBackupTarget, 'snapshot.json'), {
        schemaVersion: 1,
        existed,
      });
    },
    activate: async () => {
      if (fs.existsSync(transaction.backupTarget)) throw new Error('update rollback target already exists');
      if (transaction.platform === 'win32') {
        mirrorWindows(transaction.installTarget, transaction.backupTarget);
        try {
          mirrorWindows(transaction.incomingTarget, transaction.installTarget);
        } catch (err) {
          // The old install has a complete external snapshot and the in-place
          // destination may now be partial. Tell the transaction runner that
          // rollback is mandatory before the previous runtime is relaunched.
          err.workassRollbackReady = true;
          throw err;
        }
        return;
      }
      fs.renameSync(transaction.installTarget, transaction.backupTarget);
      try {
        fs.renameSync(transaction.incomingTarget, transaction.installTarget);
      } catch (err) {
        fs.renameSync(transaction.backupTarget, transaction.installTarget);
        throw err;
      }
    },
    startRuntime: async () => {
      const receipt = await startInstalledRuntime(transaction);
      expectedDaemonBind = String(receipt?.runtime?.daemonBind || '');
      return receipt;
    },
    launchInstalled: async () => {
      const executable = transaction.platform === 'darwin'
        ? path.join(transaction.installTarget, 'Contents', 'MacOS', 'Workass')
        : path.join(transaction.installTarget, 'Workass.exe');
      const child = await spawnDetached(executable, [], {
        cwd: transaction.installTarget,
        detached: true,
        // Workass.exe is a GUI-subsystem executable and does not create a
        // console. Do not give Windows a hidden startup state that can outlive
        // Electron's own BrowserWindow show request.
        windowsHide: transaction.platform !== 'win32',
        stdio: 'ignore',
        env: {
          ...targetRuntimeEnv(process.env),
          WORKASS_CONTROLLER_RECOVERY: '1',
          // Older Windows releases interpret this flag by creating their only
          // BrowserWindow hidden. Omit it on Windows so rollback to one of
          // those releases is visible too; macOS still uses its bounded
          // hidden-until-ready path.
          ...(transaction.platform === 'darwin' ? { WORKASS_UPDATE_RELAUNCH: '1' } : {}),
        },
      }, { spawnProcess });
      launchedPID = child.pid || 0;
    },
    healthy: async (expectedVersion = transaction.targetVersion) => {
      const [daemon, shell] = await Promise.all([
        requestJSON(transaction.daemonHealthURL), requestJSON(transaction.shellStatusURL),
      ]);
      return runtimeIsHealthy({
        daemon,
        shell,
        expectedVersion,
        expectedBind: expectedDaemonBind,
        requireVisibleWindow: transaction.requireVisibleWindow === true,
      });
    },
    stopLaunched: async () => {
      await stopLaunchedProcessTree(launchedPID, { platform: transaction.platform });
      if (transaction.platform === 'darwin' && launchAgentPath && launchdDomain) {
        spawnSync('/bin/launchctl', ['bootout', launchdDomain, launchAgentPath], { stdio: 'ignore' });
      }
      // The new shell may already have started its bundled daemon. Stop that
      // exact loopback service before moving its executable tree or launching
      // the rollback release. This is required on Windows, where executable
      // files remain locked while the daemon is alive, and prevents an old app
      // from accidentally reconnecting to the failed new daemon on either OS.
      await requestDaemonShutdown(transaction.daemonHealthURL);
      const stopped = await waitUntil(async () => !(await requestJSON(transaction.daemonHealthURL, 600)), { attempts: 80, delayMs: 250 });
      if (!stopped) throw new Error('failed release daemon did not stop before rollback');
    },
    rollback: async () => {
      if (transaction.platform === 'win32') {
        mirrorWindows(transaction.backupTarget, transaction.installTarget);
        return;
      }
      if (fs.existsSync(transaction.incomingTarget)) throw new Error('failed release holding path already exists');
      if (fs.existsSync(transaction.installTarget)) {
        fs.renameSync(transaction.installTarget, transaction.incomingTarget);
      }
      try {
        fs.renameSync(transaction.backupTarget, transaction.installTarget);
      } catch (err) {
        if (!fs.existsSync(transaction.installTarget) && fs.existsSync(transaction.incomingTarget)) {
          try { await renamePathWithRetry(transaction.incomingTarget, transaction.installTarget); } catch { /* preserve original rollback error */ }
        }
        throw err;
      }
    },
    restoreMutableState: async () => {
      const snapshotReceipt = JSON.parse(fs.readFileSync(
        path.join(transaction.mutableStateBackupTarget, 'snapshot.json'),
        'utf8',
      ));
      if (snapshotReceipt?.schemaVersion !== 1 || typeof snapshotReceipt.existed !== 'boolean') {
        throw new Error('mutable state rollback snapshot is invalid');
      }
      const snapshotState = path.join(transaction.mutableStateBackupTarget, 'state');
      if (snapshotReceipt.existed && !fs.statSync(snapshotState, { throwIfNoEntry: false })?.isDirectory()) {
        throw new Error('mutable state rollback snapshot is incomplete');
      }
      if (!snapshotReceipt.existed && fs.existsSync(snapshotState)) {
        throw new Error('mutable state rollback snapshot contradicts its receipt');
      }
      if (fs.existsSync(transaction.failedMutableStateTarget)) {
        throw new Error('failed mutable state holding path already exists');
      }
      const failedStateExisted = fs.existsSync(transaction.mutableStateTarget);
      if (failedStateExisted) fs.renameSync(transaction.mutableStateTarget, transaction.failedMutableStateTarget);
      try {
        if (snapshotReceipt.existed) fs.renameSync(snapshotState, transaction.mutableStateTarget);
      } catch (err) {
        if (failedStateExisted && !fs.existsSync(transaction.mutableStateTarget)) {
          fs.renameSync(transaction.failedMutableStateTarget, transaction.mutableStateTarget);
        }
        throw err;
      }
    },
    cleanup: async () => {
      fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
      fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
      fs.rmSync(transaction.mutableStateBackupTarget, { recursive: true, force: true });
    },
    cleanupFailed: async () => {
      fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
      fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
      fs.rmSync(transaction.mutableStateBackupTarget, { recursive: true, force: true });
      fs.rmSync(transaction.failedMutableStateTarget, { recursive: true, force: true });
    },
  };
}

async function runTransaction(rawTransaction, operations) {
  const transaction = validateTransaction(rawTransaction);
  const ops = operations || defaultOperations(transaction);
  const wait = ops.waitUntil || waitUntil;
  updateReceipt(transaction, 'armed');
  if (!await wait(ops.shellExited, { attempts: 160, delayMs: 250 })) {
    return updateReceipt(transaction, 'failed', { activated: false, error: 'the old Workass shell did not exit' });
  }
  await ops.stopDaemonService();
  if (!await wait(ops.daemonDown, { attempts: 80, delayMs: 250 })) {
    try {
      await ops.launchInstalled();
      const recovered = await wait(() => ops.healthy(transaction.currentVersion), { attempts: 240, delayMs: 250 });
      return updateReceipt(transaction, recovered ? 'rollback_healthy' : 'failed', {
        activated: false,
        rolledBack: false,
        error: 'the old Workass daemon did not stop for the update',
        ...(recovered ? {} : { rollbackError: 'previous release did not recover' }),
      });
    } catch (recoveryError) {
      return updateReceipt(transaction, 'failed', {
        activated: false,
        rolledBack: false,
        error: 'the old Workass daemon did not stop for the update',
        rollbackError: String(recoveryError && recoveryError.message || recoveryError),
      });
    }
  }
  let activated = false;
  let mutableStateSnapshotted = false;
  let mutableStateRestored = false;
  try {
    await ops.verifyIncoming();
    updateReceipt(transaction, 'activating');
    await ops.snapshotMutableState();
    mutableStateSnapshotted = true;
    await ops.activate();
    activated = true;
    await ops.startRuntime?.();
    await ops.launchInstalled();
    if (await wait(() => ops.healthy(transaction.targetVersion), { attempts: 240, delayMs: 250 })) {
      let cleanupWarning = '';
      try { await ops.cleanup(); }
      catch (err) { cleanupWarning = String(err && err.message || err); }
      return updateReceipt(transaction, 'healthy', {
        activated: true,
        ...(cleanupWarning ? { cleanupWarning } : {}),
      });
    }
    throw new Error('new release did not recover daemon health, controller authority, and provider catalog');
  } catch (activationError) {
    try {
      await ops.stopLaunched();
      const rollbackReady = activated || activationError?.workassRollbackReady === true;
      if (rollbackReady) await ops.rollback();
      if (activated && mutableStateSnapshotted) {
        await ops.restoreMutableState();
        mutableStateRestored = true;
      }
      await ops.startRuntime?.();
      await ops.launchInstalled();
      const recovered = await wait(() => ops.healthy(transaction.currentVersion), { attempts: 240, delayMs: 250 });
      let cleanupWarning = '';
      if (recovered) {
        try { await ops.cleanupFailed?.(); }
        catch (err) { cleanupWarning = String(err && err.message || err); }
      }
      return updateReceipt(transaction, recovered ? 'rollback_healthy' : 'failed', {
        activated: false,
        rolledBack: rollbackReady,
        mutableStateRolledBack: mutableStateRestored,
        error: String(activationError && activationError.message || activationError),
        ...(cleanupWarning ? { cleanupWarning } : {}),
        ...(recovered ? {} : { rollbackError: 'previous release did not recover' }),
      });
    } catch (rollbackError) {
      return updateReceipt(transaction, 'failed', {
        activated: false,
        rolledBack: false,
        error: String(activationError && activationError.message || activationError),
        rollbackError: String(rollbackError && rollbackError.message || rollbackError),
      });
    }
  }
}

async function main(argv = process.argv.slice(2)) {
  if (argv[0] === '--self-test') return 0;
  if (argv[0] !== '--transaction' || !argv[1]) throw new Error('usage: update-worker.js --transaction ABSOLUTE_PATH');
  const transactionPath = path.resolve(argv[1]);
  const transaction = JSON.parse(fs.readFileSync(transactionPath, 'utf8'));
  await runTransaction(transaction);
  return 0;
}

if (require.main === module) {
  main().then((code) => { process.exitCode = code; }).catch((err) => {
    process.stderr.write(`workass update worker failed: ${err && err.message || err}\n`);
    process.exitCode = 1;
  });
}

module.exports = {
  atomicJSON,
  defaultOperations,
  main,
  mirrorWindowsDirectory,
  requestJSON,
  requestDaemonShutdown,
  renamePathWithRetry,
  spawnDetached,
  runTransaction,
  runtimeIsHealthy,
  startInstalledRuntime,
  stopLaunchedProcessTree,
  targetRuntimeEnv,
  updateReceipt,
  validateTransaction,
  verifyMacIncoming,
  verifyWindowsPE,
  verifyWindowsIncoming,
  waitUntil,
};
