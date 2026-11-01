import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { releaseRoot, repoRoot, run, write } from './release-pipeline-helpers.mjs';

const publishingStage = `#!/bin/sh
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
FLAKE_LINE
printf 'publication=%s\\n' "$publication"
`;

function shipOnce(t, stage) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-release-ship-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const remote = path.join(root, 'remote.git');
  const repo = path.join(root, 'repo');
  const bin = path.join(root, 'bin');
  const feed = path.join(root, 'feed');
  const localRelease = path.join(repo, 'scripts', 'release');
  assert.equal(run('git', ['init', '--bare', remote]).status, 0);
  assert.equal(run('git', ['init', '-b', 'main', repo]).status, 0);
  for (const relative of ['ship.sh', 'lib/source-state.sh', 'lib/next-version.mjs', 'lib/failure-summary.mjs']) {
    const source = path.join(releaseRoot, relative);
    const target = path.join(localRelease, relative);
    write(target, fs.readFileSync(source));
    fs.chmodSync(target, 0o755);
  }
  write(path.join(repo, '.gitignore'), '.dev/\n');
  write(path.join(localRelease, 'stage-updates.sh'), stage);
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

  return run('sh', [path.join(localRelease, 'ship.sh'), '--macos-output', feed], {
    cwd: repo,
    env: { ...process.env, PATH: `${bin}:${process.env.PATH}` },
  });
}

test('ship is one quiet command from clean pushed main to one final receipt', (t) => {
  const result = shipOnce(t, publishingStage.replace('FLAKE_LINE\n', ''));
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

test('a published release still names a test that only passed its rerun', (t) => {
  const flake = 'WORKASS_TEST_FLAKE name=TestSlow suite=go_tests where=workass/internal/acp first=failed rerun_seconds=1.000 cause=slow log=/l';
  const result = shipOnce(t, publishingStage.replace('FLAKE_LINE', `echo '${flake}'`));
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /WORKASS_RELEASE_PUBLISHED/);
  assert.ok(result.stdout.trim().split('\n').includes(flake));
});

test('a failed release prints the failing tests and the log, not its passing lines', (t) => {
  const result = shipOnce(t, `#!/bin/sh
line=1
while [ "$line" -le 40 ]; do printf 'WORKASS_TEST_SUITE name=suite_%03d status=passed exit=0\\n' "$line"; line=$((line + 1)); done
echo 'WORKASS_TEST_FAILED name=TestBroken suite=go_tests where=workass/internal/acp rerun=failed_again cause="wanted 1, got 2" log=/l'
echo 'WORKASS_TEST_SUITE_TOTAL correctness=failed performance=over_budget seconds=9.000'
exit 3
`);
  assert.equal(result.status, 3);
  const lines = result.stderr.trim().split('\n');
  assert.match(lines[0], /^WORKASS_RELEASE_FAILED version=0\.1\.94 log=.+\.log$/);
  assert.match(lines[1], /^WORKASS_TEST_FAILED name=TestBroken .*cause="wanted 1, got 2"/);
  assert.equal(lines.length, 2);
  assert.doesNotMatch(result.stderr, /status=passed/);
});

test('a failed release with no test failure still shows its cause', (t) => {
  const result = shipOnce(t, `#!/bin/sh
echo 'WORKASS_GATE_PHASE name=go_build status=passed seconds=1'
echo 'codesign: identity not found'
exit 4
`);
  assert.equal(result.status, 4);
  assert.match(result.stderr, /codesign: identity not found/);
  assert.doesNotMatch(result.stderr, /status=passed/);
});
