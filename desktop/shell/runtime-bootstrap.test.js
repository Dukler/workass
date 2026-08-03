'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const { ensurePackagedDaemon, healthCheck, launchAgentPlist } = require('./runtime-bootstrap');

function fixture() {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-runtime-bootstrap-'));
  const home = path.join(root, 'home');
  const resourcesPath = path.join(home, 'Applications', 'Workass.app', 'Contents', 'Resources');
  const runtimeRoot = path.join(resourcesPath, 'runtime');
  for (const file of [
    'workass',
    'node/darwin-arm64/bin/node',
    'frontier-hosts/darwin-arm64/claude-native-host.mjs',
    'frontier-hosts/darwin-arm64/codex-native-host.mjs',
    'frontier-hosts/darwin-arm64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
  ]) {
    const target = path.join(runtimeRoot, file);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, 'fixture', { mode: 0o755 });
  }
  fs.writeFileSync(path.join(runtimeRoot, 'manifest.json'), JSON.stringify({ schemaVersion: 1, platform: 'darwin', arch: 'arm64', version: '1.2.3', build: '7' }));
  const dataRoot = path.join(home, 'Library', 'Application Support', 'Workass');
  return {
    root, home, resourcesPath,
    runtime: {
      profile: 'prod', daemonURL: 'http://127.0.0.1:8788', daemonPort: 8788, daemonBind: 'localhost',
      launchdLabel: 'com.workass.daemon', dataRoot, stateDir: path.join(dataRoot, 'state'),
      logRoot: path.join(home, 'Library', 'Logs', 'Workass', 'prod'), browserControlFile: path.join(dataRoot, 'run', 'browser-control.json'),
    },
  };
}

test('packaged bootstrap is a no-op while the daemon is already healthy', async () => {
  const { runtime, resourcesPath, home } = fixture();
  const calls = [];
  const result = await ensurePackagedDaemon({ runtime, resourcesPath, home, uid: 501, check: async () => true, spawn: (_bin, args) => { calls.push(args); return { status: 0 }; } });
  assert.equal(result.status, 'already-running');
  assert.deepEqual(calls, []);
});

test('clean-Mac bootstrap installs the bundled runtime as a user LaunchAgent', async () => {
  const { runtime, resourcesPath, home } = fixture();
  const calls = [];
  let healthChecks = 0;
  const result = await ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501,
    check: async () => { healthChecks += 1; return healthChecks > 1; },
    spawn: (_bin, args) => { calls.push(args); return { status: 0, stdout: '', stderr: '' }; },
  });
  assert.equal(result.status, 'installed-and-running');
  assert.deepEqual(calls.map((args) => args[0]), ['bootout', 'bootstrap', 'enable', 'kickstart']);
  const plist = fs.readFileSync(result.plistPath, 'utf8');
  assert.match(plist, /Contents\/Resources\/runtime\/workass/);
  assert.match(plist, /Contents\/Resources\/runtime\/node\/darwin-arm64\/bin/);
  assert.doesNotMatch(plist, /Contents\/Resources\/runtime\/adapters/);
  assert.doesNotMatch(plist, /node_modules\/electron/);
});

test('launchd plist escapes paths rather than injecting XML', () => {
  const plist = launchAgentPlist({
    label: 'com.workass.daemon', executable: '/Applications/A&B/Workass', stateDir: '/tmp/<state>', port: 8788,
    bind: 'localhost', workingDir: '/tmp/runtime', logRoot: '/tmp/log', runtimePath: '/usr/bin', home: '/Users/test',
    profile: 'prod', dataRoot: '/tmp/data', browserControlFile: '/tmp/browser.json',
  });
  assert.match(plist, /A&amp;B/);
  assert.match(plist, /&lt;state&gt;/);
});

test('first launch from a disk image does not install a LaunchAgent with an ephemeral path', async () => {
  const fixtureData = fixture();
  const volumeResources = path.join(fixtureData.root, 'Volumes', 'Workass', 'Workass.app', 'Contents', 'Resources');
  fs.mkdirSync(path.dirname(volumeResources), { recursive: true });
  fs.cpSync(fixtureData.resourcesPath, volumeResources, { recursive: true });
  const calls = [];
  const result = await ensurePackagedDaemon({
    runtime: fixtureData.runtime, resourcesPath: volumeResources, home: fixtureData.home, uid: 501,
    check: async () => false,
    spawn: (_bin, args) => { calls.push(args); return { status: 0 }; },
  });
  assert.equal(result.status, 'move-to-applications');
  assert.deepEqual(calls, []);
});

test('health probe rejects unrelated services occupying the Workass port', async (t) => {
  let body = JSON.stringify({ app: 'something-else' });
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(body);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const address = server.address();
  const url = `http://127.0.0.1:${address.port}`;
  assert.equal(await healthCheck(url), false);
  body = JSON.stringify({ app: 'workass' });
  assert.equal(await healthCheck(url), true);
});
