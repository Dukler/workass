'use strict';

const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { spawnSync } = require('node:child_process');

function healthCheck(url, timeoutMs = 700, expectedVersion = '') {
  return new Promise((resolve) => {
    const parsed = new URL(url);
    const transport = parsed.protocol === 'https:' ? https : http;
    const request = transport.get(`${url}/workass/health`, {
      timeout: timeoutMs,
      // The local shell starts before it can read a newly minted self-signed
      // certificate. This probe is loopback-only; remote Workass links use WSS.
      rejectUnauthorized: parsed.hostname === '127.0.0.1' || parsed.hostname === 'localhost' ? false : true,
    }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => {
        if (body.length < 4096) body += chunk;
      });
      response.on('end', () => {
        try {
          const parsed = JSON.parse(body);
          resolve(response.statusCode >= 200 && response.statusCode < 300 && parsed.app === 'workass' &&
            (!expectedVersion || parsed.version === expectedVersion));
        } catch {
          resolve(false);
        }
      });
    });
    request.on('timeout', () => request.destroy());
    request.on('error', () => resolve(false));
  });
}

function healthInstanceId(url, timeoutMs = 700) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL(url); } catch { resolve(''); return; }
    if (!['127.0.0.1', 'localhost', '::1'].includes(parsed.hostname)) { resolve(''); return; }
    const healthURL = new URL('/workass/health', parsed);
    const transport = healthURL.protocol === 'https:' ? https : healthURL.protocol === 'http:' ? http : null;
    if (!transport) { resolve(''); return; }
    const request = transport.get(healthURL, { timeout: timeoutMs, rejectUnauthorized: false }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => {
        if (body.length < 4096) body += chunk;
        if (body.length > 4096) request.destroy();
      });
      response.on('end', () => {
        try {
          const identity = JSON.parse(body);
          const instanceId = typeof identity.instanceId === 'string' ? identity.instanceId.trim() : '';
          resolve(response.statusCode >= 200 && response.statusCode < 300 && identity.app === 'workass' &&
            /^i-[a-z0-9_-]{1,32}$/iu.test(instanceId) ? instanceId : '');
        } catch { resolve(''); }
      });
      response.on('error', () => resolve(''));
    });
    request.on('timeout', () => { request.destroy(); resolve(''); });
    request.on('error', () => resolve(''));
  });
}

async function waitForHealth(url, { attempts = 120, delayMs = 250, check = healthCheck, expectedVersion = '', timeoutMs = Infinity } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) return false;
    if (await check(url, Math.min(700, remaining), expectedVersion)) return true;
    const pauseMs = Math.min(delayMs, Math.max(0, deadline - Date.now()));
    if (pauseMs <= 0) return false;
    await new Promise((resolve) => setTimeout(resolve, pauseMs));
  }
  return false;
}

function portableDaemonCandidates({ resourcesPath, executablePath, platform = process.platform } = {}) {
  const names = platform === 'win32' ? ['workass-daemon.exe', 'workass.exe'] : ['workass'];
  const candidates = [];
  if (executablePath) {
    for (const name of names) candidates.push(path.join(path.dirname(executablePath), name));
  }
  if (resourcesPath) {
    for (const name of names) {
      candidates.push(path.join(resourcesPath, 'runtime', name));
      candidates.push(path.join(resourcesPath, name));
    }
  }
  return [...new Set(candidates)];
}

function bundledPortableDaemon({ resourcesPath, executablePath, platform = process.platform } = {}) {
  const candidates = portableDaemonCandidates({ resourcesPath, executablePath, platform });
  for (const candidate of candidates) {
    if (fs.existsSync(candidate)) return candidate;
  }
  return '';
}

function portableReleaseManifest(resourcesPath) {
  const manifestPath = path.resolve(resourcesPath, '..', 'manifest.json');
  try {
    const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
    if (manifest.schemaVersion !== 2 || manifest.platform !== 'windows' || !String(manifest.version || '').trim()) return null;
    return manifest;
  } catch {
    return null;
  }
}

function spawnPortableDaemon({ runtime, executable, platform = process.platform, childSpawn = spawn, env = {}, workingDir } = {}) {
  if (!runtime || !executable) throw new Error('runtime and executable are required');
  fs.mkdirSync(runtime.stateDir, { recursive: true });
  fs.mkdirSync(runtime.logRoot, { recursive: true });
  const stdout = fs.openSync(path.join(runtime.logRoot, 'daemon.out.log'), 'a');
  let stderr;
  const args = [
    ...(runtime.profile === 'prod' ? ['--prod'] : []),
    '--headless',
    '--state-dir', runtime.stateDir,
    '--port', String(runtime.daemonPort),
    '--bind', runtime.daemonBind,
  ];
  let child;
  try {
    stderr = fs.openSync(path.join(runtime.logRoot, 'daemon.err.log'), 'a');
    child = childSpawn(executable, args, {
      cwd: workingDir || path.dirname(executable),
      env: {
        ...process.env,
        ...env,
        WORKASS_PROFILE: runtime.profile,
        WORKASS_DATA_ROOT: runtime.dataRoot,
        WORKASS_BROWSER_CONTROL_FILE: runtime.browserControlFile,
        ...(runtime.profile === 'prod' ? { WORKASS_PROD: '1' } : {}),
      },
      detached: true,
      windowsHide: platform === 'win32',
      stdio: ['ignore', stdout, stderr],
    });
  } finally {
    fs.closeSync(stdout);
    if (stderr !== undefined) fs.closeSync(stderr);
  }
  if (typeof child.unref === 'function') child.unref();
  return { child, args };
}

async function ensurePortableDaemon({
  runtime,
  resourcesPath,
  executablePath = process.execPath,
  platform = process.platform,
  check = healthCheck,
  childSpawn = spawn,
  wait = waitForHealth,
	waitForDown = waitForUnhealthy,
	shutdown = postLocalRecoveryShutdown,
  daemonExecutable = '',
} = {}) {
  if (!runtime || !resourcesPath) throw new Error('runtime and resourcesPath are required');

  const executable = daemonExecutable && fs.existsSync(daemonExecutable)
    ? daemonExecutable
    : bundledPortableDaemon({ resourcesPath, executablePath, platform });
  if (!executable) return { status: 'no-bundled-runtime', candidates: portableDaemonCandidates({ resourcesPath, executablePath, platform }) };

	const manifest = portableReleaseManifest(resourcesPath);
	const expectedVersion = manifest?.version || '';

	if (await check(runtime.daemonURL, 700, expectedVersion)) return { status: 'already-running', manifest };
	if (expectedVersion && await check(runtime.daemonURL)) {
		if (!await shutdown(runtime.daemonURL)) throw new Error('stale Workass daemon refused shutdown');
		if (!await waitForDown(runtime.daemonURL, { check })) throw new Error('stale Workass daemon did not stop');
	}

  const { child, args } = spawnPortableDaemon({ runtime, executable, platform, childSpawn });
	if (!await wait(runtime.daemonURL, { check, expectedVersion })) {
    try { child.kill?.(); } catch { /* best effort */ }
    throw new Error(`bundled Workass daemon did not become healthy: ${executable}`);
  }
	return { status: 'started-and-running', executable, args, pid: child.pid || null, manifest };
}

function postLocalRecoveryShutdown(url, timeoutMs = 1500) {
  return new Promise((resolve) => {
    let parsed;
    try { parsed = new URL(url); } catch { resolve(false); return; }
    if (!['127.0.0.1', 'localhost', '::1'].includes(parsed.hostname)) { resolve(false); return; }
    const transport = parsed.protocol === 'https:' ? https : http;
    const request = transport.request({
      protocol: parsed.protocol,
      hostname: parsed.hostname,
      port: parsed.port || (parsed.protocol === 'https:' ? 443 : 80),
      path: '/workass/recovery/shutdown', method: 'POST', timeout: timeoutMs,
      rejectUnauthorized: false,
    }, (response) => {
      response.resume();
      response.on('end', () => resolve(response.statusCode === 202));
    });
    request.on('timeout', () => { request.destroy(); resolve(false); });
    request.on('error', () => resolve(false));
    request.end();
  });
}

async function waitForUnhealthy(url, { attempts = 40, delayMs = 100, check = healthCheck } = {}) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (!await check(url)) return true;
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  return false;
}

// This is the shell-owned recovery transaction behind ⌘, → Enter.  It is
// intentionally local: it can stop only a daemon on loopback and then uses the
// exact sibling binary the portable shell would use for a cold launch.
async function restartDaemonAndRecover({
  runtime, resourcesPath, executablePath = process.execPath, platform = process.platform,
  daemonExecutable = '', check = healthCheck, childSpawn = spawn, wait = waitForHealth,
  waitForDown = waitForUnhealthy, shutdown = postLocalRecoveryShutdown, readInstanceId = healthInstanceId,
} = {}) {
  if (!runtime || !resourcesPath) throw new Error('runtime and resourcesPath are required');
  const executable = daemonExecutable && fs.existsSync(daemonExecutable)
    ? daemonExecutable
    : bundledPortableDaemon({ resourcesPath, executablePath, platform });
  if (!executable) throw new Error('bundled Workass daemon was not found');

  const wasHealthy = await check(runtime.daemonURL);
  const oldInstanceId = wasHealthy ? await readInstanceId(runtime.daemonURL) : '';
  if (wasHealthy && !oldInstanceId) throw new Error('daemon instance identity is unavailable; restart was not attempted');
  const shutdownAccepted = wasHealthy ? await shutdown(runtime.daemonURL) : false;
  if (wasHealthy && !shutdownAccepted) throw new Error('daemon refused the local recovery shutdown');
  // A legacy Windows Task Scheduler service can restart a healthy daemon in less than our
  // 100 ms observation interval.  Missing that tiny down window is not a
  // failure: the only user-visible requirement is a healthy daemon after the
  // accepted graceful stop.  The subsequent ensure call verifies exactly that.
  const stoppedObserved = wasHealthy ? await waitForDown(runtime.daemonURL, { check }) : true;
  const receipt = await ensurePortableDaemon({
    runtime, resourcesPath, executablePath, platform, daemonExecutable: executable, check, childSpawn, wait,
  });
  const newInstanceId = await readInstanceId(runtime.daemonURL);
  if (!newInstanceId || (wasHealthy && newInstanceId === oldInstanceId)) {
    throw new Error('daemon restart was not confirmed; the running instance did not change');
  }
  return { ...receipt, shutdownAccepted, stoppedObserved, oldInstanceId: oldInstanceId || null, newInstanceId };
}

// Compatibility cleanup only: new launches never register or enable a service.
function removeLegacyDaemonService({ runtime, home, uid, run = spawnSync }) {
  const plistPath = path.join(home, 'Library', 'LaunchAgents', `${runtime.launchdLabel}.plist`);
  if (!fs.existsSync(plistPath)) return false;
  const result = run('/bin/launchctl', ['bootout', `gui/${uid}/${runtime.launchdLabel}`], {
    encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
  });
  if (result.error || (result.status !== 0 && result.status !== 3 && result.status !== 113 &&
      !/could not find service/i.test(String(result.stderr || '')))) {
    throw new Error('could not remove the legacy Workass background service');
  }
  fs.unlinkSync(plistPath);
  return result.status === 0;
}

async function ensurePackagedDaemon({
  runtime, resourcesPath, platform = process.platform, home = os.homedir(), uid = process.getuid?.(),
  spawn: legacyServiceRun = spawnSync, childSpawn = spawn, check = healthCheck, forceInstall = false,
  wait = waitForHealth, waitForDown = waitForUnhealthy, shutdown = postLocalRecoveryShutdown,
} = {}) {
  if (platform !== 'darwin') return { status: 'unsupported-platform' };
  if (!runtime || !resourcesPath) throw new Error('runtime and resourcesPath are required');

  const bundledRoot = path.join(resourcesPath, 'runtime');
  const manifestPath = path.join(bundledRoot, 'manifest.json');
  if (!fs.existsSync(manifestPath)) {
    if (!forceInstall && await check(runtime.daemonURL)) return { status: 'already-running' };
    return { status: 'no-bundled-runtime' };
  }
  const appRoot = path.resolve(resourcesPath, '..', '..');
  const durableRoots = ['/Applications', path.join(home, 'Applications')];
  if (!durableRoots.some((root) => appRoot === root || appRoot.startsWith(`${root}${path.sep}`))) {
    return { status: 'move-to-applications', appRoot };
  }
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  if (manifest.schemaVersion !== 1 || manifest.platform !== 'darwin' || !manifest.arch || !manifest.version || !manifest.build) {
    throw new Error('invalid bundled runtime manifest');
  }
  const releaseMarkerPath = path.join(runtime.dataRoot, 'runtime-release.json');
  let releaseMarker = null;
  try { releaseMarker = JSON.parse(fs.readFileSync(releaseMarkerPath, 'utf8')); } catch { /* first launch */ }
  const markerMatches = releaseMarker?.schemaVersion === 1 &&
    releaseMarker.version === manifest.version && String(releaseMarker.build) === String(manifest.build) &&
    releaseMarker.appRoot === appRoot;

  const executable = path.join(bundledRoot, 'workass');
  const nodeBin = path.join(bundledRoot, 'node', `darwin-${manifest.arch}`, 'bin');
  const frontierHosts = path.join(bundledRoot, 'frontier-hosts', `darwin-${manifest.arch}`);
  for (const required of [
    executable,
    path.join(nodeBin, 'node'),
    path.join(frontierHosts, 'claude-native-host.mjs'),
    path.join(frontierHosts, 'codex-native-host.mjs'),
    path.join(frontierHosts, 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'),
  ]) {
    if (!fs.existsSync(required)) throw new Error(`bundled runtime is incomplete: ${required}`);
  }

  const removed = removeLegacyDaemonService({ runtime, home, uid, run: legacyServiceRun });
  if (removed && !await waitForDown(runtime.daemonURL, { check })) {
    throw new Error('legacy Workass daemon did not stop');
  }
  if (!forceInstall && markerMatches && await check(runtime.daemonURL, 700, manifest.version)) {
    return { status: 'already-running', manifest, releaseMarkerPath };
  }
  if (!forceInstall && await check(runtime.daemonURL)) {
    if (!await shutdown(runtime.daemonURL)) throw new Error('stale Workass daemon refused shutdown');
    if (!await waitForDown(runtime.daemonURL, { check })) throw new Error('stale Workass daemon did not stop');
  }

  const runtimePath = [
    nodeBin,
    path.join(home, '.local', 'bin'),
    path.join(home, '.npm-global', 'bin'),
    path.join(home, '.bun', 'bin'),
    '/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin', '/usr/sbin', '/sbin',
  ].join(':');
  const { child, args } = spawnPortableDaemon({
    runtime, executable, platform, childSpawn,
    env: { PATH: runtimePath, HOME: home },
  });
  try {
    if (typeof child.once === 'function') {
      await new Promise((resolve, reject) => {
        child.once('spawn', resolve);
        child.once('error', reject);
      });
    }
    if (!await wait(runtime.daemonURL, { check, expectedVersion: manifest.version, attempts: 2400, delayMs: 50, timeoutMs: 120000 })) {
      throw new Error('bundled Workass daemon did not become healthy');
    }
  } catch (error) {
    if (child.pid && typeof child.once === 'function' && child.exitCode === null && child.signalCode === null) {
      const exited = new Promise(resolve => child.once('exit', resolve));
      child.kill();
      await exited;
    } else {
      child.kill?.();
    }
    throw error;
  }
  const markerIncoming = `${releaseMarkerPath}.incoming-${process.pid}`;
  fs.writeFileSync(markerIncoming, JSON.stringify({
    schemaVersion: 1, version: manifest.version, build: manifest.build, appRoot,
  }) + '\n', { mode: 0o600 });
  fs.renameSync(markerIncoming, releaseMarkerPath);
  return { status: 'started-and-running', manifest, executable, args, pid: child.pid || null, releaseMarkerPath };
}

async function restartPackagedDaemonAndRecover({
  runtime, resourcesPath, platform = process.platform, home = os.homedir(), uid = process.getuid?.(),
  check = healthCheck, waitForDown = waitForUnhealthy, shutdown = postLocalRecoveryShutdown,
  launchctlSpawn = spawnSync, childSpawn = spawn, wait = waitForHealth, readInstanceId = healthInstanceId,
} = {}) {
  if (platform !== 'darwin') throw new Error('packaged daemon recovery is supported on macOS only');
  const executable = path.join(resourcesPath, 'runtime', 'workass');
  if (!fs.existsSync(executable)) throw new Error('bundled Workass daemon was not found');
  const wasHealthy = await check(runtime.daemonURL);
  const oldInstanceId = wasHealthy ? await readInstanceId(runtime.daemonURL) : '';
  if (wasHealthy && !oldInstanceId) throw new Error('daemon instance identity is unavailable; restart was not attempted');
  const shutdownAccepted = wasHealthy ? await shutdown(runtime.daemonURL) : false;
  if (wasHealthy && !shutdownAccepted) throw new Error('daemon refused the local recovery shutdown');
  const stoppedObserved = wasHealthy ? await waitForDown(runtime.daemonURL, { check }) : true;
  if (!stoppedObserved) throw new Error('Workass daemon did not stop for recovery');
  const receipt = await ensurePackagedDaemon({
    runtime, resourcesPath, platform, home, uid, spawn: launchctlSpawn, childSpawn, check, wait, forceInstall: true,
    waitForDown, shutdown,
  });
  const newInstanceId = await readInstanceId(runtime.daemonURL);
  if (!newInstanceId || (wasHealthy && newInstanceId === oldInstanceId)) {
    throw new Error('daemon restart was not confirmed; the running instance did not change');
  }
  return { ...receipt, shutdownAccepted, stoppedObserved, oldInstanceId: oldInstanceId || null, newInstanceId };
}

module.exports = {
  bundledPortableDaemon,
  ensurePackagedDaemon,
  ensurePortableDaemon,
  healthCheck,
  healthInstanceId,
  portableDaemonCandidates,
	portableReleaseManifest,
  postLocalRecoveryShutdown,
  restartDaemonAndRecover,
  restartPackagedDaemonAndRecover,
  spawnPortableDaemon,
  waitForUnhealthy,
  waitForHealth,
};
