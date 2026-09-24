import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

export const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
export const releaseRoot = path.join(repoRoot, 'scripts', 'release');
export const inputTool = path.join(releaseRoot, 'lib', 'release-input.mjs');
export const candidateTool = path.join(releaseRoot, 'lib', 'verify-candidate.mjs');
export const publicationTool = path.join(releaseRoot, 'lib', 'verify-publication.mjs');
export const nextVersionTool = path.join(releaseRoot, 'lib', 'next-version.mjs');
export const gateReceiptTool = path.join(releaseRoot, 'lib', 'repository-gate.mjs');
export const commit = '1'.repeat(40);

export function run(command, args, options = {}) {
  return spawnSync(command, args, { encoding: 'utf8', ...options });
}

export function write(file, contents = file) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, contents);
}

export function sha256(contents) {
  return crypto.createHash('sha256').update(contents).digest('hex');
}

export function fileArtifact(file) {
  const bytes = fs.readFileSync(file);
  return { name: path.basename(file), sha256: sha256(bytes), size: bytes.length };
}

export function makeReleaseInput(root, version = '1.2.3') {
  for (const relative of [
    'renderer/index.html',
    'macos/runtime/workass',
    'macos/runtime/workass-tools',
    'macos/electron/darwin-arm64/Electron.app/Contents/MacOS/Electron',
    'macos/runtime/node/darwin-arm64/bin/node',
    'macos/runtime/frontier-hosts/darwin-arm64/claude-native-host.mjs',
    'windows/runtime/workass-daemon.exe',
    'windows/runtime/workass-tools.exe',
    'windows/electron/win32-x64/electron.exe',
    'windows/runtime/node/windows-amd64/node.exe',
    'windows/runtime/frontier-hosts/windows-amd64/codex-native-host.mjs',
  ]) write(path.join(root, ...relative.split('/')), relative);
  const created = run(process.execPath, [inputTool, 'create', '--root', root, '--version', version, '--commit', commit]);
  assert.equal(created.status, 0, created.stderr);
}
