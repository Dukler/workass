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

test('Windows verify-only mode performs readback without a release mutation', (t) => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-windows-readback-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const devState = path.join(repoRoot, '.dev');
  fs.mkdirSync(devState, { recursive: true });
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
