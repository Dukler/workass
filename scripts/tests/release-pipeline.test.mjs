import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const releaseRoot = path.join(repoRoot, 'scripts', 'release');
const inputTool = path.join(releaseRoot, 'lib', 'release-input.mjs');
const candidateTool = path.join(releaseRoot, 'lib', 'verify-candidate.mjs');
const publicationTool = path.join(releaseRoot, 'lib', 'verify-publication.mjs');
const nextVersionTool = path.join(releaseRoot, 'lib', 'next-version.mjs');
const gateReceiptTool = path.join(releaseRoot, 'lib', 'repository-gate.mjs');
const commit = '1'.repeat(40);

function run(command, args, options = {}) {
  return spawnSync(command, args, { encoding: 'utf8', ...options });
}

function write(file, contents = file) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, contents);
}

function sha256(contents) {
  return crypto.createHash('sha256').update(contents).digest('hex');
}

function fileArtifact(file) {
  const bytes = fs.readFileSync(file);
  return { name: path.basename(file), sha256: sha256(bytes), size: bytes.length };
}

function makeReleaseInput(root, version = '1.2.3') {
  for (const relative of [
    'renderer/index.html',
    'macos/runtime/workass',
    'macos/electron/darwin-arm64/Electron.app/Contents/MacOS/Electron',
    'macos/runtime/node/darwin-arm64/bin/node',
    'macos/runtime/frontier-hosts/darwin-arm64/claude-native-host.mjs',
    'windows/runtime/workass-daemon.exe',
    'windows/electron/win32-x64/electron.exe',
    'windows/runtime/node/windows-amd64/node.exe',
    'windows/runtime/frontier-hosts/windows-amd64/codex-native-host.mjs',
  ]) write(path.join(root, ...relative.split('/')), relative);
  const created = run(process.execPath, [inputTool, 'create', '--root', root, '--version', version, '--commit', commit]);
  assert.equal(created.status, 0, created.stderr);
}

test('release input is exact-version, exact-commit, and content addressed', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-input-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  makeReleaseInput(root);

  const verified = run(process.execPath, [inputTool, 'verify', '--root', root, '--version', '1.2.3', '--commit', commit]);
  assert.equal(verified.status, 0, verified.stderr);
  assert.match(verified.stdout, /WORKASS_RELEASE_INPUT_VERIFIED/);

  fs.appendFileSync(path.join(root, 'renderer', 'index.html'), 'changed');
  const changed = run(process.execPath, [inputTool, 'verify', '--root', root, '--version', '1.2.3', '--commit', commit]);
  assert.notEqual(changed.status, 0);
  assert.match(changed.stderr, /content hashes/);

  const wrongVersion = run(process.execPath, [inputTool, 'verify', '--root', root, '--version', '1.2.4', '--commit', commit]);
  assert.notEqual(wrongVersion.status, 0);
});

test('timing helper keeps full phase output in logs and preserves a failing command status', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-timing-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const timingFile = path.join(root, 'timings.log');
  const phaseLogs = path.join(root, 'phases');
  const helper = path.join(releaseRoot, 'lib', 'timing.sh');
  const shell = `
    . "$1"
    WORKASS_RELEASE_TIMING_FILE="$2"
    WORKASS_RELEASE_PHASE_LOG_DIR="$3"
    export WORKASS_RELEASE_TIMING_FILE WORKASS_RELEASE_PHASE_LOG_DIR
    pass_phase() { printf 'complete passing output\\n'; }
    fail_phase() { line=1; while [ "$line" -le 60 ]; do printf 'failing output %03d\\n' "$line"; line=$((line + 1)); done; return 7; }
    workass_release_run_phase contract_pass pass_phase
    if workass_release_run_phase contract_fail fail_phase; then exit 9; else [ "$?" -eq 7 ]; fi
  `;
  const result = run('sh', ['-c', shell, 'release-timing-test', helper, timingFile, phaseLogs]);
  assert.equal(result.status, 0, result.stderr);
  const timing = fs.readFileSync(timingFile, 'utf8');
  assert.match(timing, /name=contract_pass status=passed seconds=\d+/);
  assert.match(timing, /name=contract_fail status=failed seconds=\d+/);
  assert.equal(fs.readFileSync(path.join(phaseLogs, 'contract_pass.log'), 'utf8'), 'complete passing output\n');
  const failureLog = fs.readFileSync(path.join(phaseLogs, 'contract_fail.log'), 'utf8');
  assert.match(failureLog, /failing output 001/);
  assert.match(failureLog, /failing output 060/);
  assert.doesNotMatch(result.stdout, /complete passing output|failing output/);
  assert.doesNotMatch(result.stderr, /failing output 001/);
  assert.match(result.stderr, /failing output 060/);
});

test('parallel phase helper waits for both isolated phases and propagates failure', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-parallel-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const helper = path.join(releaseRoot, 'lib', 'timing.sh');
  const shell = `
    . "$1"
    WORKASS_RELEASE_PHASE_LOG_DIR="$2"
    export WORKASS_RELEASE_PHASE_LOG_DIR
    marker_root="$3"
    left_phase() { sleep 0.1; printf left > "$marker_root/left.done"; }
    right_phase() { sleep 0.1; printf right > "$marker_root/right.done"; return 5; }
    if workass_release_run_parallel_pair left left_phase right right_phase; then exit 9; else [ "$?" -eq 5 ]; fi
  `;
  const result = run('sh', ['-c', shell, 'release-parallel-test', helper, path.join(root, 'phases'), root]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.readFileSync(path.join(root, 'left.done'), 'utf8'), 'left');
  assert.equal(fs.readFileSync(path.join(root, 'right.done'), 'utf8'), 'right');
  assert.equal(fs.readFileSync(path.join(root, 'phases', 'left.log'), 'utf8'), '');
  assert.equal(fs.readFileSync(path.join(root, 'phases', 'right.log'), 'utf8'), '');
});

test('repository gate receipt binds the exact commit, gate, OS, and toolchain', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-gate-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const repo = path.join(root, 'repo');
  const receipt = path.join(root, 'receipt.json');
  const gate = path.join(repo, 'scripts', 'gate.sh');
  write(gate, '#!/bin/sh\necho first\n');

  const record = () => run(process.execPath, [gateReceiptTool, 'record', '--repo', repo,
    '--receipt', receipt, '--commit', commit]);
  const verify = (candidateCommit = commit) => run(process.execPath, [gateReceiptTool, 'verify', '--repo', repo,
    '--receipt', receipt, '--commit', candidateCommit]);

  assert.equal(record().status, 0);
  assert.equal(verify().status, 0);
  assert.notEqual(verify('2'.repeat(40)).status, 0);

  write(gate, '#!/bin/sh\necho changed\n');
  assert.notEqual(verify().status, 0);
  assert.equal(record().status, 0);
  assert.equal(verify().status, 0);

  write(receipt, '{broken');
  assert.notEqual(verify().status, 0);
  assert.equal(record().status, 0);
  assert.equal(verify().status, 0);
});

test('release renderer mismatch fails before the slow Go gate', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-renderer-gate-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const gate = path.join(root, 'scripts', 'gate.sh');
  const bin = path.join(root, 'bin');
  const goMarker = path.join(root, 'go-was-called');
  fs.mkdirSync(path.join(root, 'desktop', 'renderer2', 'node_modules'), { recursive: true });
  write(path.join(root, 'desktop', 'renderer2', 'dist', 'index.html'), 'fresh renderer');
  write(path.join(root, 'cmd', 'workass', 'embedded', 'dist', 'index.html'), 'stale renderer');
  fs.mkdirSync(path.dirname(gate), { recursive: true });
  const gateSource = fs.readFileSync(path.join(repoRoot, 'scripts', 'gate.sh'), 'utf8')
    .replace('export PATH="/opt/homebrew/bin:$PATH"', 'export PATH="$PATH"');
  write(gate, gateSource);
  fs.chmodSync(gate, 0o755);
  for (const name of ['npx', 'npm']) {
    const command = path.join(bin, name);
    write(command, '#!/bin/sh\nexit 0\n');
    fs.chmodSync(command, 0o755);
  }
  const go = path.join(bin, 'go');
  write(go, `#!/bin/sh\ntouch "${goMarker}"\nexit 0\n`);
  fs.chmodSync(go, 0o755);

  const result = run('sh', [gate], {
    cwd: root,
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}`, WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER: '1' },
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /renderer build differs from committed embedded output/);
  assert.equal(fs.existsSync(goMarker), false);
});

test('source-state gate rejects dirty or unpublished main', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-source-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const remote = path.join(root, 'remote.git');
  const repo = path.join(root, 'repo');
  assert.equal(run('git', ['init', '--bare', remote]).status, 0);
  assert.equal(run('git', ['init', '-b', 'main', repo]).status, 0);
  write(path.join(repo, 'tracked.txt'), 'one\n');
  for (const args of [
    ['-C', repo, 'config', 'user.name', 'Workass Test'],
    ['-C', repo, 'config', 'user.email', 'workass-test@example.invalid'],
    ['-C', repo, 'add', 'tracked.txt'],
    ['-C', repo, 'commit', '-m', 'initial'],
    ['-C', repo, 'remote', 'add', 'origin', remote],
    ['-C', repo, 'push', '-u', 'origin', 'main'],
  ]) assert.equal(run('git', args).status, 0);

  const helper = path.join(releaseRoot, 'lib', 'source-state.sh');
  const check = () => run('sh', ['-c', '. "$1"; workass_release_require_source "$2"', 'source-state-test', helper, repo]);
  assert.equal(check().status, 0);

  fs.appendFileSync(path.join(repo, 'tracked.txt'), 'dirty\n');
  assert.notEqual(check().status, 0);
  assert.equal(run('git', ['-C', repo, 'add', 'tracked.txt']).status, 0);
  assert.equal(run('git', ['-C', repo, 'commit', '-m', 'unpushed']).status, 0);
  assert.notEqual(check().status, 0);
});

test('default release build number is stable for the exact commit', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-build-number-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const repo = path.join(root, 'repo');
  assert.equal(run('git', ['init', '-b', 'main', repo]).status, 0);
  write(path.join(repo, 'tracked.txt'), 'one\n');
  for (const args of [
    ['-C', repo, 'config', 'user.name', 'Workass Test'],
    ['-C', repo, 'config', 'user.email', 'workass-test@example.invalid'],
    ['-C', repo, 'add', 'tracked.txt'],
  ]) assert.equal(run('git', args).status, 0);
  const committed = run('git', ['-C', repo, 'commit', '-m', 'initial'], {
    env: { ...process.env, GIT_AUTHOR_DATE: '2026-08-23T12:34:56Z', GIT_COMMITTER_DATE: '2026-08-23T12:34:56Z' },
  });
  assert.equal(committed.status, 0, committed.stderr);
  const revision = run('git', ['-C', repo, 'rev-parse', 'HEAD']).stdout.trim();
  const helper = path.join(releaseRoot, 'lib', 'source-state.sh');
  const readBuild = () => run('sh', ['-c', '. "$1"; workass_release_build_number "$2" "$3"',
    'release-build-number-test', helper, repo, revision]);
  assert.equal(readBuild().stdout.trim(), '20260823123456');
  assert.equal(readBuild().stdout.trim(), '20260823123456');
});

test('one release version resolver selects the next patch after every published surface', () => {
  const resolved = run(process.execPath, [nextVersionTool, '0.1.92', '0.1.93', '0.1.91']);
  assert.equal(resolved.status, 0, resolved.stderr);
  assert.equal(resolved.stdout.trim(), '0.1.94');
  assert.notEqual(run(process.execPath, [nextVersionTool, '0.1.093']).status, 0);
});

test('candidate receipt binds both archives, both feeds, input, version, and commit', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-candidate-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const input = path.join(root, 'input');
  const candidate = path.join(root, 'candidate');
  const version = '1.2.3';
  const build = '20260814010101';
  write(path.join(input, 'manifest.json'), '{}\n');

  const macArchiveName = `Workass-${version}-darwin-arm64.zip`;
  const macArchiveBytes = Buffer.from('mac archive');
  const macFeedRoot = path.join(candidate, 'macos-feed');
  write(path.join(macFeedRoot, macArchiveName), macArchiveBytes);
  const macFeed = {
    schemaVersion: 1,
    product: 'Workass',
    version,
    build: Number(build),
    platform: 'darwin',
    arch: 'arm64',
    designatedRequirement: 'identifier "com.workass.app"',
    artifacts: { update: { name: macArchiveName, url: macArchiveName, sha256: sha256(macArchiveBytes), size: macArchiveBytes.length } },
  };
  write(path.join(macFeedRoot, 'workass-darwin-arm64-release.json'), `${JSON.stringify(macFeed)}\n`);
  write(path.join(macFeedRoot, 'SHA256SUMS'), `${sha256(macArchiveBytes)}  ${macArchiveName}\n`);
  write(path.join(candidate, 'macos-app', 'Workass.app', 'Contents', 'Resources', 'app', 'package.json'), JSON.stringify({ version }));
  write(path.join(candidate, 'macos-app', 'Workass.app', 'Contents', 'Resources', 'runtime', 'manifest.json'),
    JSON.stringify({ version, build, platform: 'darwin', arch: 'arm64' }));

  const windowsBundleName = `Workass-${version}-windows-amd64`;
  const windowsRoot = path.join(candidate, 'windows', version);
  const windowsArchiveBytes = Buffer.from('windows archive');
  write(path.join(windowsRoot, `${windowsBundleName}.zip`), windowsArchiveBytes);
  const windowsFeed = {
    schemaVersion: 1,
    product: 'Workass',
    version,
    platform: 'windows',
    arch: 'amd64',
    portable: true,
    authenticode: false,
    artifacts: { update: {
      name: `${windowsBundleName}.zip`,
      url: `https://github.com/Dukler/workass/releases/download/v${version}/${windowsBundleName}.zip`,
      sha256: sha256(windowsArchiveBytes),
      size: windowsArchiveBytes.length,
    } },
  };
  write(path.join(windowsRoot, 'workass-windows-amd64-release.json'), `${JSON.stringify(windowsFeed)}\n`);
  write(path.join(windowsRoot, 'SHA256SUMS'), `${sha256(windowsArchiveBytes)}  ${windowsBundleName}.zip\n`);
  const windowsBundle = path.join(windowsRoot, windowsBundleName);
  write(path.join(windowsBundle, 'resources', 'app', 'package.json'), JSON.stringify({ version }));
  write(path.join(windowsBundle, 'manifest.json'), JSON.stringify({ version, platform: 'windows', arch: 'amd64', revision: commit }));
  for (const relative of [
    'Workass.exe',
    'workass-daemon.exe',
    'resources/renderer/index.html',
    'node/windows-amd64/node.exe',
    'frontier-hosts/windows-amd64/claude-native-host.mjs',
    'frontier-hosts/windows-amd64/codex-native-host.mjs',
    'frontier-hosts/windows-amd64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
  ]) write(path.join(windowsBundle, ...relative.split('/')), relative);

  const recorded = run(process.execPath, [candidateTool, 'record', '--root', candidate, '--input', input,
    '--version', version, '--build', build, '--commit', commit]);
  assert.equal(recorded.status, 0, recorded.stderr);
  const receipt = JSON.parse(fs.readFileSync(path.join(candidate, 'receipt.json'), 'utf8'));
  assert.equal(receipt.commit, commit);
  assert.deepEqual(Object.keys(receipt.artifacts).sort(), ['macos', 'macosFeed', 'windows', 'windowsFeed']);

  fs.appendFileSync(path.join(windowsRoot, 'workass-windows-amd64-release.json'), ' ');
  const rerecorded = run(process.execPath, [candidateTool, 'record', '--root', candidate, '--input', input,
    '--version', version, '--build', build, '--commit', commit]);
  assert.notEqual(rerecorded.status, 0);
});

test('paired publication receipt binds exact Mac bytes and verified GitHub assets', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-publication-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const candidate = path.join(root, 'candidate');
  const macosOutput = path.join(root, 'published-macos');
  const version = '1.2.3';
  const build = '20260823123456';
  const archiveName = `Workass-${version}-darwin-arm64.zip`;
  const macArchive = Buffer.from('mac archive');
  const macFeed = {
    schemaVersion: 1,
    product: 'Workass',
    version,
    build: Number(build),
    platform: 'darwin',
    arch: 'arm64',
    designatedRequirement: 'identifier "com.workass.app"',
    artifacts: { update: { name: archiveName, url: archiveName, sha256: sha256(macArchive), size: macArchive.length } },
  };
  for (const target of [path.join(candidate, 'macos-feed'), macosOutput]) {
    write(path.join(target, archiveName), macArchive);
    write(path.join(target, 'workass-darwin-arm64-release.json'), `${JSON.stringify(macFeed)}\n`);
    write(path.join(target, 'SHA256SUMS'), `${sha256(macArchive)}  ${archiveName}\n`);
  }

  const windowsRoot = path.join(candidate, 'windows', version);
  const windowsArchiveName = `Workass-${version}-windows-amd64.zip`;
  write(path.join(windowsRoot, windowsArchiveName), 'windows archive');
  write(path.join(windowsRoot, 'workass-windows-amd64-release.json'), 'windows feed');
  write(path.join(windowsRoot, 'SHA256SUMS'), 'windows sums');
  write(path.join(candidate, 'input-manifest.json'), 'input');
  const candidateReceipt = {
    schemaVersion: 1,
    product: 'Workass',
    version,
    build: Number(build),
    commit,
    inputManifest: fileArtifact(path.join(candidate, 'input-manifest.json')),
    artifacts: {
      macos: fileArtifact(path.join(candidate, 'macos-feed', archiveName)),
      macosFeed: fileArtifact(path.join(candidate, 'macos-feed', 'workass-darwin-arm64-release.json')),
      windows: fileArtifact(path.join(windowsRoot, windowsArchiveName)),
      windowsFeed: fileArtifact(path.join(windowsRoot, 'workass-windows-amd64-release.json')),
    },
  };
  write(path.join(candidate, 'receipt.json'), `${JSON.stringify(candidateReceipt)}\n`);
  const windowsPublication = {
    schemaVersion: 1,
    product: 'Workass',
    kind: 'windows-publication',
    status: 'verified',
    repository: 'Dukler/workass',
    version,
    commit,
    tag: `v${version}`,
    releaseUrl: `https://github.com/Dukler/workass/releases/tag/v${version}`,
    latestManifestUrl: 'https://github.com/Dukler/workass/releases/latest/download/workass-windows-amd64-release.json',
    assets: {
      archive: fileArtifact(path.join(windowsRoot, windowsArchiveName)),
      manifest: fileArtifact(path.join(windowsRoot, 'workass-windows-amd64-release.json')),
      checksums: fileArtifact(path.join(windowsRoot, 'SHA256SUMS')),
    },
  };
  write(path.join(candidate, 'windows-publication.json'), `${JSON.stringify(windowsPublication)}\n`);

  const args = ['--root', candidate, '--macos-output', macosOutput, '--version', version, '--build', build, '--commit', commit];
  const recorded = run(process.execPath, [publicationTool, 'record', ...args]);
  assert.equal(recorded.status, 0, recorded.stderr);
  assert.match(recorded.stdout, /WORKASS_PAIRED_PUBLICATION_VERIFIED/);
  assert.equal(run(process.execPath, [publicationTool, 'verify', ...args]).status, 0);

  fs.appendFileSync(path.join(macosOutput, archiveName), 'changed');
  assert.notEqual(run(process.execPath, [publicationTool, 'verify', ...args]).status, 0);
});

test('Windows verify-only mode performs readback without a release mutation', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-readback-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const bin = path.join(root, 'bin');
  const releaseDir = path.join(root, 'release');
  const version = '1.2.3';
  const archiveName = `Workass-${version}-windows-amd64.zip`;
  const manifestName = 'workass-windows-amd64-release.json';
  const archive = Buffer.from('windows archive');
  write(path.join(releaseDir, archiveName), archive);
  write(path.join(releaseDir, manifestName), 'windows manifest');
  write(path.join(releaseDir, 'SHA256SUMS'), `${sha256(archive)}  ${archiveName}\n`);
  const asset = (name) => {
    const local = fileArtifact(path.join(releaseDir, name));
    return { name, size: local.size, digest: `sha256:${local.sha256}` };
  };
  const fullView = path.join(root, 'full-view.json');
  const latestView = path.join(root, 'latest-view.json');
  write(fullView, JSON.stringify({
    tagName: `v${version}`,
    targetCommitish: commit,
    name: `Workass ${version} — Windows portable`,
    isDraft: false,
    isPrerelease: false,
    assets: [asset(archiveName), asset(manifestName), asset('SHA256SUMS')],
    url: `https://github.com/Dukler/workass/releases/tag/v${version}`,
  }));
  write(latestView, JSON.stringify({ tagName: `v${version}`, isDraft: false, isPrerelease: false }));
  const calls = path.join(root, 'gh-calls.log');
  write(path.join(bin, 'gh'), `#!/bin/sh
printf '%s\\n' "$*" >> "$WORKASS_TEST_GH_CALLS"
if [ "$1" = auth ] && [ "$2" = status ]; then exit 0; fi
if [ "$1" = release ] && [ "$2" = view ]; then
  case "$3" in v*) command cat "$WORKASS_TEST_GH_FULL" ;; *) command cat "$WORKASS_TEST_GH_LATEST" ;; esac
  exit 0
fi
exit 97
`);
  write(path.join(bin, 'curl'), `#!/bin/sh
output=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output=$2; shift 2; else shift; fi
done
[ -n "$output" ] || exit 2
command cp "$WORKASS_TEST_MANIFEST" "$output"
`);
  fs.chmodSync(path.join(bin, 'gh'), 0o755);
  fs.chmodSync(path.join(bin, 'curl'), 0o755);
  const receipt = path.join(root, 'windows-publication.json');
  const result = run('sh', [path.join(releaseRoot, 'publish-windows.sh'),
    '--version', version, '--commit', commit, '--release-dir', releaseDir,
    '--verify-only', '--receipt', receipt], {
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      WORKASS_TEST_GH_CALLS: calls,
      WORKASS_TEST_GH_FULL: fullView,
      WORKASS_TEST_GH_LATEST: latestView,
      WORKASS_TEST_MANIFEST: path.join(releaseDir, manifestName),
    },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(fs.readFileSync(receipt, 'utf8')).kind, 'windows-publication');
  const ghCalls = fs.readFileSync(calls, 'utf8');
  assert.match(ghCalls, /release view/);
  assert.doesNotMatch(ghCalls, /release (create|upload)/);
});

test('ship is one quiet command from clean pushed main to one final receipt', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-ship-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const remote = path.join(root, 'remote.git');
  const repo = path.join(root, 'repo');
  const bin = path.join(root, 'bin');
  const feed = path.join(root, 'feed');
  const localRelease = path.join(repo, 'scripts', 'release');
  assert.equal(run('git', ['init', '--bare', remote]).status, 0);
  assert.equal(run('git', ['init', '-b', 'main', repo]).status, 0);
  for (const relative of ['ship.sh', 'lib/source-state.sh', 'lib/next-version.mjs']) {
    const source = path.join(releaseRoot, relative);
    const target = path.join(localRelease, relative);
    write(target, fs.readFileSync(source));
    fs.chmodSync(target, 0o755);
  }
  write(path.join(repo, '.gitignore'), '.dev/\n');
  write(path.join(localRelease, 'stage-updates.sh'), `#!/bin/sh
version=''
while [ "$#" -gt 0 ]; do
  if [ "$1" = --version ]; then version=$2; shift 2; else shift; fi
done
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
commit=$(git -C "$repo_root" rev-parse HEAD)
candidate="$repo_root/.dev/fake-candidate"
mkdir -p "$candidate"
publication="$candidate/publication.json"
printf '{"schemaVersion":1,"product":"Workass","kind":"paired-publication","status":"verified","version":"%s","commit":"%s","windows":{"releaseUrl":"https://github.com/Dukler/workass/releases/tag/v%s"}}\\n' "$version" "$commit" "$version" > "$publication"
line=1
while [ "$line" -le 100 ]; do printf 'verbose internal line %03d\\n' "$line"; line=$((line + 1)); done
printf 'publication=%s\\n' "$publication"
`);
  fs.chmodSync(path.join(localRelease, 'stage-updates.sh'), 0o755);
  write(path.join(feed, 'workass-darwin-arm64-release.json'), JSON.stringify({ version: '0.1.93' }));
  write(path.join(bin, 'gh'), `#!/bin/sh
if [ "$1" = auth ] && [ "$2" = status ]; then exit 0; fi
if [ "$1" = release ] && [ "$2" = view ]; then
  printf '{"tagName":"v0.1.93","isDraft":false,"isPrerelease":false}\\n'
  exit 0
fi
exit 97
`);
  write(path.join(bin, 'plutil'), '#!/bin/sh\nprintf "0.1.92\\n"\n');
  fs.chmodSync(path.join(bin, 'gh'), 0o755);
  fs.chmodSync(path.join(bin, 'plutil'), 0o755);
  for (const args of [
    ['-C', repo, 'config', 'user.name', 'Workass Test'],
    ['-C', repo, 'config', 'user.email', 'workass-test@example.invalid'],
    ['-C', repo, 'add', '.'],
    ['-C', repo, 'commit', '-m', 'release source'],
    ['-C', repo, 'remote', 'add', 'origin', remote],
    ['-C', repo, 'push', '-u', 'origin', 'main'],
  ]) assert.equal(run('git', args).status, 0);

  const result = run('sh', [path.join(localRelease, 'ship.sh'), '--macos-output', feed], {
    cwd: repo,
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /WORKASS_RELEASE_PUBLISHED/);
  assert.match(result.stdout, /version=0\.1\.94/);
  assert.match(result.stdout, /activation=not-requested/);
  assert.doesNotMatch(result.stdout, /verbose internal line/);
  assert.ok(result.stdout.trim().split('\n').length <= 9);
  const log = result.stdout.match(/^log=(.+)$/m)?.[1];
  assert.ok(log);
  const fullOutput = fs.readFileSync(log, 'utf8');
  assert.match(fullOutput, /verbose internal line 001/);
  assert.match(fullOutput, /verbose internal line 100/);
});

test('canonical pipeline stages both platforms from one verified input and publishes only explicitly', () => {
  const orchestrator = fs.readFileSync(path.join(releaseRoot, 'stage-updates.sh'), 'utf8');
  const preparer = fs.readFileSync(path.join(releaseRoot, 'prepare-input.sh'), 'utf8');
  const sourcePreparer = fs.readFileSync(path.join(releaseRoot, 'prepare-source.sh'), 'utf8');
  const ship = fs.readFileSync(path.join(releaseRoot, 'ship.sh'), 'utf8');
  const publisher = fs.readFileSync(path.join(releaseRoot, 'publish-windows.sh'), 'utf8');
  const gate = fs.readFileSync(path.join(repoRoot, 'scripts', 'gate.sh'), 'utf8');
  const rendererPackage = JSON.parse(fs.readFileSync(path.join(repoRoot, 'desktop', 'renderer2', 'package.json'), 'utf8'));
  const macStage = fs.readFileSync(path.join(repoRoot, 'scripts', 'stage-macos-local-update.sh'), 'utf8');
  const windowsStage = fs.readFileSync(path.join(repoRoot, 'scripts', 'stage-windows-portable.sh'), 'utf8');
  const macPackage = fs.readFileSync(path.join(repoRoot, 'scripts', 'package-workass-macos.sh'), 'utf8');

  const preparePhase = orchestrator.indexOf('workass_release_run_phase prepare_release_input prepare_input');
  const platformPhases = orchestrator.indexOf('workass_release_run_parallel_pair stage_macos stage_macos stage_windows stage_windows');
  const verifyPhase = orchestrator.indexOf('workass_release_run_phase verify_candidates verify_candidate');
  assert.ok(preparePhase >= 0 && preparePhase < platformPhases);
  assert.ok(platformPhases < verifyPhase);
  assert.match(orchestrator, /if \[ -f "\$candidate\/receipt\.json" \]; then[\s\S]{0,160}verify_cached_candidate/);
  assert.match(orchestrator, /WORKASS_RELEASE_CANDIDATE_REUSED/);
  assert.match(orchestrator, /if \[ "\$publish" -eq 1 \]; then/);
  assert.match(orchestrator, /workass_release_run_phase verify_published_release verify_published_release/);
  assert.match(orchestrator, /publication=\$candidate\/publication\.json/);
  assert.match(orchestrator, /WORKASS_RELEASE_PHASE_LOG_DIR/);
  assert.doesNotMatch(orchestrator, /install-workass|update-worker|\.updcard|--clobber/);
  assert.match(orchestrator, /WORKASS_RELEASE_TOTAL status=passed seconds=\$release_seconds/);
  assert.doesNotMatch(orchestrator, /budget_seconds|TIME_BUDGET/);
  assert.ok(orchestrator.indexOf('publish_windows publish_windows') < orchestrator.indexOf('publish_macos publish_macos'));

  assert.equal((preparer.match(/scripts\/gate\.sh/g) || []).length, 1);
  assert.doesNotMatch(preparer, /sync-renderer2\.sh/);
  assert.match(preparer, /repository-gate\.mjs/);
  assert.match(preparer, /workass_release_run_phase repository_gate_cached verify_gate_receipt/);
  assert.match(preparer, /workass_release_run_phase repository_gate_receipt record_gate_receipt/);
  assert.match(preparer, /WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER=1/);
  assert.match(preparer, /WORKASS_GATE_FRESH=1/);
  assert.match(preparer, /cmd\/workass\/embedded\/dist\/\." "\$incoming\/renderer/);
  assert.match(preparer, /node "\$input_tool" create/);
  assert.match(preparer, /node "\$input_tool" verify/);
  assert.match(preparer, /workass_release_run_parallel_pair macos_daemon build_macos_daemon windows_daemon build_windows_daemon/);

  assert.ok(sourcePreparer.indexOf('npm run build') < sourcePreparer.indexOf('scripts/sync-renderer2.sh'));
  assert.match(sourcePreparer, /diff -qr desktop\/renderer2\/dist cmd\/workass\/embedded\/dist/);

  assert.equal((ship.match(/stage-updates\.sh/g) || []).length, 1);
  assert.match(ship, /--publish/);
  assert.doesNotMatch(ship, /codesign|unzip|shasum|verify-publication|__workass-shell|updcard/);

  assert.match(publisher, /--verify-only/);
  assert.match(publisher, /windows-publication/);

  const rendererBuild = gate.indexOf('npm run build --silent');
  const rendererTests = gate.indexOf('npm test --silent');
  const shellTests = gate.indexOf('node --test desktop/shell/*.test.js');
  const rendererSnapshot = gate.indexOf('WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER');
  const goBuild = gate.indexOf('go build ./...');
  assert.equal(rendererPackage.scripts.test, 'node --experimental-strip-types --test tests/*.test.ts');
  assert.equal(rendererPackage.scripts.benchmark, 'node --experimental-strip-types --test tests/*.bench.ts');
  assert.ok(rendererTests >= 0 && rendererTests < goBuild);
  assert.ok(shellTests >= 0 && shellTests < goBuild);
  assert.ok(rendererBuild >= 0 && rendererBuild < goBuild);
  assert.ok(rendererSnapshot >= 0 && rendererSnapshot < goBuild);
  assert.match(gate, /if \[ "\$\{WORKASS_GATE_FRESH:-0\}" = 1 \]; then[\s\S]*go test \.\/\.\.\. -count=1 -p=2 -parallel=2[\s\S]*else[\s\S]*go test \.\/\.\.\. -p=2 -parallel=2/);

  assert.match(macStage, /release-input\.mjs" verify/);
  assert.match(macStage, /--renderer-root "\$release_input\/renderer"/);
  assert.match(windowsStage, /release-input\.mjs" verify/);
  assert.doesNotMatch(windowsStage, /skip-build/);
  assert.match(windowsStage, /git -C "\$repo_root" rev-parse HEAD/);
  assert.match(macPackage, /using verified release renderer/);

  for (const name of ['Workass-$version-windows-amd64.zip', 'workass-windows-amd64-release.json', 'SHA256SUMS']) {
    assert.ok(publisher.includes(name));
  }
  assert.match(publisher, /remote\.digest !== digest/);
  assert.match(publisher, /releases\/latest\/download/);
  assert.doesNotMatch(publisher, /--clobber/);
});
