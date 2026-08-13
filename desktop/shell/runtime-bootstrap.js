'use strict';

const fs = require('node:fs');
const http = require('node:http');
const https = require('node:https');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { spawnSync } = require('node:child_process');

function xmlEscape(value) {
  return String(value)
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&apos;');
}

function launchAgentPlist({ label, executable, stateDir, port, bind, workingDir, logRoot, runtimePath, home, profile, dataRoot, browserControlFile }) {
  const env = {
    PATH: runtimePath,
    HOME: home,
    WORKASS_PROFILE: profile,
    WORKASS_DATA_ROOT: dataRoot,
    WORKASS_BROWSER_CONTROL_FILE: browserControlFile,
  };
  const envXML = Object.entries(env).map(([key, value]) =>
    `    <key>${xmlEscape(key)}</key>\n    <string>${xmlEscape(value)}</string>`).join('\n');
  return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${xmlEscape(label)}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${xmlEscape(executable)}</string>
    <string>--state-dir</string>
    <string>${xmlEscape(stateDir)}</string>
    <string>--port</string>
    <string>${port}</string>
    <string>--bind</string>
    <string>${xmlEscape(bind)}</string>
  </array>
  <key>WorkingDirectory</key>
  <string>${xmlEscape(workingDir)}</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${xmlEscape(path.join(logRoot, 'workass.out.log'))}</string>
  <key>StandardErrorPath</key>
  <string>${xmlEscape(path.join(logRoot, 'workass.err.log'))}</string>
  <key>EnvironmentVariables</key>
  <dict>
${envXML}
  </dict>
</dict>
</plist>
`;
}

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

function runLaunchctl(args, spawn = spawnSync) {
  return spawn('/bin/launchctl', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

async function waitForHealth(url, { attempts = 120, delayMs = 250, check = healthCheck, expectedVersion = '' } = {}) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (await check(url, 700, expectedVersion)) return true;
    await new Promise((resolve) => setTimeout(resolve, delayMs));
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

function spawnPortableDaemon({ runtime, executable, platform = process.platform, childSpawn = spawn } = {}) {
  if (!runtime || !executable) throw new Error('runtime and executable are required');
  fs.mkdirSync(runtime.stateDir, { recursive: true });
  fs.mkdirSync(runtime.logRoot, { recursive: true });
  const stdout = fs.openSync(path.join(runtime.logRoot, 'daemon.out.log'), 'a');
  const stderr = fs.openSync(path.join(runtime.logRoot, 'daemon.err.log'), 'a');
  const args = [
    ...(runtime.profile === 'prod' ? ['--prod'] : []),
    '--headless',
    '--state-dir', runtime.stateDir,
    '--port', String(runtime.daemonPort),
    '--bind', runtime.daemonBind,
  ];
  const child = childSpawn(executable, args, {
    cwd: path.dirname(executable),
    env: {
      ...process.env,
      WORKASS_PROFILE: runtime.profile,
      WORKASS_DATA_ROOT: runtime.dataRoot,
      WORKASS_BROWSER_CONTROL_FILE: runtime.browserControlFile,
      ...(runtime.profile === 'prod' ? { WORKASS_PROD: '1' } : {}),
    },
    detached: true,
    windowsHide: platform === 'win32',
    stdio: ['ignore', stdout, stderr],
  });
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
	// The first LAN-default Windows release must stop a still-running 0.1.x
	// loopback daemon before it starts the bundled daemon on port 80.
	if (platform === 'win32' && runtime.profile === 'prod' && runtime.daemonPort !== 8788) {
		const previousURL = new URL(runtime.daemonURL);
		previousURL.hostname = '127.0.0.1';
		previousURL.port = '8788';
		const previousDaemonURL = previousURL.toString().replace(/\/$/, '');
		if (await check(previousDaemonURL)) {
			if (!await shutdown(previousDaemonURL)) throw new Error(`previous Workass daemon refused shutdown: ${previousDaemonURL}`);
			if (!await waitForDown(previousDaemonURL, { check })) throw new Error(`previous Workass daemon did not stop: ${previousDaemonURL}`);
		}
	}

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
  waitForDown = waitForUnhealthy, shutdown = postLocalRecoveryShutdown,
} = {}) {
  if (!runtime || !resourcesPath) throw new Error('runtime and resourcesPath are required');
  const executable = daemonExecutable && fs.existsSync(daemonExecutable)
    ? daemonExecutable
    : bundledPortableDaemon({ resourcesPath, executablePath, platform });
  if (!executable) throw new Error('bundled Workass daemon was not found');

  const wasHealthy = await check(runtime.daemonURL);
  const shutdownAccepted = wasHealthy ? await shutdown(runtime.daemonURL) : false;
  if (wasHealthy && !shutdownAccepted) throw new Error('daemon refused the local recovery shutdown');
  // launchd and Task Scheduler can restart a healthy daemon in less than our
  // 100 ms observation interval.  Missing that tiny down window is not a
  // failure: the only user-visible requirement is a healthy daemon after the
  // accepted graceful stop.  The subsequent ensure call verifies exactly that.
  const stoppedObserved = wasHealthy ? await waitForDown(runtime.daemonURL, { check }) : true;
  const receipt = await ensurePortableDaemon({
    runtime, resourcesPath, executablePath, platform, daemonExecutable: executable, check, childSpawn, wait,
  });
  return { ...receipt, shutdownAccepted, stoppedObserved };
}

async function ensurePackagedDaemon({ runtime, resourcesPath, platform = process.platform, home = os.homedir(), uid = process.getuid?.(), spawn = spawnSync, check = healthCheck, forceInstall = false } = {}) {
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
  try { releaseMarker = JSON.parse(fs.readFileSync(releaseMarkerPath, 'utf8')); } catch { /* first self-contained launch */ }
  const markerMatches = releaseMarker?.schemaVersion === 1 &&
    releaseMarker.version === manifest.version && String(releaseMarker.build) === String(manifest.build) &&
    releaseMarker.appRoot === appRoot;
  if (!forceInstall && markerMatches && await check(runtime.daemonURL, 700, manifest.version)) {
    return { status: 'already-running', manifest, releaseMarkerPath };
  }

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

  fs.mkdirSync(runtime.stateDir, { recursive: true });
  fs.mkdirSync(runtime.logRoot, { recursive: true });
  const launchAgents = path.join(home, 'Library', 'LaunchAgents');
  fs.mkdirSync(launchAgents, { recursive: true });
  const plistPath = path.join(launchAgents, `${runtime.launchdLabel}.plist`);
  const runtimePath = [
    nodeBin,
    path.join(home, '.local', 'bin'),
    path.join(home, '.npm-global', 'bin'),
    path.join(home, '.bun', 'bin'),
    '/opt/homebrew/bin', '/usr/local/bin', '/usr/bin', '/bin', '/usr/sbin', '/sbin',
  ].join(':');
  const plist = launchAgentPlist({
    label: runtime.launchdLabel,
    executable,
    stateDir: runtime.stateDir,
    port: runtime.daemonPort,
    bind: runtime.daemonBind,
    workingDir: bundledRoot,
    logRoot: runtime.logRoot,
    runtimePath,
    home,
    profile: runtime.profile,
    dataRoot: runtime.dataRoot,
    browserControlFile: runtime.browserControlFile,
  });
  const incoming = `${plistPath}.incoming-${process.pid}`;
  fs.writeFileSync(incoming, plist, { mode: 0o600 });
  fs.renameSync(incoming, plistPath);

  const domain = `gui/${uid}`;
  runLaunchctl(['bootout', domain, plistPath], spawn);
  const bootstrap = runLaunchctl(['bootstrap', domain, plistPath], spawn);
  if (bootstrap.status !== 0) {
    throw new Error(`could not install Workass LaunchAgent: ${(bootstrap.stderr || '').trim() || `exit ${bootstrap.status}`}`);
  }
  const enable = runLaunchctl(['enable', `${domain}/${runtime.launchdLabel}`], spawn);
  if (enable.status !== 0) throw new Error(`could not enable Workass LaunchAgent: ${(enable.stderr || '').trim() || `exit ${enable.status}`}`);
  const kickstart = runLaunchctl(['kickstart', '-k', `${domain}/${runtime.launchdLabel}`], spawn);
  if (kickstart.status !== 0) throw new Error(`could not start Workass LaunchAgent: ${(kickstart.stderr || '').trim() || `exit ${kickstart.status}`}`);
  if (!await waitForHealth(runtime.daemonURL, { check, expectedVersion: manifest.version })) throw new Error('bundled Workass daemon did not become healthy');
  fs.mkdirSync(runtime.dataRoot, { recursive: true });
  const markerIncoming = `${releaseMarkerPath}.incoming-${process.pid}`;
  fs.writeFileSync(markerIncoming, `${JSON.stringify({
    schemaVersion: 1,
    version: manifest.version,
    build: manifest.build,
    appRoot,
  }, null, 2)}\n`, { mode: 0o600 });
  fs.renameSync(markerIncoming, releaseMarkerPath);
  return { status: 'installed-and-running', manifest, plistPath, executable, releaseMarkerPath };
}

async function restartPackagedDaemonAndRecover({
  runtime, resourcesPath, platform = process.platform, home = os.homedir(), uid = process.getuid?.(),
  check = healthCheck, waitForDown = waitForUnhealthy, shutdown = postLocalRecoveryShutdown,
  launchctlSpawn = spawnSync,
} = {}) {
  if (platform !== 'darwin') throw new Error('packaged daemon recovery is supported on macOS only');
  const executable = path.join(resourcesPath, 'runtime', 'workass');
  if (!fs.existsSync(executable)) throw new Error('bundled Workass daemon was not found');
  const wasHealthy = await check(runtime.daemonURL);
  const shutdownAccepted = wasHealthy ? await shutdown(runtime.daemonURL) : false;
  if (wasHealthy && !shutdownAccepted) throw new Error('daemon refused the local recovery shutdown');
  const stoppedObserved = wasHealthy ? await waitForDown(runtime.daemonURL, { check }) : true;
  const receipt = await ensurePackagedDaemon({
    runtime, resourcesPath, platform, home, uid, spawn: launchctlSpawn, check, forceInstall: true,
  });
  return { ...receipt, shutdownAccepted, stoppedObserved };
}

module.exports = {
  bundledPortableDaemon,
  ensurePackagedDaemon,
  ensurePortableDaemon,
  healthCheck,
  launchAgentPlist,
  portableDaemonCandidates,
	portableReleaseManifest,
  postLocalRecoveryShutdown,
  restartDaemonAndRecover,
  restartPackagedDaemonAndRecover,
  spawnPortableDaemon,
  waitForUnhealthy,
  waitForHealth,
  xmlEscape,
};
