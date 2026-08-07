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

function powershellSignature(executable) {
  const quoted = String(executable).replaceAll("'", "''");
  const script = `$s=Get-AuthenticodeSignature -LiteralPath '${quoted}'; [Console]::Out.Write(($s.Status.ToString())+'|'+($s.SignerCertificate.Thumbprint))`;
  const value = runChecked('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], 'Authenticode verification');
  const [status, thumbprint] = value.split('|');
  if (status !== 'Valid' || !thumbprint) throw new Error('Authenticode signature is not valid');
  return thumbprint.toUpperCase();
}

function verifyWindowsIncoming(transaction) {
  const currentSigner = powershellSignature(path.join(transaction.installTarget, 'Workass.exe'));
  for (const executable of [
    path.join(transaction.incomingTarget, 'Workass.exe'),
    path.join(transaction.incomingTarget, 'workass-daemon.exe'),
  ]) {
    if (powershellSignature(executable) !== currentSigner) throw new Error('incoming executable signer does not match the installed app');
  }
  const manifest = JSON.parse(fs.readFileSync(path.join(transaction.incomingTarget, 'manifest.json'), 'utf8'));
  if (manifest.version !== transaction.targetVersion || manifest.platform !== 'windows') {
    throw new Error('incoming Windows runtime does not match the release manifest');
  }
}

function validateTransaction(transaction) {
  if (!transaction || transaction.schemaVersion !== 1) throw new Error('unsupported update transaction');
  for (const field of ['updateId', 'platform', 'currentVersion', 'targetVersion', 'installTarget', 'incomingTarget', 'backupTarget', 'receiptPath', 'daemonHealthURL', 'shellStatusURL']) {
    if (!String(transaction[field] || '').trim()) throw new Error(`update transaction is missing ${field}`);
  }
  for (const field of ['installTarget', 'incomingTarget', 'backupTarget', 'receiptPath']) {
    if (!path.isAbsolute(transaction[field])) throw new Error(`${field} must be absolute`);
  }
  const installParent = path.dirname(transaction.installTarget);
  if (path.dirname(transaction.incomingTarget) !== installParent || path.dirname(transaction.backupTarget) !== installParent) {
    throw new Error('update swap paths must share one parent directory');
  }
  if (transaction.platform === 'darwin') {
    if (!transaction.installTarget.endsWith('.app') || !transaction.designatedRequirement) throw new Error('invalid macOS update transaction');
  } else if (transaction.platform !== 'win32') {
    throw new Error(`unsupported update platform: ${transaction.platform}`);
  }
  return transaction;
}

function defaultOperations(transaction) {
  const launchAgentPath = String(transaction.launchAgentPath || '');
  const launchdDomain = String(transaction.launchdDomain || '');
  let launchedPID = 0;
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
    activate: async () => {
      if (fs.existsSync(transaction.backupTarget)) throw new Error('update rollback target already exists');
      fs.renameSync(transaction.installTarget, transaction.backupTarget);
      try {
        fs.renameSync(transaction.incomingTarget, transaction.installTarget);
      } catch (err) {
        fs.renameSync(transaction.backupTarget, transaction.installTarget);
        throw err;
      }
    },
    launchInstalled: async () => {
      const executable = transaction.platform === 'darwin'
        ? path.join(transaction.installTarget, 'Contents', 'MacOS', 'Workass')
        : path.join(transaction.installTarget, 'Workass.exe');
      const child = spawn(executable, [], {
        cwd: transaction.installTarget,
        detached: true,
        windowsHide: true,
        stdio: 'ignore',
        env: { ...process.env, WORKASS_CONTROLLER_RECOVERY: '1' },
      });
      launchedPID = child.pid || 0;
      child.unref();
    },
    healthy: async (expectedVersion = transaction.targetVersion) => {
      const [daemon, shell] = await Promise.all([
        requestJSON(transaction.daemonHealthURL), requestJSON(transaction.shellStatusURL),
      ]);
      return daemon?.app === 'workass' && daemon?.version === expectedVersion &&
        shell?.controller === true && Number(shell?.catalog?.readyModelCount || 0) > 0 &&
        shell?.appVersion === expectedVersion;
    },
    stopLaunched: async () => {
      if (launchedPID > 1 && pidAlive(launchedPID)) {
        try { process.kill(launchedPID, 'SIGTERM'); } catch { /* already gone */ }
        await waitUntil(() => !pidAlive(launchedPID), { attempts: 40, delayMs: 100 });
      }
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
      if (fs.existsSync(transaction.incomingTarget)) throw new Error('failed release quarantine path already exists');
      if (fs.existsSync(transaction.installTarget)) fs.renameSync(transaction.installTarget, transaction.incomingTarget);
      fs.renameSync(transaction.backupTarget, transaction.installTarget);
    },
    cleanup: async () => {
      fs.rmSync(transaction.backupTarget, { recursive: true, force: true });
    },
    cleanupFailed: async () => {
      fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
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
  try {
    await ops.verifyIncoming();
    updateReceipt(transaction, 'activating');
    await ops.activate();
    activated = true;
    await ops.launchInstalled();
    if (await wait(() => ops.healthy(transaction.targetVersion), { attempts: 240, delayMs: 250 })) {
      await ops.cleanup();
      return updateReceipt(transaction, 'healthy', { activated: true });
    }
    throw new Error('new release did not recover daemon health, controller authority, and provider catalog');
  } catch (activationError) {
    try {
      await ops.stopLaunched();
      if (activated) await ops.rollback();
      await ops.launchInstalled();
      const recovered = await wait(() => ops.healthy(transaction.currentVersion), { attempts: 240, delayMs: 250 });
      if (recovered) await ops.cleanupFailed?.();
      return updateReceipt(transaction, recovered ? 'rollback_healthy' : 'failed', {
        activated: false,
        rolledBack: activated,
        error: String(activationError && activationError.message || activationError),
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
  requestJSON,
  requestDaemonShutdown,
  runTransaction,
  updateReceipt,
  validateTransaction,
  verifyMacIncoming,
  verifyWindowsIncoming,
  waitUntil,
};
