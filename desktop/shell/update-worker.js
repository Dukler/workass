'use strict';

// Independent transactional updater. The shell copies this file and a pinned
// standalone Node executable outside the installation before it quits, so the
// worker never replaces code from underneath a running Electron or daemon
// process. No third-party packages are used.

const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const path = require('node:path');
const crypto = require('node:crypto');
const { spawn, spawnSync } = require('node:child_process');
const {
  PROGRESS_RECEIPT_SCHEMA_VERSION,
  TRANSACTION_SCHEMA_VERSION,
  expectedProgressExecutable,
  installedProgressExecutable,
  progressProcessOwnership,
  progressExecutableIsAllowed,
  progressOwnerReceiptPath,
  progressReceiptIsLive,
  progressReceiptProcessIsRunning,
  spawnVisibleUpdateProgress,
  terminateUpdateProgress,
} = require('./update-progress');

const delay = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const INSTALLATION_IDENTITY_FILE = '.workass-installation.json';
const RECEIPT_SCHEMA_VERSION = 2;
const JOURNAL_SCHEMA_VERSION = 1;
const LEASE_SCHEMA_VERSION = 1;
const WINDOWS_BACKUP_RECEIPT = 'installed-before-activation.complete.json';

function validInstallationId(value) {
  return /^install-[a-f0-9]{32}$/.test(String(value || ''));
}

function validWorkerId(value) {
  return /^worker-[a-f0-9]{32}$/.test(String(value || ''));
}

function installationIdentityPath(installTarget) {
  return path.join(installTarget, INSTALLATION_IDENTITY_FILE);
}

function readInstallationIdentity(installTarget) {
  try {
    const identity = JSON.parse(fs.readFileSync(installationIdentityPath(installTarget), 'utf8'));
    if (identity?.schemaVersion !== 1 || identity.product !== 'Workass' || !validInstallationId(identity.installationId)) return null;
    return identity;
  } catch {
    return null;
  }
}

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
  'WORKASS_CONTROLLER_RECOVERY', 'WORKASS_UPDATE_RELAUNCH',
  'WORKASS_LOCK_RECOVERY_CHILD', 'WORKASS_UPDATE_PROGRESS_ID',
]);

function targetRuntimeEnv(source = process.env) {
  const clean = { ...source };
  for (const key of STALE_RUNTIME_ENV_KEYS) delete clean[key];
  return clean;
}

function runtimeIsHealthy({
  daemon,
  shell,
  expectedVersion,
  expectedBind = '',
  expectedInstallationId = '',
  expectedInstallTarget = '',
  requireVisibleWindow = false,
}) {
  const sameInstallTarget = !expectedInstallTarget || (process.platform === 'win32'
    ? path.win32.normalize(String(shell?.installTarget || '')).toLowerCase() === path.win32.normalize(expectedInstallTarget).toLowerCase()
    : path.resolve(String(shell?.installTarget || '')) === path.resolve(expectedInstallTarget));
  return daemon?.app === 'workass' && daemon?.version === expectedVersion &&
    (!expectedBind || daemon?.bind === expectedBind) &&
    (!expectedInstallationId || shell?.installationId === expectedInstallationId) &&
    sameInstallTarget &&
    shell?.controller === true &&
    shell?.appVersion === expectedVersion &&
    Number.isSafeInteger(shell?.catalog?.readyModelCount) && shell.catalog.readyModelCount > 0 &&
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
  const descriptor = fs.openSync(incoming, 'w', 0o600);
  try {
    fs.writeFileSync(descriptor, `${JSON.stringify(value, null, 2)}\n`, 'utf8');
    fs.fsyncSync(descriptor);
  } finally {
    fs.closeSync(descriptor);
  }
  fs.renameSync(incoming, file);
}

function updateReceipt(transaction, phase, extra = {}) {
  const receipt = {
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    updateId: transaction.updateId,
    phase,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    workerId: transaction.workerId,
    workerPID: process.pid,
    updatedAt: new Date().toISOString(),
    ...extra,
  };
  atomicJSON(transaction.receiptPath, receipt);
  return receipt;
}

function readJournal(transaction) {
  try {
    const journal = JSON.parse(fs.readFileSync(transaction.journalPath, 'utf8'));
    if (journal?.schemaVersion !== JOURNAL_SCHEMA_VERSION || journal.updateId !== transaction.updateId ||
        journal.installationId !== transaction.installationId ||
        path.resolve(String(journal.installTarget || '')) !== path.resolve(transaction.installTarget) ||
        journal.previousVersion !== transaction.currentVersion || journal.targetVersion !== transaction.targetVersion) {
      throw new Error('update recovery journal identity does not match');
    }
    return journal;
  } catch (err) {
    if (err?.code === 'ENOENT') return null;
    throw err;
  }
}

function checkpoint(transaction, previous, phase, patch = {}) {
  const journal = {
    ...previous,
    ...patch,
    schemaVersion: JOURNAL_SCHEMA_VERSION,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    createdAt: previous?.createdAt || new Date().toISOString(),
    updatedAt: new Date().toISOString(),
    phase,
  };
  atomicJSON(transaction.journalPath, journal);
  return journal;
}

function startWorkerLease(transaction, {
  repeat = setInterval,
  cancelRepeat = clearInterval,
} = {}) {
  if (process.env.WORKASS_UPDATE_WORKER_ID !== transaction.workerId) {
    throw new Error('independent update worker identity does not match the transaction');
  }
  const base = {
    schemaVersion: LEASE_SCHEMA_VERSION,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    workerId: transaction.workerId,
    pid: process.pid,
    workerPath: path.resolve(__filename),
    transactionPath: path.join(transaction.transactionRoot, 'transaction.json'),
    startedAt: new Date().toISOString(),
  };
  const write = (state) => atomicJSON(transaction.leasePath, {
    ...base,
    state,
    updatedAt: new Date().toISOString(),
  });
  write('running');
  const timer = repeat(() => write('running'), 1000);
  timer.unref?.();
  return {
    stop(state = 'terminal') {
      cancelRepeat(timer);
      write(state);
    },
  };
}

function pidAlive(pid) {
  if (!Number.isInteger(pid) || pid <= 1) return false;
  try { process.kill(pid, 0); return true; } catch { return false; }
}

function processOwnsExecutable(pid, executable, {
  platform = process.platform,
  run = spawnSync,
} = {}) {
  if (!Number.isInteger(pid) || pid <= 1 || platform === 'win32') return false;
  const result = run('/bin/ps', ['-p', String(pid), '-o', 'command='], {
    encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'],
  });
  return !result.error && result.status === 0 && String(result.stdout || '').includes(path.resolve(executable));
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

function stopWindowsExecutableProcesses(executablePath, { run = spawnSync } = {}) {
  const target = String(executablePath || '').trim();
  if (!target || path.basename(target).toLowerCase() !== 'workass.exe') {
    throw new Error('Windows shell cleanup requires the exact Workass executable path');
  }
  // Electron's main PID can exit while renderer/GPU children remain orphaned
  // and keep files in the portable install locked. taskkill /PID can no longer
  // reach that tree once its root is gone, so select only processes whose
  // executable path exactly matches this installation. The independent updater
  // is updater-node.exe outside this directory and cannot select itself.
  const script = String.raw`
$ErrorActionPreference = 'Stop'
$target = [IO.Path]::GetFullPath($env:WORKASS_OLD_EXECUTABLE)
function Find-WorkassInstallProcesses {
  @(
    Get-Process -Name Workass -ErrorAction SilentlyContinue | Where-Object {
      try {
        $_.Path -and [StringComparer]::OrdinalIgnoreCase.Equals([IO.Path]::GetFullPath($_.Path), $target)
      } catch { $false }
    }
  )
}
$targets = @(Find-WorkassInstallProcesses)
$targets | ForEach-Object { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue }
$deadline = [DateTime]::UtcNow.AddSeconds(10)
do {
  $remaining = @(Find-WorkassInstallProcesses)
  if ($remaining.Count -eq 0) { break }
  Start-Sleep -Milliseconds 100
} while ([DateTime]::UtcNow -lt $deadline)
if ($remaining.Count -gt 0) { throw 'Workass install processes remained after cleanup' }
Write-Output "WORKASS_STOPPED=$($targets.Count)"
`;
  const result = run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
    windowsHide: true,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, WORKASS_OLD_EXECUTABLE: target },
  });
  if (result.error || result.status !== 0) {
    throw new Error('the old Workass install processes did not stop');
  }
  const match = String(result.stdout || '').match(/WORKASS_STOPPED=(\d+)/);
  if (!match) throw new Error('Windows shell cleanup returned no process receipt');
  return Number(match[1]);
}

function daemonServiceIsDown(transaction, { run = spawnSync } = {}) {
  if (transaction.platform === 'win32') {
    const executable = path.join(transaction.installTarget, 'workass-daemon.exe');
    const script = String.raw`
$ErrorActionPreference = 'Stop'
$target = [IO.Path]::GetFullPath($env:WORKASS_DAEMON_EXECUTABLE)
$running = @(
  Get-Process -Name workass-daemon -ErrorAction SilentlyContinue | Where-Object {
    try {
      $_.Path -and [StringComparer]::OrdinalIgnoreCase.Equals([IO.Path]::GetFullPath($_.Path), $target)
    } catch { $false }
  }
)
if ($running.Count -eq 0) { Write-Output 'WORKASS_DAEMON_DOWN'; exit 0 }
Write-Output 'WORKASS_DAEMON_RUNNING'
exit 3
`;
    const result = run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
      windowsHide: true,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'pipe'],
      env: { ...process.env, WORKASS_DAEMON_EXECUTABLE: executable },
    });
    return !result.error && result.status === 0 && /\bWORKASS_DAEMON_DOWN\b/.test(String(result.stdout || ''));
  }
  if (transaction.platform === 'darwin') {
    const launchAgentPath = String(transaction.launchAgentPath || '');
    const launchdDomain = String(transaction.launchdDomain || '');
    const label = path.basename(launchAgentPath, '.plist');
    if (!launchAgentPath || !launchdDomain || !label) return false;
    const result = run('/bin/launchctl', ['print', `${launchdDomain}/${label}`], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    });
    if (result.error || result.status === 0) return false;
    return result.status === 113 || /could not find service/i.test(String(result.stderr || ''));
  }
  return false;
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
    if (await predicate(attempt)) return true;
    await delay(delayMs);
  }
  return false;
}

async function waitUntilDeadline(predicate, {
  timeoutMs,
  delayMs = 250,
  maxProbeMs = 1500,
  now = Date.now,
  pause = delay,
  maxAttempts = Math.ceil(timeoutMs / Math.max(1, delayMs)) + 2,
} = {}) {
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) throw new Error('bounded wait requires a positive timeout');
  const deadline = now() + timeoutMs;
  for (let attempt = 0; attempt < maxAttempts; attempt += 1) {
    const remaining = deadline - now();
    if (remaining <= 0) return false;
    if (await predicate(Math.max(1, Math.min(maxProbeMs, remaining)), attempt)) return true;
    const afterProbe = deadline - now();
    if (afterProbe <= 0) return false;
    await pause(Math.min(delayMs, afterProbe));
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

function requestUpdateCancel(healthURL, updateId, timeoutMs = 1500) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL('/workass/update/cancel', healthURL); } catch { resolve(0); return; }
    if (!['127.0.0.1', 'localhost', '::1'].includes(parsed.hostname) ||
        !/^[A-Za-z0-9_-]{8,96}$/.test(String(updateId || ''))) {
      resolve(0);
      return;
    }
    const transport = parsed.protocol === 'https:' ? https : http;
    const body = Buffer.from(JSON.stringify({ updateId }));
    const request = transport.request(parsed, {
      method: 'POST',
      timeout: timeoutMs,
      rejectUnauthorized: false,
      headers: {
        'content-type': 'application/json',
        'content-length': body.length,
      },
    }, (response) => {
      response.resume();
      response.on('end', () => resolve(response.statusCode || 0));
    });
    request.on('timeout', () => { request.destroy(); resolve(0); });
    request.on('error', () => resolve(0));
    request.end(body);
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
  requiredRegularFile(path.join(transaction.incomingTarget, 'Contents', 'Resources', 'app', 'update-progress.js'), 'update progress process');
}

function requiredRegularFile(file, label) {
  const stat = fs.statSync(file, { throwIfNoEntry: false });
  if (!stat?.isFile() || stat.size <= 0) throw new Error(`incoming release has no ${label}`);
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
    [['resources', 'app', 'update-progress.js'], 'update progress process'],
    [['resources', 'app', 'update-lock-recovery.js'], 'update lock recovery'],
    [['resources', 'renderer', 'index.html'], 'renderer'],
    [['frontier-hosts', 'windows-amd64', 'claude-native-host.mjs'], 'Claude host'],
    [['frontier-hosts', 'windows-amd64', 'codex-native-host.mjs'], 'Codex host'],
    [['frontier-hosts', 'windows-amd64', 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'], 'Claude Agent SDK'],
  ]) requiredRegularFile(path.join(root, ...relative), label);
  const identity = readInstallationIdentity(root);
  if (!identity || identity.installationId !== transaction.installationId) {
    throw new Error('incoming Windows release does not belong to this portable installation');
  }
}

function verifyMacTree(transaction, root, version) {
  verifyMacIncoming({ ...transaction, incomingTarget: root, targetVersion: version });
}

function verifyWindowsTree(transaction, root, version) {
  verifyWindowsIncoming({ ...transaction, incomingTarget: root, targetVersion: version });
}

function windowsBackupReceiptPath(transaction) {
  return path.join(transaction.transactionRoot, WINDOWS_BACKUP_RECEIPT);
}

function validWindowsBackupReceipt(transaction) {
  try {
    const receipt = JSON.parse(fs.readFileSync(windowsBackupReceiptPath(transaction), 'utf8'));
    return receipt?.schemaVersion === 1 && receipt.updateId === transaction.updateId &&
      receipt.installationId === transaction.installationId &&
      receipt.version === transaction.currentVersion &&
      path.resolve(String(receipt.source || '')) === path.resolve(transaction.installTarget) &&
      path.resolve(String(receipt.backup || '')) === path.resolve(transaction.backupTarget) &&
      receipt.mirrorCompleted === true;
  } catch {
    return false;
  }
}

function commitWindowsBackupReceipt(transaction) {
  atomicJSON(windowsBackupReceiptPath(transaction), {
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    version: transaction.currentVersion,
    source: transaction.installTarget,
    backup: transaction.backupTarget,
    mirrorCompleted: true,
    completedAt: new Date().toISOString(),
  });
}

function validateTransaction(transaction) {
  if (!transaction || transaction.schemaVersion !== TRANSACTION_SCHEMA_VERSION) throw new Error('unsupported update transaction');
  if (!/^[A-Za-z0-9_-]{8,96}$/.test(String(transaction.updateId || ''))) throw new Error('update transaction has an invalid updateId');
  if (!validInstallationId(transaction.installationId)) throw new Error('update transaction has an invalid installation identity');
  if (!validWorkerId(transaction.workerId)) throw new Error('update transaction has an invalid worker identity');
  if (!/^progress-[a-f0-9]{32}$/.test(String(transaction.progressId || ''))) throw new Error('update transaction has an invalid progress identity');
  if (transaction.requireVisibleWindow !== true) throw new Error('update transaction must require a visible shell window');
  for (const field of [
    'updateId', 'platform', 'currentVersion', 'targetVersion',
    'transactionRoot', 'installTarget', 'incomingTarget', 'backupTarget',
    'mutableStateTarget', 'mutableStateBackupTarget', 'failedMutableStateTarget',
    'receiptPath', 'journalPath', 'leasePath', 'workerPath', 'workerRuntimePath',
    'progressModulePath', 'progressReceiptPath', 'progressExecutable',
    'daemonHealthURL', 'shellStatusURL',
  ]) {
    if (!String(transaction[field] || '').trim()) throw new Error(`update transaction is missing ${field}`);
  }
  for (const field of [
    'transactionRoot', 'installTarget', 'incomingTarget', 'backupTarget',
    'mutableStateTarget', 'mutableStateBackupTarget', 'failedMutableStateTarget',
    'receiptPath', 'journalPath', 'leasePath', 'workerPath', 'workerRuntimePath',
    'progressModulePath', 'progressReceiptPath', 'progressExecutable',
  ]) {
    if (!path.isAbsolute(transaction[field])) throw new Error(`${field} must be absolute`);
  }
  const updateRoot = path.dirname(path.dirname(transaction.transactionRoot));
  const dataRoot = path.dirname(updateRoot);
  if (transaction.transactionRoot !== path.join(updateRoot, 'transactions', transaction.updateId) ||
      transaction.receiptPath !== path.join(updateRoot, 'receipt.json') ||
      transaction.journalPath !== path.join(transaction.transactionRoot, 'journal.json') ||
      transaction.leasePath !== path.join(transaction.transactionRoot, 'worker-lease.json') ||
      transaction.workerPath !== path.join(transaction.transactionRoot, 'update-worker.js') ||
      transaction.progressModulePath !== path.join(transaction.transactionRoot, 'update-progress.js') ||
      transaction.workerRuntimePath !== path.join(transaction.transactionRoot, transaction.platform === 'win32' ? 'updater-node.exe' : 'updater-node') ||
      transaction.progressReceiptPath !== path.join(transaction.transactionRoot, 'progress-receipt.json') ||
      !progressExecutableIsAllowed(transaction, transaction.progressExecutable)) {
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
  const releaseTargets = [transaction.installTarget, transaction.incomingTarget, transaction.backupTarget]
    .map((target) => path.resolve(target));
  if (new Set(releaseTargets).size !== releaseTargets.length) {
    throw new Error('installed, incoming, and rollback release paths must be distinct');
  }
  if (transaction.platform === 'darwin') {
    const installParent = path.dirname(transaction.installTarget);
    if (path.dirname(transaction.incomingTarget) !== installParent || path.dirname(transaction.backupTarget) !== installParent ||
        transaction.incomingTarget !== path.join(installParent, `.Workass.app.incoming-${transaction.updateId}`) ||
        transaction.backupTarget !== path.join(installParent, `.Workass.app.previous-${transaction.updateId}`)) {
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
  if (!['darwin', 'win32'].includes(transaction.platform)) return { status: 'not-required' };
  const resourcesPath = transaction.platform === 'darwin'
    ? path.join(transaction.installTarget, 'Contents', 'Resources')
    : path.join(transaction.installTarget, 'resources');
  const appCode = path.join(resourcesPath, 'app');
  const resolveRuntimeProfile = dependencies.resolveRuntimeProfile ||
    require(path.join(appCode, 'runtime-profile.js')).resolveRuntimeProfile;
  const bootstrap = dependencies.runtimeBootstrap || {};
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
  const receipt = transaction.platform === 'darwin'
    ? await (dependencies.ensurePackagedDaemon || bootstrap.ensurePackagedDaemon ||
        require(path.join(appCode, 'runtime-bootstrap.js')).ensurePackagedDaemon)({
        runtime, resourcesPath, forceInstall: true,
      })
    : await (dependencies.ensurePortableDaemon || bootstrap.ensurePortableDaemon ||
        require(path.join(appCode, 'runtime-bootstrap.js')).ensurePortableDaemon)({
        runtime,
        resourcesPath,
        executablePath: path.join(transaction.installTarget, 'Workass.exe'),
        platform: 'win32',
      });
  if (!['installed-and-running', 'started-and-running', 'already-running'].includes(receipt?.status)) {
    throw new Error('installed Workass runtime did not start');
  }
  return { ...receipt, runtime };
}

async function launchUntilHealthy(ops, expectedVersion, {
  updateRelaunch = true,
  attempts = 480,
  delayMs = 250,
  relaunchIntervalMs = 5000,
  timeoutMs = 120000,
  now = Date.now,
  progressVisible = null,
} = {}) {
  await ops.launchInstalled({ updateRelaunch });
  let nextRelaunchAt = now() + relaunchIntervalMs;
  const pause = ops.pause || delay;
  const deadline = now() + timeoutMs;
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const remaining = deadline - now();
    if (remaining <= 0) return false;
    if (await ops.healthy(expectedVersion, Math.max(1, Math.min(1500, remaining)))) return true;
    if (progressVisible && !await progressVisible()) {
      const error = new Error('the verified update progress window stopped before Workass recovered');
      error.code = 'WORKASS_UPDATE_PROGRESS_LOST';
      throw error;
    }
    const current = now();
    if (current >= deadline) return false;
    if (current >= nextRelaunchAt) {
      // A stale Chromium singleton can outlive the old main PID. Re-launching
      // is safe: a live primary receives Electron's second-instance event and
      // shows itself; once the stale lock disappears, one retry becomes the
      // new primary instead of leaving Workass closed forever.
      await ops.launchInstalled({ updateRelaunch });
      nextRelaunchAt = current + relaunchIntervalMs;
    }
    await pause(Math.min(delayMs, Math.max(0, deadline - now())));
  }
  return false;
}

async function replaceVisibleProgress(transaction, {
  readReceipt = (file) => {
    try { return JSON.parse(fs.readFileSync(file, 'utf8')); } catch { return null; }
  },
  inspectOwnership = progressProcessOwnership,
  terminateProgress = terminateUpdateProgress,
  spawnProgress = spawnVisibleUpdateProgress,
  createProgressId = () => `progress-${crypto.randomBytes(16).toString('hex')}`,
} = {}) {
  const inspectedPids = new Set();
  for (const receiptPath of [transaction.progressReceiptPath, progressOwnerReceiptPath(transaction)]) {
    const priorReceipt = readReceipt(receiptPath);
    if (!Number.isInteger(priorReceipt?.pid) || inspectedPids.has(priorReceipt.pid)) continue;
    inspectedPids.add(priorReceipt.pid);
    const ownership = inspectOwnership(priorReceipt, transaction, { platform: transaction.platform });
    if (ownership.running && (!ownership.exact || !await terminateProgress(ownership, { platform: transaction.platform }))) {
      return false;
    }
  }
  const stagedExecutable = expectedProgressExecutable(transaction);
  const installedExecutable = installedProgressExecutable(transaction);
  const executable = fs.statSync(stagedExecutable, { throwIfNoEntry: false })?.isFile()
    ? stagedExecutable
    : transaction.platform === 'darwin' && fs.statSync(installedExecutable, { throwIfNoEntry: false })?.isFile()
      ? installedExecutable
      : '';
  if (!executable) return false;
  transaction.progressId = createProgressId();
  transaction.progressExecutable = executable;
  transaction.recoveryAttempt = Number(transaction.recoveryAttempt || 0) + 1;
  atomicJSON(path.join(transaction.transactionRoot, 'transaction.json'), transaction);
  try { fs.rmSync(transaction.progressReceiptPath, { force: true }); } catch {}
  try { fs.rmSync(progressOwnerReceiptPath(transaction), { force: true }); } catch {}
  try {
    await spawnProgress({
      command: executable,
      args: ['--workass-update-progress', path.join(transaction.transactionRoot, 'transaction.json')],
      options: {
        cwd: path.dirname(executable),
        detached: true,
        windowsHide: false,
        stdio: 'ignore',
        env: { ...process.env, WORKASS_UPDATE_PROGRESS_ID: transaction.progressId },
      },
      transaction,
    });
    return true;
  } catch {
    return false;
  }
}

function defaultOperations(transaction, dependencies = {}) {
  const launchAgentPath = String(transaction.launchAgentPath || '');
  const launchdDomain = String(transaction.launchdDomain || '');
  const mirrorWindows = dependencies.mirrorWindowsDirectory || mirrorWindowsDirectory;
  const stopWindowsProcesses = dependencies.stopWindowsExecutableProcesses || stopWindowsExecutableProcesses;
  const verifyMac = dependencies.verifyMacTree || verifyMacTree;
  const verifyWindows = dependencies.verifyWindowsTree || verifyWindowsTree;
  const spawnProcess = dependencies.spawnProcess || spawn;
  const shutdownDaemon = dependencies.requestDaemonShutdown || requestDaemonShutdown;
  const cancelUpdate = dependencies.requestUpdateCancel || requestUpdateCancel;
  const stopProcessTree = dependencies.stopLaunchedProcessTree || stopLaunchedProcessTree;
  const ownsExecutable = dependencies.processOwnsExecutable || processOwnsExecutable;
  const exactDaemonDown = dependencies.daemonServiceIsDown || daemonServiceIsDown;
  const launchedProcesses = new Map();
  let expectedDaemonBind = '';
  return {
    progressVisible: async () => {
      let receipt = null;
      try { receipt = JSON.parse(fs.readFileSync(transaction.progressReceiptPath, 'utf8')); } catch { return false; }
      return progressReceiptIsLive(receipt, transaction);
    },
    replaceProgress: () => replaceVisibleProgress(transaction),
    shellExited: () => !pidAlive(transaction.shellPID),
    stopDaemonService: async () => {
      if (transaction.platform === 'darwin' && launchAgentPath && launchdDomain) {
        spawnSync('/bin/launchctl', ['bootout', launchdDomain, launchAgentPath], { stdio: 'ignore' });
      }
      // Commit normally asks the daemon to exit first. This exact loopback
      // shutdown is also the idempotent recovery path when the commit request
      // itself never reached the Windows daemon: the worker owns the already
      // quiescent transaction and must not wait forever on a no-op service stop.
      await shutdownDaemon(transaction.daemonHealthURL);
    },
    clearUpdateFence: async () => {
      let status = await cancelUpdate(transaction.daemonHealthURL, transaction.updateId);
      if (status === 0) status = await cancelUpdate(transaction.daemonHealthURL, transaction.updateId);
      return status === 200;
    },
    // Health silence is not process death: a hung live daemon can still hold
    // Windows files and retain update admission. Only the exact executable or
    // launchd service disappearing permits activation.
    daemonDown: async () => exactDaemonDown(transaction),
    stopOldShell: async () => {
      if (transaction.platform === 'win32') {
        return stopWindowsProcesses(path.join(transaction.installTarget, 'Workass.exe'));
      }
      await stopLaunchedProcessTree(transaction.shellPID, { platform: transaction.platform });
      return 1;
    },
    verifyIncoming: async () => {
      if (transaction.platform === 'darwin') verifyMacIncoming(transaction);
      else verifyWindowsIncoming(transaction);
    },
    snapshotMutableState: async () => {
      const snapshotFile = path.join(transaction.mutableStateBackupTarget, 'snapshot.json');
      if (fs.existsSync(transaction.mutableStateBackupTarget)) {
        try {
          const prior = JSON.parse(fs.readFileSync(snapshotFile, 'utf8'));
          const priorState = path.join(transaction.mutableStateBackupTarget, 'state');
          if (prior?.schemaVersion === 1 && typeof prior.existed === 'boolean' &&
              ((prior.existed && fs.statSync(priorState, { throwIfNoEntry: false })?.isDirectory()) ||
               (!prior.existed && !fs.existsSync(priorState)))) return;
        } catch { /* an incomplete pre-activation snapshot is safe to rebuild */ }
        fs.rmSync(transaction.mutableStateBackupTarget, { recursive: true, force: true });
      }
      if (fs.existsSync(transaction.failedMutableStateTarget)) {
        throw new Error('mutable state failure holding path exists before activation');
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
      atomicJSON(snapshotFile, {
        schemaVersion: 1,
        existed,
      });
    },
    activate: async () => {
      if (transaction.platform === 'win32') {
        if (!validWindowsBackupReceipt(transaction)) {
          // A backup directory without this atomic receipt can only be an
          // interrupted mirror. It is safe to rebuild while the exact old
          // install still verifies; once target mutation starts the receipt
          // has already been committed and must never be synthesized.
          verifyWindows(transaction, transaction.installTarget, transaction.currentVersion);
          fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
          fs.rmSync(windowsBackupReceiptPath(transaction), { force: true });
          mirrorWindows(transaction.installTarget, transaction.backupTarget);
          verifyWindows(transaction, transaction.backupTarget, transaction.currentVersion);
          commitWindowsBackupReceipt(transaction);
        }
        if (!validWindowsBackupReceipt(transaction)) throw new Error('Windows rollback snapshot has no durable completion receipt');
        verifyWindows(transaction, transaction.backupTarget, transaction.currentVersion);
        try {
          mirrorWindows(transaction.incomingTarget, transaction.installTarget);
          verifyWindows(transaction, transaction.installTarget, transaction.targetVersion);
        } catch (err) {
          // The old install has a complete external snapshot and the in-place
          // destination may now be partial. Tell the transaction runner that
          // rollback is mandatory before the previous runtime is relaunched.
          err.workassRollbackReady = true;
          throw err;
        }
        return;
      }
      if (fs.existsSync(transaction.backupTarget)) {
        verifyMac(transaction, transaction.backupTarget, transaction.currentVersion);
        try {
          if (fs.existsSync(transaction.installTarget)) {
            verifyMac(transaction, transaction.installTarget, transaction.targetVersion);
            return;
          }
          if (!fs.existsSync(transaction.incomingTarget)) throw new Error('incoming app disappeared during activation recovery');
          fs.renameSync(transaction.incomingTarget, transaction.installTarget);
          verifyMac(transaction, transaction.installTarget, transaction.targetVersion);
          return;
        } catch (err) {
          // The previous app was verified before entering this block. From
          // here onward the active path may be absent or partial, so recovery
          // must restore that exact backup before relaunching.
          err.workassRollbackReady = true;
          throw err;
        }
      }
      verifyMac(transaction, transaction.installTarget, transaction.currentVersion);
      fs.renameSync(transaction.installTarget, transaction.backupTarget);
      try {
        fs.renameSync(transaction.incomingTarget, transaction.installTarget);
        verifyMac(transaction, transaction.installTarget, transaction.targetVersion);
      } catch (err) {
        if (!fs.existsSync(transaction.installTarget) && fs.existsSync(transaction.backupTarget)) {
          try { fs.renameSync(transaction.backupTarget, transaction.installTarget); }
          catch { err.workassRollbackReady = true; }
        }
        if (fs.existsSync(transaction.backupTarget)) err.workassRollbackReady = true;
        throw err;
      }
    },
    startRuntime: async () => {
      const receipt = await startInstalledRuntime(transaction);
      expectedDaemonBind = String(receipt?.runtime?.daemonBind || '');
      return receipt;
    },
    launchInstalled: async ({ updateRelaunch = transaction.platform === 'darwin' } = {}) => {
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
          // Target releases use this to surface a responsive recovery window
          // before daemon/provider hydration. Rollback launches omit it on
          // Windows because older builds interpreted it as "stay hidden".
          ...(updateRelaunch ? { WORKASS_UPDATE_RELAUNCH: '1' } : {}),
        },
      }, { spawnProcess });
      if (child.pid) {
        launchedProcesses.set(child.pid, { child, executable });
        child.once?.('exit', () => { launchedProcesses.delete(child.pid); });
      }
    },
    healthy: async (expectedVersion = transaction.targetVersion, timeoutMs = 1500) => {
      const [daemon, shell] = await Promise.all([
        requestJSON(transaction.daemonHealthURL, timeoutMs), requestJSON(transaction.shellStatusURL, timeoutMs),
      ]);
      return runtimeIsHealthy({
        daemon,
        shell,
        expectedVersion,
        expectedBind: expectedDaemonBind,
        expectedInstallationId: transaction.installationId,
        expectedInstallTarget: transaction.installTarget,
        requireVisibleWindow: transaction.requireVisibleWindow === true,
      });
    },
    stopLaunched: async () => {
      if (transaction.platform === 'win32') {
        launchedProcesses.clear();
        stopWindowsProcesses(path.join(transaction.installTarget, 'Workass.exe'));
      } else {
        for (const [pid, launched] of Array.from(launchedProcesses.entries()).reverse()) {
          if (launched.child.exitCode !== null || launched.child.signalCode !== null ||
              !ownsExecutable(pid, launched.executable, { platform: transaction.platform })) {
            launchedProcesses.delete(pid);
            continue;
          }
          await stopProcessTree(pid, { platform: transaction.platform });
          launchedProcesses.delete(pid);
        }
      }
      if (transaction.platform === 'darwin' && launchAgentPath && launchdDomain) {
        spawnSync('/bin/launchctl', ['bootout', launchdDomain, launchAgentPath], { stdio: 'ignore' });
      }
      // The new shell may already have started its bundled daemon. Stop that
      // exact loopback service before moving its executable tree or launching
      // the rollback release. This is required on Windows, where executable
      // files remain locked while the daemon is alive, and prevents an old app
      // from accidentally reconnecting to the failed new daemon on either OS.
      await shutdownDaemon(transaction.daemonHealthURL);
      const stopped = await waitUntil(() => exactDaemonDown(transaction), { attempts: 80, delayMs: 250 });
      if (!stopped) throw new Error('failed release daemon did not stop before rollback');
    },
    rollback: async () => {
      if (transaction.platform === 'win32') {
        if (!validWindowsBackupReceipt(transaction)) throw new Error('Windows rollback snapshot is incomplete');
        verifyWindows(transaction, transaction.backupTarget, transaction.currentVersion);
        mirrorWindows(transaction.backupTarget, transaction.installTarget);
        verifyWindows(transaction, transaction.installTarget, transaction.currentVersion);
        return;
      }
      if (!fs.existsSync(transaction.backupTarget)) {
        verifyMac(transaction, transaction.installTarget, transaction.currentVersion);
        return;
      }
      verifyMac(transaction, transaction.backupTarget, transaction.currentVersion);
      if (fs.existsSync(transaction.installTarget) && !fs.existsSync(transaction.incomingTarget)) {
        fs.renameSync(transaction.installTarget, transaction.incomingTarget);
      }
      if (fs.existsSync(transaction.installTarget)) {
        throw new Error('failed release holding path and installed app conflict during rollback');
      }
      try {
        fs.renameSync(transaction.backupTarget, transaction.installTarget);
        verifyMac(transaction, transaction.installTarget, transaction.currentVersion);
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
      const snapshotStateExists = fs.statSync(snapshotState, { throwIfNoEntry: false })?.isDirectory() === true;
      if (snapshotReceipt.existed && !snapshotStateExists &&
          !(fs.existsSync(transaction.failedMutableStateTarget) && fs.existsSync(transaction.mutableStateTarget))) {
        throw new Error('mutable state rollback snapshot is incomplete');
      }
      if (!snapshotReceipt.existed && snapshotStateExists) {
        throw new Error('mutable state rollback snapshot contradicts its receipt');
      }
      if (snapshotReceipt.existed && !snapshotStateExists &&
          fs.existsSync(transaction.failedMutableStateTarget) && fs.existsSync(transaction.mutableStateTarget)) {
        return;
      }
      if (!snapshotReceipt.existed && fs.existsSync(transaction.failedMutableStateTarget) &&
          !fs.existsSync(transaction.mutableStateTarget)) return;
      if (!fs.existsSync(transaction.failedMutableStateTarget) && fs.existsSync(transaction.mutableStateTarget)) {
        fs.renameSync(transaction.mutableStateTarget, transaction.failedMutableStateTarget);
      }
      try {
        if (snapshotReceipt.existed && fs.existsSync(snapshotState)) fs.renameSync(snapshotState, transaction.mutableStateTarget);
      } catch (err) {
        if (fs.existsSync(transaction.failedMutableStateTarget) && !fs.existsSync(transaction.mutableStateTarget)) {
          fs.renameSync(transaction.failedMutableStateTarget, transaction.mutableStateTarget);
        }
        throw err;
      }
    },
    cleanup: async () => {
      const progressRunning = [transaction.progressReceiptPath, progressOwnerReceiptPath(transaction)]
        .some((receiptPath) => {
          try { return progressReceiptProcessIsRunning(JSON.parse(fs.readFileSync(receiptPath, 'utf8')), transaction); }
          catch { return false; }
        });
      if (!progressRunning) {
        fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
      }
      fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
      fs.rmSync(windowsBackupReceiptPath(transaction), { force: true });
      fs.rmSync(transaction.mutableStateBackupTarget, { recursive: true, force: true });
    },
    cleanupFailed: async () => {
      const progressRunning = [transaction.progressReceiptPath, progressOwnerReceiptPath(transaction)]
        .some((receiptPath) => {
          try { return progressReceiptProcessIsRunning(JSON.parse(fs.readFileSync(receiptPath, 'utf8')), transaction); }
          catch { return false; }
        });
      if (!progressRunning) {
        fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
      }
      fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
      fs.rmSync(windowsBackupReceiptPath(transaction), { force: true });
      fs.rmSync(transaction.mutableStateBackupTarget, { recursive: true, force: true });
      fs.rmSync(transaction.failedMutableStateTarget, { recursive: true, force: true });
    },
  };
}

async function runTransaction(rawTransaction, operations) {
  const transaction = validateTransaction(rawTransaction);
  const ops = operations || defaultOperations(transaction);
  const wait = ops.waitUntil || waitUntil;
  const waitForDeadline = ops.waitUntilDeadline || waitUntilDeadline;
  const lease = operations ? null : startWorkerLease(transaction);
  let terminal = false;
  let journal = readJournal(transaction);
  const progressVisible = typeof ops.progressVisible === 'function' ? ops.progressVisible : async () => true;
  const progressReady = async () => {
    if (await progressVisible()) return true;
    if (typeof ops.replaceProgress !== 'function' || !await ops.replaceProgress()) return false;
    return progressVisible();
  };

  const save = (phase, patch = {}) => {
    journal = checkpoint(transaction, journal, phase, patch);
    return journal;
  };
  const finish = (phase, extra = {}) => {
    const installedVersion = phase === 'healthy' ? transaction.targetVersion : transaction.currentVersion;
    save(phase, { terminal: true, installedVersion, ...extra });
    terminal = true;
    return updateReceipt(transaction, phase, { installedVersion, ...extra });
  };
  const settleBeforeDaemonStop = async (error, extra = {}) => {
    let fenceCleared = false;
    try {
      fenceCleared = await ops.daemonDown(600) ||
        (typeof ops.clearUpdateFence === 'function' && await ops.clearUpdateFence());
    } catch { fenceCleared = false; }
    if (!fenceCleared) {
      save('admission_fence_pending', { terminal: false, daemonStopped: false, error, ...extra });
      return updateReceipt(transaction, 'activating', {
        installedVersion: transaction.currentVersion,
        activated: false,
        error,
        ...extra,
      });
    }
    save('daemon_fence_cleared', { daemonFenceCleared: true });
    return finish('failed', { activated: false, error, ...extra });
  };
  const requireProgress = async () => {
    if (await progressReady()) return;
    const error = new Error('the verified update progress window stopped before Workass recovered');
    error.code = 'WORKASS_UPDATE_PROGRESS_LOST';
    throw error;
  };
  const failBeforeDestructiveWork = async (cause) => {
    const error = String(cause && cause.message || cause || 'the update progress window stopped');
    try {
      const daemonAlreadyDown = await ops.daemonDown(600);
      if (!daemonAlreadyDown) {
        if (typeof ops.clearUpdateFence !== 'function' || !await ops.clearUpdateFence()) {
          save('admission_fence_pending', {
            terminal: false,
            daemonStopped: false,
            error: 'the daemon update admission fence did not clear after the progress owner was lost',
          });
          return updateReceipt(transaction, 'activating', {
            installedVersion: transaction.currentVersion,
            activated: false,
            error: 'the daemon update admission fence did not clear after the progress owner was lost',
          });
        }
        save('daemon_fence_cleared', { daemonFenceCleared: true, error });
      }
      if (daemonAlreadyDown) await ops.startRuntime?.();
      await ops.launchInstalled({ updateRelaunch: transaction.platform === 'darwin' });
      return finish('failed', {
        activated: false,
        rolledBack: false,
        error,
        recoveryRelaunched: true,
      });
    } catch (launchError) {
      return finish('failed', {
        activated: false,
        rolledBack: false,
        error,
        rollbackError: String(launchError && launchError.message || launchError),
      });
    }
  };
  const deferRecoveryUntilVisible = (cause) => {
    const error = String(cause && cause.message || cause || 'the update progress window stopped');
    save('recovery_owner_unavailable', {
      terminal: false,
      error,
      recoveryOwnerUnavailable: true,
    });
    return updateReceipt(transaction, 'activating', {
      installedVersion: journal.rolledBack ? transaction.currentVersion
        : journal.activated ? transaction.targetVersion : transaction.currentVersion,
      activated: Boolean(journal.activated && !journal.rolledBack),
      error,
      recoveryOwnerUnavailable: true,
    });
  };

  try {
    if (!journal) journal = save('armed', {
      shellStopped: false,
      daemonStopped: false,
      incomingVerified: false,
      mutableStateSnapshotted: false,
      activated: false,
      healthVerified: false,
      rollbackStarted: false,
      rolledBack: false,
      mutableStateRestored: false,
      terminal: false,
    });
    if (journal.terminal) {
      terminal = true;
      const receipt = (() => {
        try { return JSON.parse(fs.readFileSync(transaction.receiptPath, 'utf8')); } catch { return null; }
      })();
      if (receipt?.schemaVersion === RECEIPT_SCHEMA_VERSION && receipt.updateId === transaction.updateId &&
          receipt.phase === journal.phase && receipt.installationId === transaction.installationId &&
          path.resolve(String(receipt.installTarget || '')) === path.resolve(transaction.installTarget) &&
          receipt.previousVersion === transaction.currentVersion && receipt.targetVersion === transaction.targetVersion) return receipt;
      return updateReceipt(transaction, journal.phase, {
        installedVersion: journal.installedVersion,
        activated: journal.phase === 'healthy',
        ...(journal.error ? { error: journal.error } : {}),
        ...(journal.rollbackError ? { rollbackError: journal.rollbackError } : {}),
      });
    }
    updateReceipt(transaction, journal.activated || journal.rollbackStarted ? 'activating' : 'armed');

    if (!journal.shellStopped) {
      const gracefulShellAttempts = transaction.platform === 'win32' ? 20 : 160;
      const shellExitedGracefully = await wait(ops.shellExited, { attempts: gracefulShellAttempts, delayMs: 250 });
      let oldShellForced = Boolean(journal.oldShellForced);
      if (transaction.platform === 'win32') {
        try {
          oldShellForced = Number(await ops.stopOldShell()) > 0 || oldShellForced;
        } catch (err) {
          return await settleBeforeDaemonStop(
            `the old Workass shell process tree did not stop: ${String(err && err.message || err)}`,
          );
        }
        if (!await wait(ops.shellExited, { attempts: 40, delayMs: 250 })) {
          return await settleBeforeDaemonStop('the old Workass shell did not exit');
        }
      } else if (!shellExitedGracefully) {
        return await settleBeforeDaemonStop('the old Workass shell did not exit');
      }
      save('shell_stopped', { shellStopped: true, oldShellForced });
    }

    try { await requireProgress(); }
    catch (progressError) { return await failBeforeDestructiveWork(progressError); }

    if (!journal.daemonStopped) {
      await ops.stopDaemonService();
      if (!await waitForDeadline((probeTimeoutMs) => ops.daemonDown(probeTimeoutMs), {
        timeoutMs: 30000,
        delayMs: 250,
        maxProbeMs: 600,
        ...(ops.pause ? { pause: ops.pause } : {}),
      })) {
        let recoveryPhase;
        let recoveryExtra;
        try {
          // If the commit request never reached the daemon, recovery shutdown
          // may also fail while the exact prepare drain remains active. A
          // healthy-looking old shell is not usable in that state: prove that
          // this transaction's admission fence was cancelled before recovery
          // can be classified as healthy.
          if (typeof ops.clearUpdateFence !== 'function' || !await ops.clearUpdateFence()) {
            const error = 'the daemon update admission fence did not clear';
            save('admission_fence_pending', { error, daemonStopped: false, terminal: false });
            return updateReceipt(transaction, 'activating', {
              installedVersion: transaction.currentVersion,
              activated: false,
              error,
            });
          }
          save('daemon_fence_cleared', { daemonFenceCleared: true });
          const recovered = await launchUntilHealthy(ops, transaction.currentVersion, {
            updateRelaunch: transaction.platform === 'darwin',
          });
          recoveryPhase = recovered ? 'rollback_healthy' : 'failed';
          recoveryExtra = {
            activated: false,
            rolledBack: false,
            ...(journal.oldShellForced ? { oldShellForced: true } : {}),
            error: 'the old Workass daemon did not stop for the update',
            ...(recovered ? {} : { rollbackError: 'previous release did not recover' }),
          };
        } catch (recoveryError) {
          recoveryPhase = 'failed';
          recoveryExtra = {
            activated: false,
            rolledBack: false,
            error: 'the old Workass daemon did not stop for the update',
            rollbackError: String(recoveryError && recoveryError.message || recoveryError),
          };
        }
        return finish(recoveryPhase, recoveryExtra);
      }
      save('daemon_stopped', { daemonStopped: true });
    }

    const recoverActivation = async (activationError) => {
      const error = String(activationError && activationError.message || activationError || journal.error || 'update activation failed');
      let recoveryPhase;
      let recoveryExtra;
      try {
        await requireProgress();
        if (!journal.rollbackStarted) {
          save('rollback_started', {
            rollbackStarted: true,
            rollbackReady: journal.activated || activationError?.workassRollbackReady === true,
            error,
          });
        }
        await requireProgress();
        await ops.stopLaunched();
        if (journal.rollbackReady && !journal.rolledBack) {
          await requireProgress();
          await ops.rollback();
          save('rolled_back', { rolledBack: true });
        }
        if (journal.mutableStateSnapshotted && !journal.mutableStateRestored) {
          await requireProgress();
          if (!journal.stateRestoreStarted) save('restoring_state', { stateRestoreStarted: true });
          await ops.restoreMutableState();
          save('state_restored', { mutableStateRestored: true });
        }
        await requireProgress();
        await ops.startRuntime?.();
        const recovered = await launchUntilHealthy(ops, transaction.currentVersion, {
          updateRelaunch: transaction.platform === 'darwin',
          progressVisible: progressReady,
        });
        let cleanupWarning = '';
        if (recovered) {
          try { await ops.cleanupFailed?.(); }
          catch (err) { cleanupWarning = String(err && err.message || err); }
        }
        recoveryPhase = recovered ? 'rollback_healthy' : 'failed';
        recoveryExtra = {
          activated: false,
          rolledBack: Boolean(journal.rolledBack),
          mutableStateRolledBack: Boolean(journal.mutableStateRestored),
          ...(journal.oldShellForced ? { oldShellForced: true } : {}),
          error,
          ...(cleanupWarning ? { cleanupWarning } : {}),
          ...(recovered ? {} : { rollbackError: 'previous release did not recover' }),
        };
      } catch (rollbackError) {
        if (rollbackError?.code === 'WORKASS_UPDATE_PROGRESS_LOST') {
          return deferRecoveryUntilVisible(rollbackError);
        }
        recoveryPhase = 'failed';
        recoveryExtra = {
          activated: false,
          rolledBack: Boolean(journal.rolledBack),
          error,
          rollbackError: String(rollbackError && rollbackError.message || rollbackError),
        };
      }
      return finish(recoveryPhase, recoveryExtra);
    };

    if (journal.rollbackStarted) return await recoverActivation(new Error(journal.error || 'update activation was interrupted'));

    if (!journal.healthVerified) {
      try {
        await requireProgress();
        if (!journal.incomingVerified) {
          await ops.verifyIncoming();
          save('incoming_verified', { incomingVerified: true });
        }
        await requireProgress();
        updateReceipt(transaction, 'activating');
        if (!journal.mutableStateSnapshotted) {
          if (!journal.snapshotStarted) save('snapshotting_state', { snapshotStarted: true });
          await ops.snapshotMutableState();
          save('state_snapshotted', { mutableStateSnapshotted: true });
        }
        await requireProgress();
        if (!journal.activated) {
          if (!journal.activationStarted) save('activating', { activationStarted: true });
          await ops.activate();
          save('activated', { activated: true });
        }
        await requireProgress();
        await ops.startRuntime?.();
        if (!await launchUntilHealthy(ops, transaction.targetVersion, {
          updateRelaunch: true,
          progressVisible: progressReady,
        })) {
          throw new Error('new release did not recover daemon health, renderer authority, a visible window, and a populated model catalog');
        }
        // This is the commit point. Once all runtime gates have passed, persist
        // that fact before deleting the rollback tree. A worker crash after
        // cleanup must finish the proven target, never reinterpret a transient
        // later health miss as a reason to roll back without a backup.
        save('health_verified', { healthVerified: true });
      } catch (activationError) {
        return await recoverActivation(activationError);
      }
    }
    let cleanupWarning = '';
    try { await ops.cleanup(); }
    catch (err) { cleanupWarning = String(err && err.message || err); }
    return finish('healthy', {
      activated: true,
      ...(journal.oldShellForced ? { oldShellForced: true } : {}),
      ...(cleanupWarning ? { cleanupWarning } : {}),
    });
  } finally {
    lease?.stop(terminal ? 'terminal' : 'interrupted');
  }
}

function validateWorkerEntrypoint(transactionPath, transaction, workerFile = __filename) {
  const root = path.resolve(String(transaction?.transactionRoot || ''));
  if (path.resolve(transactionPath) !== path.join(root, 'transaction.json')) {
    throw new Error('update worker transaction path is not the exact transaction journal');
  }
  if (path.resolve(workerFile) !== path.join(root, 'update-worker.js')) {
    throw new Error('update worker executable does not belong to the transaction');
  }
  if (path.resolve(String(transaction.progressModulePath || '')) !== path.join(root, 'update-progress.js')) {
    throw new Error('update progress module does not belong to the transaction');
  }
}

function workerSelfTestReport(argv) {
  if (argv.length !== 3 || argv[0] !== '--self-test' || argv[1] !== '--transaction-schema' ||
      Number(argv[2]) !== TRANSACTION_SCHEMA_VERSION) {
    throw new Error('update worker self-test transaction schema is unsupported');
  }
  return {
    schemaVersion: 1,
    product: 'Workass',
    component: 'update-worker',
    supportedTransactionSchemas: [TRANSACTION_SCHEMA_VERSION],
    progressReceiptSchemaVersion: PROGRESS_RECEIPT_SCHEMA_VERSION,
  };
}

async function main(argv = process.argv.slice(2), {
  writeOutput = (value) => process.stdout.write(value),
} = {}) {
  if (argv[0] === '--self-test') {
    writeOutput(`${JSON.stringify(workerSelfTestReport(argv))}\n`);
    return 0;
  }
  if (argv[0] !== '--transaction' || !argv[1]) throw new Error('usage: update-worker.js --transaction ABSOLUTE_PATH');
  const transactionPath = path.resolve(argv[1]);
  const transaction = JSON.parse(fs.readFileSync(transactionPath, 'utf8'));
  validateWorkerEntrypoint(transactionPath, transaction);
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
  checkpoint,
  daemonServiceIsDown,
  defaultOperations,
  launchUntilHealthy,
  main,
  mirrorWindowsDirectory,
  processOwnsExecutable,
  requestJSON,
  requestDaemonShutdown,
  requestUpdateCancel,
  readJournal,
  replaceVisibleProgress,
  renamePathWithRetry,
  spawnDetached,
  runTransaction,
  runtimeIsHealthy,
  startWorkerLease,
  startInstalledRuntime,
  stopLaunchedProcessTree,
  stopWindowsExecutableProcesses,
  targetRuntimeEnv,
  updateReceipt,
  validateWorkerEntrypoint,
  validateTransaction,
  verifyMacIncoming,
  verifyWindowsPE,
  verifyWindowsIncoming,
  waitUntil,
  waitUntilDeadline,
  workerSelfTestReport,
};
