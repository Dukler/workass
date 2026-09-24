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
