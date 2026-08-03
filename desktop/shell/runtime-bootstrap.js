'use strict';

const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
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

function healthCheck(url, timeoutMs = 700) {
  return new Promise((resolve) => {
    const request = http.get(`${url}/workass/health`, { timeout: timeoutMs }, (response) => {
      let body = '';
      response.setEncoding('utf8');
      response.on('data', (chunk) => {
        if (body.length < 4096) body += chunk;
      });
      response.on('end', () => {
        try {
          const parsed = JSON.parse(body);
          resolve(response.statusCode >= 200 && response.statusCode < 300 && parsed.app === 'workass');
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

async function waitForHealth(url, { attempts = 120, delayMs = 250, check = healthCheck } = {}) {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (await check(url)) return true;
    await new Promise((resolve) => setTimeout(resolve, delayMs));
  }
  return false;
}

async function ensurePackagedDaemon({ runtime, resourcesPath, platform = process.platform, home = os.homedir(), uid = process.getuid?.(), spawn = spawnSync, check = healthCheck } = {}) {
  if (platform !== 'darwin') return { status: 'unsupported-platform' };
  if (!runtime || !resourcesPath) throw new Error('runtime and resourcesPath are required');
  if (await check(runtime.daemonURL)) return { status: 'already-running' };

  const bundledRoot = path.join(resourcesPath, 'runtime');
  const manifestPath = path.join(bundledRoot, 'manifest.json');
  if (!fs.existsSync(manifestPath)) return { status: 'no-bundled-runtime' };
  const appRoot = path.resolve(resourcesPath, '..', '..');
  const durableRoots = ['/Applications', path.join(home, 'Applications')];
  if (!durableRoots.some((root) => appRoot === root || appRoot.startsWith(`${root}${path.sep}`))) {
    return { status: 'move-to-applications', appRoot };
  }
  const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
  if (manifest.schemaVersion !== 1 || manifest.platform !== 'darwin' || !manifest.arch) {
    throw new Error('invalid bundled runtime manifest');
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
  if (!await waitForHealth(runtime.daemonURL, { check })) throw new Error('bundled Workass daemon did not become healthy');
  return { status: 'installed-and-running', manifest, plistPath, executable };
}

module.exports = { ensurePackagedDaemon, healthCheck, launchAgentPlist, waitForHealth, xmlEscape };
