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

// makeMacCandidate and makeWindowsCandidate stage the two platforms the
// candidate verifier already checked, so a Linux-focused test can build one
// complete candidate without restating either of them.
export function makeMacCandidate(candidate, version, build) {
  const archiveName = `Workass-${version}-darwin-arm64.zip`;
  const archiveBytes = Buffer.from('mac archive');
  const feedRoot = path.join(candidate, 'macos-feed');
  write(path.join(feedRoot, archiveName), archiveBytes);
  write(path.join(feedRoot, 'workass-darwin-arm64-release.json'), `${JSON.stringify({
    schemaVersion: 1,
    product: 'Workass',
    version,
    build: Number(build),
    platform: 'darwin',
    arch: 'arm64',
    designatedRequirement: 'identifier "com.workass.app"',
    artifacts: { update: { name: archiveName, url: archiveName, sha256: sha256(archiveBytes), size: archiveBytes.length } },
  })}\n`);
  write(path.join(feedRoot, 'SHA256SUMS'), `${sha256(archiveBytes)}  ${archiveName}\n`);
  const app = path.join(candidate, 'macos-app', 'Workass.app', 'Contents', 'Resources');
  write(path.join(app, 'app', 'package.json'), JSON.stringify({ version }));
  write(path.join(app, 'runtime', 'manifest.json'), JSON.stringify({ version, build, platform: 'darwin', arch: 'arm64' }));
}

export function makeWindowsCandidate(candidate, version) {
  const root = path.join(candidate, 'windows', version);
  const bundleName = `Workass-${version}-windows-amd64`;
  const archiveBytes = Buffer.from('windows archive');
  write(path.join(root, `${bundleName}.zip`), archiveBytes);
  write(path.join(root, 'workass-windows-amd64-release.json'), `${JSON.stringify({
    schemaVersion: 1,
    product: 'Workass',
    version,
    platform: 'windows',
    arch: 'amd64',
    portable: true,
    authenticode: false,
    artifacts: { update: {
      name: `${bundleName}.zip`,
      url: `https://github.com/Dukler/workass/releases/download/v${version}/${bundleName}.zip`,
      sha256: sha256(archiveBytes),
      size: archiveBytes.length,
    } },
  })}\n`);
  write(path.join(root, 'SHA256SUMS'), `${sha256(archiveBytes)}  ${bundleName}.zip\n`);
  const bundle = path.join(root, bundleName);
  write(path.join(bundle, 'resources', 'app', 'package.json'), JSON.stringify({ version }));
  write(path.join(bundle, 'manifest.json'), JSON.stringify({ version, platform: 'windows', arch: 'amd64', revision: commit }));
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
  ]) write(path.join(bundle, ...relative.split('/')), relative);
}

// makeLinuxCandidate stages the headless Linux half of a release candidate:
// one tar.gz, its own checksum file, its per-platform feed, and the extracted
// bundle the verifier walks file by file.
export function makeLinuxCandidate(candidate, version, commitSha, {
  archiveBytes = Buffer.from('linux archive'),
  feed: feedOverride = null,
} = {}) {
  const root = path.join(candidate, 'linux', version);
  const bundleName = `Workass-${version}-linux-amd64`;
  const archiveName = `${bundleName}.tar.gz`;
  write(path.join(root, archiveName), archiveBytes);
  const feed = feedOverride ?? {
    schemaVersion: 1,
    product: 'Workass',
    version,
    platform: 'linux',
    arch: 'amd64',
    headless: true,
    service: 'systemd-user',
    artifacts: { update: {
      name: archiveName,
      url: `https://github.com/Dukler/workass/releases/download/v${version}/${archiveName}`,
      sha256: sha256(archiveBytes),
      size: archiveBytes.length,
    } },
  };
  write(path.join(root, 'workass-linux-amd64-release.json'), `${JSON.stringify(feed)}\n`);
  write(path.join(root, 'SHA256SUMS-linux-amd64'), `${sha256(archiveBytes)}  ${archiveName}\n`);
  const bundle = path.join(root, bundleName);
  write(path.join(bundle, 'manifest.json'),
    JSON.stringify({ version, platform: 'linux', arch: 'amd64', headless: true, revision: commitSha }));
  for (const relative of [
    'workass',
    'workass-tools',
    'workass-agent',
    'node/linux-amd64/bin/node',
    'frontier-hosts/linux-amd64/claude-native-host.mjs',
    'frontier-hosts/linux-amd64/codex-native-host.mjs',
    'frontier-hosts/linux-amd64/pi-native-host.mjs',
    'frontier-hosts/linux-amd64/omp-native-host.mjs',
    'frontier-hosts/linux-amd64/node_modules/@anthropic-ai/claude-agent-sdk/sdk.mjs',
  ]) write(path.join(bundle, ...relative.split('/')), relative);
  return { root, bundle, archiveName, archivePath: path.join(root, archiveName) };
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
    'linux/runtime/workass',
    'linux/runtime/workass-tools',
    'linux/runtime/workass-agent',
    'linux/runtime/node/linux-amd64/bin/node',
    'linux/runtime/frontier-hosts/linux-amd64/pi-native-host.mjs',
  ]) write(path.join(root, ...relative.split('/')), relative);
  const created = run(process.execPath, [inputTool, 'create', '--root', root, '--version', version, '--commit', commit]);
  assert.equal(created.status, 0, created.stderr);
}
