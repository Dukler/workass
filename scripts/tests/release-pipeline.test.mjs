import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import {
  candidateTool,
  commit,
  fileArtifact,
  gateReceiptTool,
  inputTool,
  makeReleaseInput,
  nextVersionTool,
  publicationTool,
  releaseRoot,
  repoRoot,
  run,
  sha256,
  write,
} from './release-pipeline-helpers.mjs';

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
    'workass-tools.exe',
    'resources/renderer/index.html',
    'node/windows-amd64/node.exe',
    'frontier-hosts/windows-amd64/workass-tools.mjs',
    'frontier-hosts/windows-amd64/workass-tools.cmd',
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
  assert.equal((preparer.match(/go build -buildvcs=false -trimpath/g) || []).length, 0);
  assert.match(preparer, /workass-tools\.exe/);
  assert.match(windowsStage, /GOOS=windows GOARCH=amd64 go build -trimpath/);
  assert.match(windowsStage, /workass-tools-windows-amd64\.exe/);

  assert.ok(sourcePreparer.indexOf('npm run build') < sourcePreparer.indexOf('scripts/sync-renderer2.sh'));
  assert.match(sourcePreparer, /diff -qr desktop\/renderer2\/dist cmd\/workass\/embedded\/dist/);

  assert.equal((ship.match(/stage-updates\.sh/g) || []).length, 1);
  assert.match(ship, /--publish/);
  assert.doesNotMatch(ship, /codesign|unzip|shasum|verify-publication|__workass-shell|updcard/);

  assert.match(publisher, /--verify-only/);
  assert.match(publisher, /windows-publication/);

  const rendererBuild = gate.indexOf('npm run build --silent');
  const rendererSnapshot = gate.indexOf('WORKASS_GATE_REQUIRE_EMBEDDED_RENDERER');
  const goBuild = gate.indexOf('go build ./...');
  const suiteRunner = gate.indexOf('test_suites node scripts/test-suite.mjs');
  assert.equal(rendererPackage.scripts.test, 'node --experimental-strip-types --test tests/*.test.ts');
  assert.equal(rendererPackage.scripts.benchmark, 'node --experimental-strip-types --test tests/*.bench.ts');
  assert.ok(suiteRunner > goBuild);
  assert.match(gate, /renderer_prepare sh -c 'cd desktop\/renderer2 && npx tsc --noEmit && npm run build --silent/);
  assert.ok(rendererBuild >= 0 && rendererBuild < goBuild);
  assert.ok(rendererSnapshot >= 0 && rendererSnapshot < goBuild);
  assert.match(gate, /run_gate_phase go_build go build \.\/\.\.\./);
  assert.match(gate, /run_gate_phase go_vet go vet \.\/\.\.\./);
  const suiteSource = fs.readFileSync(path.join(repoRoot, 'scripts', 'test-suite.mjs'), 'utf8');
  assert.match(suiteSource, /scripts\/test-go-suite\.mjs/);
  assert.match(suiteSource, /args: \[path\.join\(repo, 'scripts\/test-go-suite\.mjs'\), '--cwd', repo\]/);
  assert.match(suiteSource, /path\.join\(repo, 'scripts\/tests'\)/);

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
