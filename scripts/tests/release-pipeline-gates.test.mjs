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
