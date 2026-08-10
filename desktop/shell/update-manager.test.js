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
  verifyWindowsRelease,
} = require('./update-manager');

function writeFakeWindowsPE(file) {
  const bytes = Buffer.alloc(256);
  bytes.write('MZ', 0, 'ascii');
  bytes.writeUInt32LE(128, 0x3c);
  bytes.write('PE\0\0', 128, 'ascii');
  bytes.writeUInt16LE(0x8664, 132);
  bytes.writeUInt16LE(0x20b, 152);
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, bytes);
}

function writeWindowsRelease(root, version = '1.2.0') {
  const json = (relative, value) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, `${JSON.stringify(value)}\n`);
  };
  const regular = (relative) => {
    const file = path.join(root, ...relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, 'fixture');
  };
  json(['manifest.json'], { schemaVersion: 2, platform: 'windows', arch: 'amd64', version, portable: true, electron: true });
  json(['resources', 'app', 'package.json'], { version });
  for (const relative of [
    ['resources', 'app', 'update-manager.js'],
    ['resources', 'app', 'update-worker.js'],
    ['resources', 'renderer', 'index.html'],
    ['frontier-hosts', 'windows-amd64', 'claude-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'codex-native-host.mjs'],
    ['frontier-hosts', 'windows-amd64', 'node_modules', '@anthropic-ai', 'claude-agent-sdk', 'sdk.mjs'],
  ]) regular(relative);
  writeFakeWindowsPE(path.join(root, 'Workass.exe'));
  writeFakeWindowsPE(path.join(root, 'workass-daemon.exe'));
  writeFakeWindowsPE(path.join(root, 'node', 'windows-amd64', 'node.exe'));
}

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

test('the Windows publisher marks the unsigned portable feed as updater-compatible', () => {
  const publisher = fs.readFileSync(path.join(__dirname, '..', '..', 'scripts', 'stage-windows-portable.sh'), 'utf8');
  assert.match(publisher, /platform:\s*'windows',[\s\S]{0,160}portable:\s*true,[\s\S]{0,240}authenticode:\s*false/);
  assert.doesNotMatch(publisher, /ineligible for automatic install/);
});

test('the renderer has one owned apply IPC for the complete update intent', () => {
  const preload = fs.readFileSync(path.join(__dirname, 'preload.js'), 'utf8');
  const main = fs.readFileSync(path.join(__dirname, 'main.js'), 'utf8');
  assert.match(preload, /apply:\s*\(\)\s*=>\s*ipcRenderer\.invoke\('workass-updater:apply'\)/);
  assert.match(main, /ipcMain\.handle\('workass-updater:apply',[\s\S]{0,160}own\(event\)[\s\S]{0,160}updateManager\?\.apply\(\)/);
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

test('manifest validation requires the exact platform, checksum, size, and platform trust law', () => {
  assert.equal(validateReleaseManifest(manifest(), { platform: 'darwin', arch: 'arm64' }).version, '1.2.0');
  assert.throws(() => validateReleaseManifest(manifest({ arch: 'amd64' }), { platform: 'darwin', arch: 'arm64' }), /another platform/);
  assert.throws(() => validateReleaseManifest(manifest({ designatedRequirement: '' }), { platform: 'darwin', arch: 'arm64' }), /signing requirement/);
  const windows = manifest({ platform: 'windows', arch: 'amd64', designatedRequirement: undefined });
  assert.throws(() => validateReleaseManifest(windows, { platform: 'win32', arch: 'x64' }), /portable release/);
  assert.equal(validateReleaseManifest({ ...windows, portable: true, authenticode: false }, { platform: 'win32', arch: 'x64' }).platform, 'windows');
});

test('packaged unsigned Windows builds enable the GitHub updater without PowerShell', () => {
  const { manager } = managerFixture({ platform: 'win32', arch: 'x64' });
  assert.equal(manager.snapshot().supported, true);
  const source = fs.readFileSync(path.join(__dirname, 'update-manager.js'), 'utf8');
  assert.doesNotMatch(source, /Get-AuthenticodeSignature|powershell\.exe/);
});

test('unsigned Windows staging verifies the complete portable x86-64 runtime', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-release-'));
  const incoming = path.join(root, 'incoming');
  writeWindowsRelease(incoming);
  assert.doesNotThrow(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64'));

  fs.writeFileSync(path.join(incoming, 'resources', 'app', 'package.json'), '{"version":"9.9.9"}\n');
  assert.throws(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64'), /shell version/);
  fs.writeFileSync(path.join(incoming, 'resources', 'app', 'package.json'), '{"version":"1.2.0"}\n');
  fs.writeFileSync(path.join(incoming, 'workass-daemon.exe'), 'not-a-pe');
  assert.throws(() => verifyWindowsRelease(path.join(root, 'current'), incoming, '1.2.0', 'x64'), /Windows executable|PE32/);
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

test('one apply action stages and commits an available release without a second click', async () => {
  const { manager } = managerFixture();
  const steps = [];
  manager.publish({ phase: 'available', targetVersion: '1.2.0' });
  manager.download = async () => {
    steps.push('download');
    return manager.publish({ phase: 'ready', progress: 1 });
  };
  manager.install = async () => {
    steps.push('install');
    return manager.publish({ phase: 'installing' });
  };

  const state = await manager.apply();
  assert.equal(state.phase, 'installing');
  assert.deepEqual(steps, ['download', 'install']);
});

test('a missing platform asset means current while a feed failure cannot impersonate an attempted update', async () => {
  const missing = managerFixture();
  missing.manager.deps.fetchManifest = async () => { throw Object.assign(new Error('HTTP 404'), { statusCode: 404 }); };
  assert.equal((await missing.manager.check()).phase, 'current');
  const broken = managerFixture();
  broken.manager.deps.fetchManifest = async () => { throw new Error('TLS failed'); };
  const state = await broken.manager.check();
  assert.equal(state.phase, 'check_failed');
  assert.match(state.error, /TLS failed/);
  assert.equal(state.receipt, null);
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
  assert.equal(state.phase, 'check_failed');
  assert.match(state.error, /valid JSON/);
});

test('retrying a failed availability check cannot download or install when the current release is latest', async () => {
  const { manager } = managerFixture();
  const actions = [];
  manager.deps.fetchManifest = async () => manifest({ version: '1.1.0' });
  manager.download = async () => { actions.push('download'); return manager.snapshot(); };
  manager.install = async () => { actions.push('install'); return manager.snapshot(); };
  manager.publish({ phase: 'check_failed', error: 'offline' });

  const state = await manager.apply();
  assert.equal(state.phase, 'current');
  assert.deepEqual(actions, []);
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

test('an automatic availability-check failure remains retryable without becoming an update transaction', async () => {
  const { manager } = managerFixture();
  const scheduled = [];
  const repeated = [];
  let fetches = 0;
  manager.deps.fetchManifest = async () => {
    fetches += 1;
    if (fetches === 1) throw new Error('offline');
    return manifest({ version: '1.1.0' });
  };
  manager.deps.schedule = (fn) => { const handle = { fn, unref() {} }; scheduled.push(handle); return handle; };
  manager.deps.repeat = (fn) => { const handle = { fn, unref() {} }; repeated.push(handle); return handle; };
  manager.publish({ phase: 'idle', targetVersion: null, error: null, receipt: null });

  manager.startAutoChecks({ initialDelayMs: 15, intervalMs: 30 });
  const failedCheck = await scheduled[0].fn();
  assert.equal(failedCheck.phase, 'check_failed');
  assert.equal(failedCheck.receipt, null);
  assert.equal((await repeated[0].fn()).phase, 'current');
  assert.equal(fetches, 2);
  manager.dispose();
});
