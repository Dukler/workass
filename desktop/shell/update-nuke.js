'use strict';

// Nuclear updater: everything happens after the shell quits.
//   kill every Workass process -> download the release -> verify checksum ->
//   extract -> move current contents to trash -> move new tree in ->
//   start Workass -> poll health -> delete trash (or put the old tree back).
//
// One linear script, one log file, two possible endings. Mutable state lives
// outside the install folder on every supported layout, so nothing here can
// touch chats, pairing, or provider credentials.

const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const https = require('node:https');
const { spawn, spawnSync } = require('node:child_process');

const HEALTH_TIMEOUT_MS = 120000;
const HEALTH_POLL_MS = 1000;
const KILL_SETTLE_MS = 1500;

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function writeLog(request, message) {
  try {
    if (request.logPath) fs.appendFileSync(request.logPath, `${new Date().toISOString()} ${message}\n`);
  } catch { /* logging must never fail the update */ }
}

function killAllWorkassProcesses(request) {
  const run = request.spawnSyncImpl || spawnSync;
  if (request.platform === 'win32') {
    for (const image of ['Workass.exe', 'workass-daemon.exe', 'update-progress.exe']) {
      run('taskkill', ['/F', '/IM', image, '/T'], { windowsHide: true, stdio: 'ignore' });
    }
    return;
  }
  if (request.launchAgentPath && request.launchdDomain) {
    run('/bin/launchctl', ['bootout', request.launchdDomain, request.launchAgentPath], { stdio: 'ignore' });
  }
  run('/usr/bin/pkill', ['-f', 'Workass.app/Contents/MacOS/Workass'], { stdio: 'ignore' });
}

async function downloadRelease(request) {
  const zipPath = path.join(request.workRoot, 'release.zip');
  const run = request.spawnSyncImpl || spawnSync;
  const curl = request.platform === 'win32' ? 'C:\\Windows\\System32\\curl.exe' : '/usr/bin/curl';
  let lastError = null;
  for (let attempt = 1; attempt <= 3; attempt += 1) {
    writeLog(request, `downloading attempt ${attempt}`);
    const result = run(curl, ['-fSL', '--retry', '2', '-o', zipPath, request.downloadUrl], {
      windowsHide: true, stdio: 'ignore', timeout: 15 * 60 * 1000,
    });
    if (result.status === 0 && fs.existsSync(zipPath)) {
      const stat = fs.statSync(zipPath);
      if (request.size && stat.size !== request.size) {
        lastError = new Error(`downloaded ${stat.size} bytes, expected ${request.size}`);
        writeLog(request, lastError.message);
        continue;
      }
      return zipPath;
    }
    lastError = new Error(`download failed (exit ${result.status})`);
    writeLog(request, lastError.message);
    await sleep(2000);
  }
  throw lastError || new Error('download failed');
}

function verifyChecksum(request, zipPath) {
  if (!request.sha256) return;
  const digest = crypto.createHash('sha256');
  const handle = fs.openSync(zipPath, 'r');
  try {
    const buffer = Buffer.alloc(1024 * 1024);
    let read = 0;
    while ((read = fs.readSync(handle, buffer)) > 0) digest.update(buffer.subarray(0, read));
  } finally {
    fs.closeSync(handle);
  }
  const actual = digest.digest('hex');
  if (request.sha256 && actual !== String(request.sha256).toLowerCase()) {
    throw new Error(`checksum mismatch: got ${actual.slice(0, 16)}…`);
  }
}

function extractAndLocate(request) {
  // update-manager.js ships next to this script and owns the battle-tested
  // archive validation and root discovery.
  const managerModule = require(path.join(request.workRoot, 'update-manager.js'));
  const extracted = path.join(request.workRoot, 'extracted');
  fs.rmSync(extracted, { recursive: true, force: true });
  fs.mkdirSync(extracted, { recursive: true });
  managerModule.extractArchive(path.join(request.workRoot, 'release.zip'), extracted, request.platform);
  return managerModule.findExtractedRoot(extracted, request.platform);
}

function emptyDirectory(dir) {
  fs.mkdirSync(dir, { recursive: true });
  for (const entry of fs.readdirSync(dir)) {
    fs.rmSync(path.join(dir, entry), { recursive: true, force: true });
  }
}

function moveToTrash(request, fromDir) {
  fs.mkdirSync(request.trashDir, { recursive: true });
  for (const entry of fs.readdirSync(fromDir)) {
    const source = path.join(fromDir, entry);
    let destination = path.join(request.trashDir, entry);
    try {
      fs.renameSync(source, destination);
    } catch {
      destination = destination + '-' + crypto.randomBytes(3).toString('hex');
      fs.cpSync(source, destination, { recursive: true });
      fs.rmSync(source, { recursive: true, force: true });
    }
  }
}

function restoreFromTrash(request) {
  if (!fs.existsSync(request.trashDir)) return;
  for (const entry of fs.readdirSync(request.trashDir)) {
    try {
      fs.renameSync(path.join(request.trashDir, entry), path.join(request.installDir, entry));
    } catch {
      fs.cpSync(path.join(request.trashDir, entry), path.join(request.installDir, entry), { recursive: true });
    }
  }
  fs.rmSync(request.trashDir, { recursive: true, force: true });
}

function fillInstallFolder(request, sourceRoot) {
  emptyDirectory(request.installDir);
  const run = request.spawnSyncImpl || spawnSync;
  if (request.platform === 'win32') {
    const result = run('robocopy.exe', [sourceRoot, request.installDir, '/E', '/COPY:DAT', '/DCOPY:DAT', '/XJ', '/R:5', '/W:1', '/MT:8', '/NFL', '/NDL', '/NJH', '/NJS', '/NP'], { windowsHide: true, stdio: 'ignore' });
    if (result.status == null || result.status > 7) throw new Error(`copy into install folder failed (robocopy exit ${result.status})`);
    return;
  }
  fs.cpSync(sourceRoot, request.installDir, { recursive: true });
}

function launchInstalled(request) {
  if (request.platform === 'win32') {
    const child = spawn(path.join(request.installDir, 'Workass.exe'), [], {
      cwd: request.installDir, detached: true, windowsHide: false, stdio: 'ignore',
      env: { ...(request.env || {}), WORKASS_UPDATE_RELAUNCH: '1' },
    });
    child.unref?.();
    return;
  }
  spawnSync('/usr/bin/open', [request.installDir], { stdio: 'ignore' });
}

async function waitHealthy(request) {
  if (!request.healthURL) return true;
  const deadline = Date.now() + HEALTH_TIMEOUT_MS;
  while (Date.now() < deadline) {
    const healthy = await new Promise((resolve) => {
      const req = https.get(request.healthURL, { timeout: 3000, rejectUnauthorized: false }, (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (chunk) => { if (body.length < 65536) body += chunk; });
        res.on('end', () => {
          try {
            const health = JSON.parse(body);
            resolve(health?.app === 'workass' && health?.version === request.targetVersion);
          } catch { resolve(false); }
        });
      });
      req.on('timeout', () => req.destroy());
      req.on('error', () => resolve(false));
    });
    if (healthy) return true;
    await sleep(HEALTH_POLL_MS);
  }
  return false;
}

async function runNuclearUpdate(rawRequest, deps = {}) {
  const request = { ...rawRequest };
  const write = (message) => writeLog(request, message);
  const kill = deps.kill ?? killAllWorkassProcesses;
  const download = deps.download ?? downloadRelease;
  const verify = deps.verify ?? verifyChecksum;
  const extract = deps.extract ?? extractAndLocate;
  const fill = deps.fill ?? fillInstallFolder;
  const restore = deps.restore ?? restoreFromTrash;
  const move = deps.moveToTrash ?? moveToTrash;
  const launch = deps.launch ?? launchInstalled;
  const checkHealth = deps.waitHealthy || waitHealthy;
  const settle = deps.sleep || sleep;

  for (const field of ['downloadUrl', 'targetVersion', 'installDir', 'trashDir', 'workRoot']) {
    if (!request[field]) {
      return { ok: false, error: `nuclear update request missing ${field}` };
    }
  }

  write('killing every Workass process');
  kill(request);
  await settle(KILL_SETTLE_MS);
  kill(request);
  await settle(KILL_SETTLE_MS);

  let zipPath;
  try {
    zipPath = await download(request);
    write('download complete; verifying checksum');
    verify(request, zipPath);
  } catch (err) {
    write(`download/verify failed: ${err && err.message}`);
    restore(request);
    launch(request);
    return { ok: false, restored: true, error: `download failed: ${err && err.message}` };
  }

  let sourceRoot;
  try {
    write('extracting and validating the release');
    sourceRoot = extract(request);
  } catch (err) {
    write(`extraction failed: ${err && err.message}`);
    restore(request);
    launch(request);
    return { ok: false, restored: true, error: `extraction failed: ${err && err.message}` };
  }

  write('moving current install contents aside');
  try {
    move(request, request.installDir);
  } catch (err) {
    write(`trash move failed: ${err && err.message}`);
    restore(request);
    launch(request);
    return { ok: false, restored: true, error: `could not clear the install folder: ${err && err.message}` };
  }

  try {
    write('filling install folder with the new release');
    fill(request, sourceRoot);
  } catch (err) {
    write(`fill failed: ${err && err.message}`);
    restore(request);
    launch(request);
    return { ok: false, restored: true, error: `install folder fill failed: ${err && err.message}` };
  }

  write('launching new release');
  launch(request);
  const healthy = await checkHealth(request);
  if (healthy) {
    write('new release healthy; deleting trash');
    fs.rmSync(request.trashDir, { recursive: true, force: true });
    return { ok: true };
  }

  write('new release did not turn healthy; restoring previous contents');
  kill(request);
  await settle(KILL_SETTLE_MS);
  emptyDirectory(request.installDir);
  restore(request);
  launch(request);
  return { ok: false, restored: true, error: 'the new release did not report healthy in time' };
}

module.exports = {
  HEALTH_TIMEOUT_MS,
  downloadRelease,
  fillInstallFolder,
  killAllWorkassProcesses,
  restoreFromTrash,
  runNuclearUpdate,
  verifyChecksum,
};
