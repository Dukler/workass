'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const {
  UpdateManager,
  compareVersions,
  copyLocalArtifact,
  defaultFeedURL,
  fetchReleaseManifest,
  localFeedPath,
  parseVersion,
  resolveArtifactSource,
  resolveUpdateFeed,
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

test('macOS dogfood resolves one stable local feed while public builds stay on GitHub', () => {
  const dataRoot = '/Users/test/Library/Application Support/Workass';
  assert.equal(localFeedPath(dataRoot, 'darwin', 'arm64'), path.join(dataRoot, 'update-feed', 'workass-darwin-arm64-release.json'));
  assert.equal(resolveUpdateFeed({ channel: 'local', dataRoot, platform: 'darwin', arch: 'arm64' }), localFeedPath(dataRoot, 'darwin', 'arm64'));
  assert.equal(resolveUpdateFeed({ channel: 'github', dataRoot, platform: 'darwin', arch: 'arm64' }), defaultFeedURL('darwin', 'arm64'));
  assert.throws(() => resolveUpdateFeed({ channel: 'local', dataRoot: 'C:\\Workass', platform: 'win32', arch: 'x64' }), /only on macOS/);
});

test('local feed reads and copies are bounded by the same manifest checksum', async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-local-feed-'));
  const feed = path.join(root, 'workass-darwin-arm64-release.json');
  const artifactName = 'Workass-1.2.0-darwin-arm64.zip';
  const source = path.join(root, artifactName);
  const bytes = Buffer.from('local-update-fixture');
  const sha256 = require('node:crypto').createHash('sha256').update(bytes).digest('hex');
  const localManifest = manifest({ artifacts: { update: { name: artifactName, url: artifactName, sha256, size: bytes.length } } });
  fs.writeFileSync(feed, `${JSON.stringify(localManifest)}\n`);
  fs.writeFileSync(source, bytes);
  assert.equal((await fetchReleaseManifest(feed)).version, '1.2.0');
  assert.equal(resolveArtifactSource(artifactName, feed), source);
  assert.throws(() => resolveArtifactSource('../outside.zip', feed), /name is invalid/);
  const destination = path.join(root, 'transaction', 'release.zip');
  let progress = 0;
  await copyLocalArtifact(source, destination, localManifest.artifacts.update, { onProgress: (received) => { progress = received; } });
  assert.deepEqual(fs.readFileSync(destination), bytes);
  assert.equal(progress, bytes.length);
  await assert.rejects(() => copyLocalArtifact(source, path.join(root, 'bad', 'release.zip'), { ...localManifest.artifacts.update, sha256: '0'.repeat(64) }), /checksum/);
});

test('the manager checks and stages a local Mac release without any network request', async () => {
  const { manager } = managerFixture();
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-local-manager-'));
  const feed = path.join(root, 'workass-darwin-arm64-release.json');
  const artifactName = 'Workass-1.2.0-darwin-arm64.zip';
  const bytes = Buffer.from('local-manager-archive');
  const sha256 = require('node:crypto').createHash('sha256').update(bytes).digest('hex');
  const localManifest = manifest({ artifacts: { update: { name: artifactName, url: artifactName, sha256, size: bytes.length } } });
  fs.writeFileSync(feed, `${JSON.stringify(localManifest)}\n`);
  fs.writeFileSync(path.join(root, artifactName), bytes);
  manager.feedURL = feed;
  manager.deps.extractArchive = (_archive, destination) => fs.mkdirSync(path.join(destination, 'Workass.app'), { recursive: true });
  manager.deps.stageRelease = (_source, target) => fs.mkdirSync(target, { recursive: true });
  manager.deps.verifyRelease = () => localManifest.designatedRequirement;
  assert.equal((await manager.check()).phase, 'available');
  const state = await manager.download();
  assert.equal(state.phase, 'ready');
  assert.equal(manager.prepared.designatedRequirement, localManifest.designatedRequirement);
  assert.deepEqual(fs.readFileSync(path.join(manager.prepared.transactionRoot, 'release.zip')), bytes);
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

test('an empty local feed means current while malformed local metadata stays visible', async () => {
  const empty = managerFixture();
  empty.manager.feedURL = path.join(os.tmpdir(), `missing-workass-feed-${Date.now()}`, 'workass-darwin-arm64-release.json');
  assert.equal((await empty.manager.check()).phase, 'current');
  const broken = managerFixture();
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'broken-workass-feed-'));
  broken.manager.feedURL = path.join(root, 'workass-darwin-arm64-release.json');
  fs.writeFileSync(broken.manager.feedURL, '{');
  const state = await broken.manager.check();
  assert.equal(state.phase, 'failed');
  assert.match(state.error, /valid JSON/);
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

test('automatic checks discover a newly published local release without restarting Electron', async () => {
  const { manager } = managerFixture();
  const scheduled = [];
  const repeated = [];
  const cancelled = [];
  const releases = [manifest({ version: '1.1.0' }), manifest({ version: '1.2.0' })];
  let fetches = 0;
  manager.deps.fetchManifest = async () => { fetches += 1; return releases.shift(); };
  manager.deps.schedule = (fn, delay) => { const handle = { fn, delay, unref() {} }; scheduled.push(handle); return handle; };
  manager.deps.repeat = (fn, delay) => { const handle = { fn, delay, unref() {} }; repeated.push(handle); return handle; };
  manager.deps.cancelSchedule = (handle) => cancelled.push(handle);
  manager.deps.cancelRepeat = (handle) => cancelled.push(handle);
  manager.publish({ phase: 'current', targetVersion: null, error: null });

  manager.startAutoChecks({ initialDelayMs: 15, intervalMs: 30 });
  assert.equal(scheduled[0].delay, 15);
  assert.equal(repeated[0].delay, 30);
  assert.equal((await scheduled[0].fn()).phase, 'current');
  assert.equal((await repeated[0].fn()).phase, 'available');
  assert.equal(fetches, 2);
  await repeated[0].fn();
  assert.equal(fetches, 2, 'an offered release was replaced by a background poll');

  manager.dispose();
  assert.deepEqual(cancelled, [scheduled[0], repeated[0]]);
});
