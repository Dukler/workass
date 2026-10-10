'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawn: spawnChild } = require('node:child_process');
const { once } = require('node:events');
const test = require('node:test');
const { ensurePackagedDaemon, ensurePortableDaemon, healthCheck, healthInstanceId, restartDaemonAndRecover, restartPackagedDaemonAndRecover, waitForHealth } = require('./runtime-bootstrap');

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-runtime-bootstrap-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
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

test('packaged bootstrap is a no-op while the daemon is already healthy', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  fs.mkdirSync(runtime.dataRoot, { recursive: true });
  fs.writeFileSync(path.join(runtime.dataRoot, 'runtime-release.json'), JSON.stringify({
    schemaVersion: 1,
    version: '1.2.3',
    build: '7',
    appRoot: path.resolve(resourcesPath, '..', '..'),
  }));
  const calls = [];
  const result = await ensurePackagedDaemon({ runtime, resourcesPath, home, uid: 501, check: async () => true, spawn: (_bin, args) => { calls.push(args); return { status: 0 }; } });
  assert.equal(result.status, 'already-running');
  assert.deepEqual(calls, []);
});

test('a healthy daemon without this exact release marker is replaced directly', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  const calls = [];
  let running = true;
  const result = await ensurePackagedDaemon({
    runtime, resourcesPath, home,
    check: async () => running,
    spawn: (...args) => { calls.push(args); throw new Error('no launchctl'); },
    shutdown: async () => { calls.push('shutdown'); running = false; return true; },
    childSpawn: () => { calls.push('start'); return { pid: 41, unref() {} }; },
    wait: async () => true,
  });
  assert.equal(result.status, 'started-and-running');
  assert.deepEqual(calls, ['shutdown', 'start']);
  assert.equal(fs.existsSync(path.join(home, 'Library', 'LaunchAgents')), false);
});

test('clean-Mac bootstrap starts the bundled server directly without login startup or a restart service', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  let invocation;
  const result = await ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501,
    check: async () => false,
    spawn: () => { throw new Error('clean launch must not call launchctl'); },
    childSpawn: (...args) => { invocation = args; return { pid: 42, unref() {} }; },
    wait: async (_url, options) => { assert.equal(options.expectedVersion, '1.2.3'); return true; },
  });
  assert.equal(result.status, 'started-and-running');
  assert.equal(result.pid, 42);
  assert.equal(invocation[0], path.join(resourcesPath, 'runtime', 'workass'));
  assert.equal(invocation[2].detached, true);
  assert.equal(invocation[2].env.HOME, home);
  assert.equal(invocation[2].env.WORKASS_DATA_ROOT, runtime.dataRoot);
  assert.equal(invocation[2].env.WORKASS_PROFILE, 'prod');
  assert.ok(invocation[2].env.PATH.startsWith(path.join(resourcesPath, 'runtime', 'node', 'darwin-arm64', 'bin')));
  assert.equal(fs.existsSync(path.join(home, 'Library', 'LaunchAgents')), false);
  const marker = JSON.parse(fs.readFileSync(result.releaseMarkerPath, 'utf8'));
  assert.equal(marker.version, '1.2.3');
  assert.equal(marker.build, '7');
});

for (const status of [0, 3, 113]) test(`Mac launch removes a legacy plist with bootout status ${status} without enabling a service`, async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  const plist = path.join(home, 'Library', 'LaunchAgents', `${runtime.launchdLabel}.plist`);
  fs.mkdirSync(path.dirname(plist), { recursive: true });
  fs.writeFileSync(plist, 'legacy fixture');
  const calls = [];
  const result = await ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501,
    check: async () => false,
    spawn: (bin, args) => { calls.push([bin, args]); return { status }; },
    childSpawn: () => ({ pid: 43, unref() {} }),
    wait: async () => true,
  });
  assert.equal(result.status, 'started-and-running');
  assert.deepEqual(calls, [['/bin/launchctl', ['bootout', 'gui/501/com.workass.daemon']]]);
  assert.equal(fs.existsSync(plist), false);
});

test('failed legacy-service removal refuses to start a competing Mac server', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  const plist = path.join(home, 'Library', 'LaunchAgents', `${runtime.launchdLabel}.plist`);
  fs.mkdirSync(path.dirname(plist), { recursive: true });
  fs.writeFileSync(plist, 'legacy fixture');
  await assert.rejects(ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501,
    spawn: () => ({ status: 1, stderr: 'denied' }),
    childSpawn: () => { throw new Error('must not start'); },
  }), /could not remove the legacy/);
  assert.equal(fs.existsSync(plist), true);
});

test('a killed Mac server stays stopped; opening Workass again starts a new process', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  runtime.profile = 'test';
  const reservation = http.createServer();
  await new Promise(resolve => reservation.listen(0, '127.0.0.1', resolve));
  runtime.daemonPort = reservation.address().port;
  runtime.daemonURL = `http://127.0.0.1:${runtime.daemonPort}`;
  await new Promise(resolve => reservation.close(resolve));
  fs.writeFileSync(path.join(resourcesPath, 'runtime', 'workass'), `#!${process.execPath}
const http = require('node:http');
const port = Number(process.argv[process.argv.indexOf('--port') + 1]);
const server = http.createServer((req, res) => {
  res.setHeader('content-type', 'application/json');
  res.end(JSON.stringify({ app: 'workass', version: '1.2.3' }));
});
server.listen(port, '127.0.0.1', () => process.send({ ready: true }));
`);
  const children = [];
  let ready;
  const options = {
    runtime, resourcesPath, home, check: healthCheck,
    spawn: () => { throw new Error('must not install a service'); },
    childSpawn: (executable, args, settings) => {
      const child = spawnChild(executable, args, { ...settings, stdio: [...settings.stdio, 'ipc'] });
      children.push(child);
      ready = once(child, 'message');
      return child;
    },
    wait: async (url, settings) => {
      const [message] = await ready;
      assert.deepEqual(message, { ready: true });
      return healthCheck(url, 700, settings.expectedVersion);
    },
  };
  t.after(async () => {
    for (const child of children) {
      if (child.exitCode !== null || child.signalCode !== null) continue;
      const exited = once(child, 'exit');
      child.kill('SIGKILL');
      await exited;
    }
  });
  const first = await ensurePackagedDaemon(options);
  assert.equal(await healthCheck(runtime.daemonURL), true);
  const exited = once(children[0], 'exit');
  children[0].kill('SIGKILL');
  await exited;
  assert.equal(await healthCheck(runtime.daemonURL), false);
  assert.equal(children.length, 1);
  assert.equal(fs.existsSync(path.join(home, 'Library', 'LaunchAgents')), false);
  const reopened = await ensurePackagedDaemon(options);
  assert.notEqual(first.pid, reopened.pid);
  assert.equal(children.length, 2);
  assert.equal(await healthCheck(runtime.daemonURL), true);
});

test('macOS update bootstrap permits chat hydration beyond the old 30 second cutoff', async (t) => {
  const { root, runtime, resourcesPath, home } = fixture(t);
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 0 });
  let checks = 0;
  let settled = false;
  const installing = ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501, forceInstall: true,
    check: async (_url, timeout, version) => {
      assert.equal(timeout, 700);
      assert.equal(version, '1.2.3');
      return ++checks > 160;
    },
    childSpawn: () => ({ pid: 44, unref() {}, kill() {} }),
  }).then(receipt => ({ receipt }), error => ({ error })).finally(() => { settled = true; });
  for (let tick = 0; tick < 500 && !settled; tick++) {
    await new Promise(setImmediate);
    if (!settled) t.mock.timers.tick(250);
  }
  const { receipt, error } = await installing;
  assert.equal(error, undefined, 'A healthy target at 40 seconds must not roll back at 30 seconds');
  assert.equal(receipt.status, 'started-and-running');
  assert.equal(checks, 161);
  assert.equal(Date.now(), 40000);
});

test('macOS update bootstrap still fails closed after its bounded recovery window', async (t) => {
  const { root, runtime, resourcesPath, home } = fixture(t);
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 0 });
  let checks = 0;
  let settled = false;
  const probeTimeouts = [];
  const installing = ensurePackagedDaemon({
    runtime, resourcesPath, home, uid: 501, forceInstall: true,
    check: async (_url, timeout, version) => {
      assert.ok(timeout > 0 && timeout <= 700);
      probeTimeouts.push(timeout);
      assert.equal(version, '1.2.3');
      checks++;
      return false;
    },
    childSpawn: () => ({ pid: 44, unref() {}, kill() {} }),
  }).then(receipt => ({ receipt }), error => ({ error })).finally(() => { settled = true; });
  for (let tick = 0; tick < 500 && !settled; tick++) {
    await new Promise(setImmediate);
    if (!settled) t.mock.timers.tick(250);
  }
  const { receipt, error } = await installing;
  assert.equal(receipt, undefined);
  assert.match(error.message, /bundled Workass daemon did not become healthy/);
  assert.equal(checks, 480);
  assert.equal(Date.now(), 120000);
  assert.equal(probeTimeouts.at(-1), 250);
  assert.equal(fs.existsSync(path.join(runtime.dataRoot, 'runtime-release.json')), false);
});

test('health probes share the startup deadline when each request uses its timeout', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 0 });
  const timeouts = [];
  let settled = false;
  const waiting = waitForHealth('https://127.0.0.1:18788', {
    attempts: 480, timeoutMs: 120000, expectedVersion: '1.2.3',
    check: async (_url, timeout, version) => {
      assert.equal(version, '1.2.3');
      timeouts.push(timeout);
      t.mock.timers.tick(timeout);
      return false;
    },
  }).finally(() => { settled = true; });
  for (let tick = 0; tick < 500 && !settled; tick++) {
    await new Promise(setImmediate);
    if (!settled) t.mock.timers.tick(Math.min(250, 120000 - Date.now()));
  }
  assert.equal(await waiting, false);
  assert.equal(Date.now(), 120000);
  assert.ok(timeouts.every(timeout => timeout > 0 && timeout <= 700));
  assert.ok(timeouts.length < 130);
});

test('first launch from a disk image does not start a server with an ephemeral path', async (t) => {
  const fixtureData = fixture(t);
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
  assert.equal(await healthCheck(url, 700, '9.9.9'), false);
  body = JSON.stringify({ app: 'workass', version: '9.9.9' });
  assert.equal(await healthCheck(url, 700, '9.9.9'), true);
});

test('loopback health exposes a bounded daemon instance id for restart confirmation', async (t) => {
  const server = http.createServer((_request, response) => {
    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ app: 'workass', instanceId: 'i-0123456789abcdef' }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => server.close());
  const url = 'http://127.0.0.1:' + server.address().port;
  assert.equal(await healthInstanceId(url), 'i-0123456789abcdef');
  assert.equal(await healthInstanceId('http://192.0.2.1:80'), '');
});

test('portable bootstrap starts the sibling Windows daemon when no daemon is healthy', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-bootstrap-'));
  const executablePath = path.join(root, 'Workass.exe');
  const daemonPath = path.join(root, 'workass-daemon.exe');
  fs.writeFileSync(executablePath, 'electron');
  fs.writeFileSync(daemonPath, 'daemon');
	fs.writeFileSync(path.join(root, 'manifest.json'), JSON.stringify({ schemaVersion: 2, platform: 'windows', version: '1.2.3' }));
  const runtime = {
    profile: 'prod', daemonURL: 'http://127.0.0.1:8788', daemonPort: 8788, daemonBind: 'localhost',
    dataRoot: path.join(root, 'data'), stateDir: path.join(root, 'data', 'state'),
    logRoot: path.join(root, 'data', 'logs'), browserControlFile: path.join(root, 'data', 'run', 'browser.json'),
  };
  const calls = [];
  let unrefCalled = false;
  const result = await ensurePortableDaemon({
    runtime, resourcesPath: path.join(root, 'resources'), executablePath, platform: 'win32',
    check: async () => false,
    wait: async (_url, options) => { calls.push(options); return true; },
    childSpawn: (exe, args, options) => {
      calls.push({ exe, args, options });
      return { pid: 1234, unref: () => { unrefCalled = true; } };
    },
  });
  assert.equal(result.status, 'started-and-running');
  assert.equal(result.executable, daemonPath);
  assert.deepEqual(result.args.slice(0, 2), ['--prod', '--headless']);
  assert.equal(unrefCalled, true);
  assert.equal(calls[0].exe, daemonPath);
  assert.equal(calls[0].options.cwd, root);
  assert.equal(calls[0].options.windowsHide, true);
	assert.equal(calls[1].expectedVersion, '1.2.3');
});

test('portable Windows bootstrap replaces a stale daemon already on port 80', async (t) => {
	const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-stale-'));
	const daemonPath = path.join(root, 'workass-daemon.exe');
	fs.writeFileSync(daemonPath, 'daemon');
	fs.writeFileSync(path.join(root, 'manifest.json'), JSON.stringify({ schemaVersion: 2, platform: 'windows', version: '2.0.0' }));
	const runtime = {
		profile: 'prod', daemonURL: 'https://127.0.0.1:80', daemonPort: 80, daemonBind: 'lan',
		dataRoot: path.join(root, 'data'), stateDir: path.join(root, 'data', 'state'),
		logRoot: path.join(root, 'data', 'logs'), browserControlFile: path.join(root, 'data', 'run', 'browser.json'),
	};
	let running = true;
	const calls = [];
	const result = await ensurePortableDaemon({
		runtime, resourcesPath: path.join(root, 'resources'), executablePath: path.join(root, 'Workass.exe'), platform: 'win32',
		check: async (url, _timeout, expectedVersion) => url === runtime.daemonURL && running && !expectedVersion,
		shutdown: async (url) => { calls.push(['shutdown', url]); running = false; return true; },
		waitForDown: async (t) => { calls.push(['down']); return true; },
		wait: async (_url, options) => { calls.push(['wait', options.expectedVersion]); return true; },
		childSpawn: () => { calls.push(['start']); return { pid: 81, unref() {} }; },
	});
	assert.equal(result.status, 'started-and-running');
	assert.deepEqual(calls, [
		['shutdown', runtime.daemonURL],
		['down'],
		['start'],
		['wait', '2.0.0'],
	]);
});

test('recovery stops and starts the same sibling daemon without mutating state', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-recovery-'));
  const daemonPath = path.join(root, 'workass-daemon.exe');
  fs.writeFileSync(daemonPath, 'daemon');
  const runtime = { profile: 'prod', daemonURL: 'http://127.0.0.1:8788', daemonPort: 8788, daemonBind: 'localhost', dataRoot: root, stateDir: path.join(root, 'state'), logRoot: path.join(root, 'logs'), browserControlFile: path.join(root, 'run', 'browser.json') };
  const calls = [];
  let checks = 0;
  const identities = ['i-before', 'i-after'];
  const result = await restartDaemonAndRecover({
    runtime, resourcesPath: path.join(root, 'resources'), executablePath: path.join(root, 'Workass.exe'), platform: 'win32', daemonExecutable: daemonPath,
    check: async (t) => { checks += 1; return checks === 1; },
    readInstanceId: async () => identities.shift() || '',
    shutdown: async (t) => { calls.push('shutdown'); return true; },
    waitForDown: async (t) => { calls.push('down'); return true; },
    wait: async () => true,
    childSpawn: (exe, args) => { calls.push({ start: [exe, ...args] }); return { pid: 7, unref() {} }; },
  });
  assert.equal(result.shutdownAccepted, true);
	assert.equal(result.stoppedObserved, true);
  assert.equal(result.oldInstanceId, 'i-before');
  assert.equal(result.newInstanceId, 'i-after');
  assert.deepEqual(calls.slice(0, 2), ['shutdown', 'down']);
  assert.equal(calls[2].start[0], daemonPath);
});

test('recovery rejects a healthy endpoint that still reports the old daemon instance', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-unchanged-'));
  const daemonPath = path.join(root, 'workass-daemon.exe');
  fs.writeFileSync(daemonPath, 'daemon');
  const runtime = { profile: 'prod', daemonURL: 'http://127.0.0.1:8788', daemonPort: 8788, daemonBind: 'localhost', dataRoot: root, stateDir: path.join(root, 'state'), logRoot: path.join(root, 'logs'), browserControlFile: path.join(root, 'run', 'browser.json') };
  let starts = 0;
  await assert.rejects(restartDaemonAndRecover({
    runtime, resourcesPath: path.join(root, 'resources'), platform: 'win32', daemonExecutable: daemonPath,
    check: async () => true,
    readInstanceId: async () => 'i-unchanged',
    shutdown: async () => true,
    waitForDown: async () => false,
    childSpawn: () => { starts += 1; return { pid: 13, unref() {} }; },
  }), /daemon restart was not confirmed/);
  assert.equal(starts, 0, 'a healthy unchanged instance is detected without starting a duplicate');
});

test('recovery accepts a newly started instance when the exact profile was initially down', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-down-'));
  const daemonPath = path.join(root, 'workass-daemon.exe');
  fs.writeFileSync(daemonPath, 'daemon');
  const runtime = { profile: 'dev', daemonURL: 'http://127.0.0.1:18788', daemonPort: 18788, daemonBind: 'localhost', dataRoot: root, stateDir: path.join(root, 'state'), logRoot: path.join(root, 'logs'), browserControlFile: path.join(root, 'run', 'browser.json') };
  const calls = [];
  let checks = 0;
  const result = await restartDaemonAndRecover({
    runtime, resourcesPath: path.join(root, 'resources'), platform: 'win32', daemonExecutable: daemonPath,
    check: async (t) => { checks += 1; return checks > 2; },
    readInstanceId: async () => 'i-started',
    wait: async () => true,
    childSpawn: (executable, args, options) => { calls.push({ executable, args, options }); return { pid: 15, unref() {} }; },
  });
  assert.equal(result.oldInstanceId, null);
  assert.equal(result.newInstanceId, 'i-started');
  assert.equal(calls[0].executable, daemonPath);
  assert.ok(calls[0].args.includes('--state-dir'));
  assert.equal(calls[0].options.env.WORKASS_PROFILE, 'dev');
});

test('recovery accepts an immediate supervisor replacement without observing downtime', async (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-portable-replaced-'));
  const daemonPath = path.join(root, 'workass-daemon.exe');
  fs.writeFileSync(daemonPath, 'daemon');
  const runtime = { profile: 'prod', daemonURL: 'http://127.0.0.1:8788', daemonPort: 8788, daemonBind: 'localhost', dataRoot: root, stateDir: path.join(root, 'state'), logRoot: path.join(root, 'logs'), browserControlFile: path.join(root, 'run', 'browser.json') };
  const ids = ['i-old', 'i-new'];
  let starts = 0;
  const result = await restartDaemonAndRecover({
    runtime, resourcesPath: path.join(root, 'resources'), platform: 'win32', daemonExecutable: daemonPath,
    check: async () => true,
    readInstanceId: async () => ids.shift() || 'i-new',
    shutdown: async () => true,
    waitForDown: async () => false,
    childSpawn: () => { starts += 1; return { pid: 14, unref() {} }; },
  });
  assert.equal(result.newInstanceId, 'i-new');
  assert.equal(result.stoppedObserved, false);
  assert.equal(starts, 0, 'the existing supervisor replacement is reused');
});

test('packaged macOS recovery also requires a changed daemon instance', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  const ids = ['i-old', 'i-new'];
  const starts = [];
  const result = await restartPackagedDaemonAndRecover({
    runtime, resourcesPath, home, uid: 501,
    check: async () => true,
    readInstanceId: async () => ids.shift() || '',
    shutdown: async () => true,
    waitForDown: async () => true,
    launchctlSpawn: () => { throw new Error('must not register a service'); },
    childSpawn: (bin, args) => { starts.push([bin, args]); return { pid: 45, unref() {} }; },
    wait: async () => true,
  });
  assert.equal(result.oldInstanceId, 'i-old');
  assert.equal(result.newInstanceId, 'i-new');
  assert.equal(starts.length, 1);
  assert.equal(starts[0][0], path.join(resourcesPath, 'runtime', 'workass'));
});

test('Mac recovery cannot start a competing server before the previous one stops', async (t) => {
  const { runtime, resourcesPath, home } = fixture(t);
  await assert.rejects(restartPackagedDaemonAndRecover({
    runtime, resourcesPath, home,
    check: async () => true,
    readInstanceId: async () => 'i-old',
    shutdown: async () => true,
    waitForDown: async () => false,
    childSpawn: () => { throw new Error('must not start a competing server'); },
  }), /did not stop for recovery/);
});
