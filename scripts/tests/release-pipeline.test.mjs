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

test('timing helper reports pass and preserves a failing command status', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-timing-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const timingFile = path.join(root, 'timings.log');
  const helper = path.join(releaseRoot, 'lib', 'timing.sh');
  const shell = `
    . "$1"
    WORKASS_RELEASE_TIMING_FILE="$2"
    export WORKASS_RELEASE_TIMING_FILE
    pass_phase() { :; }
    fail_phase() { return 7; }
    workass_release_run_phase contract_pass pass_phase
    if workass_release_run_phase contract_fail fail_phase; then exit 9; else [ "$?" -eq 7 ]; fi
  `;
  const result = run('sh', ['-c', shell, 'release-timing-test', helper, timingFile]);
  assert.equal(result.status, 0, result.stderr);
  const timing = fs.readFileSync(timingFile, 'utf8');
  assert.match(timing, /name=contract_pass status=passed seconds=\d+/);
  assert.match(timing, /name=contract_fail status=failed seconds=\d+/);
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

test('canonical pipeline stages both platforms from one verified input and publishes only explicitly', () => {
  const orchestrator = fs.readFileSync(path.join(releaseRoot, 'stage-updates.sh'), 'utf8');
  const preparer = fs.readFileSync(path.join(releaseRoot, 'prepare-input.sh'), 'utf8');
  const publisher = fs.readFileSync(path.join(releaseRoot, 'publish-windows.sh'), 'utf8');
  const macStage = fs.readFileSync(path.join(repoRoot, 'scripts', 'stage-macos-local-update.sh'), 'utf8');
  const windowsStage = fs.readFileSync(path.join(repoRoot, 'scripts', 'stage-windows-portable.sh'), 'utf8');
  const macPackage = fs.readFileSync(path.join(repoRoot, 'scripts', 'package-workass-macos.sh'), 'utf8');

  const preparePhase = orchestrator.indexOf('workass_release_run_phase prepare_release_input prepare_input');
  const macPhase = orchestrator.indexOf('workass_release_run_phase stage_macos stage_macos');
  const windowsPhase = orchestrator.indexOf('workass_release_run_phase stage_windows stage_windows');
  const verifyPhase = orchestrator.indexOf('workass_release_run_phase verify_candidates verify_candidate');
  assert.ok(preparePhase >= 0 && preparePhase < macPhase);
  assert.ok(macPhase < windowsPhase);
  assert.ok(windowsPhase < verifyPhase);
  assert.match(orchestrator, /if \[ -f "\$candidate\/receipt\.json" \]; then[\s\S]{0,160}verify_cached_candidate/);
  assert.match(orchestrator, /WORKASS_RELEASE_CANDIDATE_REUSED/);
  assert.match(orchestrator, /if \[ "\$publish" -eq 1 \]; then/);
  assert.doesNotMatch(orchestrator, /install-workass|update-worker|\.updcard|--clobber/);
  assert.match(orchestrator, /WORKASS_RELEASE_TOTAL status=passed seconds=\$release_seconds/);
  assert.doesNotMatch(orchestrator, /budget_seconds|TIME_BUDGET/);
  assert.ok(orchestrator.indexOf('publish_windows publish_windows') < orchestrator.indexOf('publish_macos publish_macos'));

  assert.equal((preparer.match(/scripts\/gate\.sh/g) || []).length, 1);
  assert.equal((preparer.match(/sync-renderer2\.sh/g) || []).length, 1);
  assert.match(preparer, /node "\$input_tool" create/);
  assert.match(preparer, /node "\$input_tool" verify/);

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
