import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { releaseRoot, repoRoot, run, write } from './release-pipeline-helpers.mjs';

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
