'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  UpdateManager,
  compareVersions,
  defaultFeedURL,
  parseVersion,
  validateReleaseManifest,
} = require('./update-manager');

function manifest(overrides = {}) {
  return {
    schemaVersion: 1,
    product: 'Workass',
    bundleId: 'com.workass.app',
    version: '1.2.0',
    build: 12,
    platform: 'darwin',
    arch: 'arm64',
    designatedRequirement: 'identifier "com.workass.app" and certificate root = H"abc"',
    artifacts: {
      update: {
        name: 'Workass-1.2.0-darwin-arm64.zip',
        url: 'https://releases.example.test/Workass-1.2.0-darwin-arm64.zip',
        sha256: 'a'.repeat(64),
        size: 1024,
      },
    },
    ...overrides,
  };
}

function managerFixture({ replies = [], platform = 'darwin' } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-update-manager-'));
  const resourcesPath = platform === 'darwin'
    ? path.join(root, 'Applications', 'Workass.app', 'Contents', 'Resources')
    : path.join(root, 'Workass', 'resources');
  const executablePath = platform === 'darwin'
    ? path.join(root, 'Applications', 'Workass.app', 'Contents', 'MacOS', 'Workass')
    : path.join(root, 'Workass', 'Workass.exe');
  fs.mkdirSync(resourcesPath, { recursive: true });
  fs.mkdirSync(path.dirname(executablePath), { recursive: true });
  fs.writeFileSync(executablePath, 'app');
  const arch = platform === 'darwin' ? 'arm64' : 'x64';
  const nodePath = platform === 'darwin'
    ? path.join(resourcesPath, 'runtime', 'node', 'darwin-arm64', 'bin', 'node')
    : path.join(path.dirname(executablePath), 'node', 'windows-amd64', 'node.exe');
  fs.mkdirSync(path.dirname(nodePath), { recursive: true });
  if (platform === 'darwin') fs.symlinkSync(process.execPath, nodePath);
  else { fs.copyFileSync(process.execPath, nodePath); fs.chmodSync(nodePath, 0o700); }
  const calls = [];
  let quit = false;
  const manager = new UpdateManager({
    app: { getVersion: () => '1.1.0' },
    runtime: {
      dataRoot: path.join(root, 'data'), daemonURL: 'https://127.0.0.1:18788',
      viewPort: 8799, launchdLabel: 'com.workass.daemon.test',
    },
    resourcesPath,
    executablePath,
    platform,
    arch,
    isPackaged: true,
    quit: () => { quit = true; },
    deps: {
      postLocalUpdate: async (_url, action) => {
        calls.push(action);
        return replies.shift() || { status: 500, body: {} };
      },
      spawn: (_exe, args) => { calls.push(`spawn:${path.basename(args[0])}`); return { unref() {}, kill() {} }; },
      schedule: (fn) => { fn(); return { unref() {} }; },
    },
  });
  manager.init();
  manager.manifest = manifest();
  const updateId = 'upd-fixture-1234';
  const transactionRoot = path.join(root, 'data', 'updates', 'transactions', updateId);
  const installTarget = platform === 'darwin' ? path.resolve(resourcesPath, '..', '..') : path.dirname(executablePath);
  manager.prepared = {
    updateId,
    transactionRoot,
    installTarget,
    incomingTarget: path.join(path.dirname(installTarget), '.incoming-update'),
    backupTarget: path.join(path.dirname(installTarget), '.previous-update'),
    designatedRequirement: platform === 'darwin' ? manifest().designatedRequirement : '',
  };
  manager.publish({ phase: 'ready', targetVersion: '1.2.0' });
  return { manager, calls, didQuit: () => quit };
}

test('release versions are strict and monotonic', () => {
  assert.deepEqual(parseVersion('1.2.3'), [1, 2, 3]);
  assert.equal(parseVersion('01.2.3'), null);
  assert.equal(compareVersions('1.2.3', '1.3.0'), -1);
  assert.equal(compareVersions('2.0.0', '1.9.9'), 1);
  assert.equal(compareVersions('1.2.3', '1.2.3'), 0);
});

test('platform feed names cannot collide in one GitHub release', () => {
  assert.equal(defaultFeedURL('darwin', 'arm64'), 'https://github.com/Dukler/workass/releases/latest/download/workass-darwin-arm64-release.json');
  assert.equal(defaultFeedURL('win32', 'x64'), 'https://github.com/Dukler/workass/releases/latest/download/workass-windows-amd64-release.json');
});

test('manifest validation requires the exact platform, checksum, size, and signing law', () => {
  assert.equal(validateReleaseManifest(manifest(), { platform: 'darwin', arch: 'arm64' }).version, '1.2.0');
  assert.throws(() => validateReleaseManifest(manifest({ arch: 'amd64' }), { platform: 'darwin', arch: 'arm64' }), /another platform/);
  assert.throws(() => validateReleaseManifest(manifest({ designatedRequirement: '' }), { platform: 'darwin', arch: 'arm64' }), /signing requirement/);
  const windows = manifest({ platform: 'windows', arch: 'amd64', designatedRequirement: undefined });
  assert.throws(() => validateReleaseManifest(windows, { platform: 'win32', arch: 'x64' }), /Authenticode/);
  assert.equal(validateReleaseManifest({ ...windows, authenticode: true }, { platform: 'win32', arch: 'x64' }).platform, 'windows');
});

test('busy daemon leaves the verified release staged and never starts the worker', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [{ status: 409, body: { ready: false, foregroundTurns: 1 } }] });
  const state = await manager.install();
  assert.equal(state.phase, 'busy');
  assert.equal(state.blockers.foregroundTurns, 1);
  assert.deepEqual(calls, ['prepare']);
  assert.equal(didQuit(), false);
});

test('committed handoff arms one worker, then quits only after daemon commit', async () => {
  const { manager, calls, didQuit } = managerFixture({ replies: [
    { status: 200, body: { ready: true } },
    { status: 202, body: { stopping: true } },
  ] });
  const state = await manager.install();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(calls, ['prepare', 'spawn:update-worker.js', 'commit']);
  assert.equal(didQuit(), true);
});

test('a missing platform asset means current while real feed failures stay visible', async () => {
  const missing = managerFixture();
  missing.manager.deps.fetchManifest = async () => { throw Object.assign(new Error('HTTP 404'), { statusCode: 404 }); };
  assert.equal((await missing.manager.check()).phase, 'current');
  const broken = managerFixture();
  broken.manager.deps.fetchManifest = async () => { throw new Error('TLS failed'); };
  const state = await broken.manager.check();
  assert.equal(state.phase, 'failed');
  assert.match(state.error, /TLS failed/);
});

test('updater operations are serialized so two clicks cannot arm competing transactions', async () => {
  const { manager } = managerFixture();
  let release;
  manager.deps.fetchManifest = () => new Promise((resolve) => { release = resolve; });
  const first = manager.check();
  await assert.rejects(() => manager.check(), /already running: check/);
  release(manifest());
  assert.equal((await first).phase, 'available');
});
