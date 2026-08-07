'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

const DEFAULT_FEED_ROOT = 'https://github.com/Dukler/workass/releases/latest/download/';
const MAX_MANIFEST_BYTES = 256 * 1024;
const MAX_UPDATE_BYTES = 4 * 1024 * 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 120000;

function releaseArch(platform = process.platform, arch = process.arch) {
  if (arch === 'x64') return 'amd64';
  if (arch === 'arm64') return 'arm64';
  return arch;
}

function releasePlatform(platform = process.platform) {
  return platform === 'win32' ? 'windows' : platform;
}

function defaultFeedURL(platform = process.platform, arch = process.arch) {
  return new URL(`workass-${releasePlatform(platform)}-${releaseArch(platform, arch)}-release.json`, DEFAULT_FEED_ROOT).href;
}

function parseVersion(value) {
  const match = String(value || '').trim().match(/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
  return match ? match.slice(1).map(Number) : null;
}

function compareVersions(left, right) {
  const a = parseVersion(left);
  const b = parseVersion(right);
  if (!a || !b) throw new Error('Workass release versions must use X.Y.Z');
  for (let index = 0; index < 3; index += 1) {
    if (a[index] !== b[index]) return a[index] < b[index] ? -1 : 1;
  }
  return 0;
}

function validateReleaseManifest(raw, { platform = process.platform, arch = process.arch } = {}) {
  if (!raw || raw.schemaVersion !== 1 || raw.product !== 'Workass') throw new Error('unsupported Workass release manifest');
  if (!parseVersion(raw.version)) throw new Error('release manifest has an invalid version');
  const expectedPlatform = releasePlatform(platform);
  const expectedArch = releaseArch(platform, arch);
  if (raw.platform !== expectedPlatform || raw.arch !== expectedArch) throw new Error('release manifest targets another platform');
  const artifact = raw.artifacts?.update;
  if (!artifact || !/^[a-f0-9]{64}$/i.test(String(artifact.sha256 || ''))) throw new Error('release manifest has no valid update checksum');
  if (!Number.isSafeInteger(artifact.size) || artifact.size <= 0 || artifact.size > MAX_UPDATE_BYTES) throw new Error('release manifest has an invalid update size');
  if (!String(artifact.url || '').trim()) throw new Error('release manifest has no update URL');
  if (platform === 'darwin' && !String(raw.designatedRequirement || '').trim()) throw new Error('macOS release has no signing requirement');
  if (platform === 'win32' && raw.authenticode !== true) throw new Error('Windows automatic updates require Authenticode-signed releases');
  return raw;
}

function httpsRequest(url, { timeoutMs = 15000, maxBytes = MAX_MANIFEST_BYTES, redirects = 5 } = {}) {
  return new Promise((resolve, reject) => {
    let parsed;
    try { parsed = new URL(url); } catch { reject(new Error('invalid update URL')); return; }
    if (parsed.protocol !== 'https:') { reject(new Error('updates require HTTPS')); return; }
    const request = https.get(parsed, { timeout: timeoutMs, headers: { 'user-agent': 'Workass-Updater/1', accept: 'application/json' } }, (response) => {
      const status = response.statusCode || 500;
      if ([301, 302, 303, 307, 308].includes(status)) {
        response.resume();
        if (redirects <= 0 || !response.headers.location) { reject(new Error('too many update redirects')); return; }
        let next;
        try { next = new URL(response.headers.location, parsed).href; } catch { reject(new Error('invalid update redirect')); return; }
        httpsRequest(next, { timeoutMs, maxBytes, redirects: redirects - 1 }).then(resolve, reject);
        return;
      }
      if (status < 200 || status >= 300) {
        response.resume();
        const error = new Error(`update server returned HTTP ${status}`);
        error.statusCode = status;
        reject(error);
        return;
      }
      const chunks = [];
      let total = 0;
      response.on('data', (chunk) => {
        total += chunk.length;
        if (total > maxBytes) { response.destroy(new Error('update response is too large')); return; }
        chunks.push(chunk);
      });
      response.on('end', () => resolve(Buffer.concat(chunks)));
      response.on('error', reject);
    });
    request.on('timeout', () => request.destroy(new Error('update request timed out')));
    request.on('error', reject);
  });
}

async function fetchReleaseManifest(url, options) {
  const bytes = await httpsRequest(url, { ...options, maxBytes: MAX_MANIFEST_BYTES });
  let parsed;
  try { parsed = JSON.parse(bytes.toString('utf8')); } catch { throw new Error('update manifest is not valid JSON'); }
  return parsed;
}

function downloadArtifact(url, destination, artifact, { onProgress = () => {}, redirects = 5 } = {}) {
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    if (parsed.protocol !== 'https:') { reject(new Error('updates require HTTPS')); return; }
    const request = https.get(parsed, { timeout: 30000, headers: { 'user-agent': 'Workass-Updater/1', accept: 'application/octet-stream' } }, (response) => {
      const status = response.statusCode || 500;
      if ([301, 302, 303, 307, 308].includes(status)) {
        response.resume();
        if (redirects <= 0 || !response.headers.location) { reject(new Error('too many update redirects')); return; }
        let next;
        try { next = new URL(response.headers.location, parsed).href; } catch { reject(new Error('invalid update redirect')); return; }
        downloadArtifact(next, destination, artifact, { onProgress, redirects: redirects - 1 }).then(resolve, reject);
        return;
      }
      if (status < 200 || status >= 300) { response.resume(); reject(new Error(`update download returned HTTP ${status}`)); return; }
      const partial = `${destination}.partial`;
      fs.mkdirSync(path.dirname(destination), { recursive: true, mode: 0o700 });
      const output = fs.createWriteStream(partial, { flags: 'wx', mode: 0o600 });
      const hash = crypto.createHash('sha256');
      let received = 0;
      let settled = false;
      const fail = (error) => {
        if (settled) return;
        settled = true;
        try { output.destroy(); } catch { /* best effort */ }
        try { fs.rmSync(partial, { force: true }); } catch { /* best effort */ }
        reject(error);
      };
      response.on('data', (chunk) => {
        received += chunk.length;
        if (received > artifact.size || received > MAX_UPDATE_BYTES) { response.destroy(new Error('update exceeded its signed size')); return; }
        hash.update(chunk);
        onProgress(received, artifact.size);
      });
      response.on('error', fail);
      output.on('error', fail);
      output.on('finish', () => {
        if (settled) return;
        if (received !== artifact.size) { fail(new Error('update size does not match the release manifest')); return; }
        if (hash.digest('hex').toLowerCase() !== artifact.sha256.toLowerCase()) { fail(new Error('update checksum does not match the release manifest')); return; }
        settled = true;
        fs.renameSync(partial, destination);
        resolve(destination);
      });
      response.pipe(output);
    });
    request.on('timeout', () => request.destroy(new Error('update download timed out')));
    request.on('error', reject);
  });
}

function archiveEntries(archive, platform = process.platform) {
  const command = platform === 'win32' ? 'tar.exe' : '/usr/bin/unzip';
  const args = platform === 'win32' ? ['-tf', archive] : ['-Z1', archive];
  const result = spawnSync(command, args, { encoding: 'utf8', windowsHide: true, maxBuffer: 32 * 1024 * 1024 });
  if (result.error || result.status !== 0) throw new Error('update archive could not be inspected');
  const entries = String(result.stdout || '').split(/\r?\n/).filter(Boolean);
  if (entries.length === 0 || entries.length > MAX_ARCHIVE_ENTRIES) throw new Error('update archive has an invalid entry count');
  for (const entry of entries) {
    const normalized = entry.replaceAll('\\', '/');
    if (normalized.includes('\0') || normalized.startsWith('/') || /^[A-Za-z]:\//.test(normalized) || normalized.split('/').includes('..')) {
      throw new Error('update archive contains an unsafe path');
    }
  }
  return entries;
}

function extractArchive(archive, destination, platform = process.platform) {
  archiveEntries(archive, platform);
  fs.mkdirSync(destination, { recursive: true, mode: 0o700 });
  const command = platform === 'win32' ? 'tar.exe' : '/usr/bin/ditto';
  const args = platform === 'win32' ? ['-xf', archive, '-C', destination] : ['-x', '-k', archive, destination];
  const result = spawnSync(command, args, { encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.error || result.status !== 0) throw new Error('update archive extraction failed');
}

function runChecked(command, args, label) {
  const result = spawnSync(command, args, { encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.error || result.status !== 0) throw new Error(`${label} failed`);
  return String(result.stdout || '').trim();
}

function macRequirement(appPath) {
  const output = spawnSync('/usr/bin/codesign', ['-d', '-r-', appPath], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
  if (output.error || output.status !== 0) throw new Error('installed app signing identity is unavailable');
  const combined = `${output.stdout || ''}\n${output.stderr || ''}`;
  const match = combined.match(/designated\s*=>\s*(.+)/);
  if (!match || /\bcdhash\b/i.test(match[1])) throw new Error('installed app does not have a stable signing identity');
  return match[1].trim();
}

function verifyMacRelease(currentApp, incomingApp, targetVersion) {
  const currentRequirement = macRequirement(currentApp);
  runChecked('/usr/bin/codesign', ['--verify', '--deep', '--strict', incomingApp], 'incoming app signature verification');
  runChecked('/usr/bin/codesign', ['--verify', '--strict', `-R=${currentRequirement}`, incomingApp], 'incoming app identity verification');
  const incomingRequirement = macRequirement(incomingApp);
  runChecked('/usr/bin/codesign', ['--verify', '--strict', `-R=${incomingRequirement}`, currentApp], 'mutual app identity verification');
  const version = runChecked('/usr/bin/plutil', ['-extract', 'CFBundleShortVersionString', 'raw', '-o', '-', path.join(incomingApp, 'Contents', 'Info.plist')], 'incoming app version verification');
  if (version !== targetVersion) throw new Error('incoming app version does not match the manifest');
  const runtimeRoot = path.join(incomingApp, 'Contents', 'Resources', 'runtime');
  const runtimeManifest = JSON.parse(fs.readFileSync(path.join(runtimeRoot, 'manifest.json'), 'utf8'));
  if (runtimeManifest.version !== targetVersion || runtimeManifest.platform !== 'darwin' || !fs.existsSync(path.join(runtimeRoot, 'workass'))) {
    throw new Error('incoming app and bundled daemon are not one release');
  }
  return currentRequirement;
}

function powershellSignature(executable) {
  const quoted = String(executable).replaceAll("'", "''");
  const script = `$s=Get-AuthenticodeSignature -LiteralPath '${quoted}'; [Console]::Out.Write(($s.Status.ToString())+'|'+($s.SignerCertificate.Thumbprint))`;
  const value = runChecked('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], 'Authenticode verification');
  const [status, thumbprint] = value.split('|');
  if (status !== 'Valid' || !thumbprint) throw new Error('Authenticode signature is not valid');
  return thumbprint.toUpperCase();
}

function verifyWindowsRelease(currentRoot, incomingRoot, targetVersion) {
  const currentSigner = powershellSignature(path.join(currentRoot, 'Workass.exe'));
  for (const executable of [path.join(incomingRoot, 'Workass.exe'), path.join(incomingRoot, 'workass-daemon.exe')]) {
    if (powershellSignature(executable) !== currentSigner) throw new Error('incoming executable signer does not match the installed app');
  }
  const manifest = JSON.parse(fs.readFileSync(path.join(incomingRoot, 'manifest.json'), 'utf8'));
  if (manifest.version !== targetVersion || manifest.platform !== 'windows') throw new Error('incoming portable runtime does not match the manifest');
}

function stageRelease(source, incomingTarget, platform = process.platform) {
  if (platform === 'darwin') {
    const copy = spawnSync('/usr/bin/ditto', [source, incomingTarget], { stdio: ['ignore', 'pipe', 'pipe'] });
    if (copy.error || copy.status !== 0) throw new Error('could not stage the signed app beside the installed app');
    return;
  }
  fs.cpSync(source, incomingTarget, { recursive: true, errorOnExist: true });
}

function verifyRelease(currentRoot, incomingRoot, targetVersion, platform = process.platform) {
  if (platform === 'darwin') return verifyMacRelease(currentRoot, incomingRoot, targetVersion);
  verifyWindowsRelease(currentRoot, incomingRoot, targetVersion);
  return '';
}

function postLocalUpdate(daemonURL, action, updateId, timeoutMs = 2500) {
  return new Promise((resolve) => {
    const base = new URL(daemonURL);
    if (!['127.0.0.1', 'localhost', '::1'].includes(base.hostname)) { resolve({ status: 403, body: {} }); return; }
    const transport = base.protocol === 'https:' ? https : http;
    const bytes = Buffer.from(JSON.stringify({ updateId }));
    const request = transport.request({
      protocol: base.protocol,
      hostname: base.hostname,
      port: base.port,
      path: `/workass/update/${action}`,
      method: 'POST',
      timeout: timeoutMs,
      rejectUnauthorized: false,
      headers: { 'content-type': 'application/json', 'content-length': bytes.length },
    }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => { if (body.length < 65536) body += chunk; });
      response.on('end', () => {
        let parsed = {};
        try { parsed = JSON.parse(body); } catch { /* bounded error body */ }
        resolve({ status: response.statusCode || 500, body: parsed });
      });
    });
    request.on('timeout', () => { request.destroy(); resolve({ status: 0, body: {} }); });
    request.on('error', () => resolve({ status: 0, body: {} }));
    request.end(bytes);
  });
}

function atomicJSON(file, value) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const incoming = `${file}.incoming-${process.pid}`;
  fs.writeFileSync(incoming, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(incoming, file);
}

function installedRoot(resourcesPath, executablePath, platform = process.platform) {
  return platform === 'darwin' ? path.resolve(resourcesPath, '..', '..') : path.dirname(executablePath);
}

function bundledNode(resourcesPath, executablePath, platform = process.platform, arch = process.arch) {
  if (platform === 'darwin') return path.join(resourcesPath, 'runtime', 'node', `darwin-${releaseArch(platform, arch)}`, 'bin', 'node');
  return path.join(path.dirname(executablePath), 'node', `windows-${releaseArch(platform, arch)}`, 'node.exe');
}

function findExtractedRoot(stageRoot, platform = process.platform) {
  if (platform === 'darwin') {
    const appPath = path.join(stageRoot, 'Workass.app');
    if (!fs.statSync(appPath, { throwIfNoEntry: false })?.isDirectory()) throw new Error('update archive does not contain Workass.app');
    return appPath;
  }
  const entries = fs.readdirSync(stageRoot, { withFileTypes: true }).filter((entry) => entry.isDirectory());
  if (entries.length !== 1) throw new Error('Windows update archive must contain exactly one release directory');
  return path.join(stageRoot, entries[0].name);
}

class UpdateManager {
  constructor({
    app,
    runtime,
    resourcesPath,
    executablePath = process.execPath,
    currentVersion = '',
    platform = process.platform,
    arch = process.arch,
    isPackaged = false,
    feedURL = defaultFeedURL(platform, arch),
    onState = () => {},
    quit = () => app?.quit?.(),
    deps = {},
  } = {}) {
    this.app = app;
    this.runtime = runtime;
    this.resourcesPath = resourcesPath;
    this.executablePath = executablePath;
    this.platform = platform;
    this.arch = arch;
    this.isPackaged = isPackaged;
    this.feedURL = feedURL;
    this.onState = onState;
    this.quit = quit;
    this.deps = {
      fetchManifest: fetchReleaseManifest,
      downloadArtifact,
      extractArchive,
      postLocalUpdate,
      spawn,
      stageRelease,
      verifyRelease,
      schedule: setTimeout,
      ...deps,
    };
    this.updateRoot = path.join(runtime.dataRoot, 'updates');
    this.receiptPath = path.join(this.updateRoot, 'receipt.json');
    this.currentVersion = String(currentVersion || app?.getVersion?.() || '0.0.0');
    this.manifest = null;
    this.prepared = null;
    this.receiptTimer = null;
    this.activeOperation = null;
    this.state = {
      supported: false,
      phase: 'unavailable',
      currentVersion: this.currentVersion,
      targetVersion: null,
      checkedAt: null,
      progress: 0,
      error: null,
      blockers: null,
      receipt: null,
    };
  }

  snapshot() { return JSON.parse(JSON.stringify(this.state)); }

  beginOperation(name) {
    if (this.activeOperation) throw new Error(`Workass update operation already running: ${this.activeOperation}`);
    this.activeOperation = name;
  }

  endOperation(name) {
    if (this.activeOperation === name) this.activeOperation = null;
  }

  publish(patch) {
    this.state = { ...this.state, ...patch };
    this.onState(this.snapshot());
    return this.snapshot();
  }

  init() {
    const node = bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch);
    const platformSupported = this.platform === 'darwin' || this.platform === 'win32';
    let supported = this.isPackaged && platformSupported && fs.existsSync(node);
    let unsupportedReason = this.isPackaged ? 'This build does not include the verified updater runtime.' : 'App updates are available in packaged Workass builds.';
    if (supported && this.platform === 'win32') {
      try { powershellSignature(this.executablePath); }
      catch {
        supported = false;
        unsupportedReason = 'Automatic Windows updates require an Authenticode-signed Workass release.';
      }
    }
    this.publish({
      supported,
      phase: supported ? 'idle' : 'unavailable',
      error: supported ? null : unsupportedReason,
    });
    try {
      const receipt = JSON.parse(fs.readFileSync(this.receiptPath, 'utf8'));
      if (receipt?.schemaVersion === 1) {
        const active = ['armed', 'activating'].includes(receipt.phase);
        this.publish({ phase: active ? 'installing' : receipt.phase, receipt, error: receipt.error || null });
        if (active) this.watchReceipt();
        else this.pruneTerminalPayload(receipt);
      }
    } catch { /* no prior transaction */ }
    return this.snapshot();
  }

  pruneTerminalPayload(receipt) {
    const updateId = String(receipt?.updateId || '');
    if (!/^[A-Za-z0-9_-]{8,96}$/.test(updateId)) return;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    // Keep the transaction, worker log, and JSON receipt for diagnosis, but
    // discard the multi-gigabyte downloaded/extracted payload once a durable
    // terminal receipt exists. These exact children can never name outside the
    // validated update-id directory.
    for (const child of ['release.zip', 'release.zip.partial', 'extracted']) {
      try { fs.rmSync(path.join(transactionRoot, child), { recursive: true, force: true }); } catch { /* best effort */ }
    }
  }

  watchReceipt() {
    if (this.receiptTimer) return;
    this.receiptTimer = setInterval(() => {
      try {
        const receipt = JSON.parse(fs.readFileSync(this.receiptPath, 'utf8'));
        if (!receipt || receipt.schemaVersion !== 1) return;
        const terminal = ['healthy', 'rollback_healthy', 'failed'].includes(receipt.phase);
        this.publish({ phase: terminal ? receipt.phase : 'installing', receipt, error: receipt.error || null });
        if (terminal) { clearInterval(this.receiptTimer); this.receiptTimer = null; }
      } catch { /* worker may be between atomic receipt writes */ }
    }, 1000);
    this.receiptTimer.unref?.();
  }

  async check() {
    if (!this.state.supported) return this.snapshot();
    this.beginOperation('check');
    this.publish({ phase: 'checking', error: null, blockers: null });
    try {
      const raw = await this.deps.fetchManifest(this.feedURL);
      const manifest = validateReleaseManifest(raw, { platform: this.platform, arch: this.arch });
      this.manifest = manifest;
      const available = compareVersions(this.currentVersion, manifest.version) < 0;
      return this.publish({
        phase: available ? 'available' : 'current',
        targetVersion: available ? manifest.version : null,
        checkedAt: new Date().toISOString(),
        notes: available ? String(manifest.notes || '') : '',
        size: available ? manifest.artifacts.update.size : null,
        error: null,
      });
    } catch (err) {
      // GitHub returns 404 when the latest release has no asset for this
      // platform yet. That means there is no applicable update, not that the
      // installed app is broken. Signature, checksum, parse, and network
      // failures remain explicit failures.
      if (err?.statusCode === 404) {
        return this.publish({ phase: 'current', targetVersion: null, checkedAt: new Date().toISOString(), error: null });
      }
      return this.publish({ phase: 'failed', checkedAt: new Date().toISOString(), error: String(err && err.message || err) });
    } finally {
      this.endOperation('check');
    }
  }

  async download() {
    this.beginOperation('download');
    try {
    if (this.state.phase !== 'available') throw new Error('the Workass update is not available for download');
    if (!this.manifest || compareVersions(this.currentVersion, this.manifest.version) >= 0) throw new Error('no Workass update is available');
    const updateId = `upd-${Date.now().toString(36)}-${crypto.randomBytes(8).toString('hex')}`;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    const archive = path.join(transactionRoot, 'release.zip');
    const extracted = path.join(transactionRoot, 'extracted');
    const artifact = this.manifest.artifacts.update;
    const artifactURL = new URL(artifact.url, this.feedURL);
    if (artifactURL.protocol !== 'https:') throw new Error('updates require HTTPS');
    this.publish({ phase: 'downloading', progress: 0, error: null, blockers: null });
    try {
      await this.deps.downloadArtifact(artifactURL.href, archive, artifact, {
        onProgress: (received, total) => this.publish({ progress: total > 0 ? Math.min(1, received / total) : 0 }),
      });
      this.publish({ phase: 'staging', progress: 1 });
      this.deps.extractArchive(archive, extracted, this.platform);
      const source = findExtractedRoot(extracted, this.platform);
      const installTarget = installedRoot(this.resourcesPath, this.executablePath, this.platform);
      const parent = path.dirname(installTarget);
      const incomingTarget = path.join(parent, `${this.platform === 'darwin' ? '.Workass.app' : '.Workass'}.incoming-${updateId}`);
      const backupTarget = path.join(parent, `${this.platform === 'darwin' ? '.Workass.app' : '.Workass'}.previous-${updateId}`);
      if (fs.existsSync(incomingTarget) || fs.existsSync(backupTarget)) throw new Error('update staging target already exists');
      this.deps.stageRelease(source, incomingTarget, this.platform);
      const designatedRequirement = this.deps.verifyRelease(installTarget, incomingTarget, this.manifest.version, this.platform);
      this.prepared = { updateId, transactionRoot, installTarget, incomingTarget, backupTarget, designatedRequirement };
      return this.publish({ phase: 'ready', progress: 1, error: null });
    } catch (err) {
      return this.publish({ phase: 'failed', error: String(err && err.message || err) });
    }
    } finally {
      this.endOperation('download');
    }
  }

  async install() {
    this.beginOperation('install');
    try {
    if (!this.prepared || !['ready', 'busy'].includes(this.state.phase)) throw new Error('the verified Workass update is not ready');
    const prepared = this.prepared;
    const nodeSource = bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch);
    const workerSource = path.join(__dirname, 'update-worker.js');
    const workerPath = path.join(prepared.transactionRoot, 'update-worker.js');
    fs.mkdirSync(prepared.transactionRoot, { recursive: true, mode: 0o700 });
    fs.copyFileSync(workerSource, workerPath);
    // A running macOS executable remains valid after its containing app bundle
    // is renamed, and its dylibs are already mapped. Windows locks executable
    // files, so its standalone portable node.exe must be copied outside the
    // directory that will be swapped.
    const nodePath = this.platform === 'win32' ? path.join(prepared.transactionRoot, 'updater-node.exe') : nodeSource;
    if (this.platform === 'win32') fs.copyFileSync(nodeSource, nodePath);
    const selfTest = spawnSync(nodePath, [workerPath, '--self-test'], { stdio: 'ignore', windowsHide: true });
    if (selfTest.error || selfTest.status !== 0) throw new Error('the independent update worker did not pass its self-test');

    const preparedReply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'prepare', prepared.updateId);
    if (preparedReply.status === 409) {
      return this.publish({ phase: 'busy', blockers: preparedReply.body, error: null });
    }
    if (preparedReply.status !== 200 || preparedReply.body?.ready !== true) {
      return this.publish({ phase: 'failed', error: 'the daemon could not prepare a safe update handoff' });
    }

    const transaction = {
      schemaVersion: 1,
      updateId: prepared.updateId,
      platform: this.platform,
      currentVersion: this.currentVersion,
      targetVersion: this.manifest.version,
      shellPID: process.pid,
      installTarget: prepared.installTarget,
      incomingTarget: prepared.incomingTarget,
      backupTarget: prepared.backupTarget,
      receiptPath: this.receiptPath,
      daemonHealthURL: `${this.runtime.daemonURL}/workass/health`,
      shellStatusURL: `http://127.0.0.1:${this.runtime.viewPort}/__workass-shell/status`,
      designatedRequirement: prepared.designatedRequirement,
      launchAgentPath: this.platform === 'darwin' ? path.join(os.homedir(), 'Library', 'LaunchAgents', `${this.runtime.launchdLabel}.plist`) : '',
      launchdDomain: this.platform === 'darwin' && typeof process.getuid === 'function' ? `gui/${process.getuid()}` : '',
    };
    const transactionPath = path.join(prepared.transactionRoot, 'transaction.json');
    atomicJSON(transactionPath, transaction);
    const logFD = fs.openSync(path.join(prepared.transactionRoot, 'worker.log'), 'a', 0o600);
    let worker;
    try {
      worker = this.deps.spawn(nodePath, [workerPath, '--transaction', transactionPath], {
        cwd: prepared.transactionRoot,
        detached: true,
        windowsHide: true,
        stdio: ['ignore', logFD, logFD],
      });
      worker.unref();
    } catch (err) {
      await this.deps.postLocalUpdate(this.runtime.daemonURL, 'cancel', prepared.updateId);
      throw err;
    } finally {
      fs.closeSync(logFD);
    }

    const committed = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'commit', prepared.updateId);
    if (committed.status !== 202) {
      try { worker?.kill(); } catch { /* best effort */ }
      await this.deps.postLocalUpdate(this.runtime.daemonURL, 'cancel', prepared.updateId);
      return this.publish({ phase: 'failed', error: 'the daemon did not commit the prepared update handoff' });
    }
    this.publish({ phase: 'installing', receipt: { phase: 'armed', targetVersion: this.manifest.version }, error: null });
    this.deps.schedule(() => this.quit(), 100)?.unref?.();
    return this.snapshot();
    } finally {
      this.endOperation('install');
    }
  }

  dispose() {
    if (this.receiptTimer) clearInterval(this.receiptTimer);
    this.receiptTimer = null;
  }
}

module.exports = {
  UpdateManager,
  archiveEntries,
  atomicJSON,
  bundledNode,
  compareVersions,
  defaultFeedURL,
  downloadArtifact,
  extractArchive,
  fetchReleaseManifest,
  findExtractedRoot,
  httpsRequest,
  installedRoot,
  macRequirement,
  parseVersion,
  postLocalUpdate,
  releaseArch,
  releasePlatform,
  stageRelease,
  validateReleaseManifest,
  verifyRelease,
  verifyMacRelease,
  verifyWindowsRelease,
};
