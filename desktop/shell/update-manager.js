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

const DEFAULT_FEED_ROOT = 'https://github.com/Dukler/workass/releases/latest/download/';
const MAX_MANIFEST_BYTES = 256 * 1024;
const MAX_UPDATE_BYTES = 4 * 1024 * 1024 * 1024;
const MAX_ARCHIVE_ENTRIES = 120000;
const DEFAULT_INITIAL_CHECK_DELAY_MS = 15_000;
const DEFAULT_CHECK_INTERVAL_MS = 60 * 60 * 1000;

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
  if (platform === 'win32' && raw.portable !== true) throw new Error('Windows automatic updates require a portable release');
  return raw;
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

function verifyWindowsRelease(_currentRoot, incomingRoot, targetVersion, arch = 'x64') {
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
    [['resources', 'renderer', 'index.html'], 'renderer'],
    [['frontier-hosts', `windows-${expectedArch}`, 'claude-native-host.mjs'], 'Claude host'],
    [['frontier-hosts', `windows-${expectedArch}`, 'codex-native-host.mjs'], 'Codex host'],
    [['frontier-hosts', `windows-${expectedArch}`, 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'], 'Claude Agent SDK'],
  ]) requiredRegularFile(path.join(incomingRoot, ...relative), label);
}

function stageRelease(source, incomingTarget, platform = process.platform) {
  if (platform === 'darwin') {
    const copy = spawnSync('/usr/bin/ditto', [source, incomingTarget], { stdio: ['ignore', 'pipe', 'pipe'] });
    if (copy.error || copy.status !== 0) throw new Error('could not stage the signed app beside the installed app');
    return;
  }
  fs.cpSync(source, incomingTarget, { recursive: true, errorOnExist: true });
}

function verifyRelease(currentRoot, incomingRoot, targetVersion, platform = process.platform, arch = process.arch) {
  if (platform === 'darwin') return verifyMacRelease(currentRoot, incomingRoot, targetVersion);
  verifyWindowsRelease(currentRoot, incomingRoot, targetVersion, arch);
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
    const networkRequest = typeof deps.networkRequest === 'function' ? deps.networkRequest : null;
    const dependencyOverrides = { ...deps };
    delete dependencyOverrides.networkRequest;
    this.networkRequestAvailable = Boolean(networkRequest);
    this.deps = {
      fetchManifest: (url, options = {}) => fetchReleaseManifest(url, { ...options, networkRequest }),
      downloadArtifact: (url, destination, artifact, options = {}) => downloadArtifact(
        url, destination, artifact, { ...options, networkRequest },
      ),
      extractArchive,
      postLocalUpdate,
      spawn,
      stageRelease,
      verifyRelease,
      schedule: setTimeout,
      cancelSchedule: clearTimeout,
      repeat: setInterval,
      cancelRepeat: clearInterval,
      ...dependencyOverrides,
    };
    this.updateRoot = path.join(runtime.dataRoot, 'updates');
    this.receiptPath = path.join(this.updateRoot, 'receipt.json');
    this.currentVersion = String(currentVersion || app?.getVersion?.() || '0.0.0');
    this.manifest = null;
    this.prepared = null;
    this.receiptTimer = null;
    this.initialCheckTimer = null;
    this.periodicCheckTimer = null;
    this.activeOperation = null;
    this.applyPromise = null;
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
    const verifiedNetworkAvailable = this.platform !== 'win32' || this.networkRequestAvailable;
    let supported = this.isPackaged && platformSupported && fs.existsSync(node) && verifiedNetworkAvailable;
    let unsupportedReason = this.isPackaged ? 'This build does not include the verified updater runtime.' : 'App updates are available in packaged Workass builds.';
    if (this.isPackaged && this.platform === 'win32' && !verifiedNetworkAvailable) {
      unsupportedReason = 'This Windows build does not include the verified system-network updater.';
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

  async check({ background = false } = {}) {
    if (!this.state.supported) return this.snapshot();
    this.beginOperation('check');
    const previous = this.snapshot();
    if (!background) this.publish({ phase: 'checking', error: null, blockers: null });
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
      if (err?.statusCode === 404 || err?.code === 'ENOENT') {
        if (background && previous.phase === 'available') return previous;
        return this.publish({ phase: 'current', targetVersion: null, checkedAt: new Date().toISOString(), error: null });
      }
      if (background && previous.phase === 'available') {
        return this.publish({
          phase: 'available', targetVersion: previous.targetVersion,
          checkedAt: new Date().toISOString(), error: String(err && err.message || err),
        });
      }
      return this.publish({ phase: 'check_failed', checkedAt: new Date().toISOString(), error: String(err && err.message || err) });
    } finally {
      this.endOperation('check');
    }
  }

  async autoCheck() {
    if (!this.state.supported || this.activeOperation) return this.snapshot();
    // A plain offer has no downloaded payload and may advance to a newer
    // release. Once download/staging begins, that exact transaction is pinned.
    if (!['idle', 'current', 'available', 'healthy', 'check_failed'].includes(this.state.phase)) return this.snapshot();
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
    try {
    if (this.state.phase !== 'available') throw new Error('the Workass update is not available for download');
    if (!this.manifest || compareVersions(this.currentVersion, this.manifest.version) >= 0) throw new Error('no Workass update is available');
    const updateId = `upd-${Date.now().toString(36)}-${crypto.randomBytes(8).toString('hex')}`;
    const transactionRoot = path.join(this.updateRoot, 'transactions', updateId);
    const archive = path.join(transactionRoot, 'release.zip');
    const extracted = path.join(transactionRoot, 'extracted');
    const artifact = this.manifest.artifacts.update;
    const artifactSource = resolveArtifactSource(artifact.url, this.feedURL);
    this.publish({ phase: 'downloading', progress: 0, error: null, blockers: null });
    try {
      await this.deps.downloadArtifact(artifactSource, archive, artifact, {
        onProgress: (received, total) => this.publish({ progress: total > 0 ? Math.min(1, received / total) : 0 }),
      });
      this.publish({ phase: 'staging', progress: 1 });
      this.deps.extractArchive(archive, extracted, this.platform);
      const source = findExtractedRoot(extracted, this.platform);
      const installTarget = installedRoot(this.resourcesPath, this.executablePath, this.platform);
      const parent = path.dirname(installTarget);
      const incomingTarget = this.platform === 'darwin'
        ? path.join(parent, `.Workass.app.incoming-${updateId}`)
        : path.join(transactionRoot, 'incoming-release');
      const backupTarget = this.platform === 'darwin'
        ? path.join(parent, `.Workass.app.previous-${updateId}`)
        : path.join(transactionRoot, 'installed-before-activation');
      if (fs.existsSync(incomingTarget) || fs.existsSync(backupTarget)) throw new Error('update staging target already exists');
      this.deps.stageRelease(source, incomingTarget, this.platform);
      const designatedRequirement = this.deps.verifyRelease(installTarget, incomingTarget, this.manifest.version, this.platform, this.arch);
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
      schemaVersion: 2,
      updateId: prepared.updateId,
      platform: this.platform,
      currentVersion: this.currentVersion,
      targetVersion: this.manifest.version,
      shellPID: process.pid,
      transactionRoot: prepared.transactionRoot,
      installTarget: prepared.installTarget,
      incomingTarget: prepared.incomingTarget,
      backupTarget: prepared.backupTarget,
      mutableStateTarget: this.runtime.stateDir,
      mutableStateBackupTarget: path.join(prepared.transactionRoot, 'state-before-activation'),
      failedMutableStateTarget: path.join(prepared.transactionRoot, 'state-from-failed-activation'),
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

  // One user intent owns the complete ordinary update path. The lower-level
  // methods remain separate for recovery/tests, but the renderer never asks the
  // user to click once to stage and again to activate the same verified release.
  async apply() {
    let state = this.snapshot();
    if (state.phase === 'check_failed' || state.phase === 'failed' || state.phase === 'rollback_healthy') {
      state = await this.check();
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
    const operation = this.apply().catch((err) => this.publish({
      phase: 'failed', error: String(err && err.message || err),
    }));
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
  compareVersions,
  copyLocalArtifact,
  defaultFeedURL,
  downloadArtifact,
  extractArchive,
  fetchReleaseManifest,
  findExtractedRoot,
  httpsRequest,
  installedRoot,
  localFeedPath,
  macRequirement,
  parseVersion,
  postLocalUpdate,
  releaseArch,
  releaseFeedName,
  releasePlatform,
  resolveArtifactSource,
  resolveUpdateFeed,
  stageRelease,
  validateReleaseManifest,
  verifyRelease,
  verifyMacRelease,
  verifyWindowsPE,
  verifyWindowsRelease,
};
