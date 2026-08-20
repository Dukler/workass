'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');
const { Transform } = require('node:stream');
const { pipeline } = require('node:stream/promises');
const { isMainThread, parentPort, workerData, Worker } = require('node:worker_threads');

const DEFAULT_FEED_ROOT = 'https://github.com/Dukler/workass/releases/latest/download/';
const MAX_MANIFEST_BYTES = 256 * 1024;
const MAX_UPDATE_BYTES = 4 * 1024 * 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 120000;
const DEFAULT_INITIAL_CHECK_DELAY_MS = 15_000;
const DEFAULT_CHECK_INTERVAL_MS = 60 * 60 * 1000;
const DEFAULT_WORKER_ARM_TIMEOUT_MS = 15_000;
const DEFAULT_WORKER_ARM_POLL_MS = 25;
const INSTALLATION_IDENTITY_FILE = '.workass-installation.json';
const RECEIPT_SCHEMA_VERSION = 2;
const TRANSACTION_SCHEMA_VERSION = 3;
// The worker writes once per second. Five seconds is only the cheap polling
// window; crossing it triggers an off-main exact process inspection. The
// longer takeover bound remains inside workerProcessOwnership so a legitimate
// blocking filesystem operation is never killed merely because its JS timer
// could not run.
const WORKER_HEARTBEAT_MAX_AGE_MS = 5 * 1000;
const WORKER_TAKEOVER_MAX_AGE_MS = 30 * 60 * 1000;

function validInstallationId(value) {
  return /^install-[a-f0-9]{32}$/.test(String(value || ''));
}

function installationIdentityPath(installTarget) {
  if (!path.isAbsolute(String(installTarget || ''))) throw new Error('installation target must be absolute');
  return path.join(installTarget, INSTALLATION_IDENTITY_FILE);
}

function readInstallationIdentity(installTarget) {
  try {
    const identity = JSON.parse(fs.readFileSync(installationIdentityPath(installTarget), 'utf8'));
    if (identity?.schemaVersion !== 1 || identity.product !== 'Workass' || !validInstallationId(identity.installationId)) {
      return null;
    }
    return identity;
  } catch {
    return null;
  }
}

function ensureInstallationIdentity(installTarget, {
  platform = process.platform,
  randomBytes = crypto.randomBytes,
} = {}) {
  if (platform !== 'win32') {
    const canonical = path.resolve(installTarget);
    return {
      schemaVersion: 1,
      product: 'Workass',
      installationId: `install-${crypto.createHash('sha256').update(`mac:${canonical}`).digest('hex').slice(0, 32)}`,
    };
  }
  const existing = readInstallationIdentity(installTarget);
  if (existing) return existing;
  const identity = {
    schemaVersion: 1,
    product: 'Workass',
    installationId: `install-${randomBytes(16).toString('hex')}`,
    createdAt: new Date().toISOString(),
  };
  atomicJSON(installationIdentityPath(installTarget), identity);
  const persisted = readInstallationIdentity(installTarget);
  if (!persisted || persisted.installationId !== identity.installationId) {
    throw new Error('Workass could not persist this portable installation identity');
  }
  return persisted;
}

function releaseArch(platform = process.platform, arch = process.arch) {
  if (arch === 'x64') return 'amd64';
  if (arch === 'arm64') return 'arm64';
  return arch;
}

function releasePlatform(platform = process.platform) {
  return platform === 'win32' ? 'windows' : platform;
}

function releaseFeedName(platform = process.platform, arch = process.arch) {
  return `workass-${releasePlatform(platform)}-${releaseArch(platform, arch)}-release.json`;
}

function defaultFeedURL(platform = process.platform, arch = process.arch) {
  return new URL(releaseFeedName(platform, arch), DEFAULT_FEED_ROOT).href;
}

function localFeedPath(dataRoot, platform = process.platform, arch = process.arch) {
  if (!path.isAbsolute(String(dataRoot || ''))) throw new Error('local update data root must be absolute');
  return path.join(dataRoot, 'update-feed', releaseFeedName(platform, arch));
}

function resolveUpdateFeed({ channel = 'github', dataRoot = '', platform = process.platform, arch = process.arch } = {}) {
  if (channel === 'github') return defaultFeedURL(platform, arch);
  if (channel === 'local' && platform === 'darwin') return localFeedPath(dataRoot, platform, arch);
  throw new Error('local Workass updates are supported only on macOS dogfood builds');
}

function parseVersion(value) {
  const match = String(value || '').trim().match(/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
  return match ? match.slice(1).map(BigInt) : null;
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

function receiptAppliesToInstalledVersion(receipt, currentVersion, {
  installationId = '',
  installTarget = '',
} = {}) {
  if (!receipt || receipt.schemaVersion !== RECEIPT_SCHEMA_VERSION || !parseVersion(currentVersion) ||
      !validInstallationId(installationId) || !path.isAbsolute(String(installTarget || ''))) return false;
  if (receipt.installationId !== installationId || path.resolve(receipt.installTarget || '') !== path.resolve(installTarget)) return false;
  const previousVersion = String(receipt.previousVersion || '');
  const targetVersion = String(receipt.targetVersion || '');
  if (!parseVersion(previousVersion) || !parseVersion(targetVersion)) return false;

  switch (receipt.phase) {
    // The outgoing shell and the newly activated shell may both observe the
    // worker before it writes its terminal receipt.
    case 'preparing':
    case 'armed':
    case 'activating':
      return currentVersion === previousVersion || currentVersion === targetVersion;
    case 'healthy':
      return currentVersion === String(receipt.installedVersion || targetVersion);
    case 'rollback_healthy':
    case 'failed':
      return currentVersion === String(receipt.installedVersion || previousVersion);
    default:
      return false;
  }
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
  if (platform === 'win32' && raw.portable !== true) throw new Error('Windows automatic updates require a portable release');
  return raw;
}

function snapshotReleaseManifest(manifest) {
  const copy = JSON.parse(JSON.stringify(manifest));
  const freeze = (value) => {
    if (!value || typeof value !== 'object' || Object.isFrozen(value)) return value;
    for (const child of Object.values(value)) freeze(child);
    return Object.freeze(value);
  };
  return freeze(copy);
}

function httpsRequest(url, {
  timeoutMs = 15000,
  maxBytes = MAX_MANIFEST_BYTES,
  redirects = 5,
} = {}) {
  return new Promise((resolve, reject) => {
    let parsed;
    try { parsed = new URL(url); } catch { reject(new Error('invalid update URL')); return; }
    if (parsed.protocol !== 'https:') { reject(new Error('updates require HTTPS')); return; }
    const request = https.get(parsed, {
      timeout: timeoutMs,
      headers: {
        'user-agent': 'Workass-Updater/1',
        accept: 'application/json',
        'cache-control': 'no-cache, no-store',
        pragma: 'no-cache',
      },
    }, (response) => {
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

function networkRequestResponse(url, {
  timeoutMs,
  redirects,
  networkRequest,
  accept,
}) {
  return new Promise((resolve, reject) => {
    let parsed;
    try { parsed = new URL(url); } catch { reject(new Error('invalid update URL')); return; }
    if (parsed.protocol !== 'https:') { reject(new Error('updates require HTTPS')); return; }
    if (typeof networkRequest !== 'function') { reject(new Error('verified system-network updater is unavailable')); return; }

    let request;
    try {
      request = networkRequest({
        method: 'GET',
        url: parsed.href,
        redirect: 'manual',
        // GitHub's stable /releases/latest URL intentionally redirects as new
        // immutable releases are published. Chromium's default HTTP cache can
        // otherwise preserve an older redirect/manifest while Workass stays
        // open, so updater traffic must always revalidate at the network edge.
        cache: 'no-store',
        headers: {
          'user-agent': 'Workass-Updater/1',
          accept,
          'cache-control': 'no-cache, no-store',
          pragma: 'no-cache',
        },
      });
    } catch (err) {
      reject(err);
      return;
    }
    let remainingRedirects = redirects;
    let timer = null;
    let settled = false;
    let timedOut = false;
    const finish = () => {
      if (timer) clearTimeout(timer);
      timer = null;
    };
    const fail = (err) => {
      if (settled) return;
      settled = true;
      finish();
      reject(err);
    };
    const abort = () => {
      try { request.abort(); } catch { /* best effort */ }
    };
    const touch = () => {
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => {
        timedOut = true;
        fail(new Error('update request timed out'));
        abort();
      }, timeoutMs);
      timer.unref?.();
    };

    request.on('redirect', (_statusCode, _method, redirectURL) => {
      if (remainingRedirects <= 0) {
        fail(new Error('too many update redirects'));
        abort();
        return;
      }
      let next;
      try { next = new URL(redirectURL); } catch {
        fail(new Error('invalid update redirect'));
        abort();
        return;
      }
      if (next.protocol !== 'https:') {
        fail(new Error('updates require HTTPS'));
        abort();
        return;
      }
      remainingRedirects -= 1;
      touch();
      // Electron requires this call synchronously inside the redirect event.
      try { request.followRedirect(); } catch (err) {
        fail(err);
        abort();
      }
    });
    request.on('response', (response) => {
      if (settled) return;
      const status = Number(response?.statusCode || 0);
      if (status < 200 || status >= 300) {
        response.on?.('data', () => {});
        const error = new Error(`update server returned HTTP ${status || 500}`);
        error.statusCode = status || 500;
        fail(error);
        return;
      }
      settled = true;
      touch();
      resolve({ response, request, touch, finish, timedOut: () => timedOut });
    });
    request.on('error', (err) => fail(err));
    request.on('abort', () => {
      if (!settled) fail(new Error(timedOut ? 'update request timed out' : 'update request was aborted'));
    });
    touch();
    request.end();
  });
}

async function networkRequestBytes(url, {
  timeoutMs = 15000,
  maxBytes = MAX_MANIFEST_BYTES,
  redirects = 5,
  networkRequest,
  accept = 'application/json',
} = {}) {
  const active = await networkRequestResponse(url, { timeoutMs, redirects, networkRequest, accept });
  try {
    return await new Promise((resolve, reject) => {
      const chunks = [];
      let total = 0;
      active.response.on('data', (value) => {
        active.touch();
        const chunk = Buffer.from(value);
        total += chunk.length;
        if (total > maxBytes) {
          active.request.abort();
          reject(new Error('update response is too large'));
          return;
        }
        chunks.push(chunk);
      });
      active.response.on('end', () => resolve(Buffer.concat(chunks, total)));
      active.response.on('error', reject);
      active.response.on('aborted', () => reject(new Error('update response was aborted')));
    });
  } catch (err) {
    if (active.timedOut()) throw new Error('update request timed out');
    throw err;
  } finally {
    active.finish();
  }
}

async function fetchReleaseManifest(url, options = {}) {
  if (path.isAbsolute(String(url || ''))) {
    const info = await fs.promises.stat(url);
    if (!info.isFile()) throw new Error('local update manifest is not a regular file');
    if (info.size <= 0 || info.size > MAX_MANIFEST_BYTES) throw new Error('local update manifest has an invalid size');
    const bytes = await fs.promises.readFile(url);
    let parsed;
    try { parsed = JSON.parse(bytes.toString('utf8')); } catch { throw new Error('update manifest is not valid JSON'); }
    return parsed;
  }
  const bytes = typeof options.networkRequest === 'function'
    ? await networkRequestBytes(url, { ...options, maxBytes: MAX_MANIFEST_BYTES })
    : await httpsRequest(url, { ...options, maxBytes: MAX_MANIFEST_BYTES });
  let parsed;
  try { parsed = JSON.parse(bytes.toString('utf8')); } catch { throw new Error('update manifest is not valid JSON'); }
  return parsed;
}

function copyLocalArtifact(source, destination, artifact, { onProgress = () => {} } = {}) {
  return new Promise((resolve, reject) => {
    let info;
    try { info = fs.statSync(source); } catch (err) { reject(err); return; }
    if (!info.isFile()) { reject(new Error('local update artifact is not a regular file')); return; }
    if (info.size !== artifact.size || info.size > MAX_UPDATE_BYTES) { reject(new Error('local update size does not match the release manifest')); return; }
    const partial = `${destination}.partial`;
    fs.mkdirSync(path.dirname(destination), { recursive: true, mode: 0o700 });
    const input = fs.createReadStream(source);
    const output = fs.createWriteStream(partial, { flags: 'wx', mode: 0o600 });
    const hash = crypto.createHash('sha256');
    let received = 0;
    let settled = false;
    const fail = (error) => {
      if (settled) return;
      settled = true;
      try { input.destroy(); } catch { /* best effort */ }
      try { output.destroy(); } catch { /* best effort */ }
      try { fs.rmSync(partial, { force: true }); } catch { /* best effort */ }
      reject(error);
    };
    input.on('data', (chunk) => {
      received += chunk.length;
      hash.update(chunk);
      onProgress(received, artifact.size);
    });
    input.on('error', fail);
    output.on('error', fail);
    output.on('finish', () => {
      if (settled) return;
      if (received !== artifact.size) { fail(new Error('local update size does not match the release manifest')); return; }
      if (hash.digest('hex').toLowerCase() !== artifact.sha256.toLowerCase()) { fail(new Error('local update checksum does not match the release manifest')); return; }
      settled = true;
      fs.renameSync(partial, destination);
      resolve(destination);
    });
    input.pipe(output);
  });
}

function downloadArtifact(url, destination, artifact, {
  onProgress = () => {},
  redirects = 5,
  networkRequest,
} = {}) {
  if (path.isAbsolute(String(url || ''))) return copyLocalArtifact(url, destination, artifact, { onProgress });
  if (typeof networkRequest === 'function') {
    return downloadArtifactWithNetworkRequest(url, destination, artifact, { onProgress, redirects, networkRequest });
  }
  return new Promise((resolve, reject) => {
    const parsed = new URL(url);
    if (parsed.protocol !== 'https:') { reject(new Error('updates require HTTPS')); return; }
    const request = https.get(parsed, {
      timeout: 30000,
      headers: { 'user-agent': 'Workass-Updater/1', accept: 'application/octet-stream' },
    }, (response) => {
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
        if (received > artifact.size || received > MAX_UPDATE_BYTES) { response.destroy(new Error('update exceeded its declared size')); return; }
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

async function downloadArtifactWithNetworkRequest(url, destination, artifact, {
  onProgress = () => {},
  redirects = 5,
  networkRequest,
} = {}) {
  const active = await networkRequestResponse(url, {
    timeoutMs: 30000,
    redirects,
    networkRequest,
    accept: 'application/octet-stream',
  });
  const partial = `${destination}.partial`;
  fs.mkdirSync(path.dirname(destination), { recursive: true, mode: 0o700 });
  const output = fs.createWriteStream(partial, { flags: 'wx', mode: 0o600 });
  const hash = crypto.createHash('sha256');
  let received = 0;
  const verifier = new Transform({
    transform(value, _encoding, callback) {
      active.touch();
      const chunk = Buffer.from(value);
      received += chunk.length;
      if (received > artifact.size || received > MAX_UPDATE_BYTES) {
        callback(new Error('update exceeded its declared size'));
        return;
      }
      hash.update(chunk);
      onProgress(received, artifact.size);
      callback(null, chunk);
    },
  });
  try {
    await pipeline(active.response, verifier, output);
    if (received !== artifact.size) throw new Error('update size does not match the release manifest');
    if (hash.digest('hex').toLowerCase() !== artifact.sha256.toLowerCase()) throw new Error('update checksum does not match the release manifest');
    fs.renameSync(partial, destination);
    return destination;
  } catch (err) {
    try { active.request.abort(); } catch { /* best effort */ }
    try { fs.rmSync(partial, { force: true }); } catch { /* best effort */ }
    if (active.timedOut()) throw new Error('update download timed out');
    throw err;
  } finally {
    active.finish();
  }
}

function resolveArtifactSource(artifactURL, feedURL) {
  if (path.isAbsolute(String(feedURL || ''))) {
    const name = String(artifactURL || '').trim();
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/.test(name)) throw new Error('local update artifact name is invalid');
    return path.join(path.dirname(feedURL), name);
  }
  const source = new URL(artifactURL, feedURL);
  if (source.protocol !== 'https:') throw new Error('updates require HTTPS');
  return source.href;
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

function validateArchiveLinksBeforeExtraction(archive, platform = process.platform, { run = spawnSync } = {}) {
  if (platform === 'darwin') {
    // /usr/bin/ditto extracts ZIP entries without following archive-created
    // symlinks while walking later entries. The selected Workass.app subtree is
    // then link-contained by validateExtractedTree before it is staged.
    return;
  }
  const result = run('tar.exe', ['-tvf', archive], {
    encoding: 'utf8', windowsHide: true, maxBuffer: 64 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error || result.status !== 0) throw new Error('update archive link metadata could not be inspected');
  const lines = String(result.stdout || '').split(/\r?\n/).filter(Boolean);
  if (lines.length === 0 || lines.length > MAX_ARCHIVE_ENTRIES ||
      lines.some((line) => /^[lh]/.test(line.trimStart()) || /\s(?:->|link to)\s/i.test(line))) {
    throw new Error('Windows update archives cannot contain links');
  }
}

function extractArchive(archive, destination, platform = process.platform) {
  archiveEntries(archive, platform);
  validateArchiveLinksBeforeExtraction(archive, platform);
  fs.mkdirSync(destination, { recursive: true, mode: 0o700 });
  const command = platform === 'win32' ? 'tar.exe' : '/usr/bin/ditto';
  const args = platform === 'win32' ? ['-xf', archive, '-C', destination] : ['-x', '-k', archive, destination];
  const result = spawnSync(command, args, { encoding: 'utf8', windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'] });
  if (result.error || result.status !== 0) throw new Error('update archive extraction failed');
}

function validateExtractedTree(root, platform = process.platform) {
  const rootStat = fs.lstatSync(root, { throwIfNoEntry: false });
  if (!rootStat?.isDirectory()) throw new Error('extracted update root is not a directory');
  const rootReal = fs.realpathSync(root);
  const pending = [root];
  const hardLinks = new Map();
  let inspected = 0;
  const insideRoot = (candidate) => {
    const relative = path.relative(rootReal, candidate);
    return relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative));
  };
  while (pending.length > 0) {
    const directory = pending.pop();
    for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
      inspected += 1;
      if (inspected > MAX_ARCHIVE_ENTRIES) throw new Error('extracted update tree has too many entries');
      const candidate = path.join(directory, entry.name);
      const stat = fs.lstatSync(candidate);
      if (stat.isSymbolicLink()) {
        if (platform === 'win32') throw new Error('Windows update archives cannot contain links');
        let resolved;
        try { resolved = fs.realpathSync(candidate); }
        catch { throw new Error('macOS update archive contains a broken link'); }
        if (!insideRoot(resolved)) throw new Error('macOS update archive link escapes the extracted root');
        continue;
      }
      if (stat.isDirectory()) {
        pending.push(candidate);
        continue;
      }
      if (!stat.isFile()) throw new Error('update archive contains an unsupported filesystem entry');
      if (stat.nlink > 1) {
        if (platform === 'win32') throw new Error('Windows update archives cannot contain links');
        const key = `${stat.dev}:${stat.ino}`;
        const link = hardLinks.get(key) || { expected: stat.nlink, found: 0 };
        link.found += 1;
        hardLinks.set(key, link);
      }
    }
  }
  for (const link of hardLinks.values()) {
    if (link.found !== link.expected) throw new Error('macOS update archive hard link escapes the extracted root');
  }
  return { entries: inspected };
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

function verifyWindowsRelease(_currentRoot, incomingRoot, targetVersion, arch = 'x64', installationId = '') {
  const expectedArch = releaseArch('win32', arch);
  const manifest = readReleaseJSON(path.join(incomingRoot, 'manifest.json'), 'runtime manifest');
  if (manifest.schemaVersion !== 2 || manifest.version !== targetVersion || manifest.platform !== 'windows' ||
      manifest.arch !== expectedArch || manifest.portable !== true || manifest.electron !== true) {
    throw new Error('incoming portable runtime does not match the release manifest');
  }
  const packageManifest = readReleaseJSON(path.join(incomingRoot, 'resources', 'app', 'package.json'), 'shell manifest');
  if (packageManifest.version !== targetVersion) throw new Error('incoming Windows shell version does not match the release manifest');
  verifyWindowsPE(path.join(incomingRoot, 'Workass.exe'), 'Workass.exe');
  verifyWindowsPE(path.join(incomingRoot, 'workass-daemon.exe'), 'workass-daemon.exe');
  verifyWindowsPE(path.join(incomingRoot, 'node', `windows-${expectedArch}`, 'node.exe'), 'portable node.exe');
  for (const [relative, label] of [
    [['resources', 'app', 'update-manager.js'], 'update manager'],
    [['resources', 'app', 'update-worker.js'], 'update worker'],
    [['resources', 'app', 'update-lock-recovery.js'], 'update lock recovery'],
    [['resources', 'renderer', 'index.html'], 'renderer'],
    [['frontier-hosts', `windows-${expectedArch}`, 'claude-native-host.mjs'], 'Claude host'],
    [['frontier-hosts', `windows-${expectedArch}`, 'codex-native-host.mjs'], 'Codex host'],
    [['frontier-hosts', `windows-${expectedArch}`, 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'], 'Claude Agent SDK'],
  ]) requiredRegularFile(path.join(incomingRoot, ...relative), label);
  const identity = readInstallationIdentity(incomingRoot);
  if (!identity || identity.installationId !== installationId) {
    throw new Error('incoming Windows release does not belong to this portable installation');
  }
}

function stageRelease(source, incomingTarget, platform = process.platform) {
  if (platform === 'darwin') {
    const copy = spawnSync('/usr/bin/ditto', [source, incomingTarget], { stdio: ['ignore', 'pipe', 'pipe'] });
    if (copy.error || copy.status !== 0) throw new Error('could not stage the signed app beside the installed app');
    return;
  }
  fs.cpSync(source, incomingTarget, { recursive: true, errorOnExist: true });
}

function verifyRelease(currentRoot, incomingRoot, targetVersion, platform = process.platform, arch = process.arch, installationId = '') {
  if (platform === 'darwin') return verifyMacRelease(currentRoot, incomingRoot, targetVersion);
  verifyWindowsRelease(currentRoot, incomingRoot, targetVersion, arch, installationId);
  return '';
}

function validateStageRequest(request) {
  if (!request || request.schemaVersion !== 1 || !['darwin', 'win32'].includes(request.platform) ||
      !parseVersion(request.targetVersion)) {
    throw new Error('invalid update staging request');
  }
  for (const key of ['transactionRoot', 'archive', 'extracted', 'currentRoot', 'incomingTarget']) {
    if (!path.isAbsolute(String(request[key] || ''))) throw new Error(`update staging ${key} must be absolute`);
  }
  if (request.archive !== path.join(request.transactionRoot, 'release.zip') ||
      request.extracted !== path.join(request.transactionRoot, 'extracted')) {
    throw new Error('update staging paths escaped the transaction');
  }
  if (request.platform === 'win32') {
    if (!validInstallationId(request.installationId)) throw new Error('Windows update staging has no installation identity');
    if (request.incomingTarget !== path.join(request.transactionRoot, 'incoming-release')) {
      throw new Error('Windows update staging target escaped the transaction');
    }
  } else if (path.dirname(request.incomingTarget) !== path.dirname(request.currentRoot) ||
             !path.basename(request.incomingTarget).startsWith('.Workass.app.incoming-')) {
    throw new Error('macOS update staging target must stay beside the installed app');
  }
  return request;
}

function stageAndVerifyRelease(rawRequest, dependencies = {}) {
  const request = validateStageRequest(rawRequest);
  const extract = dependencies.extractArchive || extractArchive;
  const findRoot = dependencies.findExtractedRoot || findExtractedRoot;
  const stage = dependencies.stageRelease || stageRelease;
  const verify = dependencies.verifyRelease || verifyRelease;
  const validateTree = dependencies.validateExtractedTree || validateExtractedTree;
  extract(request.archive, request.extracted, request.platform);
  const source = findRoot(request.extracted, request.platform);
  validateTree(source, request.platform);
  stage(source, request.incomingTarget, request.platform);
  if (request.platform === 'win32') {
    const currentIdentity = readInstallationIdentity(request.currentRoot);
    if (!currentIdentity || currentIdentity.installationId !== request.installationId) {
      throw new Error('installed Windows portable identity changed during staging');
    }
    atomicJSON(installationIdentityPath(request.incomingTarget), currentIdentity);
  }
  const designatedRequirement = verify(
    request.currentRoot,
    request.incomingTarget,
    request.targetVersion,
    request.platform,
    request.arch,
    request.installationId,
  );
  return { designatedRequirement: String(designatedRequirement || '') };
}

function runUpdateStageWorker(request, { WorkerClass = Worker, workerFile = __filename } = {}) {
  return new Promise((resolve, reject) => {
    let thread;
    try {
      thread = new WorkerClass(workerFile, {
        workerData: { workassTask: 'stage-update', request },
      });
    } catch (err) {
      reject(err);
      return;
    }
    let settled = false;
    const fail = (err) => {
      if (settled) return;
      settled = true;
      reject(err instanceof Error ? err : new Error(String(err || 'update staging worker failed')));
    };
    thread.once('message', (message) => {
      if (settled) return;
      if (!message || message.ok !== true) {
        fail(new Error(String(message?.error || 'update staging worker failed')));
        return;
      }
      settled = true;
      resolve(message.result || { designatedRequirement: '' });
    });
    thread.once('error', fail);
    thread.once('exit', (code) => {
      if (!settled) fail(new Error(`update staging worker exited before its receipt${code ? ` (${code})` : ''}`));
    });
  });
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
  const descriptor = fs.openSync(incoming, 'r');
  try { fs.fsyncSync(descriptor); } finally { fs.closeSync(descriptor); }
  fs.renameSync(incoming, file);
}

function isExactArmedReceipt(receipt, {
  updateId, currentVersion, targetVersion, installationId, installTarget, workerId, workerPID,
}) {
  return exactWorkerReceipt(receipt, {
    updateId, currentVersion, targetVersion, installationId, installTarget, workerId, workerPID,
  }) && receipt.phase === 'armed';
}

function exactWorkerReceipt(receipt, {
  updateId, currentVersion, targetVersion, installationId, installTarget, workerId, workerPID,
}) {
  return receipt?.schemaVersion === RECEIPT_SCHEMA_VERSION &&
    receipt.updateId === updateId && receipt.previousVersion === currentVersion &&
    receipt.targetVersion === targetVersion && receipt.installationId === installationId &&
    path.resolve(receipt.installTarget || '') === path.resolve(installTarget || '') &&
    receipt.workerId === workerId && Number.isInteger(workerPID) && workerPID > 1 && receipt.workerPID === workerPID;
}

function isExactTerminalReceipt(receipt, request) {
  if (!exactWorkerReceipt(receipt, request) || !['healthy', 'rollback_healthy', 'failed'].includes(receipt.phase)) return false;
  const expectedInstalled = receipt.phase === 'healthy' ? request.targetVersion : request.currentVersion;
  return receipt.installedVersion === expectedInstalled;
}

function spawnArmedUpdateWorker({
  command,
  args = [],
  options = {},
  receiptPath,
  updateId,
  currentVersion,
  targetVersion,
  installationId,
  installTarget,
  workerId,
  timeoutMs = DEFAULT_WORKER_ARM_TIMEOUT_MS,
  pollIntervalMs = DEFAULT_WORKER_ARM_POLL_MS,
}, {
  spawnProcess = spawn,
  readReceipt = (file) => JSON.parse(fs.readFileSync(file, 'utf8')),
  schedule = setTimeout,
  cancelSchedule = clearTimeout,
  repeat = setInterval,
  cancelRepeat = clearInterval,
  terminateChild = async (child) => {
    const exited = () => child?.exitCode != null || child?.signalCode != null;
    if (!child || !Number.isInteger(child.pid) || child.pid <= 1 || exited()) return true;
    try { child.kill(); } catch { /* inspect the process handle below */ }
    return new Promise((resolve) => {
      let settled = false;
      const finish = (value) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve(value);
      };
      const timer = setTimeout(() => finish(exited()), 5000);
      timer.unref?.();
      child.once('exit', () => finish(true));
      if (exited()) finish(true);
    });
  },
} = {}) {
  if (!path.isAbsolute(String(command || '')) || !path.isAbsolute(String(receiptPath || '')) ||
      !/^[A-Za-z0-9_-]{8,96}$/.test(String(updateId || '')) ||
      !validInstallationId(installationId) || !path.isAbsolute(String(installTarget || '')) ||
      !/^worker-[a-f0-9]{32}$/.test(String(workerId || '')) ||
      !parseVersion(currentVersion) || !parseVersion(targetVersion) ||
      !Number.isFinite(timeoutMs) || timeoutMs <= 0 ||
      !Number.isFinite(pollIntervalMs) || pollIntervalMs <= 0) {
    return Promise.reject(new Error('invalid independent update worker handoff'));
  }
  return new Promise((resolve, reject) => {
    let child;
    let timeout = null;
    let poller = null;
    let settled = false;
    let stopping = false;
    const clear = () => {
      if (timeout) cancelSchedule(timeout);
      if (poller) cancelRepeat(poller);
      timeout = null;
      poller = null;
    };
    const fail = async (err) => {
      if (settled || stopping) return;
      stopping = true;
      clear();
      const error = err instanceof Error ? err : new Error(String(err || 'independent update worker failed to arm'));
      let fenced = false;
      try { fenced = await terminateChild(child); } catch { fenced = false; }
      settled = true;
      if (!fenced) {
        const fenceError = new Error('independent update worker could not be fenced after an arm failure');
        fenceError.code = 'WORKASS_UPDATE_WORKER_FENCE_FAILED';
        reject(fenceError);
        return;
      }
      error.workassWorkerFenced = true;
      reject(error);
    };
    const inspect = () => {
      if (settled) return;
      let receipt;
      try { receipt = readReceipt(receiptPath); } catch { return; }
      const exactRequest = {
        updateId, currentVersion, targetVersion, installationId, installTarget, workerId, workerPID: child?.pid,
      };
      if (!isExactArmedReceipt(receipt, exactRequest) && !isExactTerminalReceipt(receipt, exactRequest)) return;
      settled = true;
      clear();
      child.workassUpdateReceipt = receipt;
      child.unref?.();
      resolve(child);
    };
    try {
      child = spawnProcess(command, args, options);
    } catch (err) {
      void fail(err);
      return;
    }
    if (!child || typeof child.once !== 'function') {
      void fail(new Error('independent update worker returned no process handle'));
      return;
    }
    child.once('error', (err) => { void fail(err); });
    child.once('exit', (code, signal) => {
      inspect();
      if (settled) return;
      void fail(new Error(`independent update worker exited before arming (${signal || code || 0})`));
    });
    child.once('spawn', () => {
      inspect();
      if (settled) return;
      poller = repeat(inspect, pollIntervalMs);
      timeout = schedule(() => { void fail(new Error('independent update worker did not durably arm')); }, timeoutMs);
    });
  });
}

function createProgressPublisher(publish, {
  now = Date.now,
  minIntervalMs = 100,
  minDelta = 0.01,
} = {}) {
  let lastProgress = 0;
  let lastPublishedAt = now();
  return (received, total) => {
    if (!(total > 0)) return;
    const progress = Math.max(0, Math.min(1, received / total));
    const current = now();
    if (progress < 1 && progress - lastProgress < minDelta && current - lastPublishedAt < minIntervalMs) return;
    if (progress <= lastProgress && progress < 1) return;
    lastProgress = progress;
    lastPublishedAt = current;
    publish(progress);
  };
}

function prepareUpdateWorkerRuntime({
  transactionRoot,
  workerSource,
  nodeSource,
  platform = process.platform,
}, { spawnProcess = spawn } = {}) {
  return (async () => {
    const workerPath = path.join(transactionRoot, 'update-worker.js');
    const nodePath = platform === 'win32' ? path.join(transactionRoot, 'updater-node.exe') : nodeSource;
    await fs.promises.mkdir(transactionRoot, { recursive: true, mode: 0o700 });
    await fs.promises.copyFile(workerSource, workerPath);
    if (platform === 'win32') await fs.promises.copyFile(nodeSource, nodePath);
    await new Promise((resolve, reject) => {
      let child;
      try {
        child = spawnProcess(nodePath, [workerPath, '--self-test'], {
          windowsHide: true,
          stdio: 'ignore',
        });
      } catch (err) {
        reject(err);
        return;
      }
      let settled = false;
      const fail = (err) => {
        if (settled) return;
        settled = true;
        reject(err instanceof Error ? err : new Error(String(err || 'update worker self-test failed')));
      };
      child.once('error', fail);
      child.once('exit', (code, signal) => {
        if (settled) return;
        settled = true;
        if (code === 0) resolve();
        else reject(new Error(`the independent update worker did not pass its self-test (${signal || code || 0})`));
      });
    });
    return { workerPath, nodePath };
  })();
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

function readJSONFile(file) {
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch { return null; }
}

function journalForTransaction(transaction) {
  const journal = readJSONFile(transaction?.journalPath);
  if (!journal || journal.schemaVersion !== 1 || journal.updateId !== transaction?.updateId ||
      journal.installationId !== transaction?.installationId ||
      path.resolve(String(journal.installTarget || '')) !== path.resolve(String(transaction?.installTarget || '')) ||
      journal.previousVersion !== transaction?.currentVersion || journal.targetVersion !== transaction?.targetVersion) return null;
  return journal;
}

function handoffPathForTransaction(transaction) {
  return path.join(transaction.transactionRoot, 'handoff.json');
}

function readHandoffState(transaction) {
  const handoff = readJSONFile(handoffPathForTransaction(transaction));
  if (!handoff) return null;
  if (handoff.schemaVersion !== 1 || handoff.updateId !== transaction.updateId ||
      handoff.installationId !== transaction.installationId ||
      handoff.previousVersion !== transaction.currentVersion || handoff.targetVersion !== transaction.targetVersion ||
      path.resolve(String(handoff.installTarget || '')) !== path.resolve(transaction.installTarget) ||
      !['intent', 'prepared', 'committed', 'cancelled'].includes(handoff.state)) return null;
  return handoff;
}

function writeHandoffState(transaction, state) {
  const handoff = {
    schemaVersion: 1,
    updateId: transaction.updateId,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    state,
    updatedAt: new Date().toISOString(),
  };
  atomicJSON(handoffPathForTransaction(transaction), handoff);
  return handoff;
}

function handoffNeedsCancel(transaction) {
  return ['intent', 'prepared'].includes(readHandoffState(transaction)?.state);
}

function terminalReceiptForTransaction(transaction) {
  const journal = journalForTransaction(transaction);
  if (!journal?.terminal || !['healthy', 'rollback_healthy', 'failed'].includes(journal.phase)) return null;
  const installedVersion = journal.phase === 'healthy' ? transaction.targetVersion : transaction.currentVersion;
  if (journal.installedVersion !== installedVersion) return null;
  const existing = readJSONFile(transaction.receiptPath);
  if (existing?.schemaVersion === RECEIPT_SCHEMA_VERSION && existing.updateId === transaction.updateId &&
      existing.phase === journal.phase && existing.previousVersion === transaction.currentVersion &&
      existing.targetVersion === transaction.targetVersion && existing.installedVersion === installedVersion &&
      existing.installationId === transaction.installationId &&
      existing.workerId === transaction.workerId &&
      path.resolve(String(existing.installTarget || '')) === path.resolve(transaction.installTarget)) return existing;
  const receipt = {
    schemaVersion: RECEIPT_SCHEMA_VERSION,
    updateId: transaction.updateId,
    phase: journal.phase,
    previousVersion: transaction.currentVersion,
    targetVersion: transaction.targetVersion,
    installedVersion,
    installationId: transaction.installationId,
    installTarget: transaction.installTarget,
    workerId: transaction.workerId,
    updatedAt: new Date().toISOString(),
    activated: journal.phase === 'healthy',
    ...(journal.error ? { error: journal.error } : {}),
    ...(journal.rollbackError ? { rollbackError: journal.rollbackError } : {}),
  };
  atomicJSON(transaction.receiptPath, receipt);
  return receipt;
}

function workerProcessOwnership(lease, transaction, {
  platform = process.platform,
  run = spawnSync,
  now = Date.now,
  heartbeatMaxAgeMs = WORKER_TAKEOVER_MAX_AGE_MS,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
} = {}) {
  if (!lease || lease.schemaVersion !== 1 || lease.state !== 'running' ||
      lease.updateId !== transaction?.updateId || lease.workerId !== transaction?.workerId ||
      lease.installationId !== transaction?.installationId ||
      !Number.isInteger(lease.pid) || lease.pid <= 1 || !alive(lease.pid)) return { owned: false, exact: false, stale: false };
  const workerPath = path.resolve(String(lease.workerPath || ''));
  const transactionPath = path.resolve(String(lease.transactionPath || ''));
  if (workerPath !== path.join(transaction.transactionRoot, 'update-worker.js') ||
      transactionPath !== path.join(transaction.transactionRoot, 'transaction.json')) return { owned: false, exact: false, stale: false };
  let result;
  if (platform === 'win32') {
    const script = [
      "$ErrorActionPreference = 'Stop'",
      '$process = Get-CimInstance Win32_Process -Filter (\'ProcessId = \' + $env:WORKASS_WORKER_PID)',
      'if ($null -eq $process) { exit 3 }',
      '$command = [string]$process.CommandLine',
      'if ($command.IndexOf($env:WORKASS_WORKER_PATH, [StringComparison]::OrdinalIgnoreCase) -ge 0 -and $command.IndexOf($env:WORKASS_TRANSACTION_PATH, [StringComparison]::OrdinalIgnoreCase) -ge 0) { Write-Output OWNED; exit 0 }',
      'exit 4',
    ].join('; ');
    result = run('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', script], {
      windowsHide: true,
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
      env: {
        ...process.env,
        WORKASS_WORKER_PID: String(lease.pid),
        WORKASS_WORKER_PATH: workerPath,
        WORKASS_TRANSACTION_PATH: transactionPath,
      },
    });
  } else {
    result = run('/bin/ps', ['-p', String(lease.pid), '-o', 'command='], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'],
    });
  }
  if (result.error || result.status !== 0) return { owned: false, exact: false, stale: false };
  const exact = platform === 'win32'
    ? /\bOWNED\b/.test(String(result.stdout || ''))
    : String(result.stdout || '').includes(workerPath) && String(result.stdout || '').includes(transactionPath);
  if (!exact) return { owned: false, exact: false, stale: false };
  const heartbeat = Date.parse(String(lease.updatedAt || ''));
  const age = now() - heartbeat;
  const fresh = Number.isFinite(heartbeat) && age >= 0 && age <= heartbeatMaxAgeMs;
  return { owned: fresh, exact: true, stale: !fresh, pid: lease.pid };
}

function cheapWorkerLeaseOwnership(lease, transaction, {
  now = Date.now,
  heartbeatMaxAgeMs = WORKER_HEARTBEAT_MAX_AGE_MS,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
} = {}) {
  if (!lease || lease.schemaVersion !== 1 || lease.state !== 'running' ||
      lease.updateId !== transaction?.updateId || lease.workerId !== transaction?.workerId ||
      lease.installationId !== transaction?.installationId || !Number.isInteger(lease.pid) || lease.pid <= 1 ||
      path.resolve(String(lease.workerPath || '')) !== path.join(transaction.transactionRoot, 'update-worker.js') ||
      path.resolve(String(lease.transactionPath || '')) !== path.join(transaction.transactionRoot, 'transaction.json')) {
    return { owned: false, fresh: false, alive: false };
  }
  const heartbeat = Date.parse(String(lease.updatedAt || ''));
  const age = now() - heartbeat;
  const fresh = Number.isFinite(heartbeat) && age >= 0 && age <= heartbeatMaxAgeMs;
  const running = alive(lease.pid);
  return { owned: fresh && running, fresh, alive: running, pid: lease.pid };
}

function inspectWorkerOwnershipAsync(lease, transaction, {
  WorkerClass = Worker,
  workerFile = __filename,
} = {}) {
  return new Promise((resolve, reject) => {
    let thread;
    try {
      thread = new WorkerClass(workerFile, {
        workerData: { workassTask: 'inspect-update-worker', lease, transaction },
      });
    } catch (err) {
      reject(err);
      return;
    }
    let settled = false;
    const fail = (err) => {
      if (settled) return;
      settled = true;
      reject(err instanceof Error ? err : new Error(String(err || 'update worker ownership inspection failed')));
    };
    thread.once('message', (message) => {
      if (settled) return;
      if (!message?.ok) { fail(new Error(String(message?.error || 'update worker ownership inspection failed'))); return; }
      settled = true;
      resolve(message.ownership || { owned: false, exact: false, stale: false });
    });
    thread.once('error', fail);
    thread.once('exit', (code) => {
      if (!settled) fail(new Error(`update worker ownership inspection exited early (${code || 0})`));
    });
  });
}

async function terminateExactWorker(ownership, {
  platform = process.platform,
  run = spawnSync,
  alive = (pid) => {
    try { process.kill(pid, 0); return true; } catch { return false; }
  },
  kill = process.kill,
  pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
} = {}) {
  if (!ownership?.exact || !Number.isInteger(ownership.pid) || ownership.pid <= 1) return true;
  if (platform === 'win32') {
    const result = run('taskkill.exe', ['/PID', String(ownership.pid), '/T', '/F'], {
      windowsHide: true, stdio: 'ignore',
    });
    if ((result.error || result.status !== 0) && alive(ownership.pid)) return false;
  } else {
    try { kill(ownership.pid, 'SIGTERM'); } catch { /* already exited */ }
  }
  for (let attempt = 0; attempt < 50; attempt += 1) {
    if (!alive(ownership.pid)) return true;
    await pause(100);
  }
  return !alive(ownership.pid);
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
    const networkRequest = typeof deps.networkRequest === 'function' ? deps.networkRequest : null;
    const ownershipOverride = typeof deps.workerProcessOwnership === 'function' ? deps.workerProcessOwnership : null;
    const asyncOwnershipOverride = typeof deps.inspectWorkerOwnership === 'function' ? deps.inspectWorkerOwnership : null;
    const dependencyOverrides = { ...deps };
    delete dependencyOverrides.networkRequest;
    delete dependencyOverrides.workerProcessOwnership;
    delete dependencyOverrides.inspectWorkerOwnership;
    this.networkRequestAvailable = Boolean(networkRequest);
    this.deps = {
      fetchManifest: (url, options = {}) => fetchReleaseManifest(url, { ...options, networkRequest }),
      downloadArtifact: (url, destination, artifact, options = {}) => downloadArtifact(
        url, destination, artifact, { ...options, networkRequest },
      ),
      postLocalUpdate,
      spawn,
      stageAndVerify: runUpdateStageWorker,
      schedule: setTimeout,
      cancelSchedule: clearTimeout,
      repeat: setInterval,
      cancelRepeat: clearInterval,
      inspectWorkerOwnership: asyncOwnershipOverride || (ownershipOverride
        ? async (lease, transaction, options) => ownershipOverride(lease, transaction, options)
        : inspectWorkerOwnershipAsync),
      terminateExactWorker,
      prepareWorkerRuntime: (request) => prepareUpdateWorkerRuntime(request, { spawnProcess: spawn }),
      ...dependencyOverrides,
    };
    this.updateRoot = path.join(runtime.dataRoot, 'updates');
    this.receiptPath = path.join(this.updateRoot, 'receipt.json');
    this.currentVersion = String(currentVersion || app?.getVersion?.() || '0.0.0');
    this.installTarget = installedRoot(this.resourcesPath, this.executablePath, this.platform);
    this.installationIdentity = null;
    this.manifest = null;
    this.activeRelease = null;
    this.prepared = null;
    this.receiptTimer = null;
    this.initialCheckTimer = null;
    this.periodicCheckTimer = null;
    this.activeOperation = null;
    this.discoveryPromise = null;
    this.applyPromise = null;
    this.recoveryPromise = null;
    this.watchedUpdateId = '';
    this.verifiedWorkerLeases = new Map();
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
      availableVersion: null,
    };
  }

  snapshot() { return JSON.parse(JSON.stringify(this.state)); }

  workerLeaseKey(lease, transaction) {
    if (!lease || !transaction) return '';
    return [transaction.updateId, transaction.workerId, lease.pid, lease.startedAt || ''].join(':');
  }

  rememberVerifiedWorker(lease, transaction, milliseconds = 30000) {
    const key = this.workerLeaseKey(lease, transaction);
    if (key) this.verifiedWorkerLeases.set(key, Date.now() + milliseconds);
  }

  workerWasVerifiedRecently(lease, transaction) {
    const key = this.workerLeaseKey(lease, transaction);
    if (!key) return false;
    const until = this.verifiedWorkerLeases.get(key) || 0;
    if (until <= Date.now()) {
      this.verifiedWorkerLeases.delete(key);
      return false;
    }
    return true;
  }

  rememberSpawnedWorker(transaction, worker) {
    const lease = readJSONFile(transaction?.leasePath);
    if (!lease || lease.pid !== worker?.pid || lease.updateId !== transaction.updateId ||
        lease.workerId !== transaction.workerId) return;
    const key = this.workerLeaseKey(lease, transaction);
    this.rememberVerifiedWorker(lease, transaction);
    worker.once?.('exit', () => {
      this.verifiedWorkerLeases.delete(key);
      if (this.watchedUpdateId === transaction.updateId) {
        const receipt = readJSONFile(this.receiptPath);
        if (receipt?.updateId === transaction.updateId &&
            !['healthy', 'rollback_healthy', 'failed'].includes(receipt.phase)) this.reconcileActiveReceipt(receipt);
      }
    });
  }

  pinnedRelease() {
    if (this.activeRelease) return this.activeRelease;
    if (this.prepared?.release && ['ready', 'busy', 'installing'].includes(this.state.phase)) return this.prepared.release;
    return null;
  }

  beginOperation(name) {
    if (this.activeOperation) throw new Error(`Workass update operation already running: ${this.activeOperation}`);
    this.activeOperation = name;
  }

  endOperation(name) {
    if (this.activeOperation === name) this.activeOperation = null;
  }

  async prepareHandoff(updateId) {
    let reply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'prepare', updateId);
    // A lost response must never abandon an ID that may already own the
    // daemon's admission fence. Retrying the same ID is idempotent.
    if (reply.status === 0) reply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'prepare', updateId);
    return reply;
  }

  async commitHandoff(updateId) {
    const reply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'commit', updateId);
    if (reply.status === 202) return { accepted: true, reply };
    if (reply.status !== 0) return { accepted: false, reply };
    // Prepare and the exact durable arm receipt already established a safe
    // handoff. A transport failure can mean the daemon accepted commit and
    // closed before its 202 reached Electron. From this point the independent
    // worker remains the recovery authority, so treating the handoff as owned
    // is safe even if the first request never reached the daemon: quitting the
    // shell releases the worker to stop the still-quiescent daemon itself.
    return { accepted: true, ambiguous: true, reply };
  }

  async cancelHandoff(updateId) {
    let reply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'cancel', updateId);
    if (reply.status === 0) reply = await this.deps.postLocalUpdate(this.runtime.daemonURL, 'cancel', updateId);
    return { confirmed: reply.status === 200, reply };
  }

  exactHandoffReceipt(transaction, worker) {
    const receipt = worker?.workassUpdateReceipt || readJSONFile(transaction.receiptPath);
    const request = {
      updateId: transaction.updateId,
      currentVersion: transaction.currentVersion,
      targetVersion: transaction.targetVersion,
      installationId: transaction.installationId,
      installTarget: transaction.installTarget,
      workerId: transaction.workerId,
      workerPID: worker?.pid,
    };
    if (isExactArmedReceipt(receipt, request) || isExactTerminalReceipt(receipt, request)) return receipt;
    return null;
  }

  async abortRejectedHandoff(transaction, worker, message) {
    const lease = readJSONFile(transaction.leasePath);
    const ownership = await this.deps.inspectWorkerOwnership(lease, transaction, { platform: this.platform });
    if (!ownership.exact || ownership.pid !== worker?.pid ||
        !await this.deps.terminateExactWorker(ownership, { platform: this.platform })) {
      const error = new Error('the rejected update worker could not be fenced');
      error.code = 'WORKASS_UPDATE_WORKER_FENCE_FAILED';
      throw error;
    }

    await this.releasePreparedHandoff(transaction);

    // The armed worker has not crossed activation. Remove its exact verified
    // incoming tree before recording terminal failure, so a crash between
    // these writes cannot make a later shell install a rejected transaction.
    fs.rmSync(transaction.incomingTarget, { recursive: true, force: true });
    const journal = journalForTransaction(transaction) || {};
    atomicJSON(transaction.journalPath, {
      ...journal,
      schemaVersion: 1,
      updateId: transaction.updateId,
      installationId: transaction.installationId,
      installTarget: transaction.installTarget,
      previousVersion: transaction.currentVersion,
      targetVersion: transaction.targetVersion,
      createdAt: journal.createdAt || new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      phase: 'failed',
      terminal: true,
      installedVersion: transaction.currentVersion,
      activated: false,
      error: message,
    });
    const receipt = terminalReceiptForTransaction(transaction);
    this.pruneTerminalPayload(receipt);
    return receipt;
  }

  async releasePreparedHandoff(transaction) {
    if (!handoffNeedsCancel(transaction)) return true;
    const cancelled = await this.cancelHandoff(transaction.updateId);
    if (!cancelled.confirmed) {
      const error = new Error('the daemon update fence could not be reconciled');
      error.code = 'WORKASS_UPDATE_CANCEL_UNCONFIRMED';
      throw error;
    }
    writeHandoffState(transaction, 'cancelled');
    return true;
  }

  publish(patch) {
    this.state = { ...this.state, ...patch };
    this.onState(this.snapshot());
    return this.snapshot();
  }

  init() {
    const node = bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch);
    const platformSupported = this.platform === 'darwin' || this.platform === 'win32';
    const verifiedNetworkAvailable = this.platform !== 'win32' || this.networkRequestAvailable;
    let supported = this.isPackaged && platformSupported && fs.existsSync(node) && verifiedNetworkAvailable;
    let unsupportedReason = this.isPackaged ? 'This build does not include the verified updater runtime.' : 'App updates are available in packaged Workass builds.';
    if (this.isPackaged && this.platform === 'win32' && !verifiedNetworkAvailable) {
      unsupportedReason = 'This Windows build does not include the verified system-network updater.';
    }
    if (supported) {
      try {
        this.installationIdentity = ensureInstallationIdentity(this.installTarget, { platform: this.platform });
      } catch (err) {
        supported = false;
        unsupportedReason = String(err && err.message || err);
      }
    }
    this.publish({
      supported,
      phase: supported ? 'idle' : 'unavailable',
      error: supported ? null : unsupportedReason,
      receipt: null,
    });
    try {
      const receipt = JSON.parse(fs.readFileSync(this.receiptPath, 'utf8'));
      if (receiptAppliesToInstalledVersion(receipt, this.currentVersion, {
        installationId: this.installationIdentity?.installationId,
        installTarget: this.installTarget,
      })) {
        const active = ['preparing', 'armed', 'activating'].includes(receipt.phase);
        const transaction = this.transactionForReceipt(receipt);
        const unresolvedFence = !active && transaction && handoffNeedsCancel(transaction);
        this.publish({ phase: active || unresolvedFence ? 'installing' : receipt.phase, receipt, error: receipt.error || null });
        if (active || unresolvedFence) this.reconcileActiveReceipt(receipt);
        else this.pruneTerminalPayload(receipt);
      }
    } catch { /* no prior transaction */ }
    return this.snapshot();
  }

  pruneTerminalPayload(receipt) {
    const updateId = String(receipt?.updateId || '');
    if (!/^[A-Za-z0-9_-]{8,96}$/.test(updateId)) return;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    const transaction = this.transactionForReceipt(receipt);
    const journal = transaction && journalForTransaction(transaction);
    if (!journal?.terminal || !['healthy', 'rollback_healthy', 'failed'].includes(journal.phase)) return;

    // Keep bounded JSON evidence and the worker source, but discard large
    // staging/runtime payloads. updater-node.exe is intentionally outside the
    // install so it can replace Workass; once exact worker ownership is gone it
    // must not accumulate once per release forever.
    for (const child of ['release.zip', 'release.zip.partial', 'extracted', 'updater-node.exe']) {
      try { fs.rmSync(path.join(transactionRoot, child), { recursive: true, force: true }); } catch { /* best effort */ }
    }
    if (['healthy', 'rollback_healthy'].includes(journal.phase)) {
      const installParent = path.dirname(transaction.installTarget);
      const exactIncoming = this.platform === 'darwin'
        ? path.join(installParent, `.Workass.app.incoming-${updateId}`)
        : path.join(transactionRoot, 'incoming-release');
      const exactBackup = this.platform === 'darwin'
        ? path.join(installParent, `.Workass.app.previous-${updateId}`)
        : path.join(transactionRoot, 'installed-before-activation');
      const safeTargets = [
        [transaction.incomingTarget, exactIncoming],
        [transaction.backupTarget, exactBackup],
        [transaction.mutableStateBackupTarget, path.join(transactionRoot, 'state-before-activation')],
        [transaction.failedMutableStateTarget, path.join(transactionRoot, 'state-from-failed-activation')],
      ];
      for (const [target, expected] of safeTargets) {
        if (path.resolve(String(target || '')) !== path.resolve(expected)) continue;
        try { fs.rmSync(target, { recursive: true, force: true }); } catch { /* retry on a later launch */ }
      }
    }
    const logFile = path.join(transactionRoot, 'worker.log');
    try {
      const stat = fs.statSync(logFile);
      const maximum = 1024 * 1024;
      if (stat.isFile() && stat.size > maximum) {
        const descriptor = fs.openSync(logFile, 'r');
        const tail = Buffer.alloc(maximum);
        try { fs.readSync(descriptor, tail, 0, maximum, stat.size - maximum); }
        finally { fs.closeSync(descriptor); }
        const incoming = `${logFile}.bounded-${process.pid}`;
        fs.writeFileSync(incoming, tail, { mode: 0o600 });
        fs.renameSync(incoming, logFile);
      }
    } catch { /* diagnostic truncation retries on a later terminal prune */ }

    const transactionsRoot = path.join(this.updateRoot, 'transactions');
    let terminal = [];
    try {
      terminal = fs.readdirSync(transactionsRoot, { withFileTypes: true }).flatMap((entry) => {
        if (!entry.isDirectory() || !/^[A-Za-z0-9_-]{8,96}$/.test(entry.name)) return [];
        const root = path.join(transactionsRoot, entry.name);
        const candidate = readJSONFile(path.join(root, 'transaction.json'));
        if (!candidate || candidate.schemaVersion !== TRANSACTION_SCHEMA_VERSION || candidate.updateId !== entry.name ||
            candidate.transactionRoot !== root || candidate.installationId !== this.installationIdentity?.installationId ||
            path.resolve(String(candidate.installTarget || '')) !== path.resolve(this.installTarget)) return [];
        const candidateJournal = journalForTransaction(candidate);
        if (!candidateJournal?.terminal || !['healthy', 'rollback_healthy', 'failed'].includes(candidateJournal.phase)) return [];
        return [{ root, updatedAt: Date.parse(String(candidateJournal.updatedAt || '')) || 0 }];
      }).sort((left, right) => right.updatedAt - left.updatedAt);
    } catch { terminal = []; }
    for (const old of terminal.slice(8)) {
      try { fs.rmSync(old.root, { recursive: true, force: true }); } catch { /* locked Windows payload retries later */ }
    }
  }

  transactionForUpdateId(value) {
    const updateId = String(value || '');
    if (!/^[A-Za-z0-9_-]{8,96}$/.test(updateId)) return null;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    const transactionPath = path.join(transactionRoot, 'transaction.json');
    const transaction = readJSONFile(transactionPath);
    if (!transaction || transaction.schemaVersion !== TRANSACTION_SCHEMA_VERSION ||
        transaction.updateId !== updateId || transaction.transactionRoot !== transactionRoot ||
        transaction.installationId !== this.installationIdentity?.installationId ||
        path.resolve(transaction.installTarget || '') !== path.resolve(this.installTarget) ||
        transaction.receiptPath !== this.receiptPath ||
        !/^worker-[a-f0-9]{32}$/.test(String(transaction.workerId || ''))) return null;
    const journal = readJSONFile(transaction.journalPath);
    if (!journal || journal.schemaVersion !== 1 || journal.updateId !== updateId ||
        journal.installationId !== transaction.installationId ||
        path.resolve(String(journal.installTarget || '')) !== path.resolve(transaction.installTarget) ||
        journal.previousVersion !== transaction.currentVersion || journal.targetVersion !== transaction.targetVersion) return null;
    return transaction;
  }

  transactionForReceipt(receipt) {
    const transaction = this.transactionForUpdateId(receipt?.updateId);
    if (!transaction || receipt?.previousVersion !== transaction.currentVersion ||
        receipt?.targetVersion !== transaction.targetVersion || receipt?.workerId !== transaction.workerId) return null;
    return transaction;
  }

  terminalizeInterruptedReceipt(receipt, message) {
    const terminal = {
      schemaVersion: RECEIPT_SCHEMA_VERSION,
      updateId: receipt.updateId,
      phase: 'failed',
      previousVersion: receipt.previousVersion,
      targetVersion: receipt.targetVersion,
      installedVersion: this.currentVersion,
      installationId: this.installationIdentity.installationId,
      installTarget: this.installTarget,
      updatedAt: new Date().toISOString(),
      activated: this.currentVersion === receipt.targetVersion,
      error: message,
    };
    atomicJSON(this.receiptPath, terminal);
    return this.publish({ phase: 'failed', receipt: terminal, error: terminal.error });
  }

  scheduleReceiptRecovery(receipt) {
    this.deps.schedule(() => {
      const current = readJSONFile(this.receiptPath);
      this.reconcileActiveReceipt(current?.updateId === receipt.updateId ? current : receipt);
    }, 1000)?.unref?.();
  }

  reconcileActiveReceipt(receipt) {
    if (this.recoveryPromise) return;
    const transaction = this.transactionForReceipt(receipt) || this.transactionForUpdateId(receipt?.updateId);
    if (!transaction) {
      const recovery = (async () => {
        const cancelled = await this.cancelHandoff(receipt.updateId);
        if (!cancelled.confirmed) {
          const error = new Error('the daemon update fence could not be reconciled for an incomplete transaction');
          error.code = 'WORKASS_UPDATE_CANCEL_UNCONFIRMED';
          throw error;
        }
        return this.terminalizeInterruptedReceipt(receipt, 'the interrupted Workass update has no complete recovery journal');
      })().catch((err) => {
        if (err?.code === 'WORKASS_UPDATE_CANCEL_UNCONFIRMED') {
          this.publish({ phase: 'installing', receipt, error: err.message });
          this.scheduleReceiptRecovery(receipt);
          return;
        }
        this.terminalizeInterruptedReceipt(receipt, `the interrupted Workass update could not reconcile: ${String(err && err.message || err)}`);
      });
      this.recoveryPromise = recovery;
      void recovery.finally(() => {
        if (this.recoveryPromise === recovery) this.recoveryPromise = null;
      });
      return;
    }
    const lease = readJSONFile(path.join(transaction.transactionRoot, 'worker-lease.json'));
    const cheapOwnership = cheapWorkerLeaseOwnership(lease, transaction);
    if (cheapOwnership.owned && this.workerWasVerifiedRecently(lease, transaction)) {
      this.watchReceipt(receipt.updateId);
      return;
    }
    const recovery = (async () => {
      const ownership = await this.deps.inspectWorkerOwnership(lease, transaction, { platform: this.platform });
      if (ownership.owned) {
        this.rememberVerifiedWorker(lease, transaction);
        this.watchReceipt(receipt.updateId);
        return this.snapshot();
      }
      if (ownership.exact && !await this.deps.terminateExactWorker(ownership, { platform: this.platform })) {
        const error = new Error('the previous independent update worker could not be fenced');
        error.code = 'WORKASS_UPDATE_WORKER_FENCE_FAILED';
        throw error;
      }
      return this.resumeTransaction(transaction);
    })().catch((err) => {
      if (err?.code === 'WORKASS_UPDATE_WORKER_FENCE_FAILED') {
        this.publish({ phase: 'installing', error: err.message });
        this.watchReceipt(receipt.updateId);
        return;
      }
      if (err?.code === 'WORKASS_UPDATE_CANCEL_UNCONFIRMED') {
        this.publish({ phase: 'installing', receipt, error: err.message });
        this.scheduleReceiptRecovery(receipt);
        return;
      }
      this.terminalizeInterruptedReceipt(receipt, `the interrupted Workass update could not resume: ${String(err && err.message || err)}`);
    });
    this.recoveryPromise = recovery;
    void recovery.finally(() => {
      if (this.recoveryPromise === recovery) this.recoveryPromise = null;
    });
  }

  async resumeTransaction(transaction) {
    // A prior worker may have committed its terminal journal and died before
    // publishing the matching receipt. That result is authoritative and must
    // be reconstructed without asking a draining or unavailable daemon to
    // prepare another handoff.
    const terminalReceipt = terminalReceiptForTransaction(transaction);
    if (terminalReceipt) {
      await this.releasePreparedHandoff(transaction);
      this.pruneTerminalPayload(terminalReceipt);
      return this.publish({
        phase: terminalReceipt.phase,
        receipt: terminalReceipt,
        error: terminalReceipt.error || null,
        blockers: null,
      });
    }

    await this.releasePreparedHandoff(transaction);
    writeHandoffState(transaction, 'intent');
    const prepared = await this.prepareHandoff(transaction.updateId);
    if (prepared.status === 409) {
      return this.publish({ phase: 'busy', blockers: prepared.body, error: null });
    }
    if (prepared.status === 0) {
      return this.publish({
        phase: 'busy',
        blockers: { reason: 'the daemon prepare receipt is temporarily unavailable' },
        error: null,
      });
    }
    if (prepared.status !== 200 || prepared.body?.ready !== true) {
      throw new Error('the daemon could not prepare the interrupted update handoff');
    }
    writeHandoffState(transaction, 'prepared');
    const workerId = `worker-${crypto.randomBytes(16).toString('hex')}`;
    const resumed = {
      ...transaction,
      shellPID: process.pid,
      workerId,
      recoveryAttempt: Number(transaction.recoveryAttempt || 0) + 1,
    };
    const transactionPath = path.join(transaction.transactionRoot, 'transaction.json');
    atomicJSON(transactionPath, resumed);
    atomicJSON(resumed.receiptPath, {
      schemaVersion: RECEIPT_SCHEMA_VERSION,
      updateId: resumed.updateId,
      phase: 'preparing',
      previousVersion: resumed.currentVersion,
      targetVersion: resumed.targetVersion,
      installationId: resumed.installationId,
      installTarget: resumed.installTarget,
      workerId: resumed.workerId,
      updatedAt: new Date().toISOString(),
    });
    const workerPath = path.join(transaction.transactionRoot, 'update-worker.js');
    const nodePath = this.platform === 'win32'
      ? path.join(transaction.transactionRoot, 'updater-node.exe')
      : bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch);
    if (!fs.statSync(workerPath, { throwIfNoEntry: false })?.isFile() ||
        !fs.statSync(nodePath, { throwIfNoEntry: false })?.isFile()) {
      await this.releasePreparedHandoff(transaction);
      throw new Error('the interrupted update worker runtime is incomplete');
    }
    const logFD = fs.openSync(path.join(transaction.transactionRoot, 'worker.log'), 'a', 0o600);
    let worker;
    try {
      const armWorker = this.deps.spawnArmedWorker || ((request) => spawnArmedUpdateWorker(request, {
        spawnProcess: this.deps.spawn,
      }));
      worker = await armWorker({
        command: nodePath,
        args: [workerPath, '--transaction', transactionPath],
        options: {
          cwd: transaction.transactionRoot,
          detached: true,
          windowsHide: true,
          stdio: ['ignore', logFD, logFD],
          env: { ...process.env, WORKASS_UPDATE_WORKER_ID: workerId },
        },
        receiptPath: this.receiptPath,
        updateId: transaction.updateId,
        currentVersion: transaction.currentVersion,
        targetVersion: transaction.targetVersion,
        installationId: transaction.installationId,
        installTarget: transaction.installTarget,
        workerId,
      });
    } catch (err) {
      if (err?.workassWorkerFenced !== true) throw err;
      await this.releasePreparedHandoff(transaction);
      throw err;
    } finally {
      fs.closeSync(logFD);
    }
    const handoffReceipt = this.exactHandoffReceipt(resumed, worker);
    if (!handoffReceipt) {
      const message = 'the independent update worker handoff receipt changed after arming';
      const receipt = await this.abortRejectedHandoff(resumed, worker, message);
      return this.publish({ phase: 'failed', receipt, error: message, blockers: null });
    }
    this.rememberSpawnedWorker(resumed, worker);
    if (handoffReceipt && ['healthy', 'rollback_healthy', 'failed'].includes(handoffReceipt.phase)) {
      await this.releasePreparedHandoff(resumed);
      this.pruneTerminalPayload(handoffReceipt);
      return this.publish({
        phase: handoffReceipt.phase,
        receipt: handoffReceipt,
        error: handoffReceipt.error || null,
        blockers: null,
      });
    }
    const committed = await this.commitHandoff(transaction.updateId);
    if (!committed.accepted) {
      const message = 'the daemon did not commit the interrupted update handoff';
      const receipt = await this.abortRejectedHandoff(resumed, worker, message);
      return this.publish({ phase: 'failed', receipt, error: message, blockers: null });
    }
    writeHandoffState(resumed, 'committed');
    this.publish({ phase: 'installing', receipt: readJSONFile(this.receiptPath), error: null, blockers: null });
    this.watchReceipt(transaction.updateId);
    this.deps.schedule(() => this.quit(), 100)?.unref?.();
    return this.snapshot();
  }

  watchReceipt(updateId = this.watchedUpdateId) {
    if (this.receiptTimer) return;
    this.watchedUpdateId = String(updateId || '');
    this.receiptTimer = setInterval(() => {
      try {
        const receipt = JSON.parse(fs.readFileSync(this.receiptPath, 'utf8'));
        if (!receipt || receipt.schemaVersion !== RECEIPT_SCHEMA_VERSION || receipt.updateId !== this.watchedUpdateId ||
            !receiptAppliesToInstalledVersion(receipt, this.currentVersion, {
              installationId: this.installationIdentity?.installationId,
              installTarget: this.installTarget,
            })) return;
        const terminal = ['healthy', 'rollback_healthy', 'failed'].includes(receipt.phase);
        this.publish({ phase: terminal ? receipt.phase : 'installing', receipt, error: receipt.error || null });
        if (terminal) {
          clearInterval(this.receiptTimer);
          this.receiptTimer = null;
          this.watchedUpdateId = '';
          this.pruneTerminalPayload(receipt);
          return;
        }
        const transaction = this.transactionForReceipt(receipt);
        const lease = transaction && readJSONFile(path.join(transaction.transactionRoot, 'worker-lease.json'));
        const ownership = transaction ? cheapWorkerLeaseOwnership(lease, transaction) : { owned: false };
        const verified = transaction && this.workerWasVerifiedRecently(lease, transaction);
        if (verified && ownership.owned) this.rememberVerifiedWorker(lease, transaction);
        if (!transaction || !verified) {
          clearInterval(this.receiptTimer);
          this.receiptTimer = null;
          this.watchedUpdateId = '';
          this.reconcileActiveReceipt(receipt);
        }
      } catch { /* worker may be between atomic receipt writes */ }
    }, 1000);
    this.receiptTimer.unref?.();
  }

  async runDiscovery({ background = false } = {}) {
    if (!this.state.supported) return this.snapshot();
    const previous = this.snapshot();
    const preserveTerminal = background && ['failed', 'rollback_healthy'].includes(previous.phase) && previous.receipt;
    const pinnedAtStart = this.pinnedRelease();
    if (!background && !pinnedAtStart) this.publish({ phase: 'checking', error: null, blockers: null });
    try {
      const raw = await this.deps.fetchManifest(this.feedURL);
      const manifest = snapshotReleaseManifest(validateReleaseManifest(raw, { platform: this.platform, arch: this.arch }));
      this.manifest = manifest;
      const available = compareVersions(this.currentVersion, manifest.version) < 0;
      const pinned = this.pinnedRelease();
      if (pinned) {
        const current = this.snapshot();
        const previouslyDiscovered = parseVersion(current.availableVersion) &&
          compareVersions(current.availableVersion, pinned.version) > 0 ? current.availableVersion : null;
        const newlyDiscovered = compareVersions(manifest.version, pinned.version) > 0 ? manifest.version : null;
        return this.publish({
          phase: current.phase,
          targetVersion: pinned.version,
          availableVersion: newlyDiscovered || previouslyDiscovered,
          checkedAt: new Date().toISOString(),
        });
      }
      const discovered = {
        phase: preserveTerminal ? previous.phase : (available ? 'available' : 'current'),
        targetVersion: available ? manifest.version : null,
        availableVersion: available ? manifest.version : null,
        checkedAt: new Date().toISOString(),
        notes: available ? String(manifest.notes || '') : '',
        size: available ? manifest.artifacts.update.size : null,
        error: preserveTerminal ? previous.error : null,
      };
      return this.publish(discovered);
    } catch (err) {
      // GitHub returns 404 when the latest release has no asset for this
      // platform yet. That means there is no applicable update, not that the
      // installed app is broken. Signature, checksum, parse, and network
      // failures remain explicit failures.
      if (err?.statusCode === 404 || err?.code === 'ENOENT') {
        const pinned = this.pinnedRelease();
        if (pinned) return this.publish({
          phase: this.state.phase,
          targetVersion: pinned.version,
          checkedAt: new Date().toISOString(),
        });
        if (background && previous.phase === 'available') return previous;
        if (preserveTerminal) return this.publish({
          phase: previous.phase, targetVersion: null, availableVersion: null,
          checkedAt: new Date().toISOString(), error: previous.error,
        });
        return this.publish({ phase: 'current', targetVersion: null, availableVersion: null, checkedAt: new Date().toISOString(), error: null });
      }
      const pinned = this.pinnedRelease();
      if (pinned) return this.publish({
        phase: this.state.phase,
        targetVersion: pinned.version,
        checkedAt: new Date().toISOString(),
      });
      if (background && previous.phase === 'available') {
        return this.publish({
          phase: 'available', targetVersion: previous.targetVersion,
          checkedAt: new Date().toISOString(), error: String(err && err.message || err),
        });
      }
      if (preserveTerminal) return this.publish({
        phase: previous.phase, checkedAt: new Date().toISOString(), error: previous.error,
      });
      return this.publish({ phase: 'check_failed', checkedAt: new Date().toISOString(), error: String(err && err.message || err) });
    }
  }

  check(options = {}) {
    if (this.discoveryPromise) return this.discoveryPromise;
    const discovery = this.runDiscovery(options);
    this.discoveryPromise = discovery;
    void discovery.finally(() => {
      if (this.discoveryPromise === discovery) this.discoveryPromise = null;
    });
    return discovery;
  }

  async autoCheck() {
    if (!this.state.supported || this.activeOperation) return this.snapshot();
    // A plain offer has no downloaded payload and may advance to a newer
    // release. Once download/staging begins, that exact transaction is pinned.
    if (!['idle', 'current', 'available', 'healthy', 'check_failed', 'failed', 'rollback_healthy'].includes(this.state.phase)) return this.snapshot();
    try { return await this.check({ background: true }); }
    catch { return this.snapshot(); }
  }

  startAutoChecks({
    initialDelayMs = DEFAULT_INITIAL_CHECK_DELAY_MS,
    intervalMs = DEFAULT_CHECK_INTERVAL_MS,
  } = {}) {
    this.stopAutoChecks();
    if (!this.state.supported || !Number.isFinite(initialDelayMs) || initialDelayMs < 0 ||
        !Number.isFinite(intervalMs) || intervalMs <= 0) return this.snapshot();
    const run = () => this.autoCheck();
    this.initialCheckTimer = this.deps.schedule(run, initialDelayMs);
    this.initialCheckTimer?.unref?.();
    this.periodicCheckTimer = this.deps.repeat(run, intervalMs);
    this.periodicCheckTimer?.unref?.();
    return this.snapshot();
  }

  stopAutoChecks() {
    if (this.initialCheckTimer) this.deps.cancelSchedule(this.initialCheckTimer);
    if (this.periodicCheckTimer) this.deps.cancelRepeat(this.periodicCheckTimer);
    this.initialCheckTimer = null;
    this.periodicCheckTimer = null;
  }

  async download() {
    this.beginOperation('download');
    let release = null;
    try {
    if (this.state.phase !== 'available') throw new Error('the Workass update is not available for download');
    if (!this.manifest || compareVersions(this.currentVersion, this.manifest.version) >= 0) throw new Error('no Workass update is available');
    if (this.state.targetVersion !== this.manifest.version) throw new Error('the offered Workass release changed before download');
    release = snapshotReleaseManifest(this.manifest);
    this.activeRelease = release;
    this.prepared = null;
    const updateId = `upd-${Date.now().toString(36)}-${crypto.randomBytes(8).toString('hex')}`;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    const archive = path.join(transactionRoot, 'release.zip');
    const extracted = path.join(transactionRoot, 'extracted');
    let incomingTarget = '';
    let backupTarget = '';
    const artifact = release.artifacts.update;
    const artifactSource = resolveArtifactSource(artifact.url, this.feedURL);
    this.publish({
      phase: 'downloading',
      targetVersion: release.version,
      availableVersion: null,
      progress: 0,
      error: null,
      blockers: null,
    });
    try {
      const reportProgress = createProgressPublisher(
        (progress) => this.publish({ progress }),
        { now: this.deps.now || Date.now },
      );
      await this.deps.downloadArtifact(artifactSource, archive, artifact, { onProgress: reportProgress });
      this.publish({ phase: 'staging', progress: 1 });
      const installTarget = installedRoot(this.resourcesPath, this.executablePath, this.platform);
      const parent = path.dirname(installTarget);
      incomingTarget = this.platform === 'darwin'
        ? path.join(parent, `.Workass.app.incoming-${updateId}`)
        : path.join(transactionRoot, 'incoming-release');
      backupTarget = this.platform === 'darwin'
        ? path.join(parent, `.Workass.app.previous-${updateId}`)
        : path.join(transactionRoot, 'installed-before-activation');
      if (fs.existsSync(incomingTarget) || fs.existsSync(backupTarget)) throw new Error('update staging target already exists');
      const staged = await this.deps.stageAndVerify({
        schemaVersion: 1,
        transactionRoot,
        archive,
        extracted,
        currentRoot: installTarget,
        incomingTarget,
        targetVersion: release.version,
        platform: this.platform,
        arch: this.arch,
        installationId: this.installationIdentity.installationId,
      });
      const designatedRequirement = String(staged?.designatedRequirement || '');
      const runtime = await this.deps.prepareWorkerRuntime({
        transactionRoot,
        workerSource: path.join(__dirname, 'update-worker.js'),
        nodeSource: bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch),
        platform: this.platform,
      });
      this.prepared = {
        updateId, transactionRoot, installTarget, incomingTarget, backupTarget, designatedRequirement,
        installationId: this.installationIdentity.installationId,
        targetVersion: release.version,
        artifact,
        release,
        workerPath: runtime.workerPath,
        nodePath: runtime.nodePath,
      };
      return this.publish({ phase: 'ready', targetVersion: release.version, progress: 1, error: null });
    } catch (err) {
      const cleanupErrors = [];
      if (incomingTarget && this.platform === 'darwin') {
        try { fs.rmSync(incomingTarget, { recursive: true, force: true }); }
        catch (cleanupError) { cleanupErrors.push(String(cleanupError && cleanupError.message || cleanupError)); }
      }
      try { fs.rmSync(transactionRoot, { recursive: true, force: true }); }
      catch (cleanupError) { cleanupErrors.push(String(cleanupError && cleanupError.message || cleanupError)); }
      this.prepared = null;
      const error = String(err && err.message || err);
      return this.publish({
        phase: 'failed',
        error: cleanupErrors.length > 0 ? `${error}; staging cleanup failed: ${cleanupErrors.join('; ')}` : error,
      });
    }
    } finally {
      if (this.activeRelease === release) this.activeRelease = null;
      this.endOperation('download');
    }
  }

  async install() {
    this.beginOperation('install');
    try {
    if (!this.prepared || !['ready', 'busy'].includes(this.state.phase)) throw new Error('the verified Workass update is not ready');
    const prepared = this.prepared;
    const release = prepared.release;
    if (!release || !Object.isFrozen(release) || release.version !== prepared.targetVersion ||
        release.artifacts?.update !== prepared.artifact || this.state.targetVersion !== prepared.targetVersion) {
      throw new Error('the verified Workass transaction lost its exact release metadata');
    }
    const expectedWorkerPath = path.join(prepared.transactionRoot, 'update-worker.js');
    const expectedNodePath = this.platform === 'win32'
      ? path.join(prepared.transactionRoot, 'updater-node.exe')
      : bundledNode(this.resourcesPath, this.executablePath, this.platform, this.arch);
    if (prepared.workerPath !== expectedWorkerPath || prepared.nodePath !== expectedNodePath ||
        !fs.statSync(prepared.workerPath, { throwIfNoEntry: false })?.isFile() ||
        !fs.statSync(prepared.nodePath, { throwIfNoEntry: false })?.isFile()) {
      throw new Error('the independently verified update worker runtime is incomplete');
    }
    const workerPath = prepared.workerPath;
    const nodePath = prepared.nodePath;

    const workerId = `worker-${crypto.randomBytes(16).toString('hex')}`;
    const transaction = {
      schemaVersion: TRANSACTION_SCHEMA_VERSION,
      updateId: prepared.updateId,
      platform: this.platform,
      currentVersion: this.currentVersion,
      targetVersion: prepared.targetVersion,
      shellPID: process.pid,
      workerId,
      installationId: prepared.installationId || this.installationIdentity.installationId,
      transactionRoot: prepared.transactionRoot,
      installTarget: prepared.installTarget,
      incomingTarget: prepared.incomingTarget,
      backupTarget: prepared.backupTarget,
      mutableStateTarget: this.runtime.stateDir,
      mutableStateBackupTarget: path.join(prepared.transactionRoot, 'state-before-activation'),
      failedMutableStateTarget: path.join(prepared.transactionRoot, 'state-from-failed-activation'),
      receiptPath: this.receiptPath,
      journalPath: path.join(prepared.transactionRoot, 'journal.json'),
      leasePath: path.join(prepared.transactionRoot, 'worker-lease.json'),
      daemonHealthURL: `${this.runtime.daemonURL}/workass/health`,
      shellStatusURL: `http://127.0.0.1:${this.runtime.viewPort}/__workass-shell/status`,
      requireVisibleWindow: true,
      designatedRequirement: prepared.designatedRequirement,
      launchAgentPath: this.platform === 'darwin' ? path.join(os.homedir(), 'Library', 'LaunchAgents', `${this.runtime.launchdLabel}.plist`) : '',
      launchdDomain: this.platform === 'darwin' && typeof process.getuid === 'function' ? `gui/${process.getuid()}` : '',
    };
    const transactionPath = path.join(prepared.transactionRoot, 'transaction.json');
    atomicJSON(transactionPath, transaction);
    atomicJSON(transaction.journalPath, {
      schemaVersion: 1,
      updateId: transaction.updateId,
      installationId: transaction.installationId,
      installTarget: transaction.installTarget,
      previousVersion: transaction.currentVersion,
      targetVersion: transaction.targetVersion,
      phase: 'preparing',
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
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
    });
    atomicJSON(transaction.receiptPath, {
      schemaVersion: RECEIPT_SCHEMA_VERSION,
      updateId: transaction.updateId,
      phase: 'preparing',
      previousVersion: transaction.currentVersion,
      targetVersion: transaction.targetVersion,
      installationId: transaction.installationId,
      installTarget: transaction.installTarget,
      workerId: transaction.workerId,
      updatedAt: new Date().toISOString(),
    });

    writeHandoffState(transaction, 'intent');
    const preparedReply = await this.prepareHandoff(prepared.updateId);
    if (preparedReply.status === 409) {
      return this.publish({ phase: 'busy', blockers: preparedReply.body, error: null });
    }
    if (preparedReply.status === 0) {
      return this.publish({
        phase: 'busy',
        blockers: { reason: 'the daemon prepare receipt is temporarily unavailable' },
        error: null,
      });
    }
    if (preparedReply.status !== 200 || preparedReply.body?.ready !== true) {
      return this.publish({
        phase: 'busy',
        blockers: { reason: 'the daemon could not confirm a safe update handoff' },
        error: null,
      });
    }
    writeHandoffState(transaction, 'prepared');

    const logFD = fs.openSync(path.join(prepared.transactionRoot, 'worker.log'), 'a', 0o600);
    let worker;
    try {
      const armWorker = this.deps.spawnArmedWorker || ((request) => spawnArmedUpdateWorker(request, {
        spawnProcess: this.deps.spawn,
      }));
      worker = await armWorker({
        command: nodePath,
        args: [workerPath, '--transaction', transactionPath],
        options: {
          cwd: prepared.transactionRoot,
          detached: true,
          windowsHide: true,
          stdio: ['ignore', logFD, logFD],
          env: { ...process.env, WORKASS_UPDATE_WORKER_ID: workerId },
        },
        receiptPath: this.receiptPath,
        updateId: prepared.updateId,
        currentVersion: this.currentVersion,
        targetVersion: prepared.targetVersion,
        installationId: transaction.installationId,
        installTarget: transaction.installTarget,
        workerId,
      });
    } catch (err) {
      if (err?.workassWorkerFenced !== true) throw err;
      await this.releasePreparedHandoff(transaction);
      throw err;
    } finally {
      fs.closeSync(logFD);
    }

    const handoffReceipt = this.exactHandoffReceipt(transaction, worker);
    if (!handoffReceipt) {
      const message = 'the independent update worker handoff receipt changed after arming';
      const receipt = await this.abortRejectedHandoff(transaction, worker, message);
      return this.publish({ phase: 'failed', receipt, error: message, blockers: null });
    }
    this.rememberSpawnedWorker(transaction, worker);
    if (handoffReceipt && ['healthy', 'rollback_healthy', 'failed'].includes(handoffReceipt.phase)) {
      await this.releasePreparedHandoff(transaction);
      this.pruneTerminalPayload(handoffReceipt);
      return this.publish({
        phase: handoffReceipt.phase,
        receipt: handoffReceipt,
        error: handoffReceipt.error || null,
        blockers: null,
      });
    }

    const committed = await this.commitHandoff(prepared.updateId);
    if (!committed.accepted) {
      const message = 'the daemon did not commit the prepared update handoff';
      const receipt = await this.abortRejectedHandoff(transaction, worker, message);
      return this.publish({ phase: 'failed', receipt, error: message, blockers: null });
    }
    writeHandoffState(transaction, 'committed');
    this.publish({ phase: 'installing', targetVersion: prepared.targetVersion, receipt: {
      phase: 'armed', targetVersion: prepared.targetVersion,
    }, error: null });
    this.watchReceipt(prepared.updateId);
    this.deps.schedule(() => this.quit(), 100)?.unref?.();
    return this.snapshot();
    } finally {
      this.endOperation('install');
    }
  }

  // One user intent owns the complete ordinary update path. The lower-level
  // methods remain separate for recovery/tests, but the renderer never asks the
  // user to click once to stage and again to activate the same verified release.
  async apply() {
    if (this.discoveryPromise) await this.discoveryPromise;
    let state = this.snapshot();
    if (state.phase === 'check_failed' || state.phase === 'failed' || state.phase === 'rollback_healthy') {
      if (state.phase !== 'check_failed' && this.manifest && state.targetVersion === this.manifest.version &&
          compareVersions(this.currentVersion, this.manifest.version) < 0) {
        state = this.publish({ phase: 'available', error: null, blockers: null });
      } else {
        state = await this.check();
      }
    }
    if (state.phase === 'busy' && state.receipt && ['preparing', 'armed', 'activating'].includes(state.receipt.phase)) {
      this.reconcileActiveReceipt(state.receipt);
      return this.snapshot();
    }
    if (state.phase === 'available') state = await this.download();
    if (state.phase === 'ready' || state.phase === 'busy') state = await this.install();
    return state;
  }

  // The renderer observes progress through updater state events. Do not keep
  // its click IPC pending for the entire download, staging, and handoff: start
  // the one owned apply operation and immediately return the first running
  // snapshot (download() publishes it before its first network await).
  startApply() {
    if (this.applyPromise) return this.snapshot();
    const operation = this.apply().catch((err) => {
      if (err?.code === 'WORKASS_UPDATE_CANCEL_UNCONFIRMED') {
        const receipt = readJSONFile(this.receiptPath) || this.state.receipt;
        this.publish({ phase: 'installing', receipt, error: String(err && err.message || err) });
        if (receipt?.updateId) this.scheduleReceiptRecovery(receipt);
        return this.snapshot();
      }
      return this.publish({ phase: 'failed', error: String(err && err.message || err) });
    });
    this.applyPromise = operation;
    void operation.then(() => {
      if (this.applyPromise === operation) this.applyPromise = null;
    });
    return this.snapshot();
  }

  dispose() {
    this.stopAutoChecks();
    if (this.receiptTimer) clearInterval(this.receiptTimer);
    this.receiptTimer = null;
  }
}

module.exports = {
  UpdateManager,
  archiveEntries,
  atomicJSON,
  bundledNode,
  cheapWorkerLeaseOwnership,
  compareVersions,
  copyLocalArtifact,
  createProgressPublisher,
  defaultFeedURL,
  downloadArtifact,
  ensureInstallationIdentity,
  extractArchive,
  fetchReleaseManifest,
  findExtractedRoot,
  httpsRequest,
  installedRoot,
  inspectWorkerOwnershipAsync,
  installationIdentityPath,
  localFeedPath,
  macRequirement,
  parseVersion,
  postLocalUpdate,
  prepareUpdateWorkerRuntime,
  releaseArch,
  releaseFeedName,
  releasePlatform,
  receiptAppliesToInstalledVersion,
  readInstallationIdentity,
  resolveArtifactSource,
  resolveUpdateFeed,
  runUpdateStageWorker,
  snapshotReleaseManifest,
  spawnArmedUpdateWorker,
  stageAndVerifyRelease,
  stageRelease,
  validateArchiveLinksBeforeExtraction,
  validateExtractedTree,
  validateReleaseManifest,
  verifyRelease,
  verifyMacRelease,
  verifyWindowsPE,
  verifyWindowsRelease,
  workerProcessOwnership,
  terminateExactWorker,
};

if (!isMainThread && workerData?.workassTask === 'stage-update' && parentPort) {
  try {
    parentPort.postMessage({ ok: true, result: stageAndVerifyRelease(workerData.request) });
  } catch (err) {
    parentPort.postMessage({ ok: false, error: String(err && err.message || err) });
  }
}
if (!isMainThread && workerData?.workassTask === 'inspect-update-worker' && parentPort) {
  try {
    parentPort.postMessage({
      ok: true,
      ownership: workerProcessOwnership(workerData.lease, workerData.transaction, {
        platform: workerData.transaction?.platform,
      }),
    });
  } catch (err) {
    parentPort.postMessage({ ok: false, error: String(err && err.message || err) });
  }
}
