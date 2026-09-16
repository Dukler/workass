'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const test = require('node:test');

const repo = path.resolve(__dirname, '../..');
const checker = path.join(repo, 'desktop/scripts/check-shell-dependencies.cjs');
const scripts = [
  path.join(repo, 'scripts/package-workass-macos.sh'),
  path.join(repo, 'scripts/stage-windows-portable.sh'),
];

function listedShellFiles(script) {
  const source = fs.readFileSync(script, 'utf8');
  const match = source.match(/for shell_file in ([^;]+); do/);
  assert.ok(match, `shell_file list missing in ${script}`);
  return match[1].trim().split(/\s+/u);
}

function stage(script, root) {
  const destination = path.join(root, path.basename(script));
  fs.mkdirSync(destination, { recursive: true });
  for (const name of listedShellFiles(script)) fs.copyFileSync(path.join(repo, 'desktop/shell', name), path.join(destination, name));
  // The checker follows the shell's relative require graph from this exact
  // staged directory, matching the package layout used by both scripts.
  return destination;
}

test('both packaging shell lists include every local dependency', () => {
  assert.ok(fs.existsSync(checker), 'dependency checker is required by this regression test');
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-shell-deps-'));
  try {
    for (const script of scripts) {
      const staged = stage(script, root);
      const result = spawnSync(process.execPath, [checker, staged], { encoding: 'utf8' });
      assert.equal(result.status, 0, `${script}\n${result.stdout}\n${result.stderr}`);
    }
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});

test('checker reports a missing connected-artifacts module', () => {
  assert.ok(fs.existsSync(checker), 'dependency checker is required by this regression test');
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'workass-shell-deps-missing-'));
  try {
    const staged = stage(scripts[0], root);
    fs.rmSync(path.join(staged, 'connected-artifacts.js'));
    const result = spawnSync(process.execPath, [checker, staged], { encoding: 'utf8' });
    assert.notEqual(result.status, 0);
    assert.match(`${result.stdout}\n${result.stderr}`, /main\.js requires \.\/connected-artifacts/u);
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});
